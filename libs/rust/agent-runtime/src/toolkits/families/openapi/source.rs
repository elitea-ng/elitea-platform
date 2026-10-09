//! Bounded anonymous retrieval of an admitted `OpenAPI` specification URL.
use std::time::Duration;

use reqwest::{Client, Url};
use serde_json::{Map, Value};

const MAX_SOURCE_BYTES: usize = 1024 * 1024;
const MAX_URL_BYTES: usize = 8 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SourceError {
    Invalid,
    Unavailable,
    TooLarge,
}

pub(crate) async fn load(settings: &Map<String, Value>) -> Result<Option<Value>, SourceError> {
    let source = settings
        .get("spec")
        .or_else(|| settings.get("schema_settings"))
        .or_else(|| settings.get("openapi_spec"))
        .and_then(Value::as_str);
    let Some(raw) = source
        .map(str::trim)
        .filter(|raw| raw.starts_with("https://") || raw.starts_with("http://"))
    else {
        return Ok(None);
    };
    let url = source_url(raw)?;
    let client = Client::builder()
        .https_only(true)
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(15))
        .build()
        .map_err(|_| SourceError::Unavailable)?;
    let source = fetch(&client, url.clone()).await?;
    let mut document = super::spec::parse_source(&source).map_err(|_| SourceError::Invalid)?;
    resolve_servers(&mut document, &url)?;
    Ok(Some(document))
}

fn resolve_servers(document: &mut Value, source: &Url) -> Result<(), SourceError> {
    let root = document.as_object_mut().ok_or(SourceError::Invalid)?;
    let servers = root
        .entry("servers")
        .or_insert_with(|| serde_json::json!([{"url":"/"}]));
    let servers = servers.as_array_mut().ok_or(SourceError::Invalid)?;
    for server in servers {
        let Some(raw) = server.get("url").and_then(Value::as_str) else {
            return Err(SourceError::Invalid);
        };
        // Leave server variables and absolute URLs to the existing parser.
        if Url::parse(raw).is_err() && !raw.contains('{') {
            let absolute = source.join(raw).map_err(|_| SourceError::Invalid)?;
            server["url"] = Value::String(absolute.to_string());
        }
    }
    Ok(())
}

fn source_url(raw: &str) -> Result<Url, SourceError> {
    if raw.len() > MAX_URL_BYTES {
        return Err(SourceError::TooLarge);
    }
    let url = Url::parse(raw).map_err(|_| SourceError::Invalid)?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
    {
        return Err(SourceError::Invalid);
    }
    Ok(url)
}

async fn fetch(client: &Client, url: Url) -> Result<Value, SourceError> {
    // This client has no toolkit credentials, cookies, or default authorization headers.
    let mut response = client
        .get(url)
        .send()
        .await
        .map_err(|_| SourceError::Unavailable)?;
    if !response.status().is_success() {
        return Err(SourceError::Unavailable);
    }
    if response
        .content_length()
        .is_some_and(|length| length > MAX_SOURCE_BYTES as u64)
    {
        return Err(SourceError::TooLarge);
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| SourceError::Unavailable)?
    {
        if chunk.len() > MAX_SOURCE_BYTES.saturating_sub(bytes.len()) {
            return Err(SourceError::TooLarge);
        }
        bytes.extend_from_slice(&chunk);
    }
    let text = String::from_utf8(bytes).map_err(|_| SourceError::Invalid)?;
    // The existing parser validates JSON/YAML structure, depth, references, and operation limits.
    Ok(Value::String(text))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    #[test]
    fn source_url_requires_https_without_userinfo_or_fragment() {
        for url in [
            "http://example.test/spec",
            "https://u:p@example.test/spec",
            "https://example.test/spec#part",
        ] {
            assert_eq!(
                source_url(url).expect_err("unsafe URL"),
                SourceError::Invalid
            );
        }
        assert!(source_url("https://example.test/spec?revision=2").is_ok());
    }

    #[test]
    fn resolves_relative_and_default_servers_against_specification_url() {
        let source = Url::parse("https://example.test/docs/openapi.json").expect("source");
        let mut document =
            serde_json::json!({"servers":[{"url":"../api"},{"url":"https://other.test/api"}]});
        resolve_servers(&mut document, &source).expect("relative servers");
        assert_eq!(document["servers"][0]["url"], "https://example.test/api");
        assert_eq!(document["servers"][1]["url"], "https://other.test/api");
        let mut document = serde_json::json!({});
        resolve_servers(&mut document, &source).expect("default server");
        assert_eq!(document["servers"][0]["url"], "https://example.test/");
    }

    #[tokio::test]
    async fn inline_settings_do_not_fetch() {
        let settings = serde_json::json!({"spec": "openapi: 3.0.3"});
        assert!(
            load(settings.as_object().expect("settings"))
                .await
                .expect("inline")
                .is_none()
        );
    }

    async fn server(response: String) -> (Url, tokio::task::JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("listener");
        let address = listener.local_addr().expect("address");
        let task = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.expect("connection");
            let mut request = vec![0; 8192];
            let size = socket.read(&mut request).await.expect("request");
            let _ = socket.write_all(response.as_bytes()).await;
            String::from_utf8(request[..size].to_vec()).expect("HTTP request")
        });
        (
            Url::parse(&format!("http://{address}/spec")).expect("URL"),
            task,
        )
    }

    fn client() -> Client {
        Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(Duration::from_secs(2))
            .build()
            .expect("test client")
    }

    #[tokio::test]
    async fn retrieves_document_without_credentials() {
        let body = "openapi: 3.0.3";
        let (url, task) = server(format!(
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        ))
        .await;
        assert_eq!(
            fetch(&client(), url).await.expect("document"),
            Value::String(body.to_owned())
        );
        let request = task.await.expect("server").to_lowercase();
        assert!(!request.contains("authorization:") && !request.contains("cookie:"));
    }

    #[tokio::test]
    async fn rejects_redirect_and_oversized_declared_body() {
        for (response, expected) in [
            ("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/private\r\nContent-Length: 0\r\n\r\n".to_owned(), SourceError::Unavailable),
            (format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", MAX_SOURCE_BYTES + 1), SourceError::TooLarge),
        ] {
            let (url, task) = server(response).await;
            assert_eq!(fetch(&client(), url).await.expect_err("rejected response"), expected);
            task.await.expect("server");
        }
    }

    #[tokio::test]
    async fn stalled_body_respects_client_deadline() {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("listener");
        let url = Url::parse(&format!(
            "http://{}/spec",
            listener.local_addr().expect("address")
        ))
        .expect("URL");
        let (release, done) = tokio::sync::oneshot::channel::<()>();
        let task = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.expect("connection");
            let mut request = [0; 8192];
            let received = socket.read(&mut request).await.expect("request");
            assert!(received > 0);
            socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n")
                .await
                .expect("headers");
            done.await.expect("release stalled response");
        });
        let client = Client::builder()
            .timeout(Duration::from_millis(100))
            .build()
            .expect("client");
        assert_eq!(
            fetch(&client, url).await.expect_err("deadline"),
            SourceError::Unavailable
        );
        release.send(()).expect("release server");
        task.await.expect("server");
    }

    #[tokio::test]
    async fn bounds_body_without_content_length() {
        let (url, task) = server(format!(
            "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n{}",
            "x".repeat(MAX_SOURCE_BYTES + 1)
        ))
        .await;
        assert_eq!(
            fetch(&client(), url).await.expect_err("body bound"),
            SourceError::TooLarge
        );
        task.await.expect("server");
    }
}
