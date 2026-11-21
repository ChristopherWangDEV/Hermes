"""对着一个**假服务端**验 SDK 的线格式。

为什么不用真 hermes：单元测试要保持快、无依赖；而线格式（请求体形状、
Authorization 头、SSE 解析）是可以独立验证的 —— 真服务端的端到端在
`tests/guard/sdk_e2e_test.go` 里做。
"""

import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from hermes import Client, HermesError  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    seen = {}

    def log_message(self, *args):  # 静音
        pass

    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        Handler.seen["auth"] = self.headers.get("Authorization")
        if self.path == "/health":
            return self._json(200, {"status": "ok", "workflows": 1})
        if self.path == "/v1/models":
            return self._json(200, {"data": [{"id": "triage", "owned_by": "hermes"}]})
        self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        Handler.seen["auth"] = self.headers.get("Authorization")
        n = int(self.headers.get("Content-Length", "0"))
        Handler.seen["body"] = json.loads(self.rfile.read(n).decode())
        if Handler.seen["body"].get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for piece in ["你", "好"]:
                chunk = {"choices": [{"delta": {"content": piece}}]}
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            return
        if Handler.seen["body"].get("model") == "boom":
            return self._json(503, {"error": {"message": "upstream failed", "type": "server_error"}})
        self._json(200, {
            "model": Handler.seen["body"].get("model", ""),
            "choices": [{"message": {"role": "assistant", "content": "urgent: 打印机坏了"}}],
        })


class TestClient(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), Handler)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def client(self, token=None):
        return Client(f"http://127.0.0.1:{self.port}", token=token)

    def test_health(self):
        self.assertEqual(self.client().health()["status"], "ok")

    def test_models(self):
        self.assertEqual(self.client().models(), ["triage"])

    def test_run_sends_explicit_input(self):
        r = self.client().run("triage", {"title": "打印机坏了"})
        self.assertEqual(r.content, "urgent: 打印机坏了")
        # input 必须是显式字段，不能被塞进 messages
        self.assertEqual(Handler.seen["body"]["input"], {"title": "打印机坏了"})

    def test_chat_falls_back_to_messages(self):
        self.client().chat([{"role": "user", "content": "hi"}])
        self.assertNotIn("input", Handler.seen["body"])
        self.assertEqual(Handler.seen["body"]["messages"][0]["content"], "hi")

    def test_token_is_sent_as_bearer(self):
        self.client(token="s3cret").health()
        self.assertEqual(Handler.seen["auth"], "Bearer s3cret")

    def test_stream_yields_deltas(self):
        self.assertEqual("".join(self.client().chat_stream([{"role": "user", "content": "hi"}])), "你好")

    def test_error_carries_status_and_type(self):
        with self.assertRaises(HermesError) as ctx:
            self.client().run("boom")
        self.assertEqual(ctx.exception.status, 503)
        self.assertEqual(ctx.exception.type, "server_error")


if __name__ == "__main__":
    unittest.main()
