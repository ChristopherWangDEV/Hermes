"""进程内绑定（ctypes → libhermesrt 动态库）的测试。

这里不需要假服务端：它不经过网络。测试直接编一个 `.hbc`，然后用 ctypes 跑。
没有动态库 / 没有 hermes 可执行文件时跳过，并说明原因。
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from hermes.native import HermesError, Runtime, find_library  # noqa: E402

ROOT = Path(__file__).resolve().parents[3]


def hermes_bin():
    p = ROOT / "build" / "hermes"
    return str(p) if p.exists() else None


class TestNativeBinding(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        cls.unit = os.path.join(cls.tmp, "echo.hbc")
        src = os.path.join(cls.tmp, "echo.hermes")
        with open(src, "w", encoding="utf-8") as f:
            f.write(
                "export struct In { string text; }\n"
                'export workflow echo(input: In) entry -> string { return "echo: {input.text}"; }\n'
            )
        binp = hermes_bin()
        if not binp:
            raise unittest.SkipTest("没有 build/hermes（先跑 scripts/build.sh）")
        subprocess.run([binp, "build", src, "-o", cls.unit], check=True, capture_output=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def runtime(self):
        try:
            return Runtime(self.unit)
        except HermesError as e:
            raise unittest.SkipTest(str(e))

    def test_run_returns_workflow_output(self):
        self.assertEqual(self.runtime().run({"text": "hi"}), "echo: hi")

    def test_missing_unit_is_clear(self):
        rt = self.runtime()
        with self.assertRaises(HermesError):
            rt.run({}, unit=os.path.join(self.tmp, "nope.hbc"))

    def test_binding_does_not_need_a_server(self):
        # 进程内绑定不监听端口：能跑就说明没经过网络。
        self.assertEqual(self.runtime().run({"text": "no-server"}), "echo: no-server")


if __name__ == "__main__":
    unittest.main()
