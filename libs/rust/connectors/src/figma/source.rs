//! Figma as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **List.** For each file (the `urls_or_file_keys` entries, then the files
//!   of `project_ids` and of the projects of `team_ids`): the file's pages
//!   (`GET /v1/files/{key}?depth=1`), then the pages' contents in batches of
//!   ten ids (`GET /v1/files/{key}/nodes?ids=…`), the SDK's batch size. A
//!   batch Figma cannot answer (a 400, 413 or 5xx, no answer, or a body over
//!   the response cap) is split in half until the failing page stands alone;
//!   that page is then read as its frames, one batch of ids each. A page or
//!   frame the listing discovered that still fails, or that Figma answers
//!   `null` for, fails the listing: a missing frame would read as a deleted
//!   one. A node the user named (a URL's `node-id`, `node_ids_include`)
//!   that Figma has no node for in a file is skipped with a warning, as the
//!   SDK skips a missing id: it lists nothing, so its chunks go away.
//! * **Documents.** A page's top-level nodes with a size and, unless a vector
//!   without text, are the frames; a frame larger than
//!   `subframe_extract_threshold` on either side is indexed through its
//!   children instead (recursively), the SDK's subframe extraction. Each
//!   leaf is one document, `file/{key}/frame/{node id}.txt`. Its text is the
//!   SDK's frame header (`FRAME: name [x,y WxH] #id`) followed by the frame's
//!   text by role (headings, buttons, labels, body, errors, placeholders).
//!   The model-written purpose and visual description wait for I4; a frame
//!   with no text is not listed.
//! * **Versions.** Figma versions a whole file (`version`, `lastModified`),
//!   not a node, so a frame's version is the SHA-256 of what is indexed: its
//!   text, name and size, its page and the file's name. The file's own
//!   `version` and `lastModified` are not stored on any document. Saving a file therefore rewrites
//!   exactly the frames whose indexed text changed. Each listing reads every
//!   page, so the listing is the cost of a run; fetching a document reads
//!   nothing again (the text was built with its version).
//! * **Comments** (`include_comments`, not in the SDK): one document each,
//!   `file/{key}/comment/{id}.txt`, only those pinned inside the selected pages
//!   or frames when the selection is restricted, versioned by its text and resolved state, the node it is pinned
//!   to and `parent_key`, the frame it is pinned to when that frame is
//!   listed.
//! * **Auth and limits.** `X-Figma-Token` goes to `api.figma.com` only: every
//!   request is built here from that origin and no URL a response names is
//!   followed. A 401 or 403 (Figma answers a bad token with 403) is an error
//!   naming the credential, never an empty listing; a 429 backs off within
//!   the shared bound, honouring `Retry-After`.

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fmt::Write as _;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use serde_json::{Map, Value};

use super::config::{
    API_ORIGIN, FigmaConnectorConfig, FigmaIndexArguments, FileSelection, MAX_ID_BYTES, PROVIDER,
    plain_identifier,
};
use super::text::{self, MAX_FRAME_NODES, Roles};
use crate::atlassian::{DetailedListing, Details};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references, web_url,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderName, HeaderValue, Method, Request, StatusCode, Transport, Url};

/// The SDK's `eager_split_threshold` for pages: more ids per request almost
/// always time out.
const NODES_PER_REQUEST: usize = 10;
/// How deep a frame extraction goes (a frame inside a frame inside …).
const MAX_EXTRACTION_DEPTH: usize = 8;
const X_FIGMA_TOKEN: HeaderName = HeaderName::from_static("x-figma-token");

/// One Figma toolkit's frames and comments.
pub struct FigmaSource {
    config: FigmaConnectorConfig,
    arguments: FigmaIndexArguments,
    root: Url,
    http: SourceHttp,
    listing: DetailedListing,
}

impl FigmaSource {
    /// The toolkit `config` indexed with the `index_data` `arguments`, every
    /// request through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// `api.figma.com` is not on the allowlist.
    pub fn new(
        config: FigmaConnectorConfig,
        arguments: FigmaIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let root = Url::parse(API_ORIGIN).map_err(|_| {
            Failure::Client("the API origin is not a URL".to_owned())
                .into_source_error(PROVIDER, None)
        })?;
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(&root) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            config,
            arguments,
            root,
            http,
            listing: DetailedListing::default(),
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

    /// `GET https://api.figma.com/v1/{segments...}?{query}` with the token.
    fn get(&self, segments: &[&str], query: &[(&str, &str)]) -> Result<Request, Failure> {
        let mut all = vec!["v1"];
        all.extend_from_slice(segments);
        let mut url = web_url(&self.root, all).ok_or_else(|| {
            Failure::Client("the Figma API origin cannot carry a path".to_owned())
        })?;
        if !query.is_empty() {
            url.query_pairs_mut().extend_pairs(query.iter().copied());
        }
        let mut request = Request::new(Method::GET, url);
        let mut token = HeaderValue::from_str(self.config.token())
            .map_err(|_| Failure::Client("the token is not a valid header value".to_owned()))?;
        token.set_sensitive(true);
        request.headers_mut().insert(X_FIGMA_TOKEN, token);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        Ok(request)
    }

    /// A JSON answer; a refused credential and a missing resource are errors
    /// that say what was asked for.
    async fn json(
        &self,
        segments: &[&str],
        query: &[(&str, &str)],
        what: &str,
    ) -> Result<Value, Failure> {
        match self.http.json(self.get(segments, query)?).await {
            Ok((_, value)) => Ok(value),
            Err(Failure::Status(status @ (StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN))) => {
                Err(Failure::Client(format!(
                    "{PROVIDER} refused the credential (HTTP {}) for {what}; check the toolkit's token and that it can read it",
                    status.as_u16()
                )))
            }
            Err(Failure::Status(StatusCode::NOT_FOUND)) => {
                Err(Failure::Client(format!("{PROVIDER} did not find {what}")))
            }
            Err(failure) => Err(failure),
        }
    }

    /// The keys of the files a project holds.
    async fn project_files(&self, project: &str) -> Result<Vec<String>, Failure> {
        let body = self
            .json(
                &["projects", project, "files"],
                &[],
                &format!("project '{project}'"),
            )
            .await?;
        Ok(array(&body, "files")?
            .iter()
            .filter_map(|file| file.get("key").and_then(Value::as_str))
            .filter(|key| plain_identifier(key))
            .map(ToOwned::to_owned)
            .collect())
    }

    /// The ids of the projects a team holds.
    async fn team_projects(&self, team: &str) -> Result<Vec<String>, Failure> {
        let body = self
            .json(&["teams", team, "projects"], &[], &format!("team '{team}'"))
            .await?;
        Ok(array(&body, "projects")?
            .iter()
            .filter_map(|project| match project.get("id") {
                Some(Value::String(id)) => Some(id.clone()),
                Some(Value::Number(id)) => Some(id.to_string()),
                _ => None,
            })
            .filter(|id| plain_identifier(id))
            .collect())
    }

    /// The files to index: the named ones, then those of the projects and
    /// teams, each once.
    async fn selections(&self) -> Result<Vec<FileSelection>, Failure> {
        let mut files = self.arguments.files.clone();
        // A file a project or team holds is wanted whole: it widens a URL's
        // page selection of the same file.
        let add = |files: &mut Vec<FileSelection>, key: String| {
            let whole = FileSelection {
                key,
                node_ids: Vec::new(),
            };
            match files.iter_mut().find(|known| known.key == whole.key) {
                Some(known) => known.merge(whole),
                None => files.push(whole),
            }
        };
        for project in &self.arguments.project_ids {
            for key in self.project_files(project).await? {
                add(&mut files, key);
            }
        }
        for team in &self.arguments.team_ids {
            for project in self.team_projects(team).await? {
                for key in self.project_files(&project).await? {
                    add(&mut files, key);
                }
            }
        }
        Ok(files)
    }

    /// `ids` of one file as `node id` to its `document`, at `depth` (the
    /// whole subtree when `None`). Figma answers `null` for a node it cannot
    /// return (missing or unrenderable), and an id it does not mention is the
    /// same: both are `None`, never dropped.
    async fn nodes(
        &self,
        file: &str,
        ids: &[String],
        depth: Option<&str>,
    ) -> Result<BTreeMap<String, Option<Value>>, Failure> {
        let joined = ids.join(",");
        let mut query = vec![("ids", joined.as_str())];
        if let Some(depth) = depth {
            query.push(("depth", depth));
        }
        let mut body = self
            .json(
                &["files", file, "nodes"],
                &query,
                &format!("nodes of file '{file}'"),
            )
            .await?;
        let nodes = body
            .get_mut("nodes")
            .and_then(Value::as_object_mut)
            .ok_or(Failure::InvalidResponse("a nodes answer has no `nodes`"))?;
        // The documents are taken out of the owned answer, not cloned.
        Ok(ids
            .iter()
            .map(|id| {
                let document = nodes
                    .get_mut(id)
                    .and_then(|node| node.get_mut("document"))
                    .filter(|document| document.is_object())
                    .map(Value::take);
                (id.clone(), document)
            })
            .collect())
    }

    /// `ids` read in batches, a batch Figma cannot answer split in half. The
    /// nodes read, and the ids that failed alone or that Figma answered
    /// `null` for.
    async fn nodes_bisecting(
        &self,
        file: &str,
        ids: &[String],
    ) -> Result<(BTreeMap<String, Value>, Vec<String>), Failure> {
        let mut found = BTreeMap::new();
        let mut failed = Vec::new();
        let mut queue: VecDeque<Vec<String>> = ids
            .chunks(NODES_PER_REQUEST)
            .map(<[String]>::to_vec)
            .collect();
        while let Some(batch) = queue.pop_front() {
            match self.nodes(file, &batch, None).await {
                Ok(nodes) => {
                    for (id, document) in nodes {
                        match document {
                            Some(document) => {
                                found.insert(id, document);
                            }
                            None => failed.push(id),
                        }
                    }
                }
                Err(failure) if retriable(&failure) => {
                    if batch.len() > 1 {
                        let (left, right) = batch.split_at(batch.len() / 2);
                        queue.push_front(right.to_vec());
                        queue.push_front(left.to_vec());
                    } else {
                        failed.extend(batch);
                    }
                }
                Err(failure) => return Err(failure),
            }
        }
        Ok((found, failed))
    }

    /// The nodes `ids` name, whole, in `ids` order. A page Figma cannot
    /// return in one answer is assembled from its frames, as the SDK falls
    /// back to the shallow page.
    ///
    /// With `named` the ids are the user's (a URL's node ids, or
    /// `node_ids_include`), applied to every file: one Figma has no node for
    /// in this file is skipped with a warning, as the SDK skips a missing id
    /// (its indexed chunks go away because it is no longer listed). Without
    /// it the ids were discovered by listing the file, so a page or frame
    /// that cannot be returned fails the listing: it would read as deleted.
    async fn containers(
        &self,
        file: &str,
        ids: &[String],
        named: bool,
    ) -> Result<Vec<Value>, Failure> {
        const LOST: &str = "Figma could not return a page or frame; indexing without it would drop it from the index";
        let (mut found, failed) = self.nodes_bisecting(file, ids).await?;
        for id in failed {
            let mut shallow = self
                .nodes(file, std::slice::from_ref(&id), Some("1"))
                .await?;
            let Some(Some(mut page)) = shallow.remove(&id) else {
                if named {
                    continue;
                }
                return Err(Failure::InvalidResponse(LOST));
            };
            let frame_ids: Vec<String> = text::children(&page)
                .iter()
                .filter_map(|child| child.get("id").and_then(Value::as_str))
                .map(ToOwned::to_owned)
                .collect();
            let (mut frames, still_failed) = self.nodes_bisecting(file, &frame_ids).await?;
            if !still_failed.is_empty() {
                return Err(Failure::InvalidResponse(
                    "Figma could not return a frame; indexing without it would drop it from the index",
                ));
            }
            let whole: Vec<Value> = frame_ids
                .iter()
                .filter_map(|frame_id| frames.remove(frame_id))
                .collect();
            page["children"] = Value::Array(whole);
            found.insert(id, page);
        }
        let mut containers = Vec::with_capacity(ids.len());
        for id in ids {
            match found.remove(id) {
                Some(container) => containers.push(container),
                None if named => {
                    tracing::warn!(
                        node_id = %id,
                        "a node named for indexing is not in this Figma file; skipped"
                    );
                }
                None => return Err(Failure::InvalidResponse(LOST)),
            }
        }
        Ok(containers)
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut listed = Vec::new();
        let mut details = Details::new();
        for selection in self.selections().await? {
            self.list_file(&selection, &mut listed, &mut details)
                .await?;
            self.http.check_count(listed.len())?;
        }
        Ok((listed, details))
    }

    async fn list_file(
        &self,
        selection: &FileSelection,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let key = selection.key.as_str();
        let file = self
            .json(&["files", key], &[("depth", "1")], &format!("file '{key}'"))
            .await?;
        let file_name = text_field(&file, "name").unwrap_or("Untitled").to_owned();
        let page_ids: Vec<String> = file
            .pointer("/document/children")
            .and_then(Value::as_array)
            .map(|pages| {
                pages
                    .iter()
                    .filter_map(|page| page.get("id").and_then(Value::as_str))
                    .map(ToOwned::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        // A URL's node ids win over the include list; an include list wins
        // over the exclude list (the SDK's rule).
        let named = if selection.node_ids.is_empty() {
            &self.arguments.node_ids_include
        } else {
            &selection.node_ids
        };
        let user_named = !named.is_empty();
        let page_count = page_ids.len();
        let wanted: Vec<String> = if user_named {
            named.clone()
        } else {
            page_ids
                .into_iter()
                .filter(|id| !self.arguments.node_ids_exclude.contains(id))
                .collect()
        };
        // The whole file is selected unless the user named nodes or left
        // pages out: only then do comments follow the selection.
        let restricted = user_named || wanted.len() < page_count;
        let mut seen_ids = std::collections::BTreeSet::new();
        let wanted: Vec<String> = wanted
            .into_iter()
            .filter(|id| seen_ids.insert(id.clone()))
            .collect();
        let file_info = FileInfo {
            key,
            name: &file_name,
        };
        let mut frame_keys: BTreeMap<String, String> = BTreeMap::new();
        let mut selected_ids = BTreeSet::new();
        if !wanted.is_empty() {
            for container in self.containers(key, &wanted, user_named).await? {
                if restricted && self.arguments.include_comments {
                    collect_ids(&container, &mut selected_ids);
                }
                self.list_container(&container, &file_info, listed, details, &mut frame_keys)?;
            }
        }
        if self.arguments.include_comments {
            let scope = restricted.then_some(&selected_ids);
            self.list_comments(&file_info, &frame_keys, scope, listed, details)
                .await?;
        }
        Ok(())
    }

    fn list_container(
        &self,
        container: &Value,
        file: &FileInfo<'_>,
        listed: &mut Vec<Listed>,
        details: &mut Details,
        frame_keys: &mut BTreeMap<String, String>,
    ) -> Result<(), Failure> {
        let page_id = text_field(container, "id").unwrap_or_default();
        let page_name = text_field(container, "name").unwrap_or("Untitled Page");
        let mut leaves = Vec::new();
        self.collect_leaves(text::children(container), true, 0, &mut leaves);
        for (node, text_only) in leaves {
            let Some(node_id) = text_field(node, "id") else {
                continue;
            };
            let Some(segment) = key_segment(node_id) else {
                tracing::debug!("a Figma node id is not a safe key segment; skipped");
                continue;
            };
            let key = format!("file/{}/frame/{segment}.txt", file.key);
            // The segment encoding is injective, so one key is one node id:
            // a repeat is the same frame reached twice.
            if details.contains_key(&key) {
                continue;
            }
            let roles = text::extract(node, MAX_FRAME_NODES);
            if roles.is_empty() {
                continue;
            }
            let frame = Frame::of(node);
            let body = render(&frame, &roles, self.http.limits().max_document_bytes);
            // Everything stored below is covered by the version: the file
            // name, page, frame name and box, type and the rendered text.
            let version = content_version(
                format!(
                    "{}\0{page_id}\0{page_name}\0{}\0{}x{}\0{}\0{body}",
                    file.name,
                    frame.name,
                    px(frame.width),
                    px(frame.height),
                    if text_only { "text" } else { "image" }
                )
                .as_bytes(),
            );
            let mut metadata = file.metadata();
            let mut put = |name: &str, value: Value| {
                metadata.insert(name.to_owned(), value);
            };
            put("id", Value::String(node_id.to_owned()));
            put("page_id", Value::String(page_id.to_owned()));
            put("page_name", Value::String(page_name.to_owned()));
            put("node_id", Value::String(node_id.to_owned()));
            put("node_name", Value::String(frame.name.clone()));
            put("image_width", Value::from(px(frame.width)));
            put("image_height", Value::from(px(frame.height)));
            put(
                "type",
                Value::String(if text_only { "text" } else { "image" }.to_owned()),
            );
            put("url", Value::String(file.url(Some(node_id))));
            put("title", Value::String(frame.name.clone()));
            put("text", Value::String(body.clone()));
            let size = body.len() as u64;
            listed.push(Listed::new(key.clone(), version, size, node_id.to_owned()));
            details.insert(key.clone(), metadata);
            frame_keys.insert(node_id.to_owned(), key);
            self.http.check_count(listed.len())?;
        }
        Ok(())
    }

    /// The leaf frames under `children`: the SDK's frame collection. Nodes
    /// without a size are skipped, as are vectors without text. A top-level
    /// node larger than the extraction threshold stands for its children.
    fn collect_leaves<'a>(
        &self,
        children: &'a [Value],
        top_level: bool,
        depth: usize,
        out: &mut Vec<(&'a Value, bool)>,
    ) {
        let threshold = threshold_px(self.arguments.subframe_extract_threshold);
        for node in children {
            if text_field(node, "id").is_none() || text::hidden(node) {
                continue;
            }
            let frame = Frame::of(node);
            if frame.width <= 0.0 || frame.height <= 0.0 {
                continue;
            }
            let kind = text::node_type(node).to_ascii_lowercase();
            if top_level {
                let include = &self.arguments.node_types_include;
                let exclude = &self.arguments.node_types_exclude;
                if (!include.is_empty() && !include.contains(&kind))
                    || (include.is_empty() && exclude.contains(&kind))
                {
                    continue;
                }
            }
            if matches!(kind.as_str(), "vector" | "line" | "arrow") {
                let mut found = Vec::new();
                text::texts(node, MAX_FRAME_NODES, &mut found);
                if found.is_empty() {
                    continue;
                }
            }
            if kind == "text" {
                out.push((node, true));
                continue;
            }
            if frame.width.max(frame.height) > threshold && depth < MAX_EXTRACTION_DEPTH {
                let before = out.len();
                self.collect_leaves(text::children(node), false, depth + 1, out);
                if out.len() == before {
                    out.push((node, true));
                }
            } else {
                out.push((node, false));
            }
        }
    }

    async fn list_comments(
        &self,
        file: &FileInfo<'_>,
        frame_keys: &BTreeMap<String, String>,
        scope: Option<&BTreeSet<String>>,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let body = self
            .json(
                &["files", file.key, "comments"],
                &[],
                &format!("comments of file '{}'", file.key),
            )
            .await?;
        let comments = array(&body, "comments")?;
        // A reply carries no pin of its own: it belongs where its thread's
        // first comment is pinned.
        let pins: BTreeMap<String, &str> = comments
            .iter()
            .filter_map(|comment| {
                Some((
                    scalar_id(comment.get("id"))?,
                    comment
                        .pointer("/client_meta/node_id")
                        .and_then(Value::as_str)?,
                ))
            })
            .collect();
        for comment in comments {
            if let Some(selected) = scope {
                // Restricted selection: only a comment pinned to a node of
                // the selected pages or frames; a file-level one is not
                // about them.
                let pin = comment
                    .pointer("/client_meta/node_id")
                    .and_then(Value::as_str)
                    .or_else(|| {
                        scalar_id(comment.get("parent_id"))
                            .and_then(|parent| pins.get(&parent).copied())
                    });
                if !pin.is_some_and(|pin| selected.contains(pin)) {
                    continue;
                }
            }
            let Some(id) = scalar_id(comment.get("id")).filter(|id| plain_identifier(id)) else {
                continue;
            };
            let Some(message) = text_field(comment, "message") else {
                continue;
            };
            let author = comment
                .pointer("/user/handle")
                .and_then(Value::as_str)
                .unwrap_or("unknown");
            let created = text_field(comment, "created_at").unwrap_or_default();
            let resolved = text_field(comment, "resolved_at");
            let mut text = format!("Comment by {author} on {created}");
            if let Some(resolved) = resolved {
                let _ = write!(text, " (resolved {resolved})");
            }
            text.push('\n');
            text.push_str(message);
            let text = truncate(&text, self.http.limits().max_document_bytes);
            let key = format!("file/{}/comment/{id}.txt", file.key);
            let pinned = comment
                .pointer("/client_meta/node_id")
                .and_then(Value::as_str);
            let parent = pinned.and_then(|node_id| frame_keys.get(node_id));
            // Pinning is part of what is stored, so it is part of the
            // version: a frame that gains its first text (and so is listed)
            // rewrites the comment with its `parent_key`.
            let version = content_version(
                format!(
                    "{}\0{}\0{}\0{text}",
                    file.name,
                    pinned.unwrap_or_default(),
                    parent.map_or("", String::as_str)
                )
                .as_bytes(),
            );
            let mut metadata = file.metadata();
            metadata.insert("id".into(), Value::String(id.clone()));
            metadata.insert("comment_id".into(), Value::String(id.clone()));
            metadata.insert("type".into(), Value::String("comment".into()));
            metadata.insert("author".into(), Value::String(author.to_owned()));
            metadata.insert("created_on".into(), Value::String(created.to_owned()));
            if let Some(resolved) = resolved {
                metadata.insert("resolved_on".into(), Value::String(resolved.to_owned()));
            }
            if let Some(node_id) = pinned {
                metadata.insert("node_id".into(), Value::String(node_id.to_owned()));
            }
            if let Some(parent) = parent {
                metadata.insert("parent_key".into(), Value::String(parent.clone()));
            }
            metadata.insert("url".into(), Value::String(file.url(pinned)));
            metadata.insert(
                "title".into(),
                Value::String(format!("Comment {id} on {}", file.name)),
            );
            metadata.insert("text".into(), Value::String(text.clone()));
            listed.push(Listed::new(
                key.clone(),
                version,
                text.len() as u64,
                id.clone(),
            ));
            details.insert(key, metadata);
            self.http.check_count(listed.len())?;
        }
        Ok(())
    }
}

impl ContentSource for FigmaSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let snapshot = self
            .listing
            .list_shared(PROVIDER, || self.listing())
            .await?;
        Ok(references(&snapshot.listing))
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
        // The text was built with the version it was listed under: nothing is
        // read again, so a fetch can never disagree with the listing.
        let mut metadata = snapshot.details_of(key);
        let text = match metadata.remove("text") {
            Some(Value::String(text)) => text,
            _ => String::new(),
        };
        let title = match metadata.remove("title") {
            Some(Value::String(title)) => title,
            _ => key.to_owned(),
        };
        let uri = metadata
            .get("url")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned);
        let bytes = text.into_bytes();
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title,
            uri,
            reference,
            bytes,
            metadata,
        })
    }
}

/// The file-level facts every document of a file carries: its key and name,
/// both part of every document's version. Figma's file `version` and
/// `lastModified` move on every save, so they are not stored: a re-save
/// that changes no frame writes nothing, and nothing stored can drift from
/// what the version covers.
struct FileInfo<'a> {
    key: &'a str,
    name: &'a str,
}

impl FileInfo<'_> {
    fn metadata(&self) -> Map<String, Value> {
        let mut metadata = Map::new();
        metadata.insert("file_key".into(), Value::String(self.key.to_owned()));
        metadata.insert("file_name".into(), Value::String(self.name.to_owned()));
        metadata
    }

    /// Where a reader opens the file, or one node of it (Figma spells a node
    /// id `1:2` as `1-2` in a URL).
    fn url(&self, node: Option<&str>) -> String {
        let base = format!("https://www.figma.com/design/{}", self.key);
        match node {
            Some(node) => format!("{base}?node-id={}", node.replace(':', "-")),
            None => base,
        }
    }
}

/// A frame's name and box.
struct Frame {
    id: String,
    name: String,
    x: f64,
    y: f64,
    width: f64,
    height: f64,
}

impl Frame {
    fn of(node: &Value) -> Self {
        let bounds = node.get("absoluteBoundingBox");
        let value = |name: &str| {
            bounds
                .and_then(|bounds| bounds.get(name))
                .and_then(Value::as_f64)
                .unwrap_or(0.0)
        };
        Self {
            id: text_field(node, "id").unwrap_or_default().to_owned(),
            name: one_line(text_field(node, "name").unwrap_or("Untitled")),
            x: value("x"),
            y: value("y"),
            width: value("width"),
            height: value("height"),
        }
    }
}

#[allow(
    clippy::cast_precision_loss,
    reason = "a pixel threshold is far below 2^52"
)]
fn threshold_px(threshold: u64) -> f64 {
    threshold as f64
}

/// The SDK's `int()` of a pixel measure.
fn px(value: f64) -> i64 {
    #[allow(
        clippy::cast_possible_truncation,
        reason = "Python's int() truncates a measure the same way; Figma's are far below i64"
    )]
    let truncated = value as i64;
    truncated
}

/// The frame as indexed: the SDK's header line, then its text by role.
fn render(frame: &Frame, roles: &Roles, limit: u64) -> String {
    let mut out = format!(
        "FRAME: {} [{},{} {}x{}] #{}",
        frame.name,
        px(frame.x),
        px(frame.y),
        px(frame.width),
        px(frame.height),
        frame.id
    );
    let sections: [(&str, &Vec<String>); 6] = [
        ("Headings", &roles.headings),
        ("Buttons", &roles.buttons),
        ("Labels", &roles.labels),
        ("Body", &roles.body),
        ("Errors", &roles.errors),
        ("Placeholders", &roles.placeholders),
    ];
    for (title, items) in sections {
        if items.is_empty() {
            continue;
        }
        let line = format!(
            "\n  {title}: {}",
            items
                .iter()
                .map(|item| one_line(item))
                .collect::<Vec<_>>()
                .join(" | ")
        );
        out.push_str(&line);
    }
    truncate(&out, limit)
}

/// `text` cut at a character boundary to at most `limit` bytes.
fn truncate(text: &str, limit: u64) -> String {
    let limit = usize::try_from(limit).unwrap_or(usize::MAX);
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut end = limit;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
}

/// Line breaks and control characters as spaces.
fn one_line(text: &str) -> String {
    text.chars()
        .map(|character| {
            if character.is_control() {
                ' '
            } else {
                character
            }
        })
        .collect::<String>()
        .trim()
        .to_owned()
}

/// A node id as one key segment, reversibly: `:` is `-` and `;` is `_`
/// (Figma's own separators, so ordinary ids read `1-2` and `I1-2_3-4`), and
/// a literal `-`, `_` or `~`, or any other byte, is `~` and its two hex
/// digits. Different ids never share a segment (`I1:2;3:4` is `I1-2_3-4`,
/// `I1:2_3:4` is `I1-2~5F3-4`). `None` for an id that is empty or longer
/// than 128 bytes.
fn key_segment(node_id: &str) -> Option<String> {
    if node_id.is_empty() || node_id.len() > MAX_ID_BYTES {
        return None;
    }
    let mut segment = String::with_capacity(node_id.len());
    for byte in node_id.bytes() {
        match byte {
            b':' => segment.push('-'),
            b';' => segment.push('_'),
            other if other.is_ascii_alphanumeric() => segment.push(char::from(other)),
            other => {
                let _ = write!(segment, "~{other:02X}");
            }
        }
    }
    Some(segment)
}

/// A string or numeric id as text.
fn scalar_id(value: Option<&Value>) -> Option<String> {
    match value {
        Some(Value::String(id)) if !id.is_empty() => Some(id.clone()),
        Some(Value::Number(id)) => Some(id.to_string()),
        _ => None,
    }
}

/// The id of `root` and of every node below it, within the walks' bounds.
fn collect_ids(root: &Value, out: &mut BTreeSet<String>) {
    let mut stack = vec![(root, 0_usize)];
    let mut visited = 0_usize;
    while let Some((node, depth)) = stack.pop() {
        visited += 1;
        if visited > MAX_FRAME_NODES.saturating_mul(10) {
            return;
        }
        if let Some(id) = node.get("id").and_then(Value::as_str) {
            out.insert(id.to_owned());
        }
        if depth < text::MAX_TREE_DEPTH {
            stack.extend(text::children(node).iter().map(|child| (child, depth + 1)));
        }
    }
}

fn text_field<'a>(value: &'a Value, name: &str) -> Option<&'a str> {
    value
        .get(name)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
}

fn array<'a>(body: &'a Value, name: &'static str) -> Result<&'a [Value], Failure> {
    body.get(name)
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .ok_or(Failure::InvalidResponse("an answer lacks its list"))
}

/// A failure that splitting the request may cure: Figma times out on,
/// refuses as too large, or answers with more than the response cap for, a
/// big batch of pages.
fn retriable(failure: &Failure) -> bool {
    match failure {
        Failure::Status(status) => {
            matches!(status.as_u16(), 400 | 408 | 413 | 500 | 502 | 503 | 504)
        }
        // A body over the response cap is a batch too big: its halves may fit.
        Failure::Transport(_) | Failure::TooLarge(_) => true,
        _ => false,
    }
}
