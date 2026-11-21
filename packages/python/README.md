# hermes_sdk (Python)

```sh
pip install hermes_sdk     # 下载名
```

```python
import hermes              # 引用名
```

两种用法，同一份 C ABI，零运行时依赖。

两种用法，同一份 C ABI。

## 进程内（像 PyQt 封装 Qt）

```python
from hermes import NativeRuntime   # `Runtime` 是同一个类的旧名

rt = Runtime("app.hbc")
print(rt.run({"title": "打印机坏了"}))   # 不起服务，不经过网络
```

用 `ctypes`（标准库）加载 `libhermesrt`；找不到库时会把**试过的路径**列出来。

## HTTP

```python
from hermes import Client

c = Client("http://127.0.0.1:8080", token="…")
print(c.models())
print(c.run("triage", {"title": "打印机坏了"}).content)
for piece in c.chat_stream([{"role": "user", "content": "你好"}]):
    print(piece, end="")
```

零运行时依赖。
