//! 对着**假服务端**验线格式。假服务端用 std 的 TcpListener 手写，
//! 与 SDK 本身保持同样的"零依赖"约束。
use hermes_sdk::Client;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;
use std::sync::mpsc;
use std::thread;

/// 起一个假服务端，返回 (地址, 收到的请求文本)。
fn fake(response: &'static str) -> (String, mpsc::Receiver<String>) {
    let l = TcpListener::bind("127.0.0.1:0").unwrap();
    // 客户端要求带 scheme（它自己解析，不用 url crate）——测试也照这个契约来。
    let addr = format!("http://127.0.0.1:{}", l.local_addr().unwrap().port());
    let (tx, rx) = mpsc::channel();
    thread::spawn(move || {
        if let Ok((mut s, _)) = l.accept() {
            let mut reader = BufReader::new(s.try_clone().unwrap());
            let mut req = String::new();
            let mut content_length = 0usize;
            loop {
                let mut line = String::new();
                if reader.read_line(&mut line).unwrap_or(0) == 0 {
                    break;
                }
                if let Some(v) = line.to_ascii_lowercase().strip_prefix("content-length:") {
                    content_length = v.trim().parse().unwrap_or(0);
                }
                req.push_str(&line);
                if line == "\r\n" || line == "\n" {
                    break;
                }
            }
            let mut body = vec![0u8; content_length];
            if content_length > 0 {
                use std::io::Read;
                let _ = reader.read_exact(&mut body);
                req.push_str(&String::from_utf8_lossy(&body));
            }
            let _ = tx.send(req);
            let _ = s.write_all(response.as_bytes());
        }
    });
    (addr, rx)
}

#[test]
fn health_roundtrip() {
    let (addr, _rx) = fake("HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n{\"status\":\"ok\",\"n\":1}");
    let c = Client::new(&addr, "");
    let body = c.health().unwrap();
    assert!(body.contains("ok"));
}

#[test]
fn run_sends_explicit_input_and_bearer() {
    // 响应的 Content-Length 必须与实际字节数一致，否则客户端按长度截断后
    // 解析出的内容会是空串 —— 而这正是最容易写错的地方，所以动态算。
    let payload = r#"{"model":"triage","choices":[{"message":{"content":"ok"}}]}"#;
    let response: &'static str = Box::leak(
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{}",
            payload.len(),
            payload
        )
        .into_boxed_str(),
    );
    let (addr, rx) = fake(response);
    let c = Client::new(&addr, "s3cret");
    let r = c.run("triage", Some(r#"{"title":"x"}"#)).unwrap();
    assert_eq!(r.content, "ok");

    let req = rx.recv().unwrap();
    assert!(req.contains("Authorization: Bearer s3cret"), "缺 Bearer 头：{req}");
    assert!(req.contains(r#""input":{"title":"x"}"#), "input 没显式发送：{req}");
    assert!(!req.contains("messages"), "不该把 input 塞进 messages：{req}");
}

#[test]
fn streaming_yields_deltas() {
    let body = concat!(
        "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n",
        "data: [DONE]\n\n"
    );
    let response: &'static str = Box::leak(
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\n\r\n{}",
            body.len(),
            body
        )
        .into_boxed_str(),
    );
    let (addr, _rx) = fake(response);
    let c = Client::new(&addr, "");
    let mut got = String::new();
    c.chat_stream(r#"[{"role":"user","content":"hi"}]"#, "", |piece| {
        got.push_str(piece);
        true
    })
    .unwrap();
    assert_eq!(got, "你好");
}

#[test]
fn https_is_rejected_with_a_reason() {
    let c = Client::new("https://example.invalid", "");
    let err = c.health().unwrap_err();
    // 文案在 2.0.0 改成默认英文，所以断言不应绑定某一种语言：
    // 要确认的是"说了为什么"，而不是"用了哪个词"。
    let m = err.message.as_str();
    assert!(
        m.contains("http://") && (m.contains("only") || m.contains("只支持") || m.contains("TLS")),
        "错误应说明原因：{err}"
    );
}
