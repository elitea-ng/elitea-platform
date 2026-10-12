#![allow(clippy::doc_markdown)] // `SharePoint`, `OneNote` are product names.
//! One SharePoint site's document libraries as a `ContentSource` (ADR-0030
//! I2), over Microsoft Graph.
//!
//! * **List.** The site (`sites/{host}:/{path}`), its drives (the document
//!   libraries; `form_name` picks one by display name or URL segment), then
//!   each drive's **root delta** enumeration (`drives/{id}/root/delta`): one
//!   paged walk that names every item of the drive with its content tag and
//!   size, no per-folder requests and no page cap. Graph documents the
//!   delta feed's shape, and the connector follows it: delta is served on
//!   the drive root only (a `path` scope is applied after the walk, to the
//!   reconstructed paths); an item may appear more than once across the
//!   pages and its **last occurrence wins** (a later `deleted` facet
//!   removes it); items carry `parentReference.id` but **no**
//!   `parentReference.path`, so folder paths are rebuilt from the feed's
//!   own folder items (id → name, parent id). `include_extensions` and
//!   `skip_extensions` filter by file name as in the SDK; `limit_files`
//!   caps the listing (the first files by path) only when given.
//! * **Version.** The item's `cTag`, which Graph changes when the content
//!   changes (not on a metadata edit), falling back to `eTag` and then
//!   `lastModifiedDateTime`. The SDK compares `lastModifiedDateTime`.
//! * **Key.** `{library}/{driveItem id}`: the SDK's item-id keying, stable
//!   across renames and moves within the library. The human path lives in
//!   the metadata (`Path`) and drives the `path` and extension filters; the
//!   document's media type comes from its name.
//! * **Fetch.** `drives/{drive}/items/{item}/content`, following Graph's
//!   redirect to the site host, under the document cap.
//! * Lists, list items and OneNote are not indexed (the SDK indexes no
//!   lists either; OneNote needs a delegated user token and image
//!   descriptions, and `include_onenote` is refused with that reason).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, mime_of};
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
use crate::transport::{Transport, Url};

const PROVIDER: &str = "SharePoint";
const PAGE: &str = "999";
const ITEM_SELECT: &str = "id,name,file,folder,root,size,cTag,eTag,lastModifiedDateTime,createdDateTime,webUrl,parentReference,deleted";
const DRIVE_SELECT: &str = "id,name,webUrl,driveType";
const MAX_DRIVES: usize = 10_000;
/// The delta feed of one drive may hold this many entries (folders and
/// files, whatever the scope) per document the listing may return.
const FEED_ENTRIES_PER_DOCUMENT: usize = 10;
/// Folder nesting deeper than this is a cycle in the feed, not a library.
const MAX_DEPTH: usize = 512;

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
    /// A document library's name (display name or URL segment).
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

/// A document library as the drives answer names it.
#[derive(Clone, Debug)]
struct Drive {
    id: String,
    /// The display name (`Documents`), or the drive id when the name is
    /// unusable as a key segment.
    name: String,
    /// The percent-decoded last segment of `webUrl` (`Shared Documents`),
    /// which a server-relative `path` names instead of the display name.
    url_segment: Option<String>,
}

impl Drive {
    /// Whether `wanted` names this library by display name or URL segment.
    fn is_called(&self, wanted: &str) -> bool {
        eq_ignore_case(&self.name, wanted)
            || self
                .url_segment
                .as_deref()
                .is_some_and(|segment| eq_ignore_case(segment, wanted))
    }
}

/// One delta walk to make: a drive, and the folder within it the listing
/// is scoped to.
struct Seed {
    drive: Drive,
    folder: Option<String>,
    /// The folder came from a guess (a bare path tried in every library), so
    /// its absence skips the library instead of failing the listing.
    guessed: bool,
}

/// One delta item, compacted: what the listing needs of it.
#[derive(Debug)]
enum Entry {
    Root,
    Folder {
        name: String,
        parent: Option<String>,
    },
    File {
        name: String,
        parent: Option<String>,
        version: String,
        size: u64,
        web_url: Option<String>,
        created: Option<String>,
        modified: Option<String>,
    },
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
        // Every host the connector sends to, from the one list the config
        // keeps: Graph, the site and (when redeemed) the identity provider.
        if !config.hosts().iter().all(|host| allowlist.permits(host)) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let http = SourceHttp::new(allowlist, transport);
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

    /// The site's drives.
    async fn drives(&self, site: &str) -> Result<Vec<Drive>, Failure> {
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
                    .unwrap_or_else(|| id.to_owned());
                let web_url = drive.get("webUrl").and_then(Value::as_str);
                drives.push(Drive {
                    id: id.to_owned(),
                    name,
                    url_segment: web_url.and_then(url_last_segment),
                });
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

    /// `path` relative to the site: the site prefix (compared without
    /// case, character by character) and surrounding slashes removed.
    fn relative_path(&self) -> Option<String> {
        let path = self.params.path.as_deref()?.trim().replace('\\', "/");
        let mut relative = path.trim_matches('/');
        // The configured site path is percent-encoded (it comes from a URL);
        // the argument is typed.
        let site_path = percent_decoded(self.config.site_path());
        if !site_path.is_empty() {
            if eq_ignore_case(relative, &site_path) {
                relative = "";
            } else if let Some(rest) =
                strip_prefix_ignore_case(relative, &site_path).filter(|rest| rest.starts_with('/'))
            {
                relative = rest;
            }
        }
        let relative = relative.trim_matches('/');
        (!relative.is_empty()).then(|| relative.to_owned())
    }

    /// Which drives to walk, and the folder in each the listing keeps.
    fn seeds(&self, drives: Vec<Drive>) -> Result<Vec<Seed>, Failure> {
        let folder = self.relative_path();
        if let Some(form) = self.params.form_name.as_deref() {
            let wanted = form.trim().trim_matches('/');
            let matched: Vec<Seed> = drives
                .into_iter()
                .filter(|drive| drive.is_called(wanted))
                .map(|drive| {
                    // A server-relative path repeats the library's segment.
                    let folder = folder
                        .as_deref()
                        .map(|folder| strip_library(folder, &drive))
                        .filter(|folder| !folder.is_empty());
                    Seed {
                        drive,
                        folder,
                        guessed: false,
                    }
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
                .map(|drive| Seed {
                    drive,
                    folder: None,
                    guessed: false,
                })
                .collect());
        };
        // A path whose first segment names a library (by display name or
        // URL segment) addresses that library.
        let (first, rest) = folder.split_once('/').unwrap_or((folder.as_str(), ""));
        let named: Vec<Seed> = drives
            .iter()
            .filter(|drive| drive.is_called(first))
            .map(|drive| Seed {
                drive: drive.clone(),
                folder: (!rest.is_empty()).then(|| rest.to_owned()),
                guessed: false,
            })
            .collect();
        if !named.is_empty() {
            return Ok(named);
        }
        // Else the folder may exist in any library: keep it wherever it does.
        Ok(drives
            .into_iter()
            .map(|drive| Seed {
                drive,
                folder: Some(folder.clone()),
                guessed: true,
            })
            .collect())
    }

    /// One drive's whole delta feed, compacted: item id → entry, the last
    /// occurrence of an id winning and a `deleted` facet removing it.
    async fn walk(
        &self,
        drive_id: &str,
        pages: &mut usize,
    ) -> Result<BTreeMap<String, Entry>, Failure> {
        let mut url = graph_url(&["drives", drive_id, "root", "delta"])?;
        url.query_pairs_mut()
            .append_pair("$select", ITEM_SELECT)
            .append_pair("$top", PAGE);
        let mut entries: BTreeMap<String, Entry> = BTreeMap::new();
        loop {
            *pages += 1;
            self.http().check_pages(*pages)?;
            let (_, body) = self.graph.get_json(url).await?;
            for item in body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a delta page has no value"))?
            {
                let item_id = item
                    .get("id")
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty() && valid_key(id))
                    .ok_or(Failure::InvalidResponse("a drive item has no id"))?;
                if item.get("deleted").is_some() {
                    entries.remove(item_id);
                    continue;
                }
                let text = |field: &str| item.get(field).and_then(Value::as_str).map(str::to_owned);
                let parent = item
                    .get("parentReference")
                    .and_then(|parent| parent.get("id"))
                    .and_then(Value::as_str)
                    .filter(|id| !id.is_empty())
                    .map(str::to_owned);
                let entry = if item.get("root").is_some() {
                    Entry::Root
                } else {
                    let Some(name) = item.get("name").and_then(Value::as_str).and_then(safe_name)
                    else {
                        continue;
                    };
                    if item.get("folder").is_some() {
                        Entry::Folder { name, parent }
                    } else if item.get("file").is_some() {
                        let version = text("cTag")
                            .or_else(|| text("eTag"))
                            .or_else(|| text("lastModifiedDateTime"))
                            .ok_or(Failure::InvalidResponse("a drive item carries no version"))?;
                        Entry::File {
                            name,
                            parent,
                            version,
                            size: item.get("size").and_then(Value::as_u64).unwrap_or(0),
                            web_url: text("webUrl"),
                            created: text("createdDateTime"),
                            modified: text("lastModifiedDateTime"),
                        }
                    } else {
                        continue;
                    }
                };
                entries.insert(item_id.to_owned(), entry);
                // The feed names folders and files alike, and the whole
                // library whatever the `path` scope: its own, higher bound
                // keeps memory bounded without making the document cap a
                // limit on how big a library may be.
                if entries.len() > self.max_feed_entries() {
                    return Err(Failure::InvalidResponse(
                        "the drive's delta feed is longer than the feed cap",
                    ));
                }
            }
            match body.get("@odata.nextLink").and_then(Value::as_str) {
                Some(next) => url = parse_graph_link(next)?,
                None => return Ok(entries),
            }
        }
    }

    /// How many entries one drive's delta feed may hold.
    fn max_feed_entries(&self) -> usize {
        self.http()
            .limits()
            .max_documents
            .saturating_mul(FEED_ENTRIES_PER_DOCUMENT)
    }

    /// The seed's files from its walked feed, scoped to the seed's folder:
    /// `false` when a guessed folder is not in this library.
    fn materialize(
        &self,
        seed: &Seed,
        entries: &BTreeMap<String, Entry>,
        out: &mut Vec<(Listed, Extra)>,
    ) -> Result<bool, Failure> {
        let folder_path = |id: &str| folder_path_of(entries, id);
        if let Some(wanted) = seed.folder.as_deref() {
            let exists = entries.iter().any(|(id, entry)| {
                matches!(entry, Entry::Folder { .. })
                    && folder_path(id).is_ok_and(|path| {
                        path.as_deref()
                            .is_some_and(|path| eq_ignore_case(path, wanted))
                    })
            });
            if !exists {
                if seed.guessed {
                    return Ok(false);
                }
                return Err(Failure::Client(
                    "the folder `path` names does not exist in the library".to_owned(),
                ));
            }
        }
        for (item_id, entry) in entries {
            let Entry::File {
                name,
                parent,
                version,
                size,
                web_url,
                created,
                modified,
            } = entry
            else {
                continue;
            };
            if !self.params.files.admits(name) {
                continue;
            }
            let folder = match parent {
                Some(parent) => folder_path(parent)?,
                None => None,
            };
            let path = match folder {
                Some(folder) => format!("{folder}/{name}"),
                None => name.clone(),
            };
            if let Some(wanted) = seed.folder.as_deref()
                && !under_folder(&path, wanted)
            {
                continue;
            }
            let key = format!("{}/{item_id}", seed.drive.name);
            if !valid_key(&key) {
                continue;
            }
            let mut listed = Listed::new(key, version.clone(), *size, item_id.clone());
            mime_of(name).clone_into(&mut listed.reference.mime);
            out.push((
                listed,
                Extra {
                    drive_id: seed.drive.id.clone(),
                    item_id: item_id.clone(),
                    library: seed.drive.name.clone(),
                    path,
                    name: name.clone(),
                    web_url: web_url.clone(),
                    created: created.clone(),
                    modified: modified.clone(),
                },
            ));
        }
        Ok(true)
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
            let mut found: Vec<(Listed, Extra)> = Vec::new();
            let limit = self
                .params
                .limit_files
                .map(|limit| usize::try_from(limit).unwrap_or(usize::MAX));
            for seed in &seeds {
                let entries = self.walk(&seed.drive.id, &mut pages).await?;
                // Only files under the `path` scope and passing the name
                // filters reach `found`: folders and everything outside the
                // scope never count against the document cap.
                any |= self.materialize(seed, &entries, &mut found)?;
                if let Some(limit) = limit {
                    // The first `limit_files` by library and path, kept as
                    // the drives are walked.
                    found.sort_by(|(_, a), (_, b)| {
                        (&a.library, &a.path).cmp(&(&b.library, &b.path))
                    });
                    found.truncate(limit);
                } else {
                    self.http().check_count(found.len())?;
                }
            }
            if !any && self.params.path.is_some() {
                return Err(Failure::Client(
                    "the folder `path` names was not found in any document library".to_owned(),
                ));
            }
            found.sort_by(|(_, a), (_, b)| (&a.library, &a.path).cmp(&(&b.library, &b.path)));
            self.http().check_count(found.len())?;
            for (item, extra) in found {
                extras.insert(item.reference.key.clone(), extra);
                listed.push(item);
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

/// The path of folder `id` relative to the drive root (`None` for the root
/// itself), rebuilt from the feed's folder entries. A parent the feed does
/// not name ends the path there.
///
/// # Errors
///
/// The folder entries form a cycle.
fn folder_path_of(entries: &BTreeMap<String, Entry>, id: &str) -> Result<Option<String>, Failure> {
    let mut names: Vec<&str> = Vec::new();
    let mut current = Some(id);
    while let Some(id) = current {
        match entries.get(id) {
            Some(Entry::Folder { name, parent }) => {
                names.push(name);
                current = parent.as_deref();
            }
            Some(Entry::Root | Entry::File { .. }) | None => break,
        }
        if names.len() > MAX_DEPTH {
            return Err(Failure::InvalidResponse(
                "the delta feed's folders form a cycle",
            ));
        }
    }
    if names.is_empty() {
        return Ok(None);
    }
    names.reverse();
    Ok(Some(names.join("/")))
}

/// Whether `path` lies under `folder` (`folder/…`), compared without case.
fn under_folder(path: &str, folder: &str) -> bool {
    strip_prefix_ignore_case(path, folder).is_some_and(|rest| rest.starts_with('/'))
}

/// `folder` without the library's own leading segment, when it has one.
fn strip_library(folder: &str, drive: &Drive) -> String {
    let (first, rest) = folder.split_once('/').unwrap_or((folder, ""));
    if drive.is_called(first) {
        rest.to_owned()
    } else {
        folder.to_owned()
    }
}

/// Whether two strings are equal without case, character by character
/// (Unicode lower-casing; no byte offsets of a lower-cased copy are used).
fn eq_ignore_case(a: &str, b: &str) -> bool {
    a.chars()
        .flat_map(char::to_lowercase)
        .eq(b.chars().flat_map(char::to_lowercase))
}

/// `text` after `prefix`, compared without case character by character, on
/// a character boundary of `text`.
fn strip_prefix_ignore_case<'a>(text: &'a str, prefix: &str) -> Option<&'a str> {
    let mut rest = text.char_indices();
    for wanted in prefix.chars() {
        let (_, found) = rest.next()?;
        if !found.to_lowercase().eq(wanted.to_lowercase()) {
            return None;
        }
    }
    Some(rest.as_str())
}

/// The percent-decoded last path segment of a URL (`…/Shared%20Documents`
/// → `Shared Documents`).
fn url_last_segment(url: &str) -> Option<String> {
    let parsed = Url::parse(url).ok()?;
    let last = parsed
        .path_segments()?
        .rfind(|segment| !segment.is_empty())?;
    Some(percent_decoded(last))
}

/// `%XX` sequences decoded; anything else, and invalid UTF-8, kept lossily.
fn percent_decoded(text: &str) -> String {
    let bytes = text.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%'
            && index + 2 < bytes.len()
            && let (Some(high), Some(low)) = (
                (bytes[index + 1] as char).to_digit(16),
                (bytes[index + 2] as char).to_digit(16),
            )
        {
            out.push(u8::try_from(high * 16 + low).unwrap_or(b'?'));
            index += 3;
        } else {
            out.push(bytes[index]);
            index += 1;
        }
    }
    String::from_utf8_lossy(&out).into_owned()
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
    use crate::transport::{Request, StatusCode};
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

    /// A file as a delta page carries it: `parentReference` has the drive
    /// and the parent folder's id, never a `path` (Graph does not return
    /// `parentReference.path` in delta responses).
    fn file(drive: &str, id: &str, parent: &str, name: &str, ctag: &str, size: u64) -> Value {
        json!({
            "id": id, "name": name, "size": size, "cTag": ctag, "eTag": format!("e-{ctag}"),
            "lastModifiedDateTime": "2026-10-01T00:00:00Z", "createdDateTime": "2026-09-01T00:00:00Z",
            "webUrl": format!("https://contoso.sharepoint.com/sites/eng/{name}"),
            "file": {"mimeType": "application/octet-stream"},
            "parentReference": {"driveId": drive, "id": parent},
        })
    }

    fn folder(drive: &str, id: &str, parent: &str, name: &str) -> Value {
        json!({"id": id, "name": name, "folder": {"childCount": 1},
               "parentReference": {"driveId": drive, "id": parent}})
    }

    fn root(drive: &str, id: &str) -> Value {
        json!({"id": id, "name": "root", "root": {}, "folder": {"childCount": 2},
               "parentReference": {"driveId": drive}})
    }

    fn deleted(id: &str, name: &str) -> Value {
        json!({"id": id, "name": name, "deleted": {"state": "deleted"}, "file": {},
               "parentReference": {"driveId": DOCS}})
    }

    /// The recorded tenant: a token endpoint, a site with two libraries
    /// ("Documents" at `Shared Documents`, "Specs"), "Documents" paged in
    /// two delta pages — with `notes.txt` appearing on both (the second
    /// occurrence, `c2b`, wins) and `old.md` listed then deleted — and
    /// content downloads that redirect to the site host. Delta is served
    /// on the drive root only, as Graph does for SharePoint.
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
                                    root(DOCS, "root-docs"),
                                    file(DOCS, "i1", "root-docs", "README.md", "c1", 11),
                                    folder(DOCS, "f1", "root-docs", "Specs"),
                                    file(DOCS, "i2", "f1", "notes.txt", "c2", 5),
                                    file(DOCS, "i5", "root-docs", "old.md", "c5", 2),
                                    deleted("gone", "older.md"),
                                ],
                                "@odata.nextLink": "https://graph.microsoft.com/v1.0/drives/b!docs/root/delta?token=p2"
                            }))
                            }
                            Some("p2") => Reply::json(&json!({
                                "value": [
                                    folder(DOCS, "f2", "f1", "Deep"),
                                    file(DOCS, "i3", "f2", "plan.pdf", "c3", 7),
                                    file(DOCS, "i4", "f1", "image.png", "c4", 9),
                                    file(DOCS, "i2", "f1", "notes.txt", "c2b", 6),
                                    deleted("i5", "old.md"),
                                ],
                                "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/b!docs/root/delta?token=final"
                            })),
                            other => panic!("token {other:?}"),
                        }
                    }
                    "/v1.0/drives/b!specs/root/delta" => Reply::json(&json!({
                        "value": [
                            root(SPECS, "root-specs"),
                            file(SPECS, "s1", "root-specs", "spec.md", "c9", 3),
                            folder(SPECS, "f9", "root-specs", "Archive"),
                            file(SPECS, "s2", "f9", "legacy.md", "c8", 4),
                        ],
                        "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/b!specs/root/delta?token=final"
                    })),
                    "/v1.0/drives/b!docs/items/i2/content" => {
                        Reply::status(StatusCode::FOUND).header(
                            "location",
                            "https://contoso.sharepoint.com/sites/eng/_layouts/15/download.aspx?UniqueId=i2&tempauth=xyz",
                        )
                    }
                    other if other.contains(":/") => {
                        panic!("delta is served on the drive root only, not {other}")
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

    async fn list_with(arguments: Value) -> (Vec<DocumentRef>, Arc<Scripted>) {
        let (transport, _) = fixture();
        let source =
            SharePointSource::new(config(), params(&arguments), allow(), transport.clone())
                .expect("source");
        (source.list().await.expect("listing"), transport)
    }

    fn keys(listed: &[DocumentRef]) -> Vec<&str> {
        listed.iter().map(|d| d.key.as_str()).collect()
    }

    #[tokio::test]
    async fn libraries_are_walked_by_root_delta_keyed_by_item_id_and_downloaded_through_the_redirect()
     {
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
                ("Documents/i1", "c1", 11, "text/markdown"),
                // The second occurrence of notes.txt won: version c2b, 6 bytes.
                ("Documents/i2", "c2b", 6, "text/plain"),
                ("Documents/i3", "c3", 7, "application/pdf"),
                ("Specs/s1", "c9", 3, "text/markdown"),
                ("Specs/s2", "c8", 4, "text/markdown"),
            ],
            "old.md was listed then deleted; image.png is skipped"
        );
        assert_eq!(
            tokens.load(Ordering::SeqCst),
            1,
            "one token for the whole listing"
        );
        let document = source.fetch("Documents/i2").await.expect("fetch");
        assert_eq!(document.bytes, b"notes");
        assert_eq!(
            tokens.load(Ordering::SeqCst),
            2,
            "a 401 renewed the token once"
        );
        assert_eq!(
            document.metadata["Path"], "/Documents/Specs/notes.txt",
            "the path is rebuilt from the feed's folder items"
        );
        assert_eq!(document.metadata["id"], "i2");
        assert_eq!(document.metadata["updated_on"], "2026-10-01T00:00:00Z");
        assert_eq!(document.title, "notes.txt");
        assert_eq!(document.reference.mime, "text/plain");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://contoso.sharepoint.com/sites/eng/notes.txt")
        );
        let seen = transport.seen();
        assert!(
            seen.iter().filter(|s| s.contains("/content")).count() == 2,
            "{seen:?}"
        );
        assert!(
            seen.iter()
                .filter(|s| s.contains("/delta"))
                .all(|s| s.contains("/root/delta")),
            "every delta was asked of a drive root: {seen:?}"
        );
    }

    #[tokio::test]
    async fn a_path_scopes_the_root_walk_by_library_name_or_url_segment_then_folder() {
        // The library's URL segment, then a folder that is also another
        // library's name: the folder of "Documents" is meant.
        let (listed, transport) =
            list_with(json!({"path": "/sites/eng/Shared Documents/Specs"})).await;
        assert_eq!(
            keys(&listed),
            ["Documents/i2", "Documents/i3", "Documents/i4"]
        );
        assert!(
            !transport.seen().iter().any(|s| s.contains("b!specs")),
            "only the named library was walked"
        );

        // A first segment naming a library by display name addresses it.
        let (listed, _) = list_with(json!({"path": "/sites/eng/Specs"})).await;
        assert_eq!(keys(&listed), ["Specs/s1", "Specs/s2"]);

        // A bare folder is looked for in every library: "Documents" has no
        // Archive and is skipped, "Specs" has one.
        let (listed, transport) = list_with(json!({"path": "Archive"})).await;
        assert_eq!(keys(&listed), ["Specs/s2"]);
        assert!(
            transport.seen().iter().any(|s| s.contains("b!docs")),
            "a guessed folder is looked for in every library"
        );

        // A site prefix in another case, and the library segment repeated
        // under `form_name`.
        let (listed, _) = list_with(json!({"form_name": "Shared Documents", "path": "/Sites/ENG/shared documents/specs/deep"})).await;
        assert_eq!(keys(&listed), ["Documents/i3"]);
        let (listed, _) = list_with(json!({"form_name": "Documents", "path": "Specs"})).await;
        assert_eq!(
            keys(&listed),
            ["Documents/i2", "Documents/i3", "Documents/i4"]
        );

        // A folder no library has, and one the named library has not.
        let (transport, _) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"path": "/sites/eng/Deep"})),
            allow(),
            transport,
        )
        .expect("source");
        let error = source.list().await.expect_err("no such folder");
        assert!(
            error
                .to_string()
                .contains("not found in any document library"),
            "{error}"
        );
        let (transport, _) = fixture();
        let source = SharePointSource::new(
            config(),
            params(&json!({"path": "/sites/eng/Specs/Deep"})),
            allow(),
            transport,
        )
        .expect("source");
        let error = source.list().await.expect_err("no such folder");
        assert!(
            error.to_string().contains("does not exist in the library"),
            "{error}"
        );
    }

    #[tokio::test]
    async fn form_name_matches_the_url_segment_too_and_limit_files_caps_the_listing() {
        let (listed, transport) = list_with(json!({"form_name": "shared documents"})).await;
        assert_eq!(
            keys(&listed),
            [
                "Documents/i1",
                "Documents/i2",
                "Documents/i3",
                "Documents/i4"
            ]
        );
        assert!(!transport.seen().iter().any(|s| s.contains("b!specs")));

        let (listed, transport) = list_with(json!({"form_name": "Specs", "limit_files": 1})).await;
        assert_eq!(keys(&listed), ["Specs/s2"], "Archive/legacy.md sorts first");
        assert!(
            !transport.seen().iter().any(|s| s.contains("b!docs")),
            "the other library was not walked"
        );

        // The first files by library and path (README.md, Specs/Deep/plan.pdf).
        let (listed, _) = list_with(json!({"limit_files": 2})).await;
        assert_eq!(keys(&listed), ["Documents/i1", "Documents/i3"]);

        let (listed, transport) = list_with(json!({"limit_files": 0})).await;
        assert!(listed.is_empty());
        assert!(transport.seen().is_empty());

        let (transport, _) = fixture();
        let error = SharePointSource::new(
            config(),
            params(&json!({"form_name": "Archive"})),
            allow(),
            transport,
        )
        .expect("source")
        .list()
        .await
        .expect_err("no such library");
        assert!(error.to_string().contains("form_name"), "{error}");
    }

    #[test]
    fn prefixes_are_compared_without_case_on_character_boundaries() {
        // `İ` (2 bytes) lower-cases to `i̇` (3 bytes): byte offsets of a
        // lower-cased copy would cut the original mid-character.
        assert_eq!(
            strip_prefix_ignore_case("sites/İstanbul/Docs", "sites/İstanbul"),
            Some("/Docs")
        );
        assert_eq!(
            strip_prefix_ignore_case("SITES/İSTANBUL/Docs", "sites/İstanbul"),
            Some("/Docs")
        );
        assert_eq!(
            strip_prefix_ignore_case("sites/istanbul/Docs", "sites/İstanbul"),
            None,
            "`İ` lower-cases to `i̇`, not `i`"
        );
        assert_eq!(
            strip_prefix_ignore_case("Sites/Eng/Ünïcode", "sites/ENG"),
            Some("/Ünïcode")
        );
        assert_eq!(strip_prefix_ignore_case("sites/en", "sites/eng"), None);
        assert_eq!(strip_prefix_ignore_case("sites/eng", "sites/eng"), Some(""));
        assert!(eq_ignore_case("Shared Documents", "shared DOCUMENTS"));
        assert!(eq_ignore_case("İstanbul", "İSTANBUL"));
        assert!(!eq_ignore_case("Specs", "Spec"));
        assert!(under_folder("Specs/Deep/plan.pdf", "specs/deep"));
        assert!(!under_folder("Specs/Deeper/plan.pdf", "specs/deep"));
        assert!(!under_folder("Specs", "specs"));

        let settings = json!({
            "sharepoint_configuration": {
                "site_url": "https://contoso.sharepoint.com/sites/İstanbul",
                "client_id": "app-id", "client_secret": "s3cret",
            }
        });
        let config =
            SharePointIndexConfig::parse(settings.as_object().expect("settings")).expect("config");
        let source = SharePointSource::new(
            config,
            params(&json!({"path": "/sites/İstanbul/Docs/Ü"})),
            allow(),
            Scripted::new(|_, _| panic!("no request")),
        )
        .expect("source");
        assert_eq!(source.relative_path().as_deref(), Some("Docs/Ü"));

        assert_eq!(
            url_last_segment("https://contoso.sharepoint.com/sites/eng/Shared%20Documents/"),
            Some("Shared Documents".to_owned())
        );
        assert_eq!(
            url_last_segment("https://contoso.sharepoint.com/sites/eng/%C3%9Cbersicht"),
            Some("Übersicht".to_owned())
        );
        assert_eq!(percent_decoded("a%2"), "a%2");
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
        assert_eq!(listed.len(), 6);
        let seen = throttled.seen();
        assert_eq!(
            seen[2], seen[3],
            "the throttled page was asked again: {seen:?}"
        );
    }

    #[test]
    fn every_host_the_config_names_must_be_on_the_allowlist() {
        let hosts = config()
            .hosts()
            .into_iter()
            .map(str::to_owned)
            .collect::<Vec<_>>();
        assert_eq!(
            hosts,
            [
                "graph.microsoft.com",
                "contoso.sharepoint.com",
                "login.microsoftonline.com"
            ]
        );
        for missing in &hosts {
            let rest = hosts
                .iter()
                .filter(|host| *host != missing)
                .cloned()
                .collect::<Vec<_>>()
                .join(" ");
            let result = SharePointSource::new(
                config(),
                SharePointIndexParams::default(),
                HostAllowlist::parse(Some(&rest)),
                Scripted::new(|_, _| panic!("no request")),
            );
            assert!(result.is_err(), "{missing} is required");
        }
    }

    /// A library of `big` files in `Big/` and three in `Narrow/`, one delta
    /// page: far more entries than the document cap the tests set.
    fn large_library(big: usize) -> Arc<Scripted> {
        Scripted::new(move |request, _| {
            let url = request.url();
            match (url.host_str().unwrap_or_default(), url.path()) {
                ("login.microsoftonline.com", _) => Reply::json(
                    &json!({"token_type": "Bearer", "expires_in": 3599, "access_token": "tok"}),
                ),
                ("graph.microsoft.com", "/v1.0/sites/contoso.sharepoint.com:/sites/eng") => {
                    Reply::json(&json!({"id": SITE}))
                }
                ("graph.microsoft.com", p) if p.ends_with("/drives") => {
                    Reply::json(&json!({"value": [
                        {"id": DOCS, "name": "Documents", "webUrl": "https://contoso.sharepoint.com/sites/eng/Shared%20Documents"},
                    ]}))
                }
                ("graph.microsoft.com", "/v1.0/drives/b!docs/root/delta") => {
                    let mut value = vec![
                        root(DOCS, "root-docs"),
                        folder(DOCS, "big", "root-docs", "Big"),
                        folder(DOCS, "narrow", "root-docs", "Narrow"),
                    ];
                    for n in 0..big {
                        value.push(file(
                            DOCS,
                            &format!("b{n:04}"),
                            "big",
                            &format!("f{n:04}.md"),
                            "c",
                            1,
                        ));
                    }
                    for n in 0..3 {
                        value.push(file(
                            DOCS,
                            &format!("n{n}"),
                            "narrow",
                            &format!("n{n}.md"),
                            "c",
                            1,
                        ));
                    }
                    Reply::json(&json!({"value": value}))
                }
                other => panic!("unexpected request {other:?}"),
            }
        })
    }

    #[tokio::test]
    async fn a_small_limit_or_a_narrow_path_never_trips_the_cap_on_a_large_library() {
        // 100 files in `Big/`, 3 in `Narrow/`; the cap is 20 documents. The
        // feed (106 entries with folders and root) is under its own bound.
        let limits = SourceLimits {
            max_documents: 20,
            ..SourceLimits::default()
        };
        let list = |arguments: Value| async move {
            SharePointSource::new(config(), params(&arguments), allow(), large_library(100))
                .expect("source")
                .with_limits(limits)
                .list()
                .await
        };
        let limited = list(json!({"limit_files": 10}))
            .await
            .expect("limit_files 10");
        assert_eq!(limited.len(), 10);
        let narrow = list(json!({"path": "Narrow"})).await.expect("narrow path");
        assert_eq!(narrow.len(), 3, "only files under the scope count");
        let error = list(json!({}))
            .await
            .expect_err("100 documents are over the cap");
        assert!(error.to_string().contains("listing cap"), "{error}");
        // The feed itself has its own, higher bound: 10 entries per document.
        let error = SharePointSource::new(
            config(),
            params(&json!({"limit_files": 1})),
            allow(),
            large_library(300),
        )
        .expect("source")
        .with_limits(limits)
        .list()
        .await
        .expect_err("a feed longer than the feed cap");
        assert!(error.to_string().contains("feed cap"), "{error}");
    }
}
