//! The platform operations one local turn uses (client contract 1.5/1.6,
//! `services/elitea-main/API_CONTRACT.md`), over HTTPS with the native
//! access token.
//!
//! The bearer goes to the connected deployment's origin and nowhere else:
//! every URL here is built from [`Credentials::bearer`]'s origin, and no
//! redirect is followed. Errors keep the server's machine code and its
//! human message, never a response body or a header.

use std::time::Duration;

use async_trait::async_trait;
use reqwest::{Method, StatusCode};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use serde_json::{Map, Value};

use crate::tokens::CLIENT_VERSION_HEADER;

/// How long one platform request may take. A remote toolkit call waits up
/// to 60 s on the server before it answers 504.
pub const API_TIMEOUT: Duration = Duration::from_secs(90);

/// A usable credential for the connected deployment.
#[derive(Clone)]
pub struct Bearer {
    /// The deployment origin (`https://host[:port]`), no trailing slash.
    pub origin: String,
    pub token: String,
}

impl std::fmt::Debug for Bearer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Bearer")
            .field("origin", &self.origin)
            .field("token", &"<redacted>")
            .finish()
    }
}

/// Where the D0 host gets its access token (the keychain-backed session).
#[async_trait]
pub trait Credentials: Send + Sync {
    /// The current token; `Err` when not connected or signed out.
    async fn bearer(&self) -> Result<Bearer, ApiError>;
    /// A fresh token after the platform answered 401.
    async fn refreshed(&self) -> Result<Bearer, ApiError>;
}

/// A refused or failed platform call.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ApiError {
    /// HTTP status, `None` when no answer arrived.
    pub status: Option<u16>,
    /// The server's machine code (`local_work_disabled`, …) or a local one
    /// (`network`, `invalid_response`, `not_signed_in`).
    pub code: String,
    pub message: String,
}

impl ApiError {
    pub fn local(code: &str, message: impl Into<String>) -> Self {
        Self {
            status: None,
            code: code.to_owned(),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

/// An answer, whatever its status.
#[derive(Debug)]
pub struct Answer {
    pub status: u16,
    pub body: Value,
}

impl Answer {
    fn error(&self) -> ApiError {
        let text = |key: &str| {
            self.body
                .get(key)
                .and_then(Value::as_str)
                .map(str::to_owned)
        };
        ApiError {
            status: Some(self.status),
            code: text("error").unwrap_or_else(|| format!("http_{}", self.status)),
            message: text("message")
                .unwrap_or_else(|| format!("the platform answered HTTP {}", self.status)),
        }
    }

    fn into_ok<T: DeserializeOwned>(self) -> Result<T, ApiError> {
        if !(200..300).contains(&self.status) {
            return Err(self.error());
        }
        serde_json::from_value(self.body).map_err(|_| {
            ApiError::local(
                "invalid_response",
                "the platform's answer did not have the expected shape",
            )
        })
    }
}

/// `resolveApplicationVersion` (decision 5a).
#[derive(Clone, Debug, Deserialize)]
pub struct ResolvedVersion {
    #[serde(default)]
    pub withheld_secrets: Vec<String>,
    #[serde(default)]
    pub project_context_withheld: bool,
    pub version_details: Map<String, Value>,
}

/// `startLocalTurn` (decision 5c).
#[derive(Clone, Debug, Deserialize)]
pub struct LocalTurnStarted {
    pub execution_id: String,
    pub question_id: String,
    pub response_message_id: String,
    pub memory_recall: MemoryRecall,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct MemoryRecall {
    /// Appended to the instructions after one blank line; the commit
    /// stamps the count server-side.
    #[serde(default)]
    pub text: String,
}

/// The platform client of one turn.
pub struct PlatformApi {
    http: reqwest::Client,
    credentials: std::sync::Arc<dyn Credentials>,
    client_version: String,
}

impl PlatformApi {
    /// # Errors
    ///
    /// The HTTP client cannot be built.
    pub fn new(
        credentials: std::sync::Arc<dyn Credentials>,
        client_version: &str,
    ) -> Result<Self, ApiError> {
        let http = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(API_TIMEOUT)
            .user_agent(format!("elitea-desktop/{client_version}"))
            .build()
            .map_err(|_| ApiError::local("internal", "could not build the HTTP client"))?;
        Ok(Self {
            http,
            credentials,
            client_version: client_version.to_owned(),
        })
    }

    #[must_use]
    pub fn http(&self) -> &reqwest::Client {
        &self.http
    }

    /// One request; a 401 is retried once with a refreshed token.
    ///
    /// # Errors
    ///
    /// No answer arrived, or the body is not JSON.
    pub async fn send(
        &self,
        method: Method,
        path: &str,
        body: Option<&Value>,
        headers: &[(&str, &str)],
    ) -> Result<Answer, ApiError> {
        let bearer = self.credentials.bearer().await?;
        let answer = self
            .send_with(&bearer, &method, path, body, headers)
            .await?;
        if answer.status != StatusCode::UNAUTHORIZED.as_u16() {
            return Ok(answer);
        }
        let bearer = self.credentials.refreshed().await?;
        self.send_with(&bearer, &method, path, body, headers).await
    }

    async fn send_with(
        &self,
        bearer: &Bearer,
        method: &Method,
        path: &str,
        body: Option<&Value>,
        headers: &[(&str, &str)],
    ) -> Result<Answer, ApiError> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", bearer.origin))
            .bearer_auth(&bearer.token)
            .header(CLIENT_VERSION_HEADER, &self.client_version)
            .header("Accept", "application/json");
        for (name, value) in headers {
            request = request.header(*name, *value);
        }
        if let Some(body) = body {
            request = request.json(body);
        }
        let response = request.send().await.map_err(|error| {
            if error.is_timeout() {
                ApiError::local("timeout", "the platform did not answer in time")
            } else {
                ApiError::local("network", "could not reach the platform")
            }
        })?;
        let status = response.status().as_u16();
        let bytes = response
            .bytes()
            .await
            .map_err(|_| ApiError::local("network", "the platform's answer was cut off"))?;
        let body = if bytes.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&bytes).unwrap_or(Value::Null)
        };
        Ok(Answer { status, body })
    }

    /// # Errors
    ///
    /// Refused (404, 422 …) or failed.
    pub async fn resolved_version(
        &self,
        project_id: i64,
        application_id: i64,
        version_id: i64,
    ) -> Result<ResolvedVersion, ApiError> {
        self.send(
            Method::GET,
            &format!(
                "/api/v2/elitea_core/resolved_version/prompt_lib/{project_id}/{application_id}/{version_id}"
            ),
            None,
            &[],
        )
        .await?
        .into_ok()
    }

    /// The conversation's participant that answers with this agent version.
    ///
    /// # Errors
    ///
    /// The conversation cannot be read, or holds no participant for the
    /// agent (`agent_not_in_conversation`) or runs another version of it
    /// (`agent_version_mismatch`).
    pub async fn answering_participant(
        &self,
        project_id: i64,
        conversation_id: &str,
        application_id: i64,
        version_id: i64,
    ) -> Result<i64, ApiError> {
        let detail: Value = self
            .send(
                Method::GET,
                &format!(
                    "/api/v2/elitea_core/conversation/prompt_lib/{project_id}/{}",
                    path_segment(conversation_id)?
                ),
                None,
                &[],
            )
            .await?
            .into_ok()?;
        participant_for(&detail, application_id, version_id)
    }

    /// # Errors
    ///
    /// Refused (403 `local_work_disabled`, 409, 422 …) or failed.
    pub async fn start_turn(
        &self,
        project_id: i64,
        conversation_id: &str,
        body: &Value,
    ) -> Result<LocalTurnStarted, ApiError> {
        self.send(
            Method::POST,
            &format!(
                "/api/v2/elitea_core/local_turn/prompt_lib/{project_id}/{}",
                path_segment(conversation_id)?
            ),
            Some(body),
            &[],
        )
        .await?
        .into_ok()
    }

    /// # Errors
    ///
    /// Refused (409, 410 …) or failed.
    pub async fn commit_turn(
        &self,
        project_id: i64,
        execution_id: &str,
        body: &Value,
    ) -> Result<Value, ApiError> {
        self.send(
            Method::POST,
            &format!(
                "/api/v2/elitea_core/local_turn_commit/prompt_lib/{project_id}/{execution_id}"
            ),
            Some(body),
            &[],
        )
        .await?
        .into_ok()
    }

    /// One remote toolkit call attempt; the caller reads the status.
    ///
    /// # Errors
    ///
    /// No answer arrived.
    pub async fn remote_toolkit_call(
        &self,
        project_id: i64,
        toolkit_id: i64,
        idempotency_key: &str,
        body: &Value,
    ) -> Result<Answer, ApiError> {
        self.send(
            Method::POST,
            &format!(
                "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/{project_id}/{toolkit_id}"
            ),
            Some(body),
            &[("Idempotency-Key", idempotency_key)],
        )
        .await
    }
}

/// Percent-encode one path segment (a conversation id is a number or a
/// UUID, but nothing the webview sends is trusted to be either).
///
/// Percent-encoding leaves `.` alone, and a URL parser resolves a `.` or
/// `..` segment (WHATWG also treats `%2e` as a dot), so those and the empty
/// segment are refused rather than encoded.
fn path_segment(value: &str) -> Result<String, ApiError> {
    if matches!(value, "" | "." | "..") {
        return Err(ApiError::local(
            "invalid_request",
            "conversation_id must be the conversation's id or UUID",
        ));
    }
    Ok(url::form_urlencoded::byte_serialize(value.as_bytes())
        .collect::<String>()
        .replace('+', "%20"))
}

/// The participant that answers as `application_id` on `version_id`.
///
/// The platform's `local_turn` start accepts any application participant of
/// the conversation (it does not look at versions); the version rule is the
/// conversation's own: a participant pinned to a version
/// (`entity_settings.version_id`) answers on that version only, an unpinned
/// one answers on whichever version is asked for. A participant pinned to
/// the requested version wins over an unpinned one; only when every entry of
/// the agent is pinned to another version is the turn refused.
fn participant_for(detail: &Value, application_id: i64, version_id: i64) -> Result<i64, ApiError> {
    let participants = detail
        .get("participants")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice);
    let mut unpinned = None;
    let mut other_versions: Vec<i64> = Vec::new();
    for participant in participants {
        let kind = participant.get("entity_name").and_then(Value::as_str);
        let entity = participant
            .get("entity_meta")
            .and_then(|meta| meta.get("id"))
            .and_then(Value::as_i64);
        if kind != Some("application") || entity != Some(application_id) {
            continue;
        }
        let Some(id) = participant.get("id").and_then(Value::as_i64) else {
            continue;
        };
        let pinned = participant
            .get("entity_settings")
            .and_then(|settings| settings.get("version_id"))
            .filter(|value| !value.is_null())
            .map(|value| value.as_i64().ok_or(()));
        match pinned {
            Some(Ok(pinned)) if pinned == version_id => return Ok(id),
            Some(Ok(pinned)) => {
                if !other_versions.contains(&pinned) {
                    other_versions.push(pinned);
                }
            }
            // No pin: answers on the version asked for.
            None => {
                unpinned.get_or_insert(id);
            }
            // A pin this client cannot read is not a match.
            Some(Err(())) => {}
        }
    }
    if let Some(id) = unpinned {
        return Ok(id);
    }
    Err(match other_versions.as_slice() {
        [] => ApiError::local(
            "agent_not_in_conversation",
            "the agent is not a participant of this conversation",
        ),
        [pinned] => ApiError::local(
            "agent_version_mismatch",
            format!(
                "this conversation runs version {pinned} of the agent, not version {version_id}; switch the version in the conversation first"
            ),
        ),
        pinned => ApiError::local(
            "agent_version_mismatch",
            format!(
                "this conversation runs versions {} of the agent, not version {version_id}; switch the version in the conversation first",
                pinned
                    .iter()
                    .map(i64::to_string)
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        ),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_answering_participant_is_the_agent_on_the_requested_version() {
        let detail = json!({"participants": [
            {"id": 1, "entity_name": "user", "entity_meta": {"id": 7}},
            {"id": 2, "entity_name": "application", "entity_meta": {"id": 5}, "entity_settings": {"version_id": 9}},
            {"id": 3, "entity_name": "application", "entity_meta": {"id": 6}, "entity_settings": {"version_id": 10}}
        ]});
        assert_eq!(participant_for(&detail, 6, 10), Ok(3));
        assert_eq!(
            participant_for(&detail, 5, 10).unwrap_err().code,
            "agent_version_mismatch"
        );
        assert_eq!(
            participant_for(&detail, 8, 1).unwrap_err().code,
            "agent_not_in_conversation"
        );
    }

    #[test]
    fn an_unpinned_participant_answers_on_the_requested_version() {
        // No entity_settings, an empty one, and an explicit null: all unpinned.
        for settings in [json!(null), json!({}), json!({"version_id": null})] {
            let mut entry =
                json!({"id": 4, "entity_name": "application", "entity_meta": {"id": 5}});
            if !settings.is_null() {
                entry["entity_settings"] = settings;
            }
            let detail = json!({"participants": [entry]});
            assert_eq!(participant_for(&detail, 5, 9), Ok(4));
        }
    }

    #[test]
    fn a_pinned_match_wins_and_a_later_unpinned_entry_does_not_hide_a_mismatch() {
        let detail = json!({"participants": [
            {"id": 2, "entity_name": "application", "entity_meta": {"id": 5}},
            {"id": 3, "entity_name": "application", "entity_meta": {"id": 5}, "entity_settings": {"version_id": 9}}
        ]});
        assert_eq!(participant_for(&detail, 5, 9), Ok(3));
        assert_eq!(participant_for(&detail, 5, 10), Ok(2));

        // Pinned elsewhere, then an entry of another agent with no pin: the
        // mismatch is still reported, with the version the agent runs.
        let detail = json!({"participants": [
            {"id": 2, "entity_name": "application", "entity_meta": {"id": 5}, "entity_settings": {"version_id": 7}},
            {"id": 3, "entity_name": "application", "entity_meta": {"id": 6}}
        ]});
        let error = participant_for(&detail, 5, 9).unwrap_err();
        assert_eq!(error.code, "agent_version_mismatch");
        assert!(error.message.contains("version 7"), "{}", error.message);

        // Two entries pinned to two other versions: both are named.
        let detail = json!({"participants": [
            {"id": 2, "entity_name": "application", "entity_meta": {"id": 5}, "entity_settings": {"version_id": 7}},
            {"id": 3, "entity_name": "application", "entity_meta": {"id": 5}, "entity_settings": {"version_id": 8}}
        ]});
        let error = participant_for(&detail, 5, 9).unwrap_err();
        assert_eq!(error.code, "agent_version_mismatch");
        assert!(error.message.contains("versions 7, 8"), "{}", error.message);
    }

    #[test]
    fn a_path_segment_cannot_escape_its_route() {
        // A separator or query is encoded, so the value stays one segment.
        assert_eq!(path_segment("../x?y").unwrap(), "..%2Fx%3Fy");
        assert_eq!(path_segment("%2e%2e").unwrap(), "%252e%252e");
        assert_eq!(path_segment("6f1c2d4e-8a7b").unwrap(), "6f1c2d4e-8a7b");
        // A dot segment would be resolved away by the URL parser: refused.
        for dots in ["", ".", ".."] {
            assert_eq!(path_segment(dots).unwrap_err().code, "invalid_request");
        }
        let base = url::Url::parse("https://x/api/v2/conversation/prompt_lib/1/").unwrap();
        let joined = base.join(&path_segment("../x?y").unwrap()).unwrap();
        assert_eq!(
            joined.path(),
            "/api/v2/conversation/prompt_lib/1/..%2Fx%3Fy"
        );
    }

    #[test]
    fn a_bearer_never_prints_its_token() {
        let bearer = Bearer {
            origin: "https://x".into(),
            token: "elnat_secret".into(),
        };
        assert!(!format!("{bearer:?}").contains("elnat_secret"));
    }
}
