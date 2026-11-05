//! 进程内绑定：用真动态库跑一个真 workflow。
use hermes_sdk::native::{find_library, NativeRuntime};
use std::path::Path;
use std::process::Command;

#[test]
fn runs_workflow_in_process() {
    let lib = match find_library() {
        Ok(p) => p,
        Err(e) => {
            eprintln!("跳过：{e}");
            return;
        }
    };
    let root = Path::new("../..");
    let bin = root.join("build/hermes");
    if !bin.exists() {
        eprintln!("跳过：没有 build/hermes");
        return;
    }
    let dir = std::env::temp_dir().join(format!("hermes-rs-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let src = dir.join("echo.hermes");
    std::fs::write(
        &src,
        "export struct In { string text; }\n\
         export workflow echo(input: In) entry -> string { return \"echo: {input.text}\"; }\n",
    )
    .unwrap();
    let hbc = dir.join("echo.hbc");
    let out = Command::new(&bin)
        .args(["build", src.to_str().unwrap(), "-o", hbc.to_str().unwrap()])
        .output()
        .unwrap();
    assert!(out.status.success(), "build 失败: {}", String::from_utf8_lossy(&out.stderr));

    let rt = NativeRuntime::open(hbc.to_str().unwrap(), Some(lib.to_str().unwrap())).unwrap();
    let got = rt.run(Some(r#"{"text":"hi"}"#)).unwrap();
    assert!(got.contains("echo: hi"), "输出不对: {got}");

    // 缺失 unit 必须报错并带 code。
    let err = rt.run_on(dir.join("nope.hbc").to_str().unwrap(), Some("{}")).unwrap_err();
    assert!(err.code != 0, "应带 code: {err}");
}
