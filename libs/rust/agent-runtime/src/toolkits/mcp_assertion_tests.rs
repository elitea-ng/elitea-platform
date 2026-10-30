use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_tool::mcp::rmcp::model::ClientJsonRpcMessage;
use adk_tool::mcp::rmcp::transport::streamable_http_client::StreamableHttpClient as _;
use async_trait::async_trait;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

use super::{ASSERTION_HEADER, AssertingClient};
use crate::remote_assertion::{
    ASSERTION_UNAVAILABLE, DelegationAssertionError, DelegationAssertionSource,
    DelegationAssertions, HEADER_NAME, MintedAssertion,
};

const TOKEN: &str = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln";
const ENDPOINT: &str = "https://partner.example/mcp";

struct Source {
    answer: Result<MintedAssertion, DelegationAssertionError>,
    calls: AtomicUsize,
}

#[async_trait]
impl DelegationAssertionSource for Source {
    async fn mint(&self, _audience: &str) -> Result<MintedAssertion, DelegationAssertionError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.answer.clone()
    }
}

fn source(answer: Result<MintedAssertion, DelegationAssertionError>) -> Arc<Source> {
    Arc::new(Source {
        answer,
        calls: AtomicUsize::new(0),
    })
}

fn issued() -> MintedAssertion {
    MintedAssertion::Issued {
        token: TOKEN.to_owned(),
        expires_at_unix_millis: i64::MAX / 2,
    }
}

#[test]
fn the_header_is_the_documented_one() {
    assert!(ASSERTION_HEADER.as_str().eq_ignore_ascii_case(HEADER_NAME));
}

#[tokio::test]
async fn every_request_carries_the_cached_assertion() {
    let minted = source(Ok(issued()));
    let client = AssertingClient::new(
        reqwest_mcp::Client::new(),
        DelegationAssertions::new(minted.clone(), true),
        ENDPOINT,
    );
    for _ in 0..2 {
        let headers = client
            .with_assertion(HashMap::new())
            .await
            .expect("an assertion");
        let value = headers.get(&ASSERTION_HEADER).expect("the header");
        assert_eq!(value.to_str().expect("ascii"), TOKEN);
        assert!(value.is_sensitive());
    }
    assert_eq!(minted.calls.load(Ordering::SeqCst), 1);
    // Without assertions the headers pass unchanged.
    let plain = AssertingClient::plain(reqwest_mcp::Client::new());
    assert!(
        plain
            .with_assertion(HashMap::new())
            .await
            .expect("plain")
            .is_empty()
    );
}

#[tokio::test]
async fn a_governed_request_without_an_assertion_is_refused() {
    let client = AssertingClient::new(
        reqwest_mcp::Client::new(),
        DelegationAssertions::new(source(Err(DelegationAssertionError::Unavailable)), true),
        ENDPOINT,
    );
    let error = client
        .with_assertion(HashMap::new())
        .await
        .expect_err("refused");
    assert!(error.to_string().contains(ASSERTION_UNAVAILABLE));
    // Any other run sends the request without one.
    let client = AssertingClient::new(
        reqwest_mcp::Client::new(),
        DelegationAssertions::new(source(Err(DelegationAssertionError::Unavailable)), false),
        ENDPOINT,
    );
    assert!(
        client
            .with_assertion(HashMap::new())
            .await
            .expect("sent")
            .is_empty()
    );
}

/// The assertion reaches the server as a request header.
#[tokio::test]
async fn the_server_receives_the_assertion() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.expect("accept");
        let mut request = Vec::new();
        let mut buffer = [0_u8; 4096];
        while !request.windows(4).any(|window| window == b"\r\n\r\n") {
            let read = socket.read(&mut buffer).await.expect("read");
            if read == 0 {
                break;
            }
            request.extend_from_slice(&buffer[..read]);
        }
        socket
            .write_all(b"HTTP/1.1 202 Accepted\r\ncontent-length: 0\r\n\r\n")
            .await
            .expect("write");
        String::from_utf8_lossy(&request).into_owned()
    });
    // The audience is the HTTPS endpoint; the test server answers in plain
    // HTTP on the loopback.
    let client = AssertingClient::new(
        reqwest_mcp::Client::new(),
        DelegationAssertions::new(source(Ok(issued())), true),
        ENDPOINT,
    );
    let message: ClientJsonRpcMessage = serde_json::from_value(serde_json::json!({
        "jsonrpc": "2.0",
        "method": "notifications/initialized"
    }))
    .expect("a notification");
    client
        .post_message(
            Arc::from(format!("http://{address}/mcp")),
            message,
            None,
            None,
            HashMap::new(),
        )
        .await
        .expect("accepted");
    let request = server.await.expect("server").to_ascii_lowercase();
    assert!(
        request.contains(&format!(
            "elitea-agent-assertion: {}",
            TOKEN.to_ascii_lowercase()
        )),
        "{request}"
    );
}
