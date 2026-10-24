//! JSON-RPC 2.0 over stdio, ACP's transport: one JSON object per line,
//! UTF-8, `\n`-terminated.
//!
//! The other end is an editor process, so every line is untrusted: a line
//! longer than [`MAX_LINE_BYTES`] is never buffered whole (it is skipped up
//! to its newline and answered with a parse error), a message that is not a
//! JSON-RPC 2.0 object is refused, and serde_json's own nesting limit
//! bounds the parse. The writer never puts a raw newline inside a message:
//! serde_json escapes them in strings.

use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;
use tokio::io::{AsyncBufRead, AsyncBufReadExt as _, AsyncWrite, AsyncWriteExt as _};

/// The longest line read. A prompt carries its images inline (base64), at
/// most five of at most 10 MiB each, which the attachments refuse past
/// their own bound; a line past this is refused before it is parsed.
pub const MAX_LINE_BYTES: usize = 24 * 1024 * 1024;

/// The longest method name accepted (ACP's are short; anything longer is
/// refused as an invalid request, not looked up).
const MAX_METHOD_BYTES: usize = 128;

pub const PARSE_ERROR: i32 = -32700;
pub const INVALID_REQUEST: i32 = -32600;
pub const METHOD_NOT_FOUND: i32 = -32601;
pub const INVALID_PARAMS: i32 = -32602;
pub const INTERNAL_ERROR: i32 = -32603;
/// ACP's `auth_required`.
pub const AUTH_REQUIRED: i32 = -32000;

/// A request id: a number or a string (`null` only on an error answering a
/// message whose id could not be read).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq, Hash)]
#[serde(untagged)]
pub enum Id {
    Number(i64),
    Text(String),
}

/// A JSON-RPC error object.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct RpcError {
    pub code: i32,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}

impl RpcError {
    pub fn new(code: i32, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            data: None,
        }
    }

    pub fn invalid_params(message: impl Into<String>) -> Self {
        Self::new(INVALID_PARAMS, message)
    }

    /// A refusal of the agent host, with its machine code as `data.code`.
    pub fn refused(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: INTERNAL_ERROR,
            message: message.into(),
            data: Some(serde_json::json!({ "code": code })),
        }
    }
}

/// One message read.
#[derive(Debug, PartialEq)]
pub enum Incoming {
    Request {
        id: Id,
        method: String,
        params: Value,
    },
    Notification {
        method: String,
        params: Value,
    },
    /// The answer to a request this side sent.
    Response {
        id: Id,
        result: Result<Value, RpcError>,
    },
}

/// The envelope as it arrives. `result` and `params` keep an explicit
/// `null` apart from an absent key.
#[derive(Deserialize)]
struct Envelope {
    jsonrpc: String,
    #[serde(default, deserialize_with = "present")]
    id: Option<Value>,
    #[serde(default)]
    method: Option<String>,
    #[serde(default, deserialize_with = "present")]
    params: Option<Value>,
    #[serde(default, deserialize_with = "present")]
    result: Option<Value>,
    #[serde(default)]
    error: Option<RpcError>,
}

fn present<'de, D: Deserializer<'de>>(deserializer: D) -> Result<Option<Value>, D::Error> {
    Value::deserialize(deserializer).map(Some)
}

/// A message that could not be taken, with the id to answer it under when
/// one could be read.
#[derive(Debug, PartialEq)]
pub struct Refused {
    pub id: Option<Id>,
    pub error: RpcError,
}

/// Parse one line.
///
/// # Errors
///
/// Not JSON, not a JSON-RPC 2.0 message, or an id or method of the wrong
/// shape.
pub fn parse(line: &[u8]) -> Result<Incoming, Refused> {
    let envelope: Envelope = serde_json::from_slice(line).map_err(|_| Refused {
        id: None,
        error: RpcError::new(PARSE_ERROR, "not a JSON-RPC message"),
    })?;
    let id = match envelope.id {
        None => None,
        Some(value) => Some(serde_json::from_value::<Id>(value).map_err(|_| Refused {
            id: None,
            error: RpcError::new(INVALID_REQUEST, "the id must be a number or a string"),
        })?),
    };
    let invalid = |message: &str| Refused {
        id: id.clone(),
        error: RpcError::new(INVALID_REQUEST, message),
    };
    if envelope.jsonrpc != "2.0" {
        return Err(invalid("jsonrpc must be \"2.0\""));
    }
    if let Some(method) = envelope.method {
        if method.is_empty() || method.len() > MAX_METHOD_BYTES {
            return Err(invalid("the method name is empty or too long"));
        }
        if envelope.result.is_some() || envelope.error.is_some() {
            return Err(invalid("a request carries no result or error"));
        }
        let params = envelope.params.unwrap_or(Value::Null);
        return Ok(match id {
            Some(id) => Incoming::Request { id, method, params },
            None => Incoming::Notification { method, params },
        });
    }
    let result = match (envelope.result, envelope.error) {
        (Some(result), None) => Ok(result),
        (None, Some(error)) => Err(error),
        _ => {
            return Err(invalid(
                "a response carries exactly one of result and error",
            ));
        }
    };
    match id {
        Some(id) => Ok(Incoming::Response { id, result }),
        None => Err(invalid("a response needs an id")),
    }
}

/// What reading one line gave.
#[derive(Debug, PartialEq)]
pub enum Line {
    Message(Vec<u8>),
    /// A line over the limit, skipped to its end.
    TooLong,
    /// The input ended.
    Eof,
}

/// Read one line of at most `max` bytes (without its `\n`). A longer line
/// is consumed to its newline without being kept. A last line without a
/// newline still counts.
///
/// # Errors
///
/// The input failed.
pub async fn read_line<R: AsyncBufRead + Unpin>(
    reader: &mut R,
    max: usize,
) -> std::io::Result<Line> {
    let mut line = Vec::new();
    let mut over = false;
    loop {
        let buffer = reader.fill_buf().await?;
        if buffer.is_empty() {
            return Ok(if over {
                Line::TooLong
            } else if line.is_empty() {
                Line::Eof
            } else {
                Line::Message(line)
            });
        }
        let (chunk, found) = match buffer.iter().position(|b| *b == b'\n') {
            Some(at) => (&buffer[..at], Some(at)),
            None => (buffer, None),
        };
        if !over {
            if line.len() + chunk.len() > max {
                over = true;
                line = Vec::new();
            } else {
                line.extend_from_slice(chunk);
            }
        }
        let used = found.map_or(buffer.len(), |at| at + 1);
        reader.consume(used);
        if found.is_some() {
            return Ok(if over {
                Line::TooLong
            } else {
                Line::Message(line)
            });
        }
    }
}

/// A request to send.
pub fn request(id: &Id, method: &str, params: &impl Serialize) -> Value {
    serde_json::json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params })
}

/// A notification to send.
pub fn notification(method: &str, params: &impl Serialize) -> Value {
    serde_json::json!({ "jsonrpc": "2.0", "method": method, "params": params })
}

/// A response to send.
pub fn response(id: Option<&Id>, result: Result<Value, RpcError>) -> Value {
    match result {
        Ok(result) => serde_json::json!({ "jsonrpc": "2.0", "id": id, "result": result }),
        Err(error) => serde_json::json!({ "jsonrpc": "2.0", "id": id, "error": error }),
    }
}

/// Write one message and its newline, then flush.
///
/// # Errors
///
/// The output failed.
pub async fn write_message<W: AsyncWrite + Unpin>(
    writer: &mut W,
    message: &Value,
) -> std::io::Result<()> {
    let mut line = serde_json::to_vec(message).map_err(std::io::Error::other)?;
    line.push(b'\n');
    writer.write_all(&line).await?;
    writer.flush().await
}
