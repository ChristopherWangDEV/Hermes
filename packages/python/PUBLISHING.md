# 发布到 PyPI

**包名与引用名一致**：`pip install hermes_sdk` → `import hermes`。
PyPI 会把 `_` 规范化成 `-` 做唯一性比较，所以**显示名 `hermes`，规范名 `hermes-py`**。

## 为什么不是 hermes / pyhermes / hermes_py

| 想用的名字 | 状态 |
|---|---|
| `hermes` | 已被占用（0.10.0，科研软件发布工具） |
| `pyhermes` | 已被占用（0.7.1，Hermes 消息中间件的 Python 接口） |
| `hermes_py` | 已被占用（0.2.0，且 PyPI 把 `_`/`-` 视为同一名字） |
| **`hermes`** | ✅ 可用 |

## 已发布

`0.1.0` 于 2026-11-04 上传：<https://pypi.org/project/hermes-sdk/0.1.0/>

**0.1.0 没有 `__version__`**（发布后才加上，会随 0.1.1 生效）。
用户问版本时，让他们用 `pip show hermes_sdk`。

## 构建

```sh
python -m build            # 产出 dist/*.whl 与 dist/*.tar.gz
twine check dist/*         # 元数据与 README 渲染检查
```

## 上传

```sh
twine upload dist/*                          # 交互输入 __token__
# 或
TWINE_USERNAME=__token__ TWINE_PASSWORD="pypi-…" twine upload dist/*
```

令牌从 <https://pypi.org/manage/account/token/> 生成，**只给一次**，丢了只能重发。
建议先传 TestPyPI 演练：

```sh
twine upload --repository testpypi dist/*
pip install -i https://test.pypi.org/simple/ hermes
```

## 发布前必须确认的三件事

1. **版本号**：`pyproject.toml` 的 `version`。PyPI 不允许覆盖已发布版本 —— 传错了只能发新版本。
2. **零依赖**：`dependencies = []` 不能变。`tests/guard/sdk_redline_test.go` 会检查。
3. **README 能渲染**：`twine check` 通过（含长描述）。

## 用户装上之后

SDK 本身不含运行时。两种用法各需一样东西：

- **进程内绑定**：需要本机有 Hermes 的 `libhermesrt`，用 `HERMES_HOME` 指向安装根，
  或让它在 `lib/` / `build/` 下被找到。找不到时错误信息会列出**试过的每条路径**。
- **HTTP 客户端**：只要能连上 `hermes serve`，什么都不需要。
