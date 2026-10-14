//! One Azure DevOps wiki as a `ContentSource` (ADR-0030 I2).
//!
//! * **List.** The wiki (`wiki_identifier`, else the toolkit's
//!   `default_wiki_identifier`) is read for its backing git repository and
//!   branch; `pages?path=/&recursionLevel=full` gives every page with its
//!   `gitItemPath` in one answer (sub-pages included, no page cap); the
//!   repository's tree (`trees/{id}?recursive=true`, continuation followed)
//!   gives each page file's blob id and size. A code wiki publishes one
//!   folder of its repository (`mappedPath`, `/docs`): only that folder's
//!   tree is read, never the repository's, and its attachments are the ones
//!   under `<mappedPath>/.attachments/`. A tree entry with an unusable path
//!   or id is skipped (logged with the reason), not a failed listing. `path_contains` keeps the pages
//!   whose path holds the text (case-insensitive, `-` equal to a space, as in
//!   the SDK).
//! * **Version.** The page file's git blob id: exact, and free with the
//!   listing. The SDK hashes the markdown it downloaded; the blob id is that
//!   hash, computed by the server.
//! * **Dependents.** With `include_attachments`, every file under
//!   `.attachments/` that passes `include_extensions`/`skip_extensions` is a
//!   document of its own, versioned by its blob id. The SDK keys an
//!   attachment by the page that links it (`{page id}::{path}`), which needs
//!   every page's content at listing time; here attachments are keyed by
//!   their path, so an attachment linked from two pages is one document and
//!   one linked from none is still indexed (see `SOURCE_PARITY.md`).
//! * **Fetch.** `blobs/{objectId}?$format=octetstream`, under the document
//!   cap.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{AdoClient, AdoRequest};
use super::config::{AdoFamilySettings, AdoToolkitConfig};
use super::text::{optional_bool, optional_text, optional_text_list};
use crate::egress::HostAllowlist;
use crate::pictures::{
    self, Budget, Escape, PictureBatch, PictureStep, SourcePicture, TOO_LARGE, UNREADABLE,
    UNSUPPORTED,
};
use crate::source::glob::NameFilter;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references,
    title_of, valid_git_object_id, valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Request, Transport};

const PROVIDER: &str = "Azure DevOps";
const WIKI_API: &str = "7.1";
const GIT_API: &str = "7.1";
const CONTINUATION: &str = "x-ms-continuationtoken";
const DEFAULT_BRANCH: &str = "wikiMaster";
const ATTACHMENTS_DIR: &str = ".attachments/";
const SYMLINK_MODE: &str = "120000";
/// Pages in one wiki tree answer (a defence against a runaway tree).
const MAX_PAGES: usize = 500_000;

/// The `index_data` arguments of the `ado_wiki` family
/// (`ado/wiki/ado_wrapper.py::_index_tool_params`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct AdoWikiIndexParams {
    pub wiki_identifier: Option<String>,
    pub path_contains: Option<String>,
    pub include_attachments: bool,
    pub attachments: NameFilter,
    /// Describe the pictures a page links (`![alt](/.attachments/x.png)`)
    /// with the chat model and put the description in the link's place
    /// (the SDK's `process_images`, off by default for an index).
    pub process_images: bool,
    /// The prompt for those descriptions (`image_description_prompt`).
    pub image_description_prompt: Option<String>,
}

impl AdoWikiIndexParams {
    /// Read the arguments as the request carried them.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, String> {
        Ok(Self {
            wiki_identifier: optional_text(arguments, "wiki_identifier")?,
            path_contains: optional_text(arguments, "path_contains")?,
            include_attachments: optional_bool(arguments, "include_attachments")?.unwrap_or(false),
            attachments: NameFilter::new(
                optional_text_list(arguments, "include_extensions")?,
                optional_text_list(arguments, "skip_extensions")?,
            ),
            process_images: optional_bool(arguments, "process_images")?.unwrap_or(false),
            image_description_prompt: optional_text(arguments, "image_description_prompt")?
                .filter(|prompt| !prompt.trim().is_empty()),
        })
    }
}

/// What a fetch adds to a listed document beyond its bytes.
#[derive(Clone, Debug)]
struct Extra {
    kind: &'static str,
    page_id: Option<u64>,
    page_path: Option<String>,
    remote_url: Option<String>,
    git_path: String,
}

/// One wiki, listed and fetched through the shared client.
pub struct AdoWikiSource {
    client: AdoClient,
    http: SourceHttp,
    settings: AdoFamilySettings,
    params: AdoWikiIndexParams,
    cache: ListingCache,
    /// The repository the last listing read, and each key's extras.
    side: Mutex<Side>,
}

#[derive(Default)]
struct Side {
    repository_id: String,
    wiki_name: String,
    extras: Arc<BTreeMap<String, Extra>>,
    /// With `process_images`: the picture files of the wiki's
    /// `.attachments/` folder, by repository path, to their blob id and
    /// size. The tree is read for the listing anyway.
    images: Arc<BTreeMap<String, (String, u64)>>,
    /// The mapped folder of a code wiki (`docs`), else empty.
    prefix: String,
}

impl AdoWikiSource {
    /// The configured wiki, every request through `allowlist` (fail-closed)
    /// and then `transport`.
    ///
    /// # Errors
    ///
    /// The organization's host is not on the allowlist.
    pub fn new(
        config: AdoToolkitConfig,
        params: AdoWikiIndexParams,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let (connection, settings) = config.into_parts();
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(connection.organization_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            client: AdoClient::new(connection, http.transport()),
            http,
            settings,
            params,
            cache: ListingCache::default(),
            side: Mutex::new(Side::default()),
        })
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SourceLimits) -> Self {
        self.http = self.http.with_limits(limits);
        self
    }

    #[must_use]
    pub fn with_backoff(mut self, backoff: Backoff) -> Self {
        self.http = self.http.with_backoff(backoff);
        self
    }

    #[must_use]
    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Self {
        self.http = self.http.with_timeouts(timeouts);
        self
    }

    fn build(&self, request: AdoRequest<'_>) -> Result<Request, Failure> {
        self.client
            .build(request)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    fn wiki(&self) -> Result<&str, Failure> {
        self.params
            .wiki_identifier
            .as_deref()
            .or(self.settings.default_wiki_identifier.as_deref())
            .ok_or_else(|| {
                Failure::Client(
                    "no wiki is named: pass `wiki_identifier` or set the toolkit's default wiki"
                        .to_owned(),
                )
            })
    }

    /// The tree of `repository` at `branch`: each file's path to its blob id
    /// and size.
    ///
    /// `mapped` is the repository folder the wiki shows (empty: the root).
    /// Only that folder's tree is read; the keys are repository paths
    /// without the leading slash (`docs/Home.md`).
    async fn tree(
        &self,
        repository: &str,
        branch: &str,
        mapped: &str,
    ) -> Result<BTreeMap<String, (String, u64)>, Failure> {
        let items = ["git", "repositories", repository, "items"];
        let folder = if mapped.is_empty() { "/" } else { mapped };
        let prefix = mapped.trim_matches('/');
        let request = self.build(
            AdoRequest::get(&items, GIT_API)
                .query("path", folder)
                .query("recursionLevel", "None")
                .query("versionDescriptor.versionType", "branch")
                .query("versionDescriptor.version", branch),
        )?;
        let (_, body) = self.http.json(request).await?;
        let item = body
            .get("value")
            .and_then(Value::as_array)
            .and_then(|values| values.first())
            .unwrap_or(&body);
        let root = item
            .get("objectId")
            .and_then(Value::as_str)
            .filter(|id| valid_git_object_id(id))
            .map(str::to_ascii_lowercase)
            .ok_or(Failure::InvalidResponse(
                "the wiki root item has no tree id",
            ))?;
        let segments = ["git", "repositories", repository, "trees", root.as_str()];
        let mut files = BTreeMap::new();
        let mut continuation: Option<String> = None;
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let request = self.build(
                AdoRequest::get(&segments, GIT_API)
                    .query("recursive", "true")
                    .query_opt("continuationToken", continuation.clone()),
            )?;
            let (headers, body) = self.http.json(request).await?;
            let entries = body
                .get("treeEntries")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a tree answer has no entries"))?;
            for entry in entries {
                let text = |name: &str| entry.get(name).and_then(Value::as_str);
                let (Some(path), Some(kind), Some(id)) = (
                    text("relativePath"),
                    text("gitObjectType"),
                    text("objectId"),
                ) else {
                    tracing::debug!("a wiki tree entry is incomplete; skipped");
                    continue;
                };
                if kind != "blob" || text("mode") == Some(SYMLINK_MODE) {
                    continue;
                }
                let relative = path.trim_start_matches('/');
                let path = if prefix.is_empty() {
                    relative.to_owned()
                } else {
                    format!("{prefix}/{relative}")
                };
                if !valid_key(&path) || !valid_git_object_id(id) {
                    tracing::debug!("a wiki tree entry has an unsafe path or id; skipped");
                    continue;
                }
                let size = entry.get("size").and_then(Value::as_u64).unwrap_or(0);
                files.insert(path, (id.to_ascii_lowercase(), size));
                if files.len() > MAX_PAGES {
                    return Err(Failure::InvalidResponse(
                        "the wiki repository holds more files than the listing cap",
                    ));
                }
            }
            continuation = headers
                .get(CONTINUATION)
                .and_then(|value| value.to_str().ok())
                .map(str::trim)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if continuation.is_none() {
                return Ok(files);
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One pass: the wiki, its pages, the tree, the attachments.
    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let wiki = self.wiki()?;
        let wiki_segments = ["wiki", "wikis", wiki];
        let request = self.build(AdoRequest::get(&wiki_segments, WIKI_API))?;
        let (_, wiki_body) = self.http.json(request).await?;
        let repository = wiki_body
            .get("repositoryId")
            .and_then(Value::as_str)
            .filter(|id| !id.is_empty() && valid_key(id))
            .ok_or(Failure::InvalidResponse("the wiki names no repository"))?
            .to_owned();
        let wiki_name = wiki_body
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or(wiki)
            .to_owned();
        let branch = wiki_body
            .get("versions")
            .and_then(Value::as_array)
            .and_then(|versions| versions.first())
            .and_then(|version| version.get("version"))
            .and_then(Value::as_str)
            .filter(|branch| !branch.is_empty())
            .unwrap_or(DEFAULT_BRANCH)
            .to_owned();

        let mapped = mapped_path(&wiki_body)?;
        let prefix = mapped.trim_matches('/').to_owned();

        let pages_segments = ["wiki", "wikis", wiki, "pages"];
        let request = self.build(
            AdoRequest::get(&pages_segments, WIKI_API)
                .query("path", "/")
                .query("recursionLevel", "full")
                .query("includeContent", "false"),
        )?;
        let (_, root) = self.http.json(request).await?;
        let mut pages = Vec::new();
        flatten(&root, &mut pages)?;

        let files = self.tree(&repository, &branch, &mapped).await?;
        let needle = self
            .params
            .path_contains
            .as_deref()
            .map(comparable_path)
            .filter(|needle| !needle.is_empty());

        let mut listed = Vec::new();
        let mut extras = BTreeMap::new();
        for page in pages {
            let Some(git_path) = page.git_item_path.as_deref() else {
                continue;
            };
            let git_path = git_path.trim_start_matches('/');
            let Some((object_id, size)) = files.get(git_path) else {
                continue;
            };
            if let Some(needle) = &needle
                && !comparable_path(&page.path).contains(needle.as_str())
            {
                continue;
            }
            let key = format!("{}.md", page.path.trim_matches('/'));
            if !valid_key(&key) {
                tracing::debug!("a wiki page has an unsafe path; skipped");
                continue;
            }
            extras.insert(
                key.clone(),
                Extra {
                    kind: "page",
                    page_id: page.id,
                    page_path: Some(page.path.clone()),
                    remote_url: page.remote_url.clone(),
                    git_path: git_path.to_owned(),
                },
            );
            listed.push(Listed::new(
                key,
                object_id.clone(),
                *size,
                object_id.clone(),
            ));
            self.http.check_count(listed.len())?;
        }
        // `.attachments/` sits at the wiki's root: inside the mapped
        // folder of a code wiki, not at the repository's.
        let attachments_dir = if prefix.is_empty() {
            ATTACHMENTS_DIR.to_owned()
        } else {
            format!("{prefix}/{ATTACHMENTS_DIR}")
        };
        let images: BTreeMap<String, (String, u64)> = if self.params.process_images {
            files
                .iter()
                .filter(|(path, _)| {
                    path.starts_with(&attachments_dir) && pictures::has_image_extension(path)
                })
                .map(|(path, entry)| (path.clone(), entry.clone()))
                .collect()
        } else {
            BTreeMap::new()
        };
        if self.params.include_attachments {
            for (path, (object_id, size)) in &files {
                if !path.starts_with(&attachments_dir) {
                    continue;
                }
                let key = path[prefix.len()..].trim_start_matches('/').to_owned();
                if !self.params.attachments.admits(&key) || extras.contains_key(&key) {
                    continue;
                }
                extras.insert(
                    key.clone(),
                    Extra {
                        kind: "attachment",
                        page_id: None,
                        page_path: None,
                        remote_url: None,
                        git_path: path.clone(),
                    },
                );
                listed.push(Listed::new(
                    key,
                    object_id.clone(),
                    *size,
                    object_id.clone(),
                ));
                self.http.check_count(listed.len())?;
            }
        }
        *self
            .side
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Side {
            repository_id: repository,
            wiki_name,
            extras: Arc::new(extras),
            images: Arc::new(images),
            prefix: prefix.clone(),
        };
        Ok(listed)
    }

    /// The picture step for `key`: only a page, and only with
    /// `process_images` (an attachment is its own document).
    #[must_use]
    pub fn picture_step(&self, key: &str) -> Option<PictureStep> {
        if !self.params.process_images {
            return None;
        }
        let (_, _, extras) = self.side();
        extras
            .get(key)
            .is_some_and(|extra| extra.kind == "page")
            .then(|| PictureStep::Inline {
                prompt: self.params.image_description_prompt.clone(),
            })
    }

    /// The pictures a page links, read from the wiki's repository (the
    /// SDK's `_process_images`): `![alt](/.attachments/x.png)` links whose
    /// target has an image extension and names a file of the wiki's
    /// `.attachments/` folder. The SDK also fetched any other `http(s)` link
    /// with an unauthenticated `requests.get`; this connector sends no
    /// request to a host the toolkit is not configured for, so those links
    /// stay as they are (counted `unsupported_type`). `None`: the page has
    /// no picture to read.
    #[allow(clippy::too_many_lines)] // One pass over the page's links.
    pub async fn pictures(&self, key: &str, bytes: &[u8]) -> Option<PictureBatch> {
        if !self.params.process_images {
            return None;
        }
        let text = std::str::from_utf8(bytes).ok()?;
        if pictures::holds_token(text) {
            return None;
        }
        let links = markdown_images(text);
        if links.is_empty() {
            return None;
        }
        let (repository, _, _) = self.side();
        let images = self
            .side
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .images
            .clone();
        let prefix = self.mapped_prefix();
        let mut batch = PictureBatch {
            template: text.to_owned(),
            ..PictureBatch::default()
        };
        let mut budget = Budget::default();
        for link in links {
            // The target without Azure DevOps' ` =WxH` size, `?query`, `#fragment`.
            let target = attachment_target(&link.url);
            if !target.starts_with("/.attachments/") || !pictures::has_image_extension(target) {
                // An external link (the SDK fetched it unauthenticated; no
                // request leaves for a host the toolkit is not configured
                // for), or a file that is not a picture.
                batch.skip(UNSUPPORTED);
                continue;
            }
            if let Err(reason) = budget.may_read() {
                batch.skip(reason);
                continue;
            }
            let relative = pictures::percent_decode(target.trim_start_matches('/'));
            let path = if prefix.is_empty() {
                relative
            } else {
                format!("{prefix}/{relative}")
            };
            let Some((object_id, size)) = images.get(&path) else {
                tracing::debug!(
                    page = key,
                    "a wiki picture link names no file; left as it is"
                );
                batch.skip(UNREADABLE);
                continue;
            };
            if *size > pictures::MAX_PICTURE_BYTES {
                batch.skip(TOO_LARGE);
                continue;
            }
            let segments = [
                "git",
                "repositories",
                repository.as_str(),
                "blobs",
                object_id.as_str(),
            ];
            let Ok(mut request) =
                self.build(AdoRequest::get(&segments, GIT_API).query("$format", "octetstream"))
            else {
                batch.skip(UNSUPPORTED);
                continue;
            };
            request
                .headers_mut()
                .insert(ACCEPT, HeaderValue::from_static("application/octet-stream"));
            let data = match self
                .http
                .raw_within(request, pictures::MAX_PICTURE_BYTES)
                .await
            {
                Ok(data) => data,
                Err(failure) => {
                    tracing::debug!(page = key, "a wiki picture could not be read");
                    batch.skip(pictures::skip_reason(&failure));
                    continue;
                }
            };
            if let Err(reason) = budget.take(data.len() as u64) {
                batch.skip(reason);
                continue;
            }
            let token = pictures::token(batch.pictures.len());
            let markup = format!("![{}]({})", link.alt, link.url);
            batch.template = batch.template.replace(&markup, &token);
            batch.pictures.push(SourcePicture {
                token,
                name: link.alt.clone(),
                bytes: data,
                prompt: None,
                before: format!("![{}](", link.alt),
                after: ")".to_owned(),
                fallback: markup,
                escapes: &[Escape::MarkdownLink],
            });
        }
        (!batch.pictures.is_empty() || !batch.skipped.is_empty()).then_some(batch)
    }

    /// Whether the page `bytes` link a wiki attachment picture: asked
    /// before anything else is, so a page without one costs no wait for
    /// the model.
    #[must_use]
    pub fn may_have_pictures(&self, bytes: &[u8]) -> bool {
        self.params.process_images
            && std::str::from_utf8(bytes).is_ok_and(|text| {
                markdown_images(text).iter().any(|link| {
                    let target = attachment_target(&link.url);
                    target.starts_with("/.attachments/") && pictures::has_image_extension(target)
                })
            })
    }

    /// The repository folder a code wiki shows, without slashes at the ends
    /// (empty for a project wiki).
    fn mapped_prefix(&self) -> String {
        self.side
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .prefix
            .clone()
    }

    fn side(&self) -> (String, String, Arc<BTreeMap<String, Extra>>) {
        let side = self
            .side
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        (
            side.repository_id.clone(),
            side.wiki_name.clone(),
            Arc::clone(&side.extras),
        )
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

/// A link's target as a path: Azure DevOps' ` =WxH` size suffix, the
/// `?query` and the `#fragment` cut.
fn attachment_target(url: &str) -> &str {
    let mut target = url.trim();
    if let Some(at) = target.rfind(" =")
        && target[at + 2..]
            .split_once('x')
            .is_some_and(|(width, height)| {
                width.bytes().all(|b| b.is_ascii_digit())
                    && height.bytes().all(|b| b.is_ascii_digit())
            })
    {
        target = target[..at].trim_end();
    }
    target.split(['?', '#']).next().unwrap_or(target)
}

/// One `![alt](url)` the SDK's `!\[(.*?)\]\((.*?)\)` finds: neither part
/// crosses a line, and the first `](` and the first `)` after it close them.
struct ImageLink {
    alt: String,
    url: String,
}

/// The distinct links of `text`, in the order they first appear.
fn markdown_images(text: &str) -> Vec<ImageLink> {
    let mut found: Vec<ImageLink> = Vec::new();
    let mut at = 0;
    while let Some(offset) = text[at..].find("![") {
        let start = at + offset;
        let rest = &text[start + 2..];
        let line = rest.find('\n').unwrap_or(rest.len());
        let Some(close) = rest[..line].find("](") else {
            at = start + 2;
            continue;
        };
        let after = &rest[close + 2..];
        let line_after = after.find('\n').unwrap_or(after.len());
        let Some(end) = after[..line_after].find(')') else {
            at = start + 2;
            continue;
        };
        let alt = rest[..close].to_owned();
        let url = after[..end].to_owned();
        if !found.iter().any(|f| f.alt == alt && f.url == url) {
            found.push(ImageLink { alt, url });
        }
        at = start + 2 + close + 2 + end + 1;
    }
    found
}

/// One page of the wiki tree.
struct Page {
    id: Option<u64>,
    path: String,
    git_item_path: Option<String>,
    remote_url: Option<String>,
}

fn flatten(node: &Value, out: &mut Vec<Page>) -> Result<(), Failure> {
    let text = |name: &str| node.get(name).and_then(Value::as_str).map(str::to_owned);
    if let Some(path) = text("path") {
        if path.contains(['\u{0}', '\n', '\r']) {
            tracing::debug!("a wiki page path is malformed; skipped");
        } else {
            out.push(Page {
                id: node.get("id").and_then(Value::as_u64),
                path,
                git_item_path: text("gitItemPath"),
                remote_url: text("remoteUrl").or_else(|| text("url")),
            });
            if out.len() > MAX_PAGES {
                return Err(Failure::InvalidResponse(
                    "the wiki holds more pages than the listing cap",
                ));
            }
        }
    }
    if let Some(children) = node.get("subPages").and_then(Value::as_array) {
        for child in children {
            flatten(child, out)?;
        }
    }
    Ok(())
}

/// The repository folder a code wiki shows (`mappedPath`, documented on the
/// wiki), `/`-prefixed without a trailing slash; empty for a project wiki or
/// a root mapping.
fn mapped_path(wiki: &Value) -> Result<String, Failure> {
    let Some(mapped) = wiki.get("mappedPath").and_then(Value::as_str) else {
        return Ok(String::new());
    };
    let trimmed = mapped.trim().trim_matches('/');
    if trimmed.is_empty() {
        return Ok(String::new());
    }
    if !valid_key(trimmed) {
        return Err(Failure::InvalidResponse("the wiki's mapped path is unsafe"));
    }
    Ok(format!("/{trimmed}"))
}

/// The SDK's `path_contains` comparison form: lower case, `-` as a space.
fn comparable_path(path: &str) -> String {
    path.to_lowercase().replace('-', " ")
}

impl ContentSource for AdoWikiSource {
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
        self.http
            .check_size(listed.reference.size)
            .map_err(failed)?;
        let (repository, wiki_name, extras) = self.side();
        let extra = extras
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let segments = [
            "git",
            "repositories",
            repository.as_str(),
            "blobs",
            listed.handle.as_str(),
        ];
        let mut request = self
            .build(AdoRequest::get(&segments, GIT_API).query("$format", "octetstream"))
            .map_err(failed)?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/octet-stream"));
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("type".to_owned(), Value::String(extra.kind.to_owned()));
        metadata.insert("wiki".to_owned(), Value::String(wiki_name));
        metadata.insert("object_id".to_owned(), Value::String(listed.handle.clone()));
        metadata.insert("git_path".to_owned(), Value::String(extra.git_path.clone()));
        if let Some(id) = extra.page_id {
            metadata.insert("id".to_owned(), Value::String(id.to_string()));
        }
        if let Some(path) = &extra.page_path {
            metadata.insert("path".to_owned(), Value::String(path.clone()));
        }
        let title = extra
            .page_path
            .as_deref()
            .map_or_else(|| title_of(key), title_of);
        Ok(Document {
            title,
            uri: extra.remote_url.clone(),
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
    use serde_json::json;

    const ROOT: &str = "1111111111111111111111111111111111111111";
    const HOME: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const CHILD: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
    const IMAGE: &str = "cccccccccccccccccccccccccccccccccccccccc";
    const SPEC: &str = "dddddddddddddddddddddddddddddddddddddddd";
    const REPO: &str = "9f0e8d7c-0000-4000-8000-000000000001";

    fn config() -> AdoToolkitConfig {
        let settings = json!({
            "ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"},
            "project": "Fabrikam",
            "default_wiki_identifier": "Fabrikam.wiki",
        });
        AdoToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn params(value: &Value) -> AdoWikiIndexParams {
        AdoWikiIndexParams::from_arguments(value.as_object().expect("object")).expect("params")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("dev.azure.com"))
    }

    fn entry(path: &str, id: &str, size: u64) -> Value {
        json!({"relativePath": path, "gitObjectType": "blob", "objectId": id, "size": size, "mode": "100644"})
    }

    fn wiki_fixture() -> Arc<Scripted> {
        Scripted::new(|request, _| wiki_reply(request))
    }

    fn wiki_reply(request: &Request) -> Reply {
        {
            let path = request.url().path();
            match path {
                "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki" => Reply::json(&json!({
                    "id": "w1", "name": "Fabrikam.wiki", "repositoryId": REPO,
                    "versions": [{"version": "wikiMaster"}], "type": "projectWiki"
                })),
                "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki/pages" => {
                    assert_eq!(query(request, "recursionLevel").as_deref(), Some("full"));
                    Reply::json(&json!({
                        "path": "/", "subPages": [
                            {"id": 1, "path": "/Home", "gitItemPath": "/Home.md", "remoteUrl": "https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/Fabrikam.wiki/1/Home",
                             "subPages": [
                                {"id": 2, "path": "/Home/Child Page", "gitItemPath": "/Home/Child-Page.md", "remoteUrl": "https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/Fabrikam.wiki/2/Child-Page"},
                             ]},
                            {"id": 3, "path": "/Orphan", "gitItemPath": "/Orphan.md"},
                        ]
                    }))
                }
                p if p == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/items") => {
                    assert_eq!(
                        query(request, "versionDescriptor.version").as_deref(),
                        Some("wikiMaster")
                    );
                    Reply::json(&json!({"objectId": ROOT}))
                }
                p if p
                    == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/trees/{ROOT}") =>
                {
                    match query(request, "continuationToken").as_deref() {
                        None => Reply::json(&json!({"treeEntries": [
                            entry("Home.md", HOME, 11),
                            entry("Home/Child-Page.md", CHILD, 7),
                        ]}))
                        .header(CONTINUATION, "t2"),
                        Some("t2") => Reply::json(&json!({"treeEntries": [
                            entry(".attachments/diagram.png", IMAGE, 3),
                            entry(".attachments/spec.pdf", SPEC, 5),
                            entry(".order", HOME, 1),
                        ]})),
                        other => panic!("token {other:?}"),
                    }
                }
                p if p
                    == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/blobs/{CHILD}") =>
                {
                    Reply::bytes(b"# Child")
                }
                p if p
                    == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/blobs/{IMAGE}") =>
                {
                    Reply::bytes(b"PNGDATA")
                }
                p if p
                    == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/blobs/{SPEC}") =>
                {
                    Reply::bytes(b"%PDF-")
                }
                other => panic!("unexpected request {other}"),
            }
        }
    }

    #[tokio::test]
    async fn pages_are_versioned_by_blob_id_and_attachments_are_opt_in_and_filtered() {
        let transport = wiki_fixture();
        let source = AdoWikiSource::new(
            config(),
            params(&json!({"include_attachments": true, "skip_extensions": ["png"]})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size))
            .collect();
        assert_eq!(
            keys,
            [
                (".attachments/spec.pdf", SPEC, 5),
                ("Home.md", HOME, 11),
                ("Home/Child Page.md", CHILD, 7),
            ],
            "the orphan page has no file in the tree and is not listed"
        );
        assert_eq!(listed[0].mime, "application/pdf");
        assert_eq!(listed[2].mime, "text/markdown");
        let page = source.fetch("Home/Child Page.md").await.expect("page");
        assert_eq!(page.bytes, b"# Child");
        assert_eq!(page.title, "Child Page");
        assert_eq!(page.metadata["id"], "2");
        assert_eq!(page.metadata["type"], "page");
        assert_eq!(page.metadata["wiki"], "Fabrikam.wiki");
        assert_eq!(
            page.uri.as_deref(),
            Some("https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/Fabrikam.wiki/2/Child-Page")
        );
        let attachment = source.fetch(".attachments/spec.pdf").await.expect("pdf");
        assert_eq!(attachment.metadata["type"], "attachment");
        assert_eq!(attachment.bytes, b"%PDF-");
        let blob = transport.requests().pop().expect("blob");
        assert!(
            blob.headers()
                .get("authorization")
                .expect("pat")
                .is_sensitive()
        );
    }

    #[tokio::test]
    async fn path_contains_filters_pages_with_dashes_as_spaces_and_attachments_stay_off() {
        let source = AdoWikiSource::new(
            config(),
            params(&json!({"path_contains": "child-PAGE"})),
            allow(),
            wiki_fixture(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed.iter().map(|d| d.key.as_str()).collect();
        assert_eq!(keys, ["Home/Child Page.md"]);
    }

    #[tokio::test]
    async fn without_a_wiki_identifier_the_listing_says_so_and_401_is_reported() {
        let settings = json!({
            "ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"},
            "project": "Fabrikam",
        });
        let without_wiki =
            AdoToolkitConfig::parse(settings.as_object().expect("s")).expect("config");
        let source = AdoWikiSource::new(
            without_wiki,
            AdoWikiIndexParams::default(),
            allow(),
            Scripted::new(|_, _| panic!("no request")),
        )
        .expect("source");
        let error = source.list().await.expect_err("no wiki");
        assert!(error.to_string().contains("wiki_identifier"), "{error}");

        let denied = Scripted::new(|_, _| Reply::status(StatusCode::UNAUTHORIZED));
        let source = AdoWikiSource::new(config(), AdoWikiIndexParams::default(), allow(), denied)
            .expect("source");
        let error = source.list().await.expect_err("401");
        assert!(error.to_string().contains("HTTP 401"), "{error}");
    }

    #[tokio::test]
    async fn a_rate_limited_listing_waits_for_retry_after_and_goes_on() {
        let throttled = Scripted::new(|request, index| {
            if index == 0 {
                Reply::rate_limited(Some("0"))
            } else {
                wiki_reply(request)
            }
        });
        let source = AdoWikiSource::new(
            config(),
            AdoWikiIndexParams::default(),
            allow(),
            throttled.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 2);
        let seen = throttled.seen();
        assert_eq!(seen[0], seen[1], "the throttled request was repeated");
    }

    #[tokio::test]
    async fn a_code_wiki_reads_only_its_mapped_folder_and_skips_bad_entries() {
        const DOCS: &str = "2222222222222222222222222222222222222222";
        let seen_paths = Arc::new(Mutex::new(Vec::<String>::new()));
        let record = Arc::clone(&seen_paths);
        let transport = Scripted::new(move |request, _| {
            let path = request.url().path().to_owned();
            record.lock().expect("lock").push(path.clone());
            match path.as_str() {
                "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki" => Reply::json(&json!({
                    "id": "w1", "name": "Fabrikam.wiki", "repositoryId": REPO,
                    "versions": [{"version": "main"}], "type": "codeWiki",
                    "mappedPath": "/docs"
                })),
                "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki/pages" => Reply::json(&json!({
                    "path": "/", "subPages": [
                        {"id": 1, "path": "/Home", "gitItemPath": "/docs/Home.md"},
                        {"id": 2, "path": "/Bad\nName", "gitItemPath": "/docs/Bad.md"},
                    ]
                })),
                p if p == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/items") => {
                    assert_eq!(query(request, "path").as_deref(), Some("/docs"));
                    assert_eq!(
                        query(request, "versionDescriptor.version").as_deref(),
                        Some("main")
                    );
                    Reply::json(
                        &json!({"objectId": DOCS, "gitObjectType": "tree", "path": "/docs"}),
                    )
                }
                p if p
                    == format!("/contoso/Fabrikam/_apis/git/repositories/{REPO}/trees/{DOCS}") =>
                {
                    Reply::json(&json!({"treeEntries": [
                        entry("Home.md", HOME, 11),
                        entry("odd\nname.md", CHILD, 7),
                        entry(".attachments/spec.pdf", SPEC, 5),
                        {"relativePath": "no-id.md", "gitObjectType": "blob"},
                    ]}))
                }
                other => panic!("unexpected request {other}"),
            }
        });
        let source = AdoWikiSource::new(
            config(),
            params(&json!({"include_attachments": true})),
            allow(),
            transport,
        )
        .expect("source");
        let listed = source
            .list()
            .await
            .expect("listing survives the bad entries");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str()))
            .collect();
        assert_eq!(
            keys,
            [(".attachments/spec.pdf", SPEC), ("Home.md", HOME)],
            "attachments come from <mapped>/.attachments/, keyed relative to the wiki"
        );
        assert!(
            seen_paths
                .lock()
                .expect("lock")
                .iter()
                .all(|path| !path.ends_with(&format!("/trees/{ROOT}"))),
            "the repository's own tree is never read"
        );
    }

    #[tokio::test]
    async fn process_images_reads_attachment_pictures_and_leaves_every_other_link() {
        let source = AdoWikiSource::new(
            config(),
            params(&json!({"process_images": true, "image_description_prompt": "Read it."})),
            allow(),
            wiki_fixture(),
        )
        .expect("source");
        source.list().await.expect("listing");
        assert_eq!(
            source.picture_step("Home.md"),
            Some(PictureStep::Inline {
                prompt: Some("Read it.".to_owned())
            })
        );
        let off = AdoWikiSource::new(config(), params(&json!({})), allow(), wiki_fixture())
            .expect("source");
        off.list().await.expect("listing");
        assert_eq!(off.picture_step("Home.md"), None, "off by default");
        assert!(
            off.pictures("Home.md", b"![a](/.attachments/diagram.png)")
                .await
                .is_none()
        );

        let text = "intro ![diagram](/.attachments/diagram.png) and ![ext](https://example.com/x.png) \
and ![gone](/.attachments/missing.png) and ![doc](/.attachments/spec.pdf) end ![diagram](/.attachments/diagram.png)";
        let batch = source
            .pictures("Home.md", text.as_bytes())
            .await
            .expect("batch");
        assert_eq!(batch.pictures.len(), 1, "one distinct readable picture");
        assert_eq!(batch.pictures[0].bytes, b"PNGDATA");
        assert_eq!(
            batch.skipped.get(UNSUPPORTED),
            Some(&2),
            "the external link and the pdf"
        );
        assert_eq!(batch.skipped.get(UNREADABLE), Some(&1), "the missing file");
        assert!(!batch.skipped.contains_key(pictures::FETCH_FAILED));
        assert_eq!(
            batch.render(&[Some("A chart")]),
            "intro ![diagram](A chart) and ![ext](https://example.com/x.png) \
and ![gone](/.attachments/missing.png) and ![doc](/.attachments/spec.pdf) end ![diagram](A chart)",
            "every occurrence of the link takes the description"
        );
        assert_eq!(
            batch.render(&[None]),
            text,
            "an undescribed picture is the link"
        );
    }

    #[test]
    fn image_links_are_found_like_the_sdk_regex() {
        let links = markdown_images("![a](x) ![b]( y z ) ![no close](q\n) ![c](x) ![a](x) [d](e)");
        let pairs: Vec<(&str, &str)> = links
            .iter()
            .map(|l| (l.alt.as_str(), l.url.as_str()))
            .collect();
        assert_eq!(pairs, [("a", "x"), ("b", " y z "), ("c", "x")]);
    }

    fn picture_wiki() -> AdoWikiSource {
        AdoWikiSource::new(
            config(),
            params(&json!({"process_images": true})),
            allow(),
            wiki_fixture(),
        )
        .expect("source")
    }

    #[tokio::test]
    async fn size_suffixes_queries_and_fragments_do_not_hide_a_picture() {
        let source = picture_wiki();
        source.list().await.expect("listing");
        let text = "![a](/.attachments/diagram.png =300x200) ![b](/.attachments/diagram.png?raw=1) ![c](/.attachments/diagram.png#top) ![d](/.attachments/diagram.png =x100) ![e](/.attachments/notes.txt =1x1)";
        assert!(source.may_have_pictures(text.as_bytes()));
        let batch = source
            .pictures("Home.md", text.as_bytes())
            .await
            .expect("batch");
        assert_eq!(batch.pictures.len(), 4, "{:?}", batch.skipped);
        assert!(batch.pictures.iter().all(|p| p.bytes == b"PNGDATA"));
        assert_eq!(batch.skipped.get(UNSUPPORTED), Some(&1), "the txt, counted");
        // Each link is replaced as written, size suffix and all.
        assert_eq!(
            batch.render(&[Some("x"), Some("x"), Some("x"), Some("x")]),
            "![a](x) ![b](x) ![c](x) ![d](x) ![e](/.attachments/notes.txt =1x1)"
        );
        assert!(
            !source.may_have_pictures(
                b"![ext](https://example.com/x.png) ![doc](/.attachments/a.pdf)"
            )
        );
    }

    #[tokio::test]
    async fn a_description_cannot_break_out_of_the_link() {
        let source = picture_wiki();
        source.list().await.expect("listing");
        let batch = source
            .pictures("Home.md", b"x ![a](/.attachments/diagram.png) y")
            .await
            .expect("batch");
        assert_eq!(
            batch.render(&[Some("A (graph)\n\n of [x] and \\ done")]),
            "x ![a](A (graph\\) of [x\\] and \\\\ done) y"
        );
    }

    #[tokio::test]
    async fn a_blob_that_fails_is_retried_only_when_the_failure_is_transient() {
        let status = |code: StatusCode| {
            let fixture = Scripted::new(move |request, _| {
                if request.url().path().ends_with(&format!("/blobs/{IMAGE}")) {
                    Reply::status(code)
                } else {
                    wiki_reply(request)
                }
            });
            AdoWikiSource::new(
                config(),
                params(&json!({"process_images": true})),
                allow(),
                fixture,
            )
            .expect("source")
        };
        let text = b"![a](/.attachments/diagram.png)";
        let transient = status(StatusCode::BAD_GATEWAY);
        transient.list().await.expect("listing");
        let batch = transient.pictures("Home.md", text).await.expect("counted");
        assert_eq!(batch.template.as_bytes(), text);
        assert_eq!(batch.skipped.get(pictures::FETCH_FAILED), Some(&1));
        let denied = status(StatusCode::FORBIDDEN);
        denied.list().await.expect("listing");
        let batch = denied.pictures("Home.md", text).await.expect("counted");
        assert_eq!(batch.skipped.get(pictures::FORBIDDEN), Some(&1));
        assert_eq!(batch.skipped.get(pictures::FETCH_FAILED), None);
    }
}
