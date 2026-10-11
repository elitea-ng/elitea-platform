//! `ado_wiki` operations (`tools/ado/wiki/ado_wrapper.py`, `azure-devops`
//! `WikiClient`/`CoreClient` v7.0).

use reqwest::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::families::ado::client::{
    AdoBody, AdoClient, AdoClientError, AdoClientErrorCode, AdoRequest, AdoScope, bounded_output,
    response_shape_failure,
};
use crate::toolkits::families::ado::config::AdoToolkitConfig;
use crate::toolkits::families::ado::format::as_dict;

const WIKI_API: &str = "7.0";
const PROJECTS_API: &str = "7.0";
/// The provider text the SDK retries a page write or move on, without the
/// version descriptor.
const UNRESOLVED_VERSION: &str = "either is invalid or does not exist";
const MISSING_WIKI: &str = "Wiki identifier must be provided either as a parameter or configured as default in toolkit settings. Please either pass 'wiki_identified' parameter or configure 'default_wiki_identifier' in the toolkit.";

/// How `get_wiki_page` addresses the page.
pub(crate) enum PageAddress<'a> {
    Id(u64),
    Path(&'a str),
}

pub(crate) struct GetWikiPage<'a> {
    pub(crate) wiki_identified: Option<&'a str>,
    pub(crate) address: Option<PageAddress<'a>>,
    pub(crate) include_content: bool,
    pub(crate) recursion_level: &'a str,
}

pub(crate) struct ModifyWikiPage<'a> {
    pub(crate) wiki_identified: Option<&'a str>,
    pub(crate) page_name: &'a str,
    pub(crate) page_content: &'a str,
    pub(crate) version_identifier: &'a str,
    pub(crate) version_type: &'a str,
    pub(crate) expanded: bool,
}

pub(crate) struct RenameWikiPage<'a> {
    pub(crate) wiki_identified: Option<&'a str>,
    pub(crate) old_page_name: &'a str,
    pub(crate) new_page_name: &'a str,
    pub(crate) version_identifier: &'a str,
    pub(crate) version_type: &'a str,
}

pub(crate) struct AdoWikiClient {
    ado: AdoClient,
    default_wiki: Option<Box<str>>,
}

impl AdoWikiClient {
    pub(crate) fn new(config: AdoToolkitConfig) -> Result<Self, AdoClientError> {
        let (connection, settings) = config.into_parts();
        Ok(Self::with_client(
            crate::toolkits::families::ado::client::client(connection)?,
            settings.default_wiki_identifier,
        ))
    }

    pub(crate) fn with_client(ado: AdoClient, default_wiki: Option<Box<str>>) -> Self {
        Self { ado, default_wiki }
    }

    pub(crate) fn default_wiki(&self) -> Option<&str> {
        self.default_wiki.as_deref()
    }

    /// `_resolve_wiki_identifier`: the argument, else the configured default.
    fn resolve<'a>(&'a self, wiki_identified: Option<&'a str>) -> Option<&'a str> {
        wiki_identified
            .filter(|wiki| !wiki.is_empty())
            .or(self.default_wiki.as_deref())
    }

    pub(crate) async fn get_wiki(
        &self,
        wiki_identified: Option<&str>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(wiki_identified) else {
            return Ok(Value::String(format!(
                "Error during the attempt to extract wiki: {MISSING_WIKI}"
            )));
        };
        let segments = ["wiki", "wikis", wiki];
        let value = self
            .ado
            .json(AdoRequest::get(&segments, WIKI_API), false)
            .await?;
        bounded_output(format_wiki(&value))
    }

    /// `get_wiki_page_by_path` / `get_wiki_page_by_id`: the page content.
    ///
    /// The SDK then describes images with the toolkit LLM when
    /// `process_images` is set; that LLM is not lent to toolkits here, so the
    /// content is returned with its image references unchanged.
    pub(crate) async fn page_content(
        &self,
        wiki_identified: Option<&str>,
        address: PageAddress<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(wiki_identified) else {
            return Ok(Value::String(format!(
                "Error during the attempt to extract wiki page: {MISSING_WIKI}"
            )));
        };
        let response = self.page(wiki, &address, Some(true), None).await?;
        let content = response.page.get("content").cloned().unwrap_or(Value::Null);
        bounded_output(content)
    }

    async fn page(
        &self,
        wiki: &str,
        address: &PageAddress<'_>,
        include_content: Option<bool>,
        recursion_level: Option<&str>,
    ) -> Result<PageResponse, AdoClientError> {
        let id;
        let mut segments = vec!["wiki", "wikis", wiki, "pages"];
        let mut request_path = None;
        match address {
            PageAddress::Id(page_id) => {
                id = page_id.to_string();
                segments.push(id.as_str());
            }
            PageAddress::Path(path) => request_path = Some(*path),
        }
        let request = AdoRequest::get(&segments, WIKI_API)
            .query_opt("path", request_path)
            .query_opt("recursionLevel", recursion_level)
            .query_opt(
                "includeContent",
                include_content.map(|value| value.to_string()),
            );
        let response = self.ado.send(request, false).await?;
        let etag = response.etag().map(ToOwned::to_owned);
        match response.into_body() {
            AdoBody::Json(page) => Ok(PageResponse { etag, page }),
            AdoBody::Empty | AdoBody::Text(_) => Err(response_shape_failure(false)),
        }
    }

    /// `get_wiki_page`: comprehensive metadata, optionally with content.
    pub(crate) async fn get_wiki_page(
        &self,
        request: GetWikiPage<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(request.wiki_identified) else {
            return Ok(Value::String(format!(
                "Unexpected error during wiki page retrieval: {MISSING_WIKI}"
            )));
        };
        let Some(address) = request.address else {
            return Ok(Value::String(
                "At least one of 'page_path' or 'page_id' must be provided".to_owned(),
            ));
        };
        match self
            .page(
                wiki,
                &address,
                Some(request.include_content),
                Some(request.recursion_level),
            )
            .await
        {
            Ok(response) => bounded_output(format_page(&response, true, request.include_content)),
            Err(error) if error.code() == AdoClientErrorCode::NotFound => {
                let identifier = match address {
                    PageAddress::Id(id) => format!("ID {id}"),
                    PageAddress::Path(path) => format!("path '{path}'"),
                };
                let wiki = request
                    .wiki_identified
                    .filter(|wiki| !wiki.is_empty())
                    .or(self.default_wiki.as_deref())
                    .unwrap_or("unknown");
                Ok(Value::String(format!(
                    "Page {identifier} not found in wiki '{wiki}'. Please verify the page exists and the identifier is correct."
                )))
            }
            Err(error) => Err(error),
        }
    }

    pub(crate) async fn delete_page(
        &self,
        wiki_identified: Option<&str>,
        address: PageAddress<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(wiki_identified) else {
            return Ok(Value::String(format!(
                "Unable to delete wiki page: {MISSING_WIKI}"
            )));
        };
        let id;
        let mut segments = vec!["wiki", "wikis", wiki, "pages"];
        let mut request_path = None;
        match &address {
            PageAddress::Id(page_id) => {
                id = page_id.to_string();
                segments.push(id.as_str());
            }
            PageAddress::Path(path) => request_path = Some(*path),
        }
        self.ado
            .send(
                AdoRequest::new(Method::DELETE, AdoScope::Project, &segments, WIKI_API)
                    .query_opt("path", request_path),
                true,
            )
            .await?;
        Ok(Value::String(match address {
            PageAddress::Path(path) => format!("Page '{path}' in wiki '{wiki}' has been deleted"),
            PageAddress::Id(id) => {
                format!("Page with id '{id}' in wiki '{wiki}' has been deleted")
            }
        }))
    }

    /// `rename_wiki_page`: a page move, retried without the version
    /// descriptor when the provider cannot resolve that version.
    pub(crate) async fn rename_wiki_page(
        &self,
        request: RenameWikiPage<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(request.wiki_identified) else {
            return Ok(Value::String(format!(
                "Unable to rename wiki page: {MISSING_WIKI}"
            )));
        };
        let segments = ["wiki", "wikis", wiki, "pagemoves"];
        let comment = format!(
            "Page rename from '{}' to '{}'",
            request.old_page_name, request.new_page_name
        );
        let body = json!({"newPath":request.new_page_name,"path":request.old_page_name});
        let first = self
            .ado
            .send(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, WIKI_API)
                    .query("comment", comment.clone())
                    .query("versionDescriptor.versionType", request.version_type)
                    .query("versionDescriptor.version", request.version_identifier)
                    .json(body.clone()),
                true,
            )
            .await;
        let response = match first {
            Err(error) if error.provider_message_contains(UNRESOLVED_VERSION) => {
                self.ado
                    .send(
                        AdoRequest::new(Method::POST, AdoScope::Project, &segments, WIKI_API)
                            .query("comment", comment)
                            .json(body),
                        true,
                    )
                    .await?
            }
            other => other?,
        };
        let etag = response.etag().map(ToOwned::to_owned);
        let AdoBody::Json(moved) = response.into_body() else {
            return Err(response_shape_failure(true));
        };
        bounded_output(json!({"eTag":etag,"page_move":as_dict(&moved)}))
    }

    /// `modify_wiki_page`: create the wiki when it does not exist, read the
    /// page's eTag (a missing page is created), then write the content.
    pub(crate) async fn modify_wiki_page(
        &self,
        request: ModifyWikiPage<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(wiki) = self.resolve(request.wiki_identified) else {
            return Ok(Value::String(format!(
                "Unable to modify wiki page: {MISSING_WIKI}"
            )));
        };
        let wikis_segments = ["wiki", "wikis"];
        let wikis = self
            .ado
            .collection(AdoRequest::get(&wikis_segments, WIKI_API))
            .await?;
        // The SDK compares names only, so a wiki addressed by id would be
        // "created" again; an id match counts as existing here.
        let exists = wikis.iter().any(|existing| {
            existing.get("name").and_then(Value::as_str) == Some(wiki)
                || existing.get("id").and_then(Value::as_str) == Some(wiki)
        });
        if !exists {
            let projects_segments = ["projects"];
            let projects = self
                .ado
                .collection(AdoRequest::get(&projects_segments, PROJECTS_API).organization())
                .await?;
            let Some(project_id) = projects
                .iter()
                .find(|project| {
                    project.get("name").and_then(Value::as_str) == Some(self.ado.project())
                })
                .and_then(|project| project.get("id").and_then(Value::as_str))
            else {
                return Ok(Value::String("Project ID has not been found.".to_owned()));
            };
            self.ado
                .json(
                    AdoRequest::new(Method::POST, AdoScope::Project, &wikis_segments, WIKI_API)
                        .json(json!({"name":wiki,"projectId":project_id})),
                    true,
                )
                .await?;
        }
        let version = match self
            .page(wiki, &PageAddress::Path(request.page_name), None, None)
            .await
        {
            Ok(page) => page.etag,
            Err(error) if error.code() == AdoClientErrorCode::NotFound => None,
            Err(error) => return Err(error),
        };
        let segments = ["wiki", "wikis", wiki, "pages"];
        let body = json!({"content":request.page_content});
        let first = self
            .ado
            .send(
                AdoRequest::new(Method::PUT, AdoScope::Project, &segments, WIKI_API)
                    .query("path", request.page_name)
                    .query("versionDescriptor.versionType", request.version_type)
                    .query("versionDescriptor.version", request.version_identifier)
                    .if_match(version.as_deref())
                    .json(body.clone()),
                true,
            )
            .await;
        let response = match first {
            Err(error) if error.provider_message_contains(UNRESOLVED_VERSION) => {
                self.ado
                    .send(
                        AdoRequest::new(Method::PUT, AdoScope::Project, &segments, WIKI_API)
                            .query("path", request.page_name)
                            .if_match(version.as_deref())
                            .json(body),
                        true,
                    )
                    .await?
            }
            other => other?,
        };
        let etag = response.etag().map(ToOwned::to_owned);
        let AdoBody::Json(page) = response.into_body() else {
            return Err(response_shape_failure(true));
        };
        bounded_output(format_page(
            &PageResponse { etag, page },
            request.expanded,
            false,
        ))
    }
}

struct PageResponse {
    etag: Option<String>,
    page: Value,
}

/// `_format_wiki_response`.
fn format_wiki(wiki: &Value) -> Value {
    let field = |name: &str| wiki.get(name).cloned().unwrap_or(Value::Null);
    let mut result = Map::new();
    result.insert("id".to_owned(), field("id"));
    result.insert("name".to_owned(), field("name"));
    result.insert("type".to_owned(), field("type"));
    result.insert("url".to_owned(), field("url"));
    result.insert("project_id".to_owned(), field("projectId"));
    result.insert("repository_id".to_owned(), field("repositoryId"));
    result.insert("mapped_path".to_owned(), field("mappedPath"));
    if let Some(remote) = wiki
        .get("remoteUrl")
        .filter(|remote| remote.as_str().is_some_and(|remote| !remote.is_empty()))
    {
        result.insert("remote_url".to_owned(), remote.clone());
    }
    if let Some(versions) = wiki
        .get("versions")
        .and_then(Value::as_array)
        .filter(|versions| !versions.is_empty())
    {
        result.insert(
            "versions".to_owned(),
            Value::Array(
                versions
                    .iter()
                    .map(|version| {
                        json!({
                            "version":version.get("version").cloned().unwrap_or(Value::Null),
                            "version_type":version.get("versionType").cloned().unwrap_or(Value::Null),
                            "version_options":version.get("versionOptions").cloned().unwrap_or(Value::Null)
                        })
                    })
                    .collect(),
            ),
        );
    }
    Value::Object(result)
}

/// `_format_wiki_page_response`.
fn format_page(response: &PageResponse, expanded: bool, include_content: bool) -> Value {
    let page = &response.page;
    let field = |value: &Value, name: &str| value.get(name).cloned().unwrap_or(Value::Null);
    let etag = response.etag.clone().map_or(Value::Null, Value::String);
    if !expanded {
        return json!({"eTag":etag,"id":field(page, "id"),"page":field(page, "url")});
    }
    let sub_pages = page
        .get("subPages")
        .and_then(Value::as_array)
        .map(|pages| {
            pages
                .iter()
                .map(|sub_page| {
                    let mut entry = json!({
                        "id":field(sub_page, "id"),
                        "path":field(sub_page, "path"),
                        "order":field(sub_page, "order"),
                        "git_item_path":field(sub_page, "gitItemPath"),
                        "url":field(sub_page, "url"),
                        "remote_url":field(sub_page, "remoteUrl")
                    });
                    if let Some(nested) = sub_page
                        .get("subPages")
                        .and_then(Value::as_array)
                        .filter(|nested| !nested.is_empty())
                        && let Some(object) = entry.as_object_mut()
                    {
                        object.insert(
                            "sub_pages".to_owned(),
                            Value::Array(
                                nested
                                    .iter()
                                    .map(|nested| {
                                        json!({
                                            "id":field(nested, "id"),
                                            "path":field(nested, "path"),
                                            "order":field(nested, "order")
                                        })
                                    })
                                    .collect(),
                            ),
                        );
                    }
                    entry
                })
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let mut detail = json!({
        "id":field(page, "id"),
        "path":field(page, "path"),
        "git_item_path":field(page, "gitItemPath"),
        "remote_url":field(page, "remoteUrl"),
        "url":field(page, "url"),
        "order":field(page, "order"),
        "is_parent_page":field(page, "isParentPage"),
        "is_non_conformant":field(page, "isNonConformant"),
        "sub_pages":sub_pages
    });
    if include_content && let Some(object) = detail.as_object_mut() {
        object.insert("content".to_owned(), field(page, "content"));
    }
    json!({"eTag":etag,"page":detail})
}
