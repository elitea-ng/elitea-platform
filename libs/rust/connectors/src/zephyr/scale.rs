//! Zephyr Scale (Cloud) as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **Configuration.** `zephyr_configuration.token` (the key the SDK's
//!   Zephyr Scale toolkit and the agent-runtime family read) and an optional
//!   `base_url`. The SDK builds `ZephyrScale(token=...)`, which only ever
//!   calls the Cloud API. the vendor serves it from one fixed origin per data
//!   residency region: `https://api.zephyrscale.smartbear.com/v2` (US, the
//!   default when no `base_url` is given) and
//!   `https://eu.api.zephyrscale.smartbear.com/v2` (EU). The `base_url`
//!   chooses between those two (a trailing `/` or `/v2` is accepted); any
//!   other host is refused, so the token is only ever sent to a Zephyr
//!   Scale Cloud API origin. (the vendor's overview page names only the US
//!   origin; the EU one is the origin of the EU residency region and is
//!   corroborated by third-party Zephyr Scale integrations.)
//! * **`index_data` arguments.** `project_key` (required) and `jql`, the
//!   SDK's JQL-like filter: `folder = "x"`, `folderPath = "a/b"`,
//!   `exactFolderMatch`, `includeSubfolders` (default true),
//!   `label = "x"` / `label in ("x", "y")`, `text ~ "x"`, `limit = n`;
//!   `orderBy` and `orderDirection` are accepted and ignored (an index has
//!   no order). The SDK ignored what it did not understand; this refuses an
//!   unknown key, a malformed clause, and the two filters it cannot honour
//!   here, `customFields` and `steps`, rather than index more than was asked
//!   for. A `folder` or `folderPath` that matches no folder is an error (the
//!   SDK then listed the whole project).
//! * **List.** Every test case of the project (or of the matching folders),
//!   paged with `maxResults` and `startAt` until `isLast`, and the project's
//!   folders of the three types (`TEST_CASE`, `TEST_PLAN`, `TEST_CYCLE`) as
//!   documents of their own, as the SDK yields them. A case is
//!   `testcase/{key}.json`, a folder `folder/{id}.json`.
//! * **Content and version.** The case with its steps, else its script. The
//!   version is the SHA-256 of those bytes: Zephyr Scale exposes no
//!   modification stamp on a case (the SDK used the last version's
//!   `createdOn`, which an in-place edit never moves), so a listing reads
//!   every case's steps. The content is kept from the listing: a fetch
//!   reads nothing again and can never see a version other than the listed
//!   one.
//! * **Auth and limits.** A 401 or 403 is an error naming the credential,
//!   never an empty listing; a 429 backs off within the shared bound.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use futures_util::StreamExt as _;
use serde_json::{Map, Value, json};
use url::Url;

use super::{ZephyrClient, case_metadata};
use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, COMMENT_LISTING_CONCURRENCY, DetailedListing,
    Details, safe_identifier, text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references};
use crate::testmgmt::{
    https_base, identifier_argument, printable_secret, refuse, scalar_text, text_argument,
};
use crate::transport::Transport;

pub const PROVIDER: &str = "Zephyr Scale";
/// `zephyr-python-api`'s `DEFAULT_BASE_URL`, the only origin the SDK's
/// Zephyr Scale wrapper ever calls.
pub const CLOUD_API: &str = "https://api.zephyrscale.smartbear.com/v2";
/// The EU data residency region's Cloud API origin.
pub const CLOUD_API_EU: &str = "https://eu.api.zephyrscale.smartbear.com/v2";
/// Every Zephyr Scale Cloud API origin the token may be sent to.
pub const CLOUD_API_HOSTS: [&str; 2] = [
    "api.zephyrscale.smartbear.com",
    "eu.api.zephyrscale.smartbear.com",
];
const FOLDER_TYPES: [&str; 3] = ["TEST_CASE", "TEST_PLAN", "TEST_CYCLE"];

/// Invocation-scoped Zephyr Scale authority for one index run.
pub struct ZephyrScaleConfig {
    base: Url,
    token: zeroize::Zeroizing<String>,
}

impl ZephyrScaleConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `zephyr_configuration` or token.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("zephyr_configuration")
            .and_then(Value::as_object)
            .ok_or(refuse(
                PROVIDER,
                AtlassianConfigErrorCode::InvalidConfiguration,
            ))?;
        let base = match configuration.get("base_url") {
            Some(Value::String(text)) if !text.trim().is_empty() => origin_for(text),
            // Absent, null, blank or not a string: the default origin. The
            // SDK's field is only a label for the Cloud API.
            _ => CLOUD_API,
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

/// The `SmartBear` origin the credentials go to for the configured
/// `base_url`: the EU one when its host is the documented EU Zephyr Scale
/// API host, else the US one. The SDK's required "Zephyr base URL" field may
/// hold a Jira or any other URL in existing configurations, so an unknown
/// host is not refused; it is ignored with one warning naming its host only
/// (never the path or query). The credential is never sent to the given
/// host.
fn origin_for(given: &str) -> &'static str {
    let host = Url::parse(given.trim())
        .ok()
        .and_then(|url| url.host_str().map(str::to_ascii_lowercase));
    match host.as_deref() {
        Some("eu.api.zephyrscale.smartbear.com") => CLOUD_API_EU,
        Some("api.zephyrscale.smartbear.com") => CLOUD_API,
        other => {
            tracing::warn!(
                host = other.unwrap_or("unparseable"),
                "the Zephyr Scale `base_url` is not a Zephyr Scale API host; using the US API"
            );
            CLOUD_API
        }
    }
}

/// Folder id -> (parent id, name).
type FolderIndex = BTreeMap<String, (Option<String>, String)>;

fn folder_index(folders: &[Value]) -> Result<FolderIndex, Failure> {
    let mut by_id = FolderIndex::new();
    for folder in folders {
        let id = folder
            .get("id")
            .and_then(scalar_text)
            .ok_or(Failure::InvalidResponse("a folder has no id"))?;
        let parent = folder.get("parentId").and_then(scalar_text);
        let name = text_of(folder, "/name").unwrap_or_default().to_owned();
        by_id.insert(id, (parent, name));
    }
    Ok(by_id)
}

/// The `/`-joined names from the root folder down to `id`.
fn folder_path(by_id: &FolderIndex, id: &str) -> String {
    let mut names = Vec::new();
    let mut current = Some(id.to_owned());
    let mut hops = 0;
    while let Some(at) = current.take() {
        hops += 1;
        if hops > 64 {
            break;
        }
        match by_id.get(&at) {
            Some((parent, name)) => {
                names.push(name.clone());
                current.clone_from(parent);
            }
            None => break,
        }
    }
    names.reverse();
    names.join("/")
}

/// The lower-cased text of a case's VALUES for `text ~ "x"` (the SDK's
/// `str(tc.values())`): every scalar value, descending into nested lists
/// and objects' values, but never a field NAME (so `text ~ "status"` does
/// not match every case just because each has a `status` key).
fn values_text(case: &Value) -> String {
    fn walk(value: &Value, out: &mut String) {
        match value {
            Value::Null => {}
            Value::String(text) => {
                out.push_str(&text.to_lowercase());
                out.push('\n');
            }
            Value::Array(items) => items.iter().for_each(|item| walk(item, out)),
            Value::Object(map) => map.values().for_each(|item| walk(item, out)),
            other => {
                out.push_str(&other.to_string().to_lowercase());
                out.push('\n');
            }
        }
    }
    let mut out = String::new();
    walk(case, &mut out);
    out
}

/// The parsed `jql` filter.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ScaleFilter {
    pub folder: Option<String>,
    pub folder_path: Option<String>,
    pub exact_folder_match: bool,
    pub include_subfolders: bool,
    pub labels: Vec<String>,
    pub text: Option<String>,
    pub limit: Option<usize>,
}

/// The `index_data` arguments the connector honours.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ZephyrScaleIndexArguments {
    pub project_key: String,
    pub filter: ScaleFilter,
}

impl ZephyrScaleIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// No `project_key`, an argument of the wrong type, or a `jql` this
    /// connector cannot honour (see the module documentation).
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid = || refuse(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let project_key =
            identifier_argument(PROVIDER, arguments, "project_key")?.ok_or_else(invalid)?;
        let filter = match text_argument(PROVIDER, arguments, "jql")? {
            Some(jql) => parse_jql(&jql).ok_or_else(invalid)?,
            None => ScaleFilter {
                include_subfolders: true,
                ..ScaleFilter::default()
            },
        };
        Ok(Self {
            project_key,
            filter,
        })
    }
}

/// The SDK's `_parse_jql`, read left to right: `key = "v"`, `key ~ "v"`,
/// `key = word`, `key in ("a", "b")`, clauses joined by `AND`. `None` for
/// anything it does not understand.
fn parse_jql(jql: &str) -> Option<ScaleFilter> {
    let mut filter = ScaleFilter {
        include_subfolders: true,
        ..ScaleFilter::default()
    };
    let mut rest = jql.trim();
    while !rest.is_empty() {
        let end = rest
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
            .unwrap_or(rest.len());
        if end == 0 {
            return None;
        }
        let key = &rest[..end];
        rest = rest[end..].trim_start();
        let (operator, after) = if let Some(after) = rest.strip_prefix('=') {
            ("=", after)
        } else if let Some(after) = rest.strip_prefix('~') {
            ("~", after)
        } else if rest
            .get(..2)
            .is_some_and(|word| word.eq_ignore_ascii_case("in"))
        {
            ("in", &rest[2..])
        } else {
            return None;
        };
        rest = after.trim_start();
        let values: Vec<String> = if operator == "in" {
            let inner = rest.strip_prefix('(')?;
            let close = inner.find(')')?;
            let list = &inner[..close];
            rest = inner[close + 1..].trim_start();
            list.split(',')
                .map(|item| item.trim().trim_matches('"').to_owned())
                .filter(|item| !item.is_empty())
                .collect()
        } else if let Some(quoted) = rest.strip_prefix('"') {
            let close = quoted.find('"')?;
            let value = quoted[..close].to_owned();
            rest = quoted[close + 1..].trim_start();
            vec![value]
        } else {
            let end = rest
                .find(|c: char| c.is_whitespace() || c == ')' || c == '(' || c == '"')
                .unwrap_or(rest.len());
            if end == 0 {
                return None;
            }
            let value = rest[..end].to_owned();
            rest = rest[end..].trim_start();
            vec![value]
        };
        let single = values.first().cloned();
        match (key, operator) {
            ("folder", "=" | "~") => filter.folder = single,
            ("folderPath", "=" | "~") => filter.folder_path = single,
            ("exactFolderMatch", "=") => filter.exact_folder_match = flag(single.as_deref())?,
            ("includeSubfolders", "=") => filter.include_subfolders = flag(single.as_deref())?,
            ("label", "=" | "in") => filter.labels.extend(values),
            ("text", "=" | "~") => filter.text = single,
            ("limit", "=") => filter.limit = Some(single?.parse().ok()?),
            ("orderBy" | "orderDirection", "=") => {}
            _ => return None,
        }
        // Clauses are joined by AND.
        if !rest.is_empty() {
            let word = rest.get(..3)?;
            if !word.eq_ignore_ascii_case("and") {
                return None;
            }
            rest = rest[3..].trim_start();
            if rest.is_empty() {
                return None;
            }
        }
    }
    Some(filter)
}

fn flag(value: Option<&str>) -> Option<bool> {
    match value?.to_ascii_lowercase().as_str() {
        "true" => Some(true),
        "false" => Some(false),
        _ => None,
    }
}

/// One Zephyr Scale project's test cases and folders.
pub struct ZephyrScaleSource {
    client: ZephyrClient,
    arguments: ZephyrScaleIndexArguments,
    listing: DetailedListing,
}

impl ZephyrScaleSource {
    /// The `config` indexed with `arguments` through `allowlist`
    /// (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The Zephyr Scale host is not on the allowlist.
    pub fn new(
        config: ZephyrScaleConfig,
        arguments: ZephyrScaleIndexArguments,
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

    async fn folders(&self, folder_type: &str) -> Result<Vec<Value>, Failure> {
        self.client
            .values(
                &["folders"],
                &[
                    ("projectKey", self.arguments.project_key.clone()),
                    ("folderType", folder_type.to_owned()),
                ],
            )
            .await
    }

    /// The folder ids the `folder` / `folderPath` filters select (with
    /// their descendants unless `includeSubfolders = false`); `None` when
    /// neither filter is given.
    fn target_folders(&self, folders: &[Value]) -> Result<Option<Vec<String>>, Failure> {
        let filter = &self.arguments.filter;
        if filter.folder.is_none() && filter.folder_path.is_none() {
            return Ok(None);
        }
        let by_id = folder_index(folders)?;
        let path_of = |id: &str| folder_path(&by_id, id);
        let mut targets: BTreeSet<String> = BTreeSet::new();
        for (id, (_, name)) in &by_id {
            let by_name = filter.folder.as_ref().is_some_and(|wanted| {
                if filter.exact_folder_match {
                    name == wanted
                } else {
                    name.to_lowercase().contains(&wanted.to_lowercase())
                }
            });
            let by_path = filter
                .folder_path
                .as_ref()
                .is_some_and(|wanted| path_of(id) == *wanted);
            if by_name || by_path {
                targets.insert(id.clone());
            }
        }
        if targets.is_empty() {
            return Err(Failure::Client(
                "no test-case folder matches the `folder` or `folderPath` filter".to_owned(),
            ));
        }
        if filter.include_subfolders {
            loop {
                let before = targets.len();
                for (id, (parent, _)) in &by_id {
                    if parent
                        .as_ref()
                        .is_some_and(|parent| targets.contains(parent))
                    {
                        targets.insert(id.clone());
                    }
                }
                if targets.len() == before {
                    break;
                }
            }
        }
        Ok(Some(targets.into_iter().collect()))
    }

    fn keep(&self, case: &Value) -> bool {
        let filter = &self.arguments.filter;
        if !filter.labels.is_empty() {
            let labels: Vec<&str> = case
                .get("labels")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(Value::as_str)
                .collect();
            if !filter
                .labels
                .iter()
                .any(|wanted| labels.contains(&wanted.as_str()))
            {
                return false;
            }
        }
        if let Some(text) = &filter.text
            && !values_text(case).contains(&text.to_lowercase())
        {
            return false;
        }
        true
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one pass: filter, cases with their content, then folders"
    )]
    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let project = self.arguments.project_key.clone();
        let mut folders_by_type: Vec<(&str, Vec<Value>)> = Vec::new();
        for folder_type in FOLDER_TYPES {
            folders_by_type.push((folder_type, self.folders(folder_type).await?));
        }
        let case_folders: &[Value] = &folders_by_type[0].1;
        let targets = self.target_folders(case_folders)?;

        let mut found: BTreeMap<String, Value> = BTreeMap::new();
        // Only the cases the filters keep are held; the rows read are
        // bounded by the page cap, and the document cap by what is kept.
        let mut collect = |cases: Vec<Value>| -> Result<(), Failure> {
            for case in cases {
                let key = text_of(&case, "/key")
                    .ok_or(Failure::InvalidResponse("a test case has no key"))?
                    .to_owned();
                if !safe_identifier(&key) {
                    return Err(Failure::InvalidResponse(
                        "a test case key is not an identifier",
                    ));
                }
                found.entry(key).or_insert(case);
            }
            Ok(())
        };
        let keep = |case: &Value| self.keep(case);
        // With a `limit`, the cap applies to what the cut leaves.
        let capped = self.arguments.filter.limit.is_none();
        match targets {
            Some(ids) => {
                for id in ids {
                    collect(
                        self.client
                            .values_where(
                                &["testcases"],
                                &[("projectKey", project.clone()), ("folderId", id)],
                                capped,
                                keep,
                            )
                            .await?,
                    )?;
                }
            }
            None => collect(
                self.client
                    .values_where(
                        &["testcases"],
                        &[("projectKey", project.clone())],
                        capped,
                        keep,
                    )
                    .await?,
            )?,
        }
        let mut selected: Vec<(String, Value)> = found.into_iter().collect();
        if let Some(limit) = self.arguments.filter.limit {
            // The SDK sorted by name (ascending) before it cut.
            selected.sort_by(|(left_key, left), (right_key, right)| {
                text_of(left, "/name")
                    .unwrap_or_default()
                    .cmp(text_of(right, "/name").unwrap_or_default())
                    .then_with(|| left_key.cmp(right_key))
            });
            selected.truncate(limit);
        }
        self.http_count(selected.len())?;

        let mut listed = Vec::new();
        let mut details = Details::new();
        let mut read =
            futures_util::stream::iter(selected.into_iter().map(|(key, case)| async move {
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
                key,
            ));
            self.http_count(listed.len())?;
            let mut meta = case_metadata(&body["testcase"]);
            meta.insert("type".into(), Value::String("TEST_CASE".into()));
            meta.insert("steps_type".into(), Value::String(kind.to_owned()));
            meta.insert("updated_on".into(), Value::String(version));
            meta.insert(
                "content".into(),
                Value::String(String::from_utf8_lossy(&bytes).into_owned()),
            );
            details.insert(document_key, meta);
        }
        drop(read);

        for (folder_type, folders) in &folders_by_type {
            let by_id = folder_index(folders)?;
            for folder in folders {
                let id = folder
                    .get("id")
                    .and_then(scalar_text)
                    .ok_or(Failure::InvalidResponse("a folder has no id"))?;
                if !safe_identifier(&id) {
                    return Err(Failure::InvalidResponse("a folder id is not an identifier"));
                }
                let document_key = format!("folder/{id}.json");
                if details.contains_key(&document_key) {
                    continue;
                }
                let body = json!({"folder": folder, "path": folder_path(&by_id, &id)});
                let bytes = serde_json::to_vec(&body)
                    .map_err(|_| Failure::InvalidResponse("a folder cannot be encoded"))?;
                let version = content_version(&bytes);
                listed.push(Listed::new(
                    document_key.clone(),
                    version.clone(),
                    bytes.len() as u64,
                    id,
                ));
                self.http_count(listed.len())?;
                let mut meta = case_metadata(folder);
                meta.insert("type".into(), Value::String("FOLDER".into()));
                meta.insert(
                    "folder_type".into(),
                    Value::String((*folder_type).to_owned()),
                );
                meta.insert("updated_on".into(), Value::String(version));
                meta.insert(
                    "content".into(),
                    Value::String(String::from_utf8_lossy(&bytes).into_owned()),
                );
                details.insert(document_key, meta);
            }
        }
        Ok((listed, details))
    }

    fn http_count(&self, count: usize) -> Result<(), Failure> {
        self.client.http.check_count(count)
    }
}

impl ContentSource for ZephyrScaleSource {
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

    fn config() -> ZephyrScaleConfig {
        ZephyrScaleConfig::parse(
            json!({"zephyr_configuration": {"token": "s3cr3t-tkn"}})
                .as_object()
                .unwrap(),
        )
        .expect("config")
    }

    fn arguments(value: &Value) -> ZephyrScaleIndexArguments {
        ZephyrScaleIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("api.zephyrscale.smartbear.com"))
    }

    fn envelope(values: &[Value], start: usize, total: usize, last: bool) -> Value {
        json!({"next": null, "startAt": start, "maxResults": 200, "total": total,
               "isLast": last, "values": values})
    }

    fn case(key: &str, name: &str, folder: Option<u64>, labels: &[&str]) -> Value {
        json!({"id": 1, "key": key, "name": name,
               "project": {"id": 10_000, "self": "https://api.zephyrscale.smartbear.com/v2/projects/10000"},
               "createdOn": "2026-09-01T00:00:00Z", "objective": "obj", "precondition": null,
               "estimatedTime": 138_000, "labels": labels,
               "component": null, "priority": {"id": 1}, "status": {"id": 2},
               "folder": folder.map(|id| json!({"id": id})), "owner": {"accountId": "abc"},
               "customFields": {"Team": "A"}})
    }

    fn folder(id: u64, parent: Option<u64>, name: &str, kind: &str) -> Value {
        json!({"id": id, "parentId": parent, "name": name, "index": 0, "folderType": kind,
               "project": {"id": 10_000}})
    }

    struct Project {
        cases: Vec<Value>,
        steps: BTreeMap<String, String>,
    }

    fn transport(project: Arc<std::sync::Mutex<Project>>) -> Arc<Scripted> {
        Scripted::new(move |request, _| {
            let project = project.lock().unwrap();
            assert_eq!(
                request.url().host_str(),
                Some("api.zephyrscale.smartbear.com")
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
            let path = request.url().path().to_owned();
            if path == "/v2/folders" {
                let all = vec![
                    folder(1, None, "Root", "TEST_CASE"),
                    folder(2, Some(1), "Login", "TEST_CASE"),
                    folder(3, Some(2), "Sub", "TEST_CASE"),
                    folder(4, None, "Plans", "TEST_PLAN"),
                ];
                let wanted = query(request, "folderType").unwrap();
                let values: Vec<Value> = all
                    .into_iter()
                    .filter(|f| f["folderType"] == wanted.as_str())
                    .collect();
                Reply::json(&envelope(&values, 0, values.len(), true))
            } else if path == "/v2/testcases" {
                assert_eq!(query(request, "projectKey").as_deref(), Some("PROJ"));
                let folder_filter = query(request, "folderId");
                let values: Vec<Value> = project
                    .cases
                    .iter()
                    .filter(|c| match &folder_filter {
                        Some(id) => {
                            c["folder"]["id"]
                                .as_u64()
                                .map(|folder| folder.to_string())
                                .as_deref()
                                == Some(id.as_str())
                        }
                        None => true,
                    })
                    .cloned()
                    .collect();
                // Two pages when everything is listed.
                if folder_filter.is_none() && values.len() > 1 {
                    match query(request, "startAt").as_deref() {
                        Some("0") => Reply::json(&envelope(&values[..1], 0, values.len(), false)),
                        _ => Reply::json(&envelope(&values[1..], 1, values.len(), true)),
                    }
                } else {
                    Reply::json(&envelope(&values, 0, values.len(), true))
                }
            } else if let Some(key) = path
                .strip_prefix("/v2/testcases/")
                .and_then(|rest| rest.strip_suffix("/teststeps"))
            {
                match project.steps.get(key) {
                    Some(text) => Reply::json(&envelope(
                        &[json!({"inline": {"description": text, "testData": null,
                                            "expectedResult": "ok", "customFields": {}, "reflectRef": null},
                                 "testCase": null})],
                        0,
                        1,
                        true,
                    )),
                    None => Reply::json(&envelope(&[], 0, 0, true)),
                }
            } else if path.ends_with("/testscript") {
                Reply::status(StatusCode::NOT_FOUND)
            } else {
                panic!("unexpected request {path}")
            }
        })
    }

    fn project() -> Arc<std::sync::Mutex<Project>> {
        Arc::new(std::sync::Mutex::new(Project {
            cases: vec![
                case("PROJ-T1", "Login ok", Some(2), &["smoke"]),
                case("PROJ-T2", "Login deep", Some(3), &["regression"]),
                case("PROJ-T3", "Elsewhere", Some(1), &["smoke"]),
            ],
            steps: BTreeMap::from([
                ("PROJ-T1".to_owned(), "open".to_owned()),
                ("PROJ-T2".to_owned(), "dig".to_owned()),
            ]),
        }))
    }

    #[test]
    fn the_jql_is_read_like_the_sdk_and_unknown_clauses_are_refused() {
        let filter = parse_jql(
            r#"folder = "Login Tests" AND label in ("Smoke", "Critical") AND text ~ "login" AND orderBy = "name" AND limit = 5 AND includeSubfolders = false"#,
        )
        .expect("parses");
        assert_eq!(filter.folder.as_deref(), Some("Login Tests"));
        assert_eq!(filter.labels, ["Smoke", "Critical"]);
        assert_eq!(filter.text.as_deref(), Some("login"));
        assert_eq!(filter.limit, Some(5));
        assert!(!filter.include_subfolders);
        for refused in [
            r#"steps ~ "click""#,
            r#"customFields = "{}""#,
            "folder",
            r#"folder = "x" label = "y""#,
            r#"bogus = "x""#,
            "limit = many",
            r#"folder = "x" AND"#,
        ] {
            assert!(parse_jql(refused).is_none(), "{refused}");
        }
        assert!(
            ZephyrScaleIndexArguments::parse(&Map::new()).is_err(),
            "project_key"
        );
    }

    #[test]
    fn the_origin_is_one_of_the_documented_cloud_api_regions() {
        assert_eq!(config().base_url().as_str(), CLOUD_API);
        let parse = |base: &str| {
            ZephyrScaleConfig::parse(
                json!({"zephyr_configuration": {"base_url": base, "token": "t"}})
                    .as_object()
                    .unwrap(),
            )
        };
        for (given, expected) in [
            ("https://api.zephyrscale.smartbear.com", CLOUD_API),
            ("https://api.zephyrscale.smartbear.com/v2/", CLOUD_API),
            ("https://eu.api.zephyrscale.smartbear.com/v2", CLOUD_API_EU),
            ("https://EU.api.zephyrscale.smartbear.com", CLOUD_API_EU),
        ] {
            assert_eq!(
                parse(given).unwrap().base_url().as_str(),
                expected,
                "{given}"
            );
        }
        // An unknown host (a Jira URL, anything else) is ignored: the US
        // origin is used and the credential never goes to the given host.
        for other in [
            "https://evil.example",
            "https://zephyr.atlassian.net/jira",
            "https://api.zephyrscale.smartbear.com.evil.example/v2",
            "not a url",
            "",
        ] {
            let config = parse(other).unwrap();
            assert_eq!(config.base_url().as_str(), CLOUD_API, "{other}");
        }
        for (odd, expected) in [
            ("http://eu.api.zephyrscale.smartbear.com/v2", CLOUD_API_EU),
            ("https://api.zephyrscale.smartbear.com:8443/v3", CLOUD_API),
        ] {
            assert_eq!(parse(odd).unwrap().base_url().as_str(), expected, "{odd}");
        }
        assert_eq!(
            ZephyrScaleConfig::parse(
                json!({"zephyr_configuration": {"base_url": 7, "token": "t"}})
                    .as_object()
                    .unwrap()
            )
            .unwrap()
            .base_url()
            .as_str(),
            CLOUD_API
        );
        assert!(ZephyrScaleConfig::parse(&Map::new()).is_err());
        assert!(
            ZephyrScaleConfig::parse(json!({"zephyr_configuration": {}}).as_object().unwrap())
                .is_err()
        );
    }

    #[tokio::test]
    async fn an_empty_page_before_the_reported_total_is_an_error() {
        for page in [
            // isLast false, nothing returned.
            json!({"startAt": 0, "total": 5, "isLast": false, "values": []}),
            // No isLast, total not reached.
            json!({"startAt": 0, "total": 5, "values": []}),
        ] {
            let transport = Scripted::new(move |_, _| Reply::json(&page));
            let source = ZephyrScaleSource::new(
                config(),
                arguments(&json!({"project_key": "PROJ"})),
                allow(),
                transport,
            )
            .unwrap();
            let error = source.list().await.expect_err("short").to_string();
            assert!(error.contains("total"), "{error}");
        }
        // A genuinely empty listing (total 0, isLast) is fine.
        let transport = Scripted::new(|_, _| Reply::json(&envelope(&[], 0, 0, true)));
        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ"})),
            allow(),
            transport,
        )
        .unwrap();
        assert!(source.list().await.unwrap().is_empty());
    }

    #[test]
    fn text_filter_matches_values_never_field_names() {
        let source = |jql: &str| {
            ZephyrScaleSource::new(
                config(),
                arguments(&json!({"project_key": "PROJ", "jql": jql})),
                allow(),
                Scripted::new(|_, _| panic!("no request")),
            )
            .unwrap()
        };
        let login = json!({"key": "PROJ-T1", "name": "Login works", "status": {"id": 7, "self": "x"},
                           "labels": ["Smoke"]});
        let other = json!({"key": "PROJ-T2", "name": "Checkout", "status": {"id": 8}});
        // Every case has a `status` and a `labels`-like key: not a match.
        let by_key = source(r#"text ~ "status""#);
        assert!(!by_key.keep(&login) && !by_key.keep(&other));
        let by_key = source(r#"text ~ "name""#);
        assert!(!by_key.keep(&login));
        // Values match, case-insensitively, including nested ones.
        let by_value = source(r#"text ~ "LOGIN""#);
        assert!(by_value.keep(&login) && !by_value.keep(&other));
        assert!(source(r#"text ~ "smoke""#).keep(&login));
        assert!(source(r#"text ~ "7""#).keep(&login));
    }

    #[test]
    fn non_ascii_jql_is_refused_not_a_panic() {
        for text in [
            "folder\u{20ac}\"x\"",
            "label \u{e9}",
            "label \u{e9}\u{e9} (\"a\")",
            "folder = \"x\" \u{e9}\u{e9}",
            "folder = \"x\" an\u{e9}",
            "\u{20ac} = \"x\"",
        ] {
            assert!(parse_jql(text).is_none(), "{text}");
            let arguments = json!({"project_key": "PROJ", "jql": text});
            assert!(
                ZephyrScaleIndexArguments::parse(arguments.as_object().unwrap()).is_err(),
                "{text}"
            );
        }
        // Non-ASCII values are fine.
        assert_eq!(
            parse_jql("folder = \"Caf\u{e9}\"")
                .unwrap()
                .folder
                .as_deref(),
            Some("Caf\u{e9}")
        );
    }

    #[tokio::test]
    async fn the_eu_region_is_listed_from_the_eu_origin_only() {
        let config = ZephyrScaleConfig::parse(
            json!({"zephyr_configuration": {"base_url": "https://eu.api.zephyrscale.smartbear.com/v2", "token": "s3cr3t-tkn"}})
                .as_object()
                .unwrap(),
        )
        .unwrap();
        let transport = Scripted::new(|request, _| {
            assert_eq!(
                request.url().host_str(),
                Some("eu.api.zephyrscale.smartbear.com")
            );
            let path = request.url().path();
            if path == "/v2/folders" {
                Reply::json(&envelope(&[], 0, 0, true))
            } else if path == "/v2/testcases" {
                Reply::json(&envelope(
                    &[case("PROJ-T9", "EU case", None, &[])],
                    0,
                    1,
                    true,
                ))
            } else if path.ends_with("/teststeps") {
                Reply::json(&envelope(&[], 0, 0, true))
            } else {
                Reply::status(StatusCode::NOT_FOUND)
            }
        });
        let source = ZephyrScaleSource::new(
            config,
            arguments(&json!({"project_key": "PROJ"})),
            HostAllowlist::parse(Some("eu.api.zephyrscale.smartbear.com")),
            transport,
        )
        .unwrap();
        assert_eq!(source.list().await.unwrap().len(), 1);
        // The US allowlist does not permit the EU origin.
        assert!(
            ZephyrScaleSource::new(
                ZephyrScaleConfig::parse(
                    json!({"zephyr_configuration": {"base_url": "https://eu.api.zephyrscale.smartbear.com", "token": "t"}})
                        .as_object()
                        .unwrap()
                )
                .unwrap(),
                arguments(&json!({"project_key": "PROJ"})),
                allow(),
                Scripted::new(|_, _| panic!("no request may be sent")),
            )
            .is_err()
        );
    }

    #[tokio::test]
    async fn the_document_cap_counts_kept_cases_not_the_projects_size() {
        let many = |n: usize| {
            let cases: Vec<Value> = (1..=n)
                .map(|id| {
                    let label: &[&str] = if id <= 2 { &["keep"] } else { &["drop"] };
                    case(&format!("PROJ-T{id}"), &format!("Case {id}"), None, label)
                })
                .collect();
            Scripted::new(move |request, _| {
                let path = request.url().path();
                if path == "/v2/folders" {
                    Reply::json(&envelope(&[], 0, 0, true))
                } else if path == "/v2/testcases" {
                    Reply::json(&envelope(&cases, 0, cases.len(), true))
                } else if path.ends_with("/teststeps") {
                    Reply::json(&envelope(&[], 0, 0, true))
                } else {
                    Reply::status(StatusCode::NOT_FOUND)
                }
            })
        };
        let limits = SourceLimits {
            max_documents: 3,
            ..SourceLimits::default()
        };
        let narrow = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ", "jql": r#"label = "keep""#})),
            allow(),
            many(10),
        )
        .unwrap()
        .with_limits(limits);
        assert_eq!(narrow.list().await.unwrap().len(), 2);
        let wide = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ"})),
            allow(),
            many(10),
        )
        .unwrap()
        .with_limits(limits);
        assert!(wide.list().await.is_err());
        // `limit` cuts before the cap is applied.
        let limited = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ", "jql": "limit = 3"})),
            allow(),
            many(10),
        )
        .unwrap()
        .with_limits(limits);
        assert_eq!(limited.list().await.unwrap().len(), 3);
    }

    #[tokio::test]
    async fn cases_and_folders_are_listed_with_content_versions() {
        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ"})),
            allow(),
            transport(project()),
        )
        .unwrap();
        let listed = source.list().await.expect("list");
        let keys: Vec<&str> = listed.iter().map(|r| r.key.as_str()).collect();
        for expected in [
            "testcase/PROJ-T1.json",
            "testcase/PROJ-T2.json",
            "testcase/PROJ-T3.json",
            "folder/1.json",
            "folder/2.json",
            "folder/3.json",
            "folder/4.json",
        ] {
            assert!(keys.contains(&expected), "{expected} in {keys:?}");
        }
        let document = source.fetch("testcase/PROJ-T1.json").await.expect("case");
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["steps"][0]["inline"]["description"], "open");
        assert_eq!(document.metadata["type"], "TEST_CASE");
        assert_eq!(document.metadata["steps_type"], "steps");
        let empty = source
            .fetch("testcase/PROJ-T3.json")
            .await
            .expect("no steps");
        assert_eq!(empty.metadata["steps_type"], "empty");
        let folder = source.fetch("folder/3.json").await.expect("folder");
        let body: Value = serde_json::from_slice(&folder.bytes).unwrap();
        assert_eq!(body["path"], "Root/Login/Sub");
        assert_eq!(folder.metadata["type"], "FOLDER");
        assert_eq!(folder.metadata["folder_type"], "TEST_CASE");
    }

    #[tokio::test]
    async fn the_folder_filter_reaches_the_subfolders_and_the_label_filter_narrows() {
        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ", "jql": r#"folderPath = "Root/Login""#})),
            allow(),
            transport(project()),
        )
        .unwrap();
        let cases: Vec<String> = source
            .list()
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.key)
            .filter(|key| key.starts_with("testcase/"))
            .collect();
        assert_eq!(cases, ["testcase/PROJ-T1.json", "testcase/PROJ-T2.json"]);

        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ", "jql": r#"folder = "login" AND includeSubfolders = false AND label in ("smoke")"#})),
            allow(),
            transport(project()),
        )
        .unwrap();
        let cases: Vec<String> = source
            .list()
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.key)
            .filter(|key| key.starts_with("testcase/"))
            .collect();
        assert_eq!(cases, ["testcase/PROJ-T1.json"]);

        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ", "jql": r#"folder = "Nowhere""#})),
            allow(),
            transport(project()),
        )
        .unwrap();
        let error = source.list().await.expect_err("no folder").to_string();
        assert!(error.contains("no test-case folder matches"), "{error}");
    }

    #[tokio::test]
    async fn the_version_moves_with_the_steps_and_nothing_else() {
        let shared = project();
        let list = |shared: &Arc<std::sync::Mutex<Project>>| {
            let source = ZephyrScaleSource::new(
                config(),
                arguments(&json!({"project_key": "PROJ"})),
                allow(),
                transport(Arc::clone(shared)),
            )
            .unwrap();
            async move {
                source
                    .list()
                    .await
                    .unwrap()
                    .into_iter()
                    .map(|r| (r.key, r.version))
                    .collect::<BTreeMap<_, _>>()
            }
        };
        let before = list(&shared).await;
        assert_eq!(list(&shared).await, before);
        shared
            .lock()
            .unwrap()
            .steps
            .insert("PROJ-T1".into(), "open wide".into());
        let after = list(&shared).await;
        let moved: Vec<&String> = before
            .iter()
            .filter(|(key, version)| after.get(*key) != Some(*version))
            .map(|(key, _)| key)
            .collect();
        assert_eq!(moved, ["testcase/PROJ-T1.json"]);
        // A case deleted at the source is gone from the listing.
        shared
            .lock()
            .unwrap()
            .cases
            .retain(|c| c["key"] != "PROJ-T3");
        assert!(!list(&shared).await.contains_key("testcase/PROJ-T3.json"));
    }

    #[tokio::test]
    async fn a_refused_credential_is_named_and_a_rate_limit_backs_off() {
        for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
            let source = ZephyrScaleSource::new(
                config(),
                arguments(&json!({"project_key": "PROJ"})),
                allow(),
                Scripted::new(move |_, _| Reply::status(status)),
            )
            .unwrap();
            let text = source.list().await.expect_err("refused").to_string();
            assert!(text.contains("refused the credential"), "{text}");
            assert!(!text.contains("s3cr3t"), "{text}");
        }
        let source = ZephyrScaleSource::new(
            config(),
            arguments(&json!({"project_key": "PROJ"})),
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
            ZephyrScaleSource::new(
                config(),
                arguments(&json!({"project_key": "PROJ"})),
                HostAllowlist::parse(Some("github.com")),
                Scripted::new(|_, _| panic!("no request may be sent")),
            )
            .is_err()
        );
    }
}
