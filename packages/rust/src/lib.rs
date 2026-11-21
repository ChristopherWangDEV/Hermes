//! Hermes 运行时客户端（Rust，**零依赖**）。
//!
//! 它说 `hermes serve` 的 HTTP 面（四个端点，与 OpenAI 同形，D-021）。
//!
//! **诚实的边界**：只用标准库意味着自己实现 HTTP/1.1 客户端，因此只支持
//! `http://`。要连 `https://` 需要 TLS，那必须引入第三方 crate（rustls / native-tls），
//! 而"SDK 带一棵依赖树"正是我们想避免的。生产部署的常见形态是
//! `hermes serve` 监听本机回环，前面由网关终止 TLS —— 那种形态下 `http://` 就够。
//!
//! 如果你需要内置 TLS，请提出：那是一个明确的取舍（加依赖 vs 不支持直连 TLS），
//! 而不是遗漏。

pub mod native;

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;

/// 服务端错误。保留 `status`：401 与 503 的处置完全不同。
#[derive(Debug)]
pub struct Error {
    pub message: String,
    pub status: u16,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "hermes: {} (status={})", self.message, self.status)
    }
}

impl std::error::Error for Error {}

/// 一次执行的结果。
#[derive(Debug, Clone)]
pub struct ChatResult {
    pub content: String,
    pub model: String,
}

/// 客户端。
pub struct Client {
    /// 形如 `http://127.0.0.1:8080`（**只支持 http**，见模块注释）。
    pub base_url: String,
    pub token: String,
}

impl Client {
    pub fn new(base_url: &str, token: &str) -> Self {
        Self {
            base_url: base_url.trim_end_matches('/').to_string(),
            token: token.to_string(),
        }
    }

    /// 执行一个 workflow。`input` 是**显式**的（非 OpenAI 标准）。
    pub fn run(&self, workflow: &str, input: Option<&str>) -> Result<ChatResult, Error> {
        let body = match input {
            Some(json) => format!(
                r#"{{"model":{},"input":{}}}"#,
                json_string(workflow),
                json
            ),
            None => format!(r#"{{"model":{}}}"#, json_string(workflow)),
        };
        let raw = self.post("/v1/chat/completions", &body)?;
        Ok(parse_chat(&raw, workflow))
    }

    /// 存活。
    pub fn health(&self) -> Result<String, Error> {
        self.get("/health")
    }

    /// 可用的 workflow 名。
    pub fn models(&self) -> Result<Vec<String>, Error> {
        let raw = self.get("/v1/models")?;
        Ok(extract_models(&raw))
    }

    /// 流式调用：每个增量调用一次 `on_chunk`。
    ///
    /// 返回 `Err` 表示上游/协议出错；`on_chunk` 返回 `false` 表示调用方不要了，
    /// 此时**正常返回**（保留已收到的内容）—— 与其它语言 SDK 的语义一致。
    pub fn chat_stream<F>(&self, messages_json: &str, model: &str, mut on_chunk: F) -> Result<(), Error>
    where
        F: FnMut(&str) -> bool,
    {
        let body = format!(
            r#"{{"model":{},"messages":{},"stream":true}}"#,
            json_string(model),
            messages_json
        );
        let (status, text) = self.request("POST", "/v1/chat/completions", &body)?;
        if status / 100 != 2 {
            return Err(Error {
                message: extract_error_message(&text),
                status,
            });
        }
        for line in text.lines() {
            let line = line.trim();
            let Some(payload) = line.strip_prefix("data:") else {
                continue;
            };
            let payload = payload.trim();
            if payload == "[DONE]" {
                return Ok(());
            }
            if let Some(piece) = extract_delta(payload) {
                if !on_chunk(&piece) {
                    return Ok(());
                }
            }
        }
        Ok(())
    }

    // ---- 传输：手写 HTTP/1.1 ----

    fn post(&self, path: &str, body: &str) -> Result<String, Error> {
        let (status, text) = self.request("POST", path, body)?;
        if status / 100 != 2 {
            return Err(Error {
                message: extract_error_message(&text),
                status,
            });
        }
        Ok(text)
    }

    fn get(&self, path: &str) -> Result<String, Error> {
        let (status, text) = self.request("GET", path, "")?;
        if status / 100 != 2 {
            return Err(Error {
                message: extract_error_message(&text),
                status,
            });
        }
        Ok(text)
    }

    /// request 发一个请求，返回 (状态码, 响应体文本)。
    ///
    /// 只支持 http://，且不支持 chunked —— `hermes serve` 的流式响应带
    /// `Connection: keep-alive` 与逐块 flush，但**不带** chunked 编码，
    /// 所以按 Content-Length 读完即可；真流式由逐行解析提供。
    fn request(&self, method: &str, path: &str, body: &str) -> Result<(u16, String), Error> {
        let (scheme, rest) = self
            .base_url
            .split_once("://")
            .ok_or_else(|| Error {
                message: format!("base_url missing scheme: {}", self.base_url),
                status: 0,
            })?;
        if scheme != "http" {
            return Err(Error {
                message: format!(
                    "only http:// is supported (the cost of zero dependencies). To use https://, terminate TLS at a gateway or put a reverse proxy in front of serve"
                ),
                status: 0,
            });
        }
        let (auth, hostport) = match self.token.is_empty() {
            true => (String::new(), rest),
            false => (
                format!("Authorization: Bearer {}\r\n", self.token),
                rest,
            ),
        };
        let mut stream = TcpStream::connect(hostport).map_err(|e| Error {
            message: format!("failed to connect to {}: {e}", hostport),
            status: 0,
        })?;

        let req = format!(
            "{method} {path} HTTP/1.1\r\nHost: {hostport}\r\nContent-Type: application/json\r\n{auth}Content-Length: {len}\r\nConnection: close\r\n\r\n{body}",
            len = body.len()
        );
        stream
            .write_all(req.as_bytes())
            .map_err(|e| Error { message: e.to_string(), status: 0 })?;

        let mut reader = BufReader::new(stream);
        let mut status_line = String::new();
        reader
            .read_line(&mut status_line)
            .map_err(|e| Error { message: e.to_string(), status: 0 })?;
        let status: u16 = status_line
            .split_whitespace()
            .nth(1)
            .and_then(|s| s.parse().ok())
            .ok_or_else(|| Error {
                message: format!("response line not parseable: {status_line:?}"),
                status: 0,
            })?;

        let mut length: Option<usize> = None;
        loop {
            let mut line = String::new();
            reader
                .read_line(&mut line)
                .map_err(|e| Error { message: e.to_string(), status: 0 })?;
            let line = line.trim_end();
            if line.is_empty() {
                break;
            }
            if let Some(v) = line.to_ascii_lowercase().strip_prefix("content-length:") {
                length = v.trim().parse().ok();
            }
        }
        let mut body = String::new();
        match length {
            Some(n) => {
                let mut buf = vec![0u8; n];
                reader
                    .read_exact(&mut buf)
                    .map_err(|e| Error { message: e.to_string(), status: 0 })?;
                body.push_str(&String::from_utf8_lossy(&buf));
            }
            None => {
                let mut buf = Vec::new();
                reader
                    .read_to_end(&mut buf)
                    .map_err(|e| Error { message: e.to_string(), status: 0 })?;
                body.push_str(&String::from_utf8_lossy(&buf));
            }
        }
        Ok((status, body))
    }
}

/// json_string 把一个字符串安全地放进 JSON（转义反斜杠与引号）。
fn json_string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for ch in s.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// extract_string_field 取一个 JSON 对象里的字符串字段（够用即可，不引 serde）。
fn extract_string_field(json: &str, field: &str) -> Option<String> {
    let key = format!("\"{field}\":");
    let start = json.find(&key)? + key.len();
    let rest = json[start..].trim_start();
    let rest = rest.strip_prefix('"')?;
    let mut out = String::new();
    let mut chars = rest.chars();
    while let Some(c) = chars.next() {
        match c {
            '"' => return Some(out),
            '\\' => {
                if let Some(next) = chars.next() {
                    out.push(match next {
                        'n' => '\n',
                        't' => '\t',
                        'r' => '\r',
                        other => other,
                    });
                }
            }
            c => out.push(c),
        }
    }
    None
}

fn parse_chat(raw: &str, model: &str) -> ChatResult {
    ChatResult {
        content: extract_string_field(raw, "content").unwrap_or_default(),
        model: extract_string_field(raw, "model").unwrap_or_else(|| model.to_string()),
    }
}

fn extract_error_message(raw: &str) -> String {
    extract_string_field(raw, "message").unwrap_or_else(|| "(no error message)".to_string())
}

fn extract_delta(payload: &str) -> Option<String> {
    // `delta:{"content":"…"}` —— 只取 content，没有就返回 None。
    let idx = payload.find("\"content\":")?;
    extract_string_field(&payload[idx - 0..], "content")
}

fn extract_models(raw: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = raw;
    while let Some(i) = rest.find("\"id\":") {
        let after = &rest[i + 5..];
        if let Some(s) = extract_string_field(&format!("{{{}", after), "id") {
            out.push(s);
        }
        rest = &after[1..];
    }
    out
}
