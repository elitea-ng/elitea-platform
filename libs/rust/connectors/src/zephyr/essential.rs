//! Zephyr Essential as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **Configuration.** `zephyr_essential_configuration.{base_url,token}`:
//!   the REST API root (default `https://prod-api.zephyr4jiracloud.com/v2`,
//!   HTTPS, a path prefix is kept) and the API token, sent as a bearer to
//!   that origin only.
//! * **List.** Every test case (`GET /testcases`, `maxResults` and
//!   `startAt`, until `isLast`). The SDK read the first page only (a
//!   `maxResults` of `None` left the API's default of ten); this lists them
//!   all. A case is `testcase/{key}.json`.
//! * **Content and version.** The case's steps (`/testcases/{key}/teststeps`,
//!   paged), else its test script (`/testcases/{key}/testscript`, a 404 is
//!   "none"). The document is the case with its steps or script; its
//!   version is the SHA-256 of those bytes (the SDK hashed the steps or
//!   script alone, so a renamed case or a changed label never moved it).
//!   Essential gives no revision stamp on a case, so a listing reads every
//!   case's steps. The content is kept from the listing, so a fetch reads
//!   nothing again and can never see a version other than the listed one.
//! * **Auth and limits.** A 401 or 403 is an error naming the credential,
//!   never an empty listing; a 429 backs off within the shared bound.
//!   Essential has no per-case permission this API exposes: a case the token
//!   lists is one its owner may read.

use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use futures_util::StreamExt as _;
use serde_json::{Map, Value, json};
use url::Url;

use super::{ZephyrClient, case_metadata};
use crate::atlassian::{
    AtlassianConfigError, COMMENT_LISTING_CONCURRENCY, DetailedListing, Details, safe_identifier,
    text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references};
use crate::testmgmt::{https_base, printable_secret, refuse};
use crate::transport::Transport;

pub const PROVIDER: &str = "Zephyr Essential";
/// The SDK wrapper's default when the configuration has no base URL.
pub const DEFAULT_BASE_URL: &str = "https://prod-api.zephyr4jiracloud.com/v2";

/// Invocation-scoped Zephyr Essential authority for one index run.
pub struct ZephyrEssentialConfig {
    base: Url,
    token: zeroize::Zeroizing<String>,
}

impl ZephyrEssentialConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `zephyr_essential_configuration` or token, or a base URL
    /// that is not HTTPS.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("zephyr_essential_configuration")
            .and_then(Value::as_object)
            .ok_or(refuse(
                PROVIDER,
                crate::atlassian::AtlassianConfigErrorCode::InvalidConfiguration,
            ))?;
        let base = match configuration.get("base_url") {
            None | Some(Value::Null) => DEFAULT_BASE_URL,
            Some(Value::String(value)) if value.trim().is_empty() => DEFAULT_BASE_URL,
            Some(Value::String(value)) => value,
            Some(_) => {
                return Err(refuse(
                    PROVIDER,
                    crate::atlassian::AtlassianConfigErrorCode::InvalidConfiguration,
                ));
            }
        };
        Ok(Self {
            base: https_base(PROVIDER, base, BasePath::Prefix)?,
            token: zeroize::Zeroizing::new(
                printable_secret(PROVIDER, configuration, "token")?.to_owned(),
            ),
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base
    }
}

/// One Zephyr Essential instance's test cases.
pub struct ZephyrEssentialSource {
    client: ZephyrClient,
    listing: DetailedListing,
}

impl ZephyrEssentialSource {
    /// The `config` indexed through `allowlist` (fail-closed) and then
    /// `transport`. Essential's `index_data` takes no argument beyond the
    /// worker's own.
    ///
    /// # Errors
    ///
    /// The Essential host is not on the allowlist.
    pub fn new(
        config: ZephyrEssentialConfig,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(&config.base) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let authorization = crate::testmgmt::bearer(PROVIDER, &config.token)
            .map_err(|error| SourceError::Unavailable(error.to_string()))?;
        Ok(Self {
            client: ZephyrClient::new(PROVIDER, config.base, authorization, http),
            listing: DetailedListing::default(),
        })
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SourceLimits) -> Self {
        self.client.http = self.client.http.with_limits(limits);
        self
    }

    #[must_use]
    pub fn with_backoff(mut self, backoff: Backoff) -> Self {
        self.client.http = self.client.http.with_backoff(backoff);
        self
    }

    #[must_use]
    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Self {
        self.client.http = self.client.http.with_timeouts(timeouts);
        self
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let cases = self.client.values(&["testcases"], &[]).await?;
        let mut known: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
        let mut work = Vec::with_capacity(cases.len());
        for case in cases {
            let key = text_of(&case, "/key")
                .ok_or(Failure::InvalidResponse("a test case has no key"))?
                .to_owned();
            if !safe_identifier(&key) {
                return Err(Failure::InvalidResponse(
                    "a test case key is not an identifier",
                ));
            }
            if known.insert(key.clone()) {
                work.push((key, case));
            }
        }
        let mut listed = Vec::new();
        let mut details = Details::new();
        let mut read = futures_util::stream::iter(work.into_iter().map(|(key, case)| async move {
            let (kind, content) = self.client.case_content(&key).await?;
            Ok::<_, Failure>((key, case, kind, content))
        }))
        .buffer_unordered(COMMENT_LISTING_CONCURRENCY);
        while let Some(result) = read.next().await {
            let (key, case, kind, content) = result?;
            let mut body = json!({"testcase": case});
            if !content.is_null() {
                body[kind] = content;
            }
            let bytes = serde_json::to_vec(&body)
                .map_err(|_| Failure::InvalidResponse("a test case cannot be encoded"))?;
            let version = content_version(&bytes);
            let document_key = format!("testcase/{key}.json");
            listed.push(Listed::new(
                document_key.clone(),
                version.clone(),
                bytes.len() as u64,
                key.clone(),
            ));
            self.client.http.check_count(listed.len())?;
            let mut meta = case_metadata(&case_of(&body));
            meta.insert("type".into(), Value::String("TEST_CASE".into()));
            meta.insert("steps_type".into(), Value::String(kind.to_owned()));
            meta.insert("updated_on".into(), Value::String(version));
            meta.insert(
                "content".into(),
                Value::String(String::from_utf8_lossy(&bytes).into_owned()),
            );
            details.insert(document_key, meta);
        }
        Ok((listed, details))
    }
}

fn case_of(body: &Value) -> Value {
    body.get("testcase").cloned().unwrap_or(Value::Null)
}

impl ContentSource for ZephyrEssentialSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self
            .listing
            .list_shared(PROVIDER, || self.listing())
            .await?;
        Ok(references(&listing.listing))
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let snapshot = self
            .listing
            .get_or_list(PROVIDER, || self.listing())
            .await?;
        let listed = snapshot
            .listing
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let mut metadata = snapshot.details_of(key);
        let content = match metadata.remove("content") {
            Some(Value::String(content)) => content,
            _ => String::new(),
        };
        let title = metadata
            .get("name")
            .and_then(Value::as_str)
            .map_or_else(|| listed.handle.clone(), ToOwned::to_owned);
        let mut reference = listed.reference.clone();
        reference.size = content.len() as u64;
        Ok(Document {
            title,
            uri: None,
            reference,
            bytes: content.into_bytes(),
            metadata,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;
    use serde_json::json;
    use std::sync::Mutex;

    fn config() -> ZephyrEssentialConfig {
        ZephyrEssentialConfig::parse(
            json!({"zephyr_essential_configuration": {"token": "s3cr3t-tkn"}})
                .as_object()
                .unwrap(),
        )
        .expect("config")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("prod-api.zephyr4jiracloud.com"))
    }

    /// A test case as the Zephyr API documents it.
    fn case(key: &str, name: &str) -> Value {
        json!({
            "id": 1, "key": key, "name": name,
            "project": {"id": 10_000, "self": "https://prod-api.zephyr4jiracloud.com/v2/projects/10000"},
            "createdOn": "2026-09-01T00:00:00Z", "objective": "Check it", "precondition": null,
            "labels": ["smoke"], "priority": {"id": 1}, "status": {"id": 2},
            "folder": null, "customFields": {"Team": "A"},
        })
    }

    fn envelope(values: &[Value], start: usize, total: usize, last: bool) -> Value {
        json!({"next": null, "startAt": start, "maxResults": 200, "total": total,
               "isLast": last, "values": values})
    }

    fn step(text: &str) -> Value {
        json!({"inline": {"description": text, "testData": "d", "expectedResult": "r",
                          "customFields": {}, "reflectRef": null}, "testCase": null})
    }

    /// A mutable recorded Essential.
    struct Instance {
        steps: Mutex<std::collections::BTreeMap<String, Vec<Value>>>,
        names: Mutex<std::collections::BTreeMap<String, String>>,
    }

    impl Instance {
        fn new() -> Arc<Self> {
            let instance = Arc::new(Self {
                steps: Mutex::new(std::collections::BTreeMap::new()),
                names: Mutex::new(std::collections::BTreeMap::new()),
            });
            for (key, name, text) in [("PROJ-T1", "Login", "open"), ("PROJ-T2", "Logout", "close")]
            {
                instance
                    .steps
                    .lock()
                    .unwrap()
                    .insert(key.into(), vec![step(text)]);
                instance
                    .names
                    .lock()
                    .unwrap()
                    .insert(key.into(), name.into());
            }
            instance
        }
    }

    fn transport(instance: &Arc<Instance>) -> Arc<Scripted> {
        let instance = Arc::clone(instance);
        Scripted::new(move |request, _| {
            let path = request.url().path().to_owned();
            assert_eq!(
                request.url().host_str(),
                Some("prod-api.zephyr4jiracloud.com")
            );
            assert_eq!(
                request
                    .headers()
                    .get("authorization")
                    .unwrap()
                    .to_str()
                    .unwrap(),
                "Bearer s3cr3t-tkn"
            );
            if path == "/v2/testcases" {
                // Two pages of one case each (page size is the server's to cap).
                let names = instance.names.lock().unwrap().clone();
                let all: Vec<Value> = names.iter().map(|(k, n)| case(k, n)).collect();
                match query(request, "startAt").as_deref() {
                    Some("0") => Reply::json(&envelope(&all[..1], 0, all.len(), false)),
                    Some("1") => Reply::json(&envelope(&all[1..], 1, all.len(), true)),
                    other => panic!("unexpected page {other:?}"),
                }
            } else if let Some(key) = path
                .strip_prefix("/v2/testcases/")
                .and_then(|rest| rest.strip_suffix("/teststeps"))
            {
                let steps = instance
                    .steps
                    .lock()
                    .unwrap()
                    .get(key)
                    .cloned()
                    .unwrap_or_default();
                Reply::json(&envelope(&steps, 0, steps.len(), true))
            } else if path.ends_with("/testscript") {
                Reply::status(StatusCode::NOT_FOUND)
            } else {
                panic!("unexpected request {path}")
            }
        })
    }

    #[test]
    fn the_configuration_is_strict_and_defaults_the_base() {
        assert_eq!(
            config().base_url().as_str(),
            "https://prod-api.zephyr4jiracloud.com/v2"
        );
        for bad in [
            json!({}),
            json!({"zephyr_essential_configuration": {}}),
            json!({"zephyr_essential_configuration": {"token": "a b"}}),
            json!({"zephyr_essential_configuration": {"token": "t", "base_url": "http://x.example"}}),
        ] {
            assert!(
                ZephyrEssentialConfig::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
    }

    #[tokio::test]
    async fn every_page_is_listed_and_a_case_is_its_steps() {
        let instance = Instance::new();
        let source = ZephyrEssentialSource::new(config(), allow(), transport(&instance)).unwrap();
        let listed = source.list().await.expect("list");
        let keys: Vec<_> = listed.iter().map(|r| r.key.as_str()).collect();
        assert_eq!(keys, ["testcase/PROJ-T1.json", "testcase/PROJ-T2.json"]);
        assert!(listed.iter().all(|r| r.version.len() == 64));
        let document = source.fetch("testcase/PROJ-T1.json").await.expect("fetch");
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["testcase"]["name"], "Login");
        assert_eq!(body["steps"][0]["inline"]["description"], "open");
        assert_eq!(document.metadata["type"], "TEST_CASE");
        assert_eq!(document.metadata["steps_type"], "steps");
        assert_eq!(document.metadata["labels"], json!(["smoke"]));
        assert!(!document.metadata.contains_key("content"));
        assert_eq!(document.title, "Login");
    }

    #[tokio::test]
    async fn a_changed_step_or_name_moves_the_version_and_nothing_else_does() {
        let instance = Instance::new();
        let first = ZephyrEssentialSource::new(config(), allow(), transport(&instance)).unwrap();
        let before = first.list().await.unwrap();
        let again = ZephyrEssentialSource::new(config(), allow(), transport(&instance)).unwrap();
        assert_eq!(
            again.list().await.unwrap(),
            before,
            "unchanged source, same versions"
        );
        instance
            .steps
            .lock()
            .unwrap()
            .insert("PROJ-T2".into(), vec![step("close it")]);
        instance
            .names
            .lock()
            .unwrap()
            .insert("PROJ-T1".into(), "Login v2".into());
        let after = ZephyrEssentialSource::new(config(), allow(), transport(&instance))
            .unwrap()
            .list()
            .await
            .unwrap();
        assert_ne!(after[0].version, before[0].version, "a rename moves it");
        assert_ne!(after[1].version, before[1].version, "a step edit moves it");
    }

    #[tokio::test]
    async fn a_case_without_steps_falls_back_to_its_script() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == "/v2/testcases" {
                Reply::json(&envelope(&[case("PROJ-T9", "Scripted")], 0, 1, true))
            } else if path.ends_with("/teststeps") {
                Reply::json(&envelope(&[], 0, 0, true))
            } else if path.ends_with("/testscript") {
                Reply::json(&json!({"id": 5, "text": "Given a user", "type": "bdd"}))
            } else {
                panic!("unexpected {path}")
            }
        });
        let source = ZephyrEssentialSource::new(config(), allow(), transport).unwrap();
        source.list().await.unwrap();
        let document = source.fetch("testcase/PROJ-T9.json").await.unwrap();
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["script"]["text"], "Given a user");
        assert_eq!(document.metadata["steps_type"], "script");
    }

    #[tokio::test]
    async fn a_refused_credential_is_named_and_a_rate_limit_backs_off() {
        for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
            let source = ZephyrEssentialSource::new(
                config(),
                allow(),
                Scripted::new(move |_, _| Reply::status(status)),
            )
            .unwrap();
            let text = source.list().await.expect_err("refused").to_string();
            assert!(text.contains("refused the credential"), "{text}");
            assert!(!text.contains("s3cr3t"), "{text}");
        }
        let source = ZephyrEssentialSource::new(
            config(),
            allow(),
            Scripted::new(|_, _| Reply::rate_limited(Some("1"))),
        )
        .unwrap()
        .with_backoff(Backoff {
            max_attempts: 2,
            initial: std::time::Duration::from_millis(1),
            max_wait: std::time::Duration::from_secs(5),
        });
        assert!(
            source
                .list()
                .await
                .expect_err("limited")
                .to_string()
                .contains("rate limited")
        );
    }

    #[tokio::test]
    async fn a_host_off_the_allowlist_is_refused_before_any_request() {
        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        assert!(
            ZephyrEssentialSource::new(
                config(),
                HostAllowlist::parse(Some("github.com")),
                transport
            )
            .is_err()
        );
    }
}
