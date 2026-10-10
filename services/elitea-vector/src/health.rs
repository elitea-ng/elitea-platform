//! The probe listener: plain HTTP on its own port, so a kubelet or a compose
//! health check needs no client certificate.
//!
//! * `GET /healthz` answers 200 while the process serves.
//! * `GET /readyz` answers 200 when Qdrant answers its health check, else
//!   503.
//!
//! It carries no data and reads at most one small request per connection.

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};

use crate::store::Store;

const MAX_REQUEST_BYTES: usize = 1024;
const IO_TIMEOUT: Duration = Duration::from_secs(5);

/// Serves probes on `listener` until the task is dropped.
pub async fn serve(listener: TcpListener, store: Arc<Store>) {
    loop {
        let Ok((stream, _)) = listener.accept().await else {
            continue;
        };
        let store = Arc::clone(&store);
        tokio::spawn(async move {
            let _ = tokio::time::timeout(IO_TIMEOUT, answer(stream, &store)).await;
        });
    }
}

async fn answer(mut stream: TcpStream, store: &Store) -> std::io::Result<()> {
    let mut buffer = vec![0_u8; MAX_REQUEST_BYTES];
    let mut read = 0;
    while read < buffer.len() {
        let count = stream.read(&mut buffer[read..]).await?;
        if count == 0 {
            break;
        }
        read += count;
        if buffer[..read]
            .windows(4)
            .any(|window| window == b"\r\n\r\n")
        {
            break;
        }
    }
    let line = buffer[..read]
        .split(|byte| *byte == b'\r' || *byte == b'\n')
        .next()
        .unwrap_or_default();
    let (status, body) = match line {
        b"GET /healthz HTTP/1.1" | b"GET /healthz HTTP/1.0" => ("200 OK", "ok\n"),
        b"GET /readyz HTTP/1.1" | b"GET /readyz HTTP/1.0" => {
            if store.ready().await {
                ("200 OK", "ready\n")
            } else {
                ("503 Service Unavailable", "qdrant unavailable\n")
            }
        }
        _ => ("404 Not Found", "not found\n"),
    };
    let response = format!(
        "HTTP/1.1 {status}\r\ncontent-type: text/plain\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    );
    stream.write_all(response.as_bytes()).await?;
    stream.shutdown().await
}
