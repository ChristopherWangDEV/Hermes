"""Hermes 运行时的 Python 客户端。

它说的是 `hermes serve` 的 HTTP 面（四个端点，与 OpenAI 同形，D-021），
所以**不需要安装 hermes、不需要 FFI、不需要编译任何东西** —— 只要能发 HTTP。

零运行时依赖：只用标准库。SDK 引第三方库，就等于把那棵依赖树带进每个使用者的
构建里，而这里要做的事（发请求、解析 JSON、读 SSE）标准库全都够。
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Dict, Iterator, List, Optional

from .native import NativeRuntime, Runtime, HermesError as NativeError, find_library

__all__ = [
    "Client",
    "HermesError",
    "ChatResult",
    "TOOL_MODEL",
    # 进程内绑定（像 PyQt 封装 Qt）：不经过网络，直接加载运行时动态库。
    "Runtime",
    "NativeRuntime",
    "find_library",
]

# `hermes serve` 里 workflow 名就是 model 名；这个保留名表示"用 entry workflow"。
TOOL_MODEL = ""


class HermesError(Exception):
    """服务端返回的错误。

    保留 `type` 与 `status`：调用方要能区分"没带凭证（401）"和
    "上游/预算出了问题（503）" —— 两者的处置完全不同。
    """

    def __init__(self, message: str, *, type: str = "", status: int = 0) -> None:
        super().__init__(message)
        self.type = type
        self.status = status


@dataclass
class ChatResult:
    """一次执行的结果。"""

    content: str
    model: str
    raw: Dict[str, Any]


class Client:
    """Hermes 服务端客户端。

    base_url 指向 `hermes serve --addr`；token 对应 `--token`。
    """

    def __init__(
        self,
        base_url: str = "http://127.0.0.1:8080",
        token: Optional[str] = None,
        timeout: float = 60.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    # ---- 四个端点 ----

    def health(self) -> Dict[str, Any]:
        return self._request("GET", "/health")

    def models(self) -> List[str]:
        data = self._request("GET", "/v1/models")
        return [m["id"] for m in data.get("data", [])]

    def run(self, workflow: str, input: Optional[Dict[str, Any]] = None) -> ChatResult:
        """执行一个 workflow。

        `input` 是**显式**的（非 OpenAI 标准）：workflow 的输入契约由 `.h` 里的
        struct 定义，把它映射到 messages 只能靠猜。传 `None` 表示这个 workflow
        不需要输入。
        """
        body: Dict[str, Any] = {"model": workflow}
        if input is not None:
            body["input"] = input
        raw = self._request("POST", "/v1/chat/completions", body)
        content = ""
        choices = raw.get("choices") or []
        if choices:
            content = (choices[0].get("message") or {}).get("content", "")
        return ChatResult(content=content, model=raw.get("model", workflow), raw=raw)

    def chat(self, messages: List[Dict[str, str]], model: str = TOOL_MODEL) -> ChatResult:
        """按 OpenAI 的 messages 形状调用。

        服务端会把**最后一条 user 消息**当成 `text` 传给 workflow —— 这只在
        没有显式 `input` 时发生，所以它适合"对话式"的 workflow。
        """
        body = {"model": model, "messages": messages}
        raw = self._request("POST", "/v1/chat/completions", body)
        content = ""
        choices = raw.get("choices") or []
        if choices:
            content = (choices[0].get("message") or {}).get("content", "")
        return ChatResult(content=content, model=raw.get("model", model), raw=raw)

    def chat_stream(
        self, messages: List[Dict[str, str]], model: str = TOOL_MODEL
    ) -> Iterator[str]:
        """流式调用：逐块产出文本增量。

        服务端是**真流式**（provider 层接上游 SSE），所以块与块之间可能有延迟，
        而第一个块通常最快 —— 这正是流式的意义。
        """
        body = {"model": model, "messages": messages, "stream": True}
        for line in self._post_stream(body):
            if not line.startswith("data:"):
                continue
            payload = line[len("data:") :].strip()
            if payload == "[DONE]":
                return
            chunk = json.loads(payload)
            if "error" in chunk:
                err = chunk["error"]
                raise HermesError(err.get("message", "stream error"), type=err.get("type", ""))
            for ch in chunk.get("choices") or []:
                text = (ch.get("delta") or {}).get("content")
                if text:
                    yield text

    # ---- 传输 ----

    def _headers(self) -> Dict[str, str]:
        h = {"Content-Type": "application/json"}
        if self.token:
            h["Authorization"] = "Bearer " + self.token
        return h

    def _request(self, method: str, path: str, body: Optional[Dict[str, Any]] = None) -> Any:
        data = None if body is None else json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            self.base_url + path, data=data, headers=self._headers(), method=method
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            raise self._error_from(e) from None

    def _post_stream(self, body: Dict[str, Any]) -> Iterator[str]:
        req = urllib.request.Request(
            self.base_url + "/v1/chat/completions",
            data=json.dumps(body).encode("utf-8"),
            headers=self._headers(),
            method="POST",
        )
        try:
            resp = urllib.request.urlopen(req, timeout=self.timeout)
        except urllib.error.HTTPError as e:
            raise self._error_from(e) from None
        with resp:
            for raw in resp:
                line = raw.decode("utf-8").rstrip("\r\n")
                if line:
                    yield line

    def _error_from(self, e: urllib.error.HTTPError) -> HermesError:
        try:
            payload = json.loads(e.read().decode("utf-8"))
            err = payload.get("error") or {}
            return HermesError(
                err.get("message", str(e)), type=err.get("type", ""), status=e.code
            )
        except Exception:
            return HermesError(str(e), status=e.code)

# 版本号与 pyproject.toml 的 version 必须一致：用户报 bug 时第一句话就是"你装的是哪个版本"，
# 而 `import hermes; hermes.__version__` 是最不需要翻文档的问法。
__version__ = "1.2.1"
