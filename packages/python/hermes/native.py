"""Hermes 运行时的 **进程内** 绑定（像 PyQt 封装 Qt 那样）。

与 `http` 客户端的区别：这里不经过网络、不需要起 `hermes serve`。
它用 `ctypes` 加载 `libhermesrt` 动态库，直接跑 `.hbc` / IR。

为什么用 ctypes：它是标准库。PyQt 之所以存在，是因为 Qt 是 C++ 且没有标准 FFI；
而 Hermes 的运行时导出的是**纯 C ABI**（`hermes_rt_*`），ctypes 刚好够，
不必为了"看起来像原生扩展"去引 sip/pybind11 那种构建依赖。

边界（诚实说明）：
  · 只覆盖**执行**（run）；`serve` 的模型/健康端点在进程内没有对应物。
  · 失败一律抛 `HermesError`，带原生层的返回码与错误文本。
"""

from __future__ import annotations

import ctypes
import json
import os
import platform
import sys
from typing import Any, Dict, Optional

__all__ = ["Runtime", "NativeRuntime", "HermesError", "find_library"]


class HermesError(RuntimeError):
    """原生层返回的错误。

    `code` 是 `hermes_rt_run_file` 的返回码：2 = 输入/文件问题，3 = 执行失败。
    保留它是因为"文件读不到"和"workflow 跑失败"的处置完全不同。
    """

    def __init__(self, message: str, code: int = 0) -> None:
        super().__init__(message)
        self.code = code


def _library_name() -> str:
    if sys.platform == "darwin":
        return "libhermesrt.dylib"
    if os.name == "nt":
        return "hermesrt.dll"
    return "libhermesrt.so"


def _candidates() -> list[str]:
    """按固定顺序找动态库：HERMES_HOME → 包旁边的 lib/ → 仓库布局。"""
    out: list[str] = []
    home = os.environ.get("HERMES_HOME")
    if home:
        # 发布包是 <root>/lib/；开发时 HERMES_HOME 常指向仓库根，库在 <root>/build/。
        # 两种都试，顺序固定，错误信息里会列出实际试过的路径。
        out.append(os.path.join(home, "lib", _library_name()))
        out.append(os.path.join(home, "build", _library_name()))
    here = os.path.dirname(os.path.abspath(__file__))
    # 开发布局：packages/python/hermes → 仓库根/build/
    out.append(os.path.join(here, "..", "..", "..", "build", _library_name()))
    # 发布包布局：<root>/lib/…（包被装到 <root>/python/ 旁边的形态）
    out.append(os.path.join(here, "..", "..", "lib", _library_name()))
    out.append(os.path.join(here, "..", "..", "..", "lib", _library_name()))
    return [os.path.normpath(p) for p in out]


def find_library(explicit: Optional[str] = None) -> str:
    """找到运行时动态库。

    找不到时给出**可操作的**信息（试过哪些路径），而不是一句 "not found"。
    """
    if explicit:
        if os.path.exists(explicit):
            return explicit
        raise HermesError(f"指定的库不存在: {explicit}")
    tried = _candidates()
    for p in tried:
        if os.path.exists(p):
            return p
    raise HermesError(
        "找不到运行时动态库 "
        + _library_name()
        + "。已尝试：\n  "
        + "\n  ".join(tried)
        + "\n发布包应当自带 lib/；开发时先跑 scripts/build.sh，"
        "或用 HERMES_HOME 指向安装根。"
    )


class Runtime:
    """一次加载、多次执行。

    典型用法：

        rt = Runtime("app.hbc")
        result = rt.run({"title": "打印机坏了"})
    """

    def __init__(self, unit: Optional[str] = None, library: Optional[str] = None) -> None:
        path = find_library(library)
        self.library_path = path
        self._lib = ctypes.CDLL(path)

        # 显式声明签名：不声明的话 ctypes 会把指针当 int 传，
        # 在 64 位平台上偶发崩溃，而错误信息完全对不上。
        self._lib.hermes_rt_run_file.argtypes = [
            ctypes.c_char_p,  # unit 路径
            ctypes.c_char_p,  # input JSON
            ctypes.c_size_t,  # input 长度
            ctypes.POINTER(ctypes.c_char_p),  # out
            ctypes.POINTER(ctypes.c_char_p),  # err
        ]
        self._lib.hermes_rt_run_file.restype = ctypes.c_int
        self._lib.hermes_rt_free.argtypes = [ctypes.c_char_p]
        self._lib.hermes_rt_free.restype = None

        self.unit = unit

    def run(
        self,
        input: Optional[Dict[str, Any]] = None,
        unit: Optional[str] = None,
    ) -> Any:
        """执行一个 unit（`.hbc` 或 IR 文件）。

        `unit` 省略时用构造时给的那个。`input` 是 dict；返回值是 workflow 的
        输出（字符串原样返回，对象/数组解析成 Python 值）。
        """
        target = unit or self.unit
        if not target:
            raise HermesError("没有指定 unit（构造时或调用时都要给一个）")
        if not os.path.exists(target):
            raise HermesError(f"unit 不存在: {target}", code=2)

        payload = b"" if input is None else json.dumps(input).encode("utf-8")
        out = ctypes.c_char_p()
        err = ctypes.c_char_p()
        code = self._lib.hermes_rt_run_file(
            target.encode("utf-8"), payload, len(payload), ctypes.byref(out), ctypes.byref(err)
        )
        try:
            if code != 0:
                message = err.value.decode("utf-8", "replace") if err.value else f"执行失败（code={code}）"
                raise HermesError(message, code=code)
            text = out.value.decode("utf-8") if out.value else ""
            # 原生层产出的是 C 字符串：能当 JSON 解就解，否则原样返回文本。
            try:
                return json.loads(text)
            except json.JSONDecodeError:
                return text
        finally:
            # 两块内存都由原生层分配，必须用它的 free 归还 —— 不能让 Python 的 GC 猜。
            if out.value:
                self._lib.hermes_rt_free(out)
            if err.value:
                self._lib.hermes_rt_free(err)


# 其它七个语言的绑定都叫 `NativeRuntime`；这里给同一件事一个别名，
# 让八份文档与示例的命名一致 —— 名字不一致会让"同一个概念"看起来像两种东西。
NativeRuntime = Runtime
