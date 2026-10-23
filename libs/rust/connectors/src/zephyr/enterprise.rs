//! Zephyr Enterprise as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **Configuration.** `zephyr_configuration.{base_url,token}` (the key the
//!   SDK's Zephyr Enterprise toolkit and the agent-runtime family read): the
//!   instance URL (HTTPS, a path prefix is kept) and a bearer token sent to
//!   that origin only. The `index_data` argument is `zql`, required.
//! * **List.** The test cases the ZQL matches
//!   (`GET /flex/services/rest/latest/testcase?zqlquery=`), paged with
//!   `offset` and `pagesize`. The offset is the number of result rows
//!   received, not the number of distinct cases kept, and the listing ends
//!   when that count reaches `resultSize`. A server that ignores the paging
//!   parameters and answers with everything is read as one page; one that
//!   owes more rows and then serves only rows already seen is an error,
//!   never a silently short listing (the SDK took whatever the first answer
//!   held). A case is `testcase/{id}.json`, where `id` is the result's
//!   `testcase.id`. Zephyr's documentation identifies that field as the
//!   test case *version* id (its `testcaseId` is the id shared by every
//!   version of the case); it is the id the SDK keys by and the id the
//!   documented `testcase/{testcaseVersionId}/teststep` path takes. The
//!   document is versioned by `lastModifiedOn`.
//! * **Fetch.** The case is read again (`testcase/{id}`) and refused as
//!   `SourceError::Changed` when `lastModifiedOn` moved or when the answer
//!   is for another id. Its steps are read for that same version id
//!   (`testcase/{id}/teststep`), so the stamp checked, the case and the
//!   steps all describe one case version (the SDK took the last of the
//!   case's versions, which may be another one). A steps answer that is
//!   JSON `null`, a 404, or holds no `steps` is a case without steps; a
//!   body that is not JSON is an error. The document is the case with
//!   those steps (the SDK indexed the steps alone). A case has no
//!   dependents.
//! * **Auth and limits.** A 401 or 403 is an error naming the credential,
//!   never an empty listing; a 429 backs off within the shared bound.

use std::collections::BTreeSet;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value, json};
use url::Url;

use super::ZephyrClient;
use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, DetailedListing, Details, safe_identifier,
    text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references};
use crate::testmgmt::{https_base, printable_secret, refuse, required_text, scalar_text};
use crate::transport::Transport;

pub const PROVIDER: &str = "Zephyr Enterprise";
/// `flex/services/rest/latest`, the SDK's `ZephyrEndpoints` prefix.
const API: [&str; 4] = ["flex", "services", "rest", "latest"];
/// Cases asked per page.
const PAGE_SIZE: usize = 500;

/// Invocation-scoped Zephyr Enterprise authority for one index run.
pub struct ZephyrEnterpriseConfig {
    base: Url,
    token: zeroize::Zeroizing<String>,
}

impl ZephyrEnterpriseConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `zephyr_configuration`, base URL or token, or a base URL
    /// that is not HTTPS.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("zephyr_configuration")
            .and_then(Value::as_object)
            .ok_or(refuse(
                PROVIDER,
                AtlassianConfigErrorCode::InvalidConfiguration,
            ))?;
        Ok(Self {
            base: https_base(
                PROVIDER,
                required_text(PROVIDER, configuration, "base_url", 2 * 1_024)?,
                BasePath::Prefix,
            )?,
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

/// The `index_data` arguments the connector honours.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ZephyrEnterpriseIndexArguments {
    pub zql: String,
}

impl ZephyrEnterpriseIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// No `zql`, or one that is not text.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        Ok(Self {
            zql: crate::testmgmt::text_argument(PROVIDER, arguments, "zql")?.ok_or(refuse(
                PROVIDER,
                AtlassianConfigErrorCode::InvalidConfiguration,
            ))?,
        })
    }
}

/// One Zephyr Enterprise instance's test cases for a ZQL.
pub struct ZephyrEnterpriseSource {
    client: ZephyrClient,
    arguments: ZephyrEnterpriseIndexArguments,
    listing: DetailedListing,
}

impl ZephyrEnterpriseSource {
    /// The `config` indexed with `arguments` through `allowlist`
    /// (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The Zephyr host is not on the allowlist.
    pub fn new(
        config: ZephyrEnterpriseConfig,
        arguments: ZephyrEnterpriseIndexArguments,
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
            arguments,
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

    fn path<'a>(tail: &[&'a str]) -> Vec<&'a str> {
        let mut path = API.to_vec();
        path.extend_from_slice(tail);
        path
    }

    /// The `testcase` of every result the ZQL matches, de-duplicated by id.
    async fn cases(&self) -> Result<Vec<Value>, Failure> {
        let mut cases: Vec<Value> = Vec::new();
        let mut seen: BTreeSet<String> = BTreeSet::new();
        // Result rows received so far: the paging offset.
        let mut offset = 0_usize;
        let mut pages_seen = 0_usize;
        loop {
            pages_seen += 1;
            self.client.http.check_pages(pages_seen)?;
            let mut page = self
                .client
                .json(
                    &Self::path(&["testcase"]),
                    &[
                        ("zqlquery", self.arguments.zql.clone()),
                        ("offset", offset.to_string()),
                        ("pagesize", PAGE_SIZE.to_string()),
                    ],
                )
                .await?;
            let total = page.get("resultSize").and_then(Value::as_u64);
            let results =
                crate::testmgmt::take_list(&mut page, "results", "the results are not a list")?;
            if results.is_empty() {
                // An empty page while `resultSize` says rows are still owed
                // is an error, never a silently short listing.
                if total.is_some_and(|total| (offset as u64) < total) {
                    return Err(Failure::InvalidResponse(
                        "an empty page while `resultSize` says more cases are owed",
                    ));
                }
                return Ok(cases);
            }
            let rows = results.len();
            let before = cases.len();
            for mut result in results {
                let Some(case @ Value::Object(_)) = result.get_mut("testcase").map(Value::take)
                else {
                    return Err(Failure::InvalidResponse("a result holds no `testcase`"));
                };
                let id = case
                    .get("id")
                    .and_then(scalar_text)
                    .ok_or(Failure::InvalidResponse("a test case has no id"))?;
                if !safe_identifier(&id) {
                    return Err(Failure::InvalidResponse(
                        "a test case id is not an identifier",
                    ));
                }
                if seen.insert(id) {
                    cases.push(case);
                }
            }
            self.client.http.check_count(cases.len())?;
            let got = cases.len() - before;
            let owed_before = total.is_some_and(|total| (offset as u64) < total);
            offset += rows;
            match total {
                // More rows are owed but this page held nothing new: the
                // server repeats itself rather than paging.
                Some(_) if got == 0 && owed_before => {
                    return Err(Failure::InvalidResponse(
                        "the server does not page and returned fewer cases than `resultSize`",
                    ));
                }
                Some(total) if offset as u64 >= total => return Ok(cases),
                None if rows < PAGE_SIZE => return Ok(cases),
                Some(_) | None => {}
            }
        }
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut listed = Vec::new();
        let mut details = Details::new();
        for case in self.cases().await? {
            let id = case
                .get("id")
                .and_then(scalar_text)
                .ok_or(Failure::InvalidResponse("a test case has no id"))?;
            let version = case.get("lastModifiedOn").and_then(scalar_text).ok_or(
                Failure::InvalidResponse("a test case carries no `lastModifiedOn`"),
            )?;
            let key = format!("testcase/{id}.json");
            listed.push(Listed::new(key.clone(), version, 0, id));
            self.client.http.check_count(listed.len())?;
            details.insert(key, Map::new());
        }
        Ok((listed, details))
    }

    /// The steps of the case version `version_id` (the case's `id`). A
    /// version without steps (JSON `null`, no `steps`, or a 404) is
    /// `null`; anything that is not JSON is an error.
    async fn steps(&self, version_id: &str) -> Result<Value, Failure> {
        match self
            .client
            .json_or_absent(&Self::path(&["testcase", version_id, "teststep"]), &[])
            .await?
        {
            None | Some(Value::Null) => Ok(Value::Null),
            Some(Value::Object(object)) if !object.contains_key("steps") => Ok(Value::Null),
            Some(other) => Ok(other),
        }
    }
}

impl ContentSource for ZephyrEnterpriseSource {
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
        let failed = |failure: Failure| failure.into_source_error(PROVIDER, Some(key));
        let fresh = self
            .client
            .json(&Self::path(&["testcase", &listed.handle]), &[])
            .await
            .map_err(failed)?;
        let case = fresh
            .get("testcase")
            .filter(|case| case.is_object())
            .ok_or_else(|| failed(Failure::InvalidResponse("the answer holds no `testcase`")))?;
        let version = case
            .get("lastModifiedOn")
            .and_then(scalar_text)
            .ok_or_else(|| {
                failed(Failure::InvalidResponse(
                    "a test case carries no `lastModifiedOn`",
                ))
            })?;
        if version != listed.reference.version {
            return Err(failed(Failure::Changed));
        }
        if case.get("id").and_then(scalar_text).as_deref() != Some(listed.handle.as_str()) {
            return Err(failed(Failure::InvalidResponse(
                "the answer is for another test case version",
            )));
        }
        let testcase_id = case
            .get("testcaseId")
            .and_then(scalar_text)
            .filter(|id| safe_identifier(id))
            .ok_or_else(|| failed(Failure::InvalidResponse("a test case has no `testcaseId`")))?;
        let steps = self.steps(&listed.handle).await.map_err(failed)?;
        let body = json!({"testcase": case, "steps": steps});
        let bytes = serde_json::to_vec(&body)
            .map_err(|_| failed(Failure::InvalidResponse("a test case cannot be encoded")))?;
        let mut metadata = Map::new();
        metadata.insert("updated_on".into(), Value::String(version));
        metadata.insert("id".into(), Value::String(listed.handle.clone()));
        metadata.insert("testcaseId".into(), Value::String(testcase_id));
        for (name, field) in [
            ("name", "name"),
            ("projectId", "projectId"),
            ("projectName", "projectName"),
            ("testcaseType", "testcaseType"),
        ] {
            if let Some(value) = case.get(field).filter(|value| !value.is_null()) {
                metadata.insert(name.into(), value.clone());
            }
        }
        let title = text_of(case, "/name")
            .map_or_else(|| format!("Test case {}", listed.handle), ToOwned::to_owned);
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title,
            uri: None,
            reference,
            bytes,
            metadata,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;

    fn config() -> ZephyrEnterpriseConfig {
        ZephyrEnterpriseConfig::parse(
            json!({"zephyr_configuration": {"base_url": "https://zephyr.corp.example/zephyr", "token": "s3cr3t-tkn"}})
                .as_object()
                .unwrap(),
        )
        .expect("config")
    }

    fn arguments(zql: &str) -> ZephyrEnterpriseIndexArguments {
        ZephyrEnterpriseIndexArguments::parse(json!({"zql": zql}).as_object().unwrap())
            .expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("zephyr.corp.example"))
    }

    fn case(id: u64, modified: u64, name: &str) -> Value {
        json!({"id": id, "testcaseId": id * 10, "name": name, "projectId": 75,
               "projectName": "Shop", "testcaseType": "Manual", "lastModifiedOn": modified})
    }

    fn results(cases: &[Value], size: usize) -> Value {
        json!({"resultSize": size,
               "results": cases.iter().map(|c| json!({"testcase": c, "tcrCatalogTreeId": 1})).collect::<Vec<_>>()})
    }

    const PREFIX: &str = "/zephyr/flex/services/rest/latest";

    #[test]
    fn the_configuration_and_arguments_are_strict() {
        assert!(ZephyrEnterpriseIndexArguments::parse(&Map::new()).is_err());
        for bad in [
            json!({}),
            json!({"zephyr_configuration": {"token": "t"}}),
            json!({"zephyr_configuration": {"base_url": "http://zephyr.corp.example", "token": "t"}}),
            json!({"zephyr_configuration": {"base_url": "https://zephyr.corp.example", "token": "a b"}}),
        ] {
            assert!(
                ZephyrEnterpriseConfig::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
    }

    #[tokio::test]
    async fn the_zql_is_paged_and_a_case_is_its_own_versions_steps() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path().to_owned();
            assert_eq!(request.url().host_str(), Some("zephyr.corp.example"));
            assert_eq!(
                request
                    .headers()
                    .get("authorization")
                    .unwrap()
                    .to_str()
                    .unwrap(),
                "Bearer s3cr3t-tkn"
            );
            if path == format!("{PREFIX}/testcase") {
                assert_eq!(
                    query(request, "zqlquery").as_deref(),
                    Some("folder=\"Smoke\"")
                );
                match query(request, "offset").as_deref() {
                    Some("0") => Reply::json(&results(&[case(1, 100, "Login")], 2)),
                    Some("1") => Reply::json(&results(&[case(2, 200, "Logout")], 2)),
                    other => panic!("unexpected offset {other:?}"),
                }
            } else if path == format!("{PREFIX}/testcase/2") {
                Reply::json(&json!({"testcase": case(2, 200, "Logout")}))
            } else if path == format!("{PREFIX}/testcase/2/teststep") {
                Reply::json(
                    &json!({"id": 5, "steps": [{"orderId": 1, "step": "Click", "data": "", "result": "Out"}]}),
                )
            } else {
                panic!("unexpected request {path}")
            }
        });
        let source = ZephyrEnterpriseSource::new(
            config(),
            arguments("folder=\"Smoke\""),
            allow(),
            transport,
        )
        .unwrap();
        let listed = source.list().await.expect("list");
        assert_eq!(
            listed
                .iter()
                .map(|r| (r.key.as_str(), r.version.as_str()))
                .collect::<Vec<_>>(),
            [("testcase/1.json", "100"), ("testcase/2.json", "200")]
        );
        let document = source.fetch("testcase/2.json").await.expect("fetch");
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["steps"]["steps"][0]["step"], "Click");
        assert_eq!(body["testcase"]["name"], "Logout");
        assert_eq!(document.metadata["testcaseId"], "20");
        assert_eq!(document.metadata["updated_on"], "200");
        assert_eq!(document.title, "Logout");
    }

    #[tokio::test]
    async fn a_server_that_ignores_paging_is_read_whole_and_one_that_repeats_is_an_error() {
        // Everything on the first answer, whatever `offset` says.
        let all = [case(1, 1, "A"), case(2, 2, "B"), case(3, 3, "C")];
        let transport = Scripted::new(move |_, _| Reply::json(&results(&all, 3)));
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        assert_eq!(source.list().await.unwrap().len(), 3);

        // Promises three, serves the same two every time.
        let two = [case(1, 1, "A"), case(2, 2, "B")];
        let transport = Scripted::new(move |_, _| Reply::json(&results(&two, 3)));
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        let error = source.list().await.expect_err("short").to_string();
        assert!(error.contains("does not page"), "{error}");

        // No match is a real empty listing.
        let transport = Scripted::new(|_, _| Reply::json(&json!({"resultSize": 0, "results": []})));
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        assert_eq!(source.list().await.unwrap(), []);
    }

    #[tokio::test]
    async fn an_empty_page_while_rows_are_owed_is_an_error() {
        // Page 1 holds one of three; page 2 is empty.
        let transport = Scripted::new(|request, _| match query(request, "offset").as_deref() {
            Some("0") => Reply::json(&results(&[case(1, 1, "A")], 3)),
            _ => Reply::json(&results(&[], 3)),
        });
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        let error = source.list().await.expect_err("short").to_string();
        assert!(error.contains("resultSize"), "{error}");
        // Empty on the very first page while rows are owed.
        let transport = Scripted::new(|_, _| Reply::json(&results(&[], 2)));
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        assert!(source.list().await.is_err());
    }

    #[tokio::test]
    async fn a_duplicate_across_pages_does_not_end_the_listing_early() {
        // Four rows, one repeated across the page boundary: the offset is
        // rows received, so the third page is asked for offset 4.
        let transport = Scripted::new(|request, _| match query(request, "offset").as_deref() {
            Some("0") => Reply::json(&results(&[case(1, 1, "A"), case(2, 2, "B")], 5)),
            Some("2") => Reply::json(&results(&[case(2, 2, "B"), case(3, 3, "C")], 5)),
            Some("4") => Reply::json(&results(&[case(4, 4, "D")], 5)),
            other => panic!("unexpected offset {other:?}"),
        });
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        let keys: Vec<_> = source
            .list()
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.key)
            .collect();
        assert_eq!(
            keys,
            [
                "testcase/1.json",
                "testcase/2.json",
                "testcase/3.json",
                "testcase/4.json"
            ]
        );
    }

    #[tokio::test]
    async fn steps_are_the_listed_versions_and_only_an_empty_answer_is_empty() {
        for (body, expected) in [
            (Some(json!(null)), true),
            (Some(json!({"id": 5, "steps": []})), false),
            (Some(json!({"id": 5})), true),
            (None, true),
        ] {
            let transport = Scripted::new(move |request, _| {
                let path = request.url().path();
                if path.ends_with("/teststep") {
                    assert!(path.ends_with("/testcase/1/teststep"), "{path}");
                    match &body {
                        Some(value) => Reply::json(value),
                        None => Reply::status(StatusCode::NOT_FOUND),
                    }
                } else if path.ends_with("/testcase") {
                    Reply::json(&results(&[case(1, 100, "A")], 1))
                } else {
                    Reply::json(&json!({"testcase": case(1, 100, "A")}))
                }
            });
            let source =
                ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport)
                    .unwrap();
            source.list().await.unwrap();
            let document = source.fetch("testcase/1.json").await.expect("fetch");
            let parsed: Value = serde_json::from_slice(&document.bytes).unwrap();
            assert_eq!(parsed["steps"].is_null(), expected, "{parsed}");
        }
    }

    #[tokio::test]
    async fn a_malformed_steps_body_or_another_versions_case_is_an_error() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path.ends_with("/teststep") {
                Reply::bytes(b"<html>gateway</html>")
            } else if path.ends_with("/testcase") {
                Reply::json(&results(&[case(1, 100, "A")], 1))
            } else {
                Reply::json(&json!({"testcase": case(1, 100, "A")}))
            }
        });
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        source.list().await.unwrap();
        assert!(source.fetch("testcase/1.json").await.is_err());

        let transport = Scripted::new(|request, _| {
            if request.url().path().ends_with("/testcase") {
                Reply::json(&results(&[case(1, 100, "A")], 1))
            } else {
                Reply::json(&json!({"testcase": case(7, 100, "A")}))
            }
        });
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        source.list().await.unwrap();
        assert!(source.fetch("testcase/1.json").await.is_err());
    }

    #[tokio::test]
    async fn a_case_edited_after_the_listing_is_changed() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path.ends_with("/testcase") {
                Reply::json(&results(&[case(1, 100, "A")], 1))
            } else {
                Reply::json(&json!({"testcase": case(1, 101, "A")}))
            }
        });
        let source =
            ZephyrEnterpriseSource::new(config(), arguments("x=1"), allow(), transport).unwrap();
        source.list().await.unwrap();
        assert!(matches!(
            source.fetch("testcase/1.json").await,
            Err(SourceError::Changed(_))
        ));
    }

    #[tokio::test]
    async fn a_refused_credential_is_named_and_a_rate_limit_backs_off() {
        for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
            let source = ZephyrEnterpriseSource::new(
                config(),
                arguments("x=1"),
                allow(),
                Scripted::new(move |_, _| Reply::status(status)),
            )
            .unwrap();
            let text = source.list().await.expect_err("refused").to_string();
            assert!(text.contains("refused the credential"), "{text}");
        }
        let source = ZephyrEnterpriseSource::new(
            config(),
            arguments("x=1"),
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
        assert!(
            ZephyrEnterpriseSource::new(
                config(),
                arguments("x=1"),
                HostAllowlist::parse(Some("github.com")),
                Scripted::new(|_, _| panic!("no request may be sent")),
            )
            .is_err()
        );
    }
}
