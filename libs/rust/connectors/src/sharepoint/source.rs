#![allow(clippy::doc_markdown)] // `SharePoint`, `OneNote` are product names.
//! One SharePoint site's document libraries as a `ContentSource` (ADR-0030
//! I2), over Microsoft Graph.
//!
//! * **List.** The site (`sites/{host}:/{path}`), its drives (the document
//!   libraries; `form_name` picks one), then each drive's — or folder's,
//!   with `path` — **delta** enumeration (`root/delta`,
//!   `root:/{folder}:/delta`): one paged walk that names every file of the
//!   subtree with its content tag and size, no per-folder requests and no
//!   page cap. `include_extensions` and `skip_extensions` filter by file
//!   name as in the SDK; `limit_files` caps the listing only when given.
//! * **Version.** The item's `cTag`, which Graph changes when the content
//!   changes (not on a metadata edit), falling back to `eTag` and then
//!   `lastModifiedDateTime`. The SDK compares `lastModifiedDateTime`.
//! * **Key.** `{library}/{folder path}/{name}`: readable, stable while a
//!   file stays in place (a moved file is a new document). The SDK keys by
//!   the item id, which differs between its REST and Graph backends.
//! * **Fetch.** `drives/{drive}/items/{item}/content`, following Graph's
//!   redirect to the site host, under the document cap.
//! * Lists, list items and OneNote are not indexed (the SDK indexes no
//!   lists either; OneNote needs a delegated user token and image
//!   descriptions, and `include_onenote` is refused with that reason).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::config::SharePointIndexConfig;
use super::graph::{GraphClient, graph_path_url, graph_url, parse_graph_link};
use crate::ado::text::{optional_bool, optional_text, optional_text_list, optional_u64, safe_name};
use crate::egress::HostAllowlist;
use crate::source::glob::NameFilter;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references,
    valid_key,
};
use crate::transport::{StatusCode, Transport, Url};

const PROVIDER: &str = "SharePoint";
const PAGE: &str = "999";
const ITEM_SELECT: &str = "id,name,file,folder,size,cTag,eTag,lastModifiedDateTime,createdDateTime,webUrl,parentReference,deleted";
const DRIVE_SELECT: &str = "id,name,webUrl,driveType";
const MAX_DRIVES: usize = 10_000;

/// The `index_data` arguments of the `sharepoint` family
/// (`sharepoint/api_wrapper.py::_index_tool_params`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SharePointIndexParams {
    /// Files listed at most, when given; `Some(0)` lists none.
    pub limit_files: Option<u64>,
    pub files: NameFilter,
    /// A folder: server-relative (`/sites/eng/Shared Documents/Specs`) or
    /// relative to the library (`Specs`, with `form_name`) or to the site.
    pub path: Option<String>,
    /// A document library's name.
    pub form_name: Option<String>,
}

impl SharePointIndexParams {
    /// Read the arguments as the request carried them.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type, or `include_onenote`.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, String> {
        if optional_bool(arguments, "include_onenote")?.unwrap_or(false) {
            return Err("`include_onenote` is not supported by this worker: OneNote pages need a delegated user token and image descriptions (ADR-0030 I4)".to_owned());
        }
        Ok(Self {
            limit_files: optional_u64(arguments, "limit_files")?,
            files: NameFilter::new(
                optional_text_list(arguments, "include_extensions")?,
                optional_text_list(arguments, "skip_extensions")?,
            ),
            path: optional_text(arguments, "path")?,
            form_name: optional_text(arguments, "form_name")?,
        })
    }
}

/// What a fetch adds to a listed document beyond its bytes.
#[derive(Clone, Debug)]
struct Extra {
    drive_id: String,
    item_id: String,
    library: String,
    path: String,
    name: String,
    web_url: Option<String>,
    created: Option<String>,
    modified: Option<String>,
}

/// One site's document libraries, listed and fetched through Graph.
pub struct SharePointSource {
    graph: GraphClient,
    config: SharePointIndexConfig,
    params: SharePointIndexParams,
    cache: ListingCache,
    extras: Mutex<Arc<BTreeMap<String, Extra>>>,
}

/// One delta walk to make: a drive, and the folder within it when the
/// listing is scoped.
struct Seed {
    drive_id: String,
    library: String,
    folder: Option<String>,
    /// The folder came from a guess (a bare path tried in every library), so
    /// a 404 skips the library instead of failing the listing.
    guessed: bool,
}

impl SharePointSource {
    /// The configured site, every request through `allowlist` (fail-closed)
    /// and then `transport`.
    ///
    /// # Errors
    ///
    /// Graph, the identity provider or the site host is not on the allowlist.
    pub fn new(
        config: SharePointIndexConfig,
        params: SharePointIndexParams,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        let graph = Url::parse(super::graph::GRAPH_BASE).map_err(|_| {
            Failure::Client("Graph base".to_owned()).into_source_error(PROVIDER, None)
        })?;
        if !http.permits(&graph) || !http.permits(config.site_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        if let super::config::SharePointCredential::ClientCredential { token_endpoint, .. } =
            config.credential()
            && !http.permits(token_endpoint)
        {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let credential = config.credential().clone();
        Ok(Self {
            graph: GraphClient::new(credential, http),
            config,
            params,
            cache: ListingCache::default(),
            extras: Mutex::new(Arc::new(BTreeMap::new())),
        })
    }

    #[must_use]
    pub fn with_limits(self, limits: SourceLimits) -> Self {
        self.rebuild(|http| http.with_limits(limits))
    }

    #[must_use]
    pub fn with_backoff(self, backoff: Backoff) -> Self {
        self.rebuild(|http| http.with_backoff(backoff))
    }

    #[must_use]
    pub fn with_timeouts(self, timeouts: Timeouts) -> Self {
        self.rebuild(|http| http.with_timeouts(timeouts))
    }

    fn rebuild(self, change: impl FnOnce(SourceHttp) -> SourceHttp) -> Self {
        let Self {
            graph,
            config,
            params,
            cache,
            extras,
        } = self;
        let (credential, http) = graph.into_parts();
        Self {
            graph: GraphClient::new(credential, change(http)),
            config,
            params,
            cache,
            extras,
        }
    }

    fn http(&self) -> &SourceHttp {
        self.graph.http()
    }

    /// The site's Graph id.
    async fn site_id(&self) -> Result<String, Failure> {
        let host = self.config.site_hostname();
        let path = self.config.site_path();
        let url = if path.is_empty() {
            graph_url(&["sites", host])?
        } else {
            graph_path_url(&format!("sites/{host}:/{path}"))?
        };
        let (_, body) = self.graph.get_json(url).await?;
        body.get("id")
            .and_then(Value::as_str)
            .filter(|id| !id.is_empty() && valid_key(id))
            .map(str::to_owned)
            .ok_or(Failure::InvalidResponse("the site answer has no id"))
    }

    /// The site's drives: (id, name, webUrl).
    async fn drives(&self, site: &str) -> Result<Vec<(String, String, Option<String>)>, Failure> {
        let mut url = graph_url(&["sites", site, "drives"])?;
        url.query_pairs_mut()
            .append_pair("$select", DRIVE_SELECT)
            .append_pair("$top", PAGE);
        let mut drives = Vec::new();
        let mut pages = 0;
        loop {
            pages += 1;
            self.http().check_pages(pages)?;
            let (_, body) = self.graph.get_json(url).await?;
            for drive in body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("the drives answer has no value"))?
            {
                let id = drive
                    .get("id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty() && valid_key(id))
                    .ok_or(Failure::InvalidResponse("a drive has no id"))?;
                let name = drive
                    .get("name")
                    .and_then(Value::as_str)
                    .and_then(safe_name)
                    .unwrap_or_else(|| "Documents".to_owned());
                let web_url = drive
                    .get("webUrl")
                    .and_then(Value::as_str)
                    .map(str::to_owned);
                drives.push((id.to_owned(), name, web_url));
                if drives.len() > MAX_DRIVES {
                    return Err(Failure::InvalidResponse(
                        "the site holds more drives than the listing cap",
                    ));
                }
            }
            match body.get("@odata.nextLink").and_then(Value::as_str) {
                Some(next) => url = parse_graph_link(next)?,
                None => return Ok(drives),
            }
        }
    }

    /// `path` relative to the site: the site prefix and surrounding slashes
    /// removed.
    fn relative_path(&self) -> Option<String> {
        let path = self.params.path.as_deref()?.trim().replace('\\', "/");
        let mut relative = path.trim_matches('/').to_owned();
        let site_path = self.config.site_path();
        if !site_path.is_empty() {
            let lower = relative.to_lowercase();
            let prefix = site_path.to_lowercase();
            if lower == prefix {
                relative.clear();
            } else if let Some(rest) = lower.strip_prefix(&format!("{prefix}/")) {
                relative = relative[relative.len() - rest.len()..].to_owned();
            }
        }
        let relative = relative.trim_matches('/').to_owned();
        (!relative.is_empty()).then_some(relative)
    }

    /// Which drives to walk, and where in each.
    fn seeds(&self, drives: Vec<(String, String, Option<String>)>) -> Result<Vec<Seed>, Failure> {
        let folder = self.relative_path();
        if let Some(form) = self.params.form_name.as_deref() {
            let wanted = form.trim().trim_matches('/');
            let matched: Vec<Seed> = drives
                .into_iter()
                .filter(|(_, name, web_url)| {
                    name.eq_ignore_ascii_case(wanted)
                        || web_url.as_deref().is_some_and(|url| {
                            url.trim_end_matches('/')
                                .rsplit('/')
                                .next()
                                .is_some_and(|last| last.eq_ignore_ascii_case(wanted))
                        })
                })
                .map(|(drive_id, library, _)| Seed {
                    drive_id,
                    library,
                    folder: folder.clone(),
                    guessed: false,
                })
                .collect();
            if matched.is_empty() {
                return Err(Failure::Client(
                    "no document library has the name `form_name` gives".to_owned(),
                ));
            }
            return Ok(matched);
        }
        let Some(folder) = folder else {
            return Ok(drives
                .into_iter()
                .map(|(drive_id, library, _)| Seed {
                    drive_id,
                    library,
                    folder: None,
                    guessed: false,
                })
                .collect());
        };
        // A path whose first segment names a library addresses that library.
        let (first, rest) = folder.split_once('/').unwrap_or((folder.as_str(), ""));
        let named: Vec<Seed> = drives
            .iter()
            .filter(|(_, name, _)| name.eq_ignore_ascii_case(first))
            .map(|(drive_id, library, _)| Seed {
                drive_id: drive_id.clone(),
                library: library.clone(),
                folder: (!rest.is_empty()).then(|| rest.to_owned()),
                guessed: false,
            })
            .collect();
        if !named.is_empty() {
            return Ok(named);
        }
        // Else the folder may exist in any library: walk it wherever it does.
        Ok(drives
            .into_iter()
            .map(|(drive_id, library, _)| Seed {
                drive_id,
                library,
                folder: Some(folder.clone()),
                guessed: true,
            })
            .collect())
    }

    /// Walk one seed's delta, adding its files.
    #[allow(clippy::too_many_lines)] // One pass over a delta page: skip, key, version, size.
    async fn walk(
        &self,
        seed: &Seed,
        listed: &mut Vec<Listed>,
        extras: &mut BTreeMap<String, Extra>,
        pages: &mut usize,
    ) -> Result<bool, Failure> {
        let mut url = match &seed.folder {
            None => graph_url(&["drives", &seed.drive_id, "root", "delta"])?,
            Some(folder) => {
                for segment in folder.split('/') {
                    if segment.is_empty() || segment == ".." || segment.contains(['?', '#', '%']) {
                        return Err(Failure::Client("`path` is unusable".to_owned()));
                    }
                }
                graph_path_url(&format!("drives/{}/root:/{folder}:/delta", seed.drive_id))?
            }
        };
        url.query_pairs_mut()
            .append_pair("$select", ITEM_SELECT)
            .append_pair("$top", PAGE);
        loop {
            *pages += 1;
            self.http().check_pages(*pages)?;
            let body = match self.graph.get_json(url).await {
                Ok((_, body)) => body,
                Err(Failure::Status(StatusCode::NOT_FOUND)) if seed.guessed => return Ok(false),
                Err(Failure::Status(StatusCode::NOT_FOUND)) => {
                    return Err(Failure::Client(
                        "the folder `path` names does not exist in the library".to_owned(),
                    ));
                }
                Err(error) => return Err(error),
            };
            for item in body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a delta page has no value"))?
            {
                if item.get("deleted").is_some()
                    || item.get("folder").is_some()
                    || item.get("file").is_none()
                {
                    continue;
                }
                let item_id = item
                    .get("id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty() && valid_key(id))
                    .ok_or(Failure::InvalidResponse("a drive item has no id"))?;
                let Some(name) = item.get("name").and_then(Value::as_str).and_then(safe_name)
                else {
                    continue;
                };
                if !self.params.files.admits(&name) {
                    continue;
                }
                let folder_path = item
                    .get("parentReference")
                    .and_then(|parent| parent.get("path"))
                    .and_then(Value::as_str)
                    .and_then(|path| path.split_once("root:").map(|(_, rest)| rest))
                    .unwrap_or_default()
                    .trim_matches('/')
                    .to_owned();
                let path = if folder_path.is_empty() {
                    name.clone()
                } else {
                    format!("{folder_path}/{name}")
                };
                let key = format!("{}/{path}", seed.library);
                if !valid_key(&key) || extras.contains_key(&key) {
                    continue;
                }
                let text = |field: &str| item.get(field).and_then(Value::as_str).map(str::to_owned);
                let version = text("cTag")
                    .or_else(|| text("eTag"))
                    .or_else(|| text("lastModifiedDateTime"))
                    .ok_or(Failure::InvalidResponse("a drive item carries no version"))?;
                let size = item.get("size").and_then(Value::as_u64).unwrap_or(0);
                extras.insert(
                    key.clone(),
                    Extra {
                        drive_id: seed.drive_id.clone(),
                        item_id: item_id.to_owned(),
                        library: seed.library.clone(),
                        path,
                        name,
                        web_url: text("webUrl"),
                        created: text("createdDateTime"),
                        modified: text("lastModifiedDateTime"),
                    },
                );
                listed.push(Listed::new(key, version, size, item_id.to_owned()));
                self.http().check_count(listed.len())?;
                if let Some(limit) = self.params.limit_files
                    && listed.len() as u64 >= limit
                {
                    return Ok(true);
                }
            }
            match body.get("@odata.nextLink").and_then(Value::as_str) {
                Some(next) => url = parse_graph_link(next)?,
                None => return Ok(true),
            }
        }
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let mut listed = Vec::new();
        let mut extras = BTreeMap::new();
        if self.params.limit_files != Some(0) {
            let site = self.site_id().await?;
            let drives = self.drives(&site).await?;
            let seeds = self.seeds(drives)?;
            let mut pages = 0;
            let mut any = false;
            for seed in &seeds {
                any |= self
                    .walk(seed, &mut listed, &mut extras, &mut pages)
                    .await?;
                if self
                    .params
                    .limit_files
                    .is_some_and(|limit| listed.len() as u64 >= limit)
                {
                    break;
                }
            }
            if !any && self.params.path.is_some() {
                return Err(Failure::Client(
                    "the folder `path` names was not found in any document library".to_owned(),
                ));
            }
        }
        *self
            .extras
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Arc::new(extras);
        Ok(listed)
    }

    async fn listed(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache
            .get_or_list(|| async {
                self.listing()
                    .await
                    .map_err(|failure| failure.into_source_error(PROVIDER, None))
            })
            .await
    }

    async fn listed_fresh(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache
            .list_shared(|| async {
                self.listing()
                    .await
                    .map_err(|failure| failure.into_source_error(PROVIDER, None))
            })
            .await
    }
}

impl ContentSource for SharePointSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self.listed_fresh().await?;
        Ok(references(&listing))
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let listing = self.listed().await?;
        let listed = listing
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let failed = |failure: Failure| failure.into_source_error(PROVIDER, Some(key));
        self.http()
            .check_size(listed.reference.size)
            .map_err(failed)?;
        let extras = Arc::clone(
            &self
                .extras
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
        );
        let extra = extras
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let url = graph_url(&[
            "drives",
            &extra.drive_id,
            "items",
            &extra.item_id,
            "content",
        ])
        .map_err(failed)?;
        let bytes = self.graph.get_content(url).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("id".to_owned(), Value::String(extra.item_id.clone()));
        metadata.insert("Name".to_owned(), Value::String(extra.name.clone()));
        metadata.insert(
            "Path".to_owned(),
            Value::String(format!("/{}/{}", extra.library, extra.path)),
        );
        metadata.insert("library".to_owned(), Value::String(extra.library.clone()));
        metadata.insert("drive_id".to_owned(), Value::String(extra.drive_id.clone()));
        metadata.insert(
            "ctag".to_owned(),
            Value::String(listed.reference.version.clone()),
        );
        if let Some(created) = &extra.created {
            metadata.insert("Created".to_owned(), Value::String(created.clone()));
        }
        if let Some(modified) = &extra.modified {
            metadata.insert("updated_on".to_owned(), Value::String(modified.clone()));
        }
        if let Some(link) = &extra.web_url {
            metadata.insert("Link".to_owned(), Value::String(link.clone()));
        }
        Ok(Document {
            title: extra.name.clone(),
            uri: extra.web_url.clone(),
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
    use crate::transport::Request;
    use serde_json::json;
    use std::sync::atomic::{AtomicUsize, Ordering};

    const SITE: &str = "contoso.sharepoint.com,1111,2222";
    const DOCS: &str = "b!docs";
    const SPECS: &str = "b!specs";

    fn config() -> SharePointIndexConfig {
        let settings = json!({
            "sharepoint_configuration": {
                "site_url": "https://contoso.sharepoint.com/sites/eng",
                "client_id": "app-id", "client_secret": "s3cret",
                "oauth_discovery_endpoint": "https://login.microsoftonline.com/tenant-1",
            }
        });
        SharePointIndexConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn params(value: &Value) -> SharePointIndexParams {
        SharePointIndexParams::from_arguments(value.as_object().expect("object")).expect("params")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some(
            "graph.microsoft.com login.microsoftonline.com contoso.sharepoint.com",
        ))
    }

    fn file(drive: &str, id: &str, folder: &str, name: &str, ctag: &str, size: u64) -> Value {
        json!({
            "id": id, "name": name, "size": size, "cTag": ctag, "eTag": format!("e-{ctag}"),
            "lastModifiedDateTime": "2026-10-01T00:00:00Z", "createdDateTime": "2026-09-01T00:00:00Z",
            "webUrl": format!("https://contoso.sharepoint.com/sites/eng/{name}"),
            "file": {"mimeType": "application/octet-stream"},
            "parentReference": {"driveId": drive, "path": format!("/drives/{drive}/root:{folder}")},
        })
    }

    fn folder(id: &str, name: &str) -> Value {
        json!({"id": id, "name": name, "folder": {"childCount": 1}, "parentReference": {"path": "/drives/x/root:"}})
    }

    /// The recorded tenant: a token endpoint, a site with two libraries,
    /// "Documents" paged in two delta pages, "Specs" with one file, and
    /// content downloads that redirect to the site host.
    fn reply(request: &Request, tokens_issued: &AtomicUsize) -> Reply {
        let url = request.url();
        match (url.host_str().unwrap_or_default(), url.path()) {
            ("login.microsoftonline.com", "/tenant-1/oauth2/v2.0/token") => {
                let body =
                    std::str::from_utf8(request.body().and_then(|b| b.as_bytes()).expect("body"))
                        .expect("utf8");
                assert!(body.contains("grant_type=client_credentials"), "{body}");
                assert!(body.contains("client_id=app-id") && body.contains("client_secret=s3cret"));
                assert!(
                    body.contains("scope=https%3A%2F%2Fgraph.microsoft.com%2F.default"),
                    "{body}"
                );
                let n = tokens_issued.fetch_add(1, Ordering::SeqCst) + 1;
                Reply::json(
                    &json!({"token_type": "Bearer", "expires_in": 3599, "access_token": format!("tok-{n}")}),
                )
            }
            ("graph.microsoft.com", path) => {
                let bearer = request.headers().get("authorization").expect("bearer");
                assert!(bearer.is_sensitive());
                let bearer = bearer.to_str().expect("text").to_owned();
                if bearer == "Bearer tok-1" && path.ends_with("/content") {
                    // The first token is rejected once by the content host.
                    return Reply::status(StatusCode::UNAUTHORIZED);
                }
                match path {
                    "/v1.0/sites/contoso.sharepoint.com:/sites/eng" => {
                        Reply::json(&json!({"id": SITE, "displayName": "Engineering"}))
                    }
                    "/v1.0/sites/contoso.sharepoint.com,1111,2222/drives" => {
                        assert_eq!(query(request, "$top").as_deref(), Some("999"));
                        Reply::json(&json!({"value": [
                            {"id": DOCS, "name": "Documents", "webUrl": "https://contoso.sharepoint.com/sites/eng/Shared%20Documents", "driveType": "documentLibrary"},
                            {"id": SPECS, "name": "Specs", "webUrl": "https://contoso.sharepoint.com/sites/eng/Specs", "driveType": "documentLibrary"},
                        ]}))
                    }
                    "/v1.0/drives/b!docs/root/delta" => {
                        match query(request, "token").as_deref() {
                            None => {
                                assert_eq!(query(request, "$select").as_deref(), Some(ITEM_SELECT));
                                Reply::json(&json!({
                                "value": [
                                    file(DOCS, "i1", "", "README.md", "c1", 11),
                                    folder("f1", "General"),
                                    file(DOCS, "i2", "/General", "notes.txt", "c2", 5),
                                    {"id": "gone", "name": "old.md", "deleted": {"state": "deleted"}, "file": {}},
                                ],
                                "@odata.nextLink": "https://graph.microsoft.com/v1.0/drives/b!docs/root/delta?token=p2"
                            }))
                            }
                            Some("p2") => Reply::json(&json!({
                                "value": [
                                    file(DOCS, "i3", "/General/Deep", "plan.pdf", "c3", 7),
                                    file(DOCS, "i4", "/General", "image.png", "c4", 9),
                                ],
                                "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/b!docs/root/delta?token=final"
                            })),
                            other => panic!("token {other:?}"),
                        }
                    }
                    "/v1.0/drives/b!specs/root/delta" => Reply::json(&json!({
                        "value": [file(SPECS, "s1", "", "spec.md", "c9", 3)],
                        "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/b!specs/root/delta?token=final"
                    })),
                    "/v1.0/drives/b!docs/root:/General:/delta" => Reply::json(&json!({
                        "value": [file(DOCS, "i2", "/General", "notes.txt", "c2", 5)],
                        "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/b!docs/root:/General:/delta?token=final"
                    })),
                    "/v1.0/drives/b!specs/root:/General:/delta" => Reply::status(StatusCode::NOT_FOUND),
                    "/v1.0/drives/b!docs/items/i2/content" => {
                        Reply::status(StatusCode::FOUND).header(
                            "location",
                            "https://contoso.sharepoint.com/sites/eng/_layouts/15/download.aspx?UniqueId=i2&tempauth=xyz",
                        )
                    }
                    other => panic!("unexpected Graph request {other}"),
                }
            }
            ("contoso.sharepoint.com", "/sites/eng/_layouts/15/download.aspx") => {
                assert!(
                    request.headers().get("authorization").is_none(),
                    "pre-authenticated URL"
                );
                Reply::bytes(b"notes")
            }
            other => panic!("unexpected request {other:?}"),
        }
    }

    fn fixture() -> (Arc<Scripted>, Arc<AtomicUsize>) {
        let tokens = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&tokens);
        (
            Scripted::new(move |request, _| reply(request, &counter)),
            tokens,
        )
    }

    #[tokio::test]
    async fn libraries_are_walked_by_delta_filtered_by_name_and_downloaded_through_the_redirect() {
        let (transport, tokens) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"skip_extensions": ["png"]})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size, d.mime.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                (
                    "Documents/General/Deep/plan.pdf",
                    "c3",
                    7,
                    "application/pdf"
                ),
                ("Documents/General/notes.txt", "c2", 5, "text/plain"),
                ("Documents/README.md", "c1", 11, "text/markdown"),
                ("Specs/spec.md", "c9", 3, "text/markdown"),
            ]
        );
        assert_eq!(
            tokens.load(Ordering::SeqCst),
            1,
            "one token for the whole listing"
        );
        let document = source
            .fetch("Documents/General/notes.txt")
            .await
            .expect("fetch");
        assert_eq!(document.bytes, b"notes");
        assert_eq!(
            tokens.load(Ordering::SeqCst),
            2,
            "a 401 renewed the token once"
        );
        assert_eq!(document.metadata["Path"], "/Documents/General/notes.txt");
        assert_eq!(document.metadata["id"], "i2");
        assert_eq!(document.metadata["updated_on"], "2026-10-01T00:00:00Z");
        assert_eq!(document.title, "notes.txt");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://contoso.sharepoint.com/sites/eng/notes.txt")
        );
        let seen = transport.seen();
        assert!(
            seen.iter().filter(|s| s.contains("/content")).count() == 2,
            "{seen:?}"
        );
    }

    #[tokio::test]
    async fn path_and_form_name_scope_the_walk_and_limit_files_caps_it() {
        let (transport, _) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"path": "/sites/eng/General"})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed.iter().map(|d| d.key.as_str()).collect();
        assert_eq!(
            keys,
            ["Documents/General/notes.txt"],
            "Specs has no General folder"
        );

        let (transport, _) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"form_name": "Specs", "limit_files": 1})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].key, "Specs/spec.md");
        assert!(
            !transport.seen().iter().any(|s| s.contains("b!docs")),
            "the other library was not walked"
        );

        let (transport, _) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"limit_files": 0})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        assert!(source.list().await.expect("listing").is_empty());
        assert!(transport.seen().is_empty());
    }

    #[tokio::test]
    async fn a_refused_credential_and_a_missing_allowlist_entry_are_reported() {
        let refused = Scripted::new(|_, _| Reply::status(StatusCode::UNAUTHORIZED));
        let source =
            SharePointSource::new(config(), SharePointIndexParams::default(), allow(), refused)
                .expect("source");
        let error = source.list().await.expect_err("refused");
        assert!(
            error
                .to_string()
                .contains("refused the SharePoint app credential"),
            "{error}"
        );

        let without_login =
            HostAllowlist::parse(Some("graph.microsoft.com contoso.sharepoint.com"));
        let error = SharePointSource::new(
            config(),
            SharePointIndexParams::default(),
            without_login,
            Scripted::new(|_, _| panic!("no request")),
        )
        .err()
        .expect("refused before any request");
        assert!(error.to_string().contains("egress allowlist"), "{error}");
        assert!(
            SharePointIndexParams::from_arguments(
                json!({"include_onenote": true}).as_object().expect("o")
            )
            .expect_err("onenote")
            .contains("include_onenote")
        );
    }

    #[tokio::test]
    async fn a_throttled_graph_page_is_retried_after_the_hint() {
        let tokens = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&tokens);
        let throttled = Scripted::new(move |request, index| {
            if index == 2 {
                return Reply::rate_limited(Some("0"));
            }
            reply(request, &counter)
        });
        let source = SharePointSource::new(
            config(),
            SharePointIndexParams::default(),
            allow(),
            throttled.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 5);
        let seen = throttled.seen();
        assert_eq!(
            seen[2], seen[3],
            "the throttled page was asked again: {seen:?}"
        );
    }
}
