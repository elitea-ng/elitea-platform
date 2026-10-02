//! Public OAuth discovery runs at the guard, never as an authorization grant.

use std::time::Duration;

use serde_json::{Map, Value, json};

use super::{
    DelegatedAuthorizationRequirement, MAX_AUTH_METADATA_BYTES, valid_identity, valid_string_list,
};

const DISCOVERY_TIMEOUT: Duration = Duration::from_secs(5);
const SECRET_PLACEHOLDER: &str = "********";

impl DelegatedAuthorizationRequirement {
    /// Retain public settings only. The client secret stays in Main.
    pub(crate) fn with_configured_oauth(
        self,
        issuer: &str,
        settings: &Map<String, Value>,
    ) -> Option<Self> {
        let mut scopes = match settings.get("scopes").or_else(|| settings.get("scope")) {
            None | Some(Value::Null) => Vec::new(),
            Some(Value::String(value)) => value.split_whitespace().map(|s| json!(s)).collect(),
            Some(Value::Array(values)) => values.clone(),
            _ => return None,
        };
        if !scopes.iter().any(|scope| scope == "offline_access") {
            scopes.insert(0, json!("offline_access"));
        }
        let client_id = settings.get("client_id")?.as_str()?;
        let mut provided = json!({"mcp_client_id": client_id, "scopes": scopes});
        if settings
            .get("client_secret")
            .and_then(Value::as_str)
            .is_some_and(|s| !s.is_empty())
        {
            provided["mcp_client_secret"] = json!(SECRET_PLACEHOLDER);
        }
        let mut metadata = json!({
            "authorization_servers": [issuer.trim_end_matches('/')],
            "resource_name": self.toolkit_type(),
            "scopes_supported": scopes,
            "provided_settings": provided,
        });
        if let Some(uuid) = settings.get("configuration_uuid").filter(|v| !v.is_null()) {
            metadata["configuration_uuid"] = uuid.clone();
        }
        self.with_resource_metadata(metadata)
    }

    /// Fetch only public endpoints. Do not fetch a token or call the toolkit.
    pub(crate) async fn resolve_public_metadata(&self) -> Self {
        let Some(metadata) = self.resource_metadata().and_then(Value::as_object) else {
            return self.clone();
        };
        if metadata.contains_key("oauth_authorization_server")
            || !metadata.contains_key("provided_settings")
        {
            return self.clone();
        }
        let Some(url) = self.resource_metadata_url() else {
            return self.clone();
        };
        let discovered = tokio::time::timeout(DISCOVERY_TIMEOUT, fetch_metadata(url)).await;
        let Some(server) = discovered.ok().flatten() else {
            tracing::warn!(
                event = "delegated_auth_discovery_unavailable",
                "public authorization metadata is unavailable"
            );
            return self.clone();
        };
        self.with_discovered_metadata(&server)
            .unwrap_or_else(|| self.clone())
    }

    fn with_discovered_metadata(&self, server: &Value) -> Option<Self> {
        let server = server.as_object()?;
        let projected: Map<String, Value> = server
            .iter()
            .filter(|(key, _)| {
                matches!(
                    key.as_str(),
                    "authorization_endpoint"
                        | "token_endpoint"
                        | "registration_endpoint"
                        | "issuer"
                        | "jwks_uri"
                        | "revocation_endpoint"
                        | "userinfo_endpoint"
                        | "scopes_supported"
                        | "response_types_supported"
                        | "grant_types_supported"
                        | "token_endpoint_auth_methods_supported"
                        | "code_challenge_methods_supported"
                )
            })
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();
        let mut metadata = self.resource_metadata()?.clone();
        metadata["oauth_authorization_server"] = Value::Object(projected);
        self.clone().with_resource_metadata(metadata)
    }
}

async fn fetch_metadata(url: &str) -> Option<Value> {
    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(DISCOVERY_TIMEOUT)
        .build()
        .ok()?;
    let mut response = client
        .get(url)
        .header("accept", "application/json")
        .send()
        .await
        .ok()?;
    if !response.status().is_success()
        || response
            .content_length()
            .is_some_and(|len| len > MAX_AUTH_METADATA_BYTES as u64)
    {
        return None;
    }
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await.ok()? {
        if body.len().saturating_add(chunk.len()) > MAX_AUTH_METADATA_BYTES {
            return None;
        }
        body.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&body).ok()
}

pub(super) fn configured_metadata(metadata: Option<&Value>) -> Option<Value> {
    metadata.map(|metadata| {
        let mut metadata = metadata.clone();
        if let Some(object) = metadata.as_object_mut()
            && object.contains_key("provided_settings")
        {
            object.remove("oauth_authorization_server");
        }
        metadata
    })
}

pub(super) fn valid_configured_metadata(metadata: &Map<String, Value>) -> bool {
    for field in ["resource_name", "configuration_uuid", "toolkit_id"] {
        if metadata
            .get(field)
            .is_some_and(|v| v.as_str().is_none_or(|v| !valid_identity(v)))
        {
            return false;
        }
    }
    let Some(provided) = metadata.get("provided_settings") else {
        return true;
    };
    let Some(provided) = provided.as_object() else {
        return false;
    };
    provided.keys().all(|key| {
        matches!(
            key.as_str(),
            "mcp_client_id" | "mcp_client_secret" | "scopes"
        )
    }) && provided
        .get("mcp_client_id")
        .and_then(Value::as_str)
        .is_some_and(valid_identity)
        && provided
            .get("mcp_client_secret")
            .is_none_or(|v| v == SECRET_PLACEHOLDER)
        && provided.get("scopes").is_none_or(valid_string_list)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    async fn metadata_server(response: String) -> (String, tokio::task::JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/metadata", listener.local_addr().unwrap());
        let task = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let byte = socket.read_u8().await.unwrap();
                request.push(byte);
                assert!(request.len() < 8192);
            }
            let _ = socket.write_all(response.as_bytes()).await;
            String::from_utf8(request).unwrap()
        });
        (url, task)
    }

    #[tokio::test]
    async fn public_discovery_sends_no_credentials_and_bounds_fixed_and_chunked_bodies() {
        let valid = r#"{"authorization_endpoint":"https://login.example.test/authorize","token_endpoint":"https://login.example.test/token"}"#;
        let oversized = "x".repeat(MAX_AUTH_METADATA_BYTES + 1);
        for (response, accepted) in [
            (
                format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{valid}",
                    valid.len()
                ),
                true,
            ),
            (
                format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{oversized}",
                    oversized.len()
                ),
                false,
            ),
            (
                format!(
                    "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{:x}\r\n{oversized}\r\n0\r\n\r\n",
                    oversized.len()
                ),
                false,
            ),
            (
                "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnope".into(),
                false,
            ),
            (
                "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n".into(),
                false,
            ),
        ] {
            let (url, server) = metadata_server(response).await;
            assert_eq!(fetch_metadata(&url).await.is_some(), accepted);
            let request = server.await.unwrap().to_lowercase();
            assert!(request.contains("accept: application/json\r\n"));
            assert!(!request.contains("authorization:"));
            assert!(!request.contains("cookie:"));
        }
    }

    #[tokio::test]
    async fn public_discovery_does_not_follow_redirects() {
        let target = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let (url, server) = metadata_server(format!(
            "HTTP/1.1 302 Found\r\nLocation: http://{}/redirect\r\nContent-Length: 0\r\n\r\n",
            target.local_addr().unwrap()
        ))
        .await;
        assert!(fetch_metadata(&url).await.is_none());
        server.await.unwrap();
        assert!(
            tokio::time::timeout(Duration::from_millis(25), target.accept())
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn cancellation_drops_public_discovery_without_a_detached_request() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/metadata", listener.local_addr().unwrap());
        let mut request = Box::pin(fetch_metadata(&url));
        let (socket, _) = tokio::select! {
            result = listener.accept() => result.unwrap(),
            _ = &mut request => panic!("discovery completed before the fixture accepted it"),
        };
        // Cancelling the owner must close the TCP request without waiting five seconds.
        drop(request);
        let mut received = Vec::new();
        tokio::time::timeout(
            Duration::from_secs(1),
            socket.take(8192).read_to_end(&mut received),
        )
        .await
        .expect("cancelled discovery closes its connection")
        .unwrap();
    }

    #[test]
    fn configured_auth_metadata_is_shared_bounded_and_secret_free() {
        for family in ["sharepoint", "openapi", "custom-delegated"] {
            let settings = json!({"client_id": "public-client", "client_secret": "private-value", "configuration_uuid": "config-1", "scopes": ["read"]});
            let base = DelegatedAuthorizationRequirement::new(
                "docs".into(),
                family.into(),
                "https://api.example.test".into(),
                Some("https://login.example.test/.well-known/openid-configuration".into()),
                None,
            )
            .unwrap()
            .with_configured_oauth("https://login.example.test", settings.as_object().unwrap())
            .unwrap()
            .with_toolkit_id(Some(12));
            let resolved = base.with_discovered_metadata(&json!({
                "authorization_endpoint": "https://login.example.test/authorize", "token_endpoint": "https://login.example.test/token",
                "client_secret": "provider-secret", "unexpected": "body"
            })).unwrap();
            assert!(base.same_authority(&resolved));
            let encoded = serde_json::to_string(&resolved).unwrap();
            assert!(!encoded.contains("private-value"));
            assert!(!encoded.contains("provider-secret"));
            assert!(!encoded.contains("unexpected"));
            assert_eq!(
                resolved.resource_metadata().unwrap()["scopes_supported"],
                json!(["offline_access", "read"])
            );
            assert_eq!(resolved.resource_metadata().unwrap()["toolkit_id"], "12");
            assert!(!resolved.same_authority(&base.with_toolkit_id(Some(13))));
        }
    }

    #[test]
    fn malformed_or_secret_metadata_does_not_become_an_authorization_target() {
        let settings = json!({"client_id":"public-client", "scopes": ["read"]});
        let base = DelegatedAuthorizationRequirement::new(
            "docs".into(),
            "openapi".into(),
            "https://api.example.test".into(),
            None,
            None,
        )
        .unwrap()
        .with_configured_oauth("https://login.example.test", settings.as_object().unwrap())
        .unwrap();
        for server in [
            json!({}),
            json!({"authorization_endpoint":"http://login.example.test/auth","token_endpoint":"https://login.example.test/token"}),
            json!({"authorization_endpoint":"https://login.example.test/auth","token_endpoint":"https://user:secret@login.example.test/token"}),
        ] {
            assert!(base.with_discovered_metadata(&server).is_none());
        }
        let mut metadata = base.resource_metadata().unwrap().clone();
        metadata["provided_settings"]["mcp_client_secret"] = json!("real-secret");
        assert!(base.with_resource_metadata(metadata).is_none());
    }
}
