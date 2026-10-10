//! `ado_boards` operations over the shared ADO client
//! (`tools/ado/work_item/ado_wrapper.py::AzureDevOpsApiWrapper`).

use std::collections::BTreeMap;
use std::fmt::Write as _;

use reqwest::Method;
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;

use crate::toolkits::families::ado::client::{
    AdoClient, AdoClientError, AdoRequest, AdoScope, bounded_output, invalid_input,
    invalid_response, resource_exhausted,
};
use crate::toolkits::families::ado::config::AdoToolkitConfig;
use crate::toolkits::families::ado::format::{as_dict, python_json_string};
use crate::toolkits::families::ado::work_items::{
    DEFAULT_SEARCH_FIELDS, FieldDefinition, WORK_ITEMS_API, create_work_item,
    format_work_item_type_fields, get_work_item, id_text, transform_work_item, update_work_item,
    work_item_result, work_item_type_fields,
};
use crate::toolkits::families::python_repr::repr as python_repr;

const WIQL_API: &str = "7.1-preview.2";
const RELATION_TYPES_API: &str = "7.1-preview.2";
const COMMENTS_API: &str = "7.1-preview.4";
const PROJECTS_API: &str = "7.1-preview.4";
const WIKIS_API: &str = "7.1-preview.2";
const WIKI_PAGES_API: &str = "7.1-preview.1";
/// The SDK fetches every matched work item one by one; this bounds that loop.
pub(crate) const MAX_SEARCH_ITEMS: usize = 200;
/// The SDK pages comments three at a time after the first page.
const MAX_COMMENT_PAGES: usize = 256;

pub(crate) struct SearchWorkItems<'a> {
    pub(crate) query: &'a str,
    pub(crate) limit: Option<i64>,
    pub(crate) fields: Option<Vec<String>>,
}

pub(crate) struct GetComments<'a> {
    pub(crate) work_item_id: u64,
    pub(crate) limit_total: Option<i64>,
    pub(crate) include_deleted: bool,
    pub(crate) expand: Option<&'a str>,
    pub(crate) order: Option<&'a str>,
    pub(crate) process_images: bool,
}

/// One claim-scoped boards client with the SDK's per-instance caches.
pub(crate) struct AdoBoardsClient {
    ado: AdoClient,
    limit: i64,
    relation_types: Mutex<Option<BTreeMap<String, String>>>,
    type_fields: Mutex<BTreeMap<String, Vec<FieldDefinition>>>,
}

impl AdoBoardsClient {
    pub(crate) fn new(config: AdoToolkitConfig) -> Result<Self, AdoClientError> {
        let (connection, settings) = config.into_parts();
        Ok(Self::with_client(
            AdoClient::new(connection)?,
            settings.limit,
        ))
    }

    pub(crate) fn with_client(ado: AdoClient, limit: i64) -> Self {
        Self {
            ado,
            limit,
            relation_types: Mutex::new(None),
            type_fields: Mutex::new(BTreeMap::new()),
        }
    }

    /// `search_work_items`: WIQL, then each matched item with the requested
    /// (or default) fields.
    pub(crate) async fn search_work_items(
        &self,
        request: SearchWorkItems<'_>,
    ) -> Result<Value, AdoClientError> {
        let limit = match request.limit {
            None | Some(0) => self.limit,
            Some(limit) => limit,
        };
        let segments = ["wit", "wiql"];
        let wiql = AdoRequest::new(Method::POST, AdoScope::Project, &segments, WIQL_API)
            .query_opt("$top", (limit >= 0).then(|| limit.to_string()))
            .json(json!({"query":request.query}));
        let response = self.ado.json(wiql, false).await?;
        let work_items = response
            .get("workItems")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        if work_items.is_empty() {
            return Ok(Value::String("No work items found.".to_owned()));
        }
        if work_items.len() > MAX_SEARCH_ITEMS {
            return Ok(Value::String(format!(
                "The WIQL query matched {} work items, more than the {MAX_SEARCH_ITEMS} one search returns. Narrow the query or pass a limit of at most {MAX_SEARCH_ITEMS}.",
                work_items.len()
            )));
        }
        let fields = request
            .fields
            .unwrap_or_else(|| {
                DEFAULT_SEARCH_FIELDS
                    .iter()
                    .map(|field| (*field).to_owned())
                    .collect()
            })
            .into_iter()
            .filter(|field| !field.contains("System.Id") && !field.contains("System.WorkItemType"))
            .collect::<Vec<_>>();
        let mut parsed = Vec::with_capacity(work_items.len());
        for item in &work_items {
            let id = item
                .get("id")
                .and_then(Value::as_u64)
                .ok_or_else(invalid_response)?;
            let raw = get_work_item(
                &self.ado,
                id,
                (!fields.is_empty()).then_some(fields.as_slice()),
                None,
                None,
            )
            .await?;
            let mut result = Map::new();
            let id_value = raw.get("id").cloned().ok_or_else(invalid_response)?;
            result.insert(
                "url".to_owned(),
                Value::String(format!(
                    "{}/_workitems/edit/{}",
                    self.ado.organization_text(),
                    id_text(&id_value)
                )),
            );
            result.insert("id".to_owned(), id_value);
            let values = raw.get("fields").and_then(Value::as_object);
            for field in &fields {
                result.insert(
                    field.clone(),
                    values
                        .and_then(|values| values.get(field))
                        .cloned()
                        .unwrap_or_else(|| Value::String("N/A".to_owned())),
                );
            }
            parsed.push(Value::Object(result));
        }
        bounded_output(Value::Array(parsed))
    }

    pub(crate) async fn create_work_item(
        &self,
        work_item_json: &str,
        work_item_type: &str,
    ) -> Result<Value, AdoClientError> {
        let document = match transform_work_item(work_item_json) {
            Ok(document) => document,
            Err(message) => {
                return Ok(Value::String(format!(
                    "Issues during attempt to parse work_item_json: {message}"
                )));
            }
        };
        match create_work_item(&self.ado, document, work_item_type).await {
            Ok(created) => {
                let id = created.get("id").cloned().ok_or_else(invalid_response)?;
                let url = created
                    .get("url")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
                Ok(json!({
                    "id":id,
                    "message":format!("Work item {} created successfully. View it at {url}.", id_text(&id))
                }))
            }
            Err(error) => match error.model_visible_message() {
                Some(message) if message.contains("unknown value") => Ok(Value::String(format!(
                    "Unable to create work item due to incorrect assignee: {message}"
                ))),
                Some(message) => Ok(Value::String(format!(
                    "Error creating work item: {message}"
                ))),
                None => Err(error),
            },
        }
    }

    pub(crate) async fn update_work_item(
        &self,
        id: &str,
        work_item_json: &str,
    ) -> Result<Value, AdoClientError> {
        let numeric = id.trim().parse::<u64>().map_err(|_| invalid_input())?;
        let document = match transform_work_item(work_item_json) {
            Ok(document) => document,
            Err(message) => {
                return Ok(Value::String(format!(
                    "Issues during attempt to parse work_item_json: {message}"
                )));
            }
        };
        match update_work_item(&self.ado, &numeric.to_string(), document).await {
            Ok(updated) => {
                let id = updated.get("id").cloned().ok_or_else(invalid_response)?;
                Ok(Value::String(format!(
                    "Work item ({}) was updated.",
                    id_text(&id)
                )))
            }
            Err(error) => match error.model_visible_message() {
                Some(message) => Ok(Value::String(format!(
                    "Issues during attempt to parse work_item_json: {message}"
                ))),
                None => Err(error),
            },
        }
    }

    pub(crate) async fn delete_work_item(&self, id: u64) -> Result<Value, AdoClientError> {
        let id_text = id.to_string();
        let segments = ["wit", "workitems", id_text.as_str()];
        self.ado
            .send(
                AdoRequest::new(Method::DELETE, AdoScope::Project, &segments, WORK_ITEMS_API),
                true,
            )
            .await?;
        Ok(Value::String(format!(
            "Work item {id} was successfully deleted."
        )))
    }

    pub(crate) async fn get_work_item(
        &self,
        id: u64,
        fields: Option<&[String]>,
        as_of: Option<&str>,
        expand: Option<&str>,
    ) -> Result<Value, AdoClientError> {
        let raw = get_work_item(&self.ado, id, fields, as_of, expand).await?;
        bounded_output(work_item_result(
            self.ado.organization_text(),
            &raw,
            fields,
            expand,
        )?)
    }

    /// `get_relation_types`: `{name: referenceName}`, read once per client.
    pub(crate) async fn relation_types(&self) -> Result<BTreeMap<String, String>, AdoClientError> {
        let mut cached = self.relation_types.lock().await;
        if let Some(types) = cached.as_ref() {
            return Ok(types.clone());
        }
        let segments = ["wit", "workitemrelationtypes"];
        let relations = self
            .ado
            .collection(AdoRequest::get(&segments, RELATION_TYPES_API).organization())
            .await?;
        let mut types = BTreeMap::new();
        for relation in relations {
            let (Some(name), Some(reference)) = (
                relation.get("name").and_then(Value::as_str),
                relation.get("referenceName").and_then(Value::as_str),
            ) else {
                return Err(invalid_response());
            };
            types.insert(name.to_owned(), reference.to_owned());
        }
        *cached = Some(types.clone());
        Ok(types)
    }

    pub(crate) async fn link_work_items(
        &self,
        source_id: u64,
        target_id: u64,
        link_type: &str,
        attributes: Option<&Map<String, Value>>,
    ) -> Result<Value, AdoClientError> {
        let types = self.relation_types().await?;
        if !types.values().any(|reference| reference == link_type) {
            let listed = Value::Object(
                types
                    .iter()
                    .map(|(name, reference)| (name.clone(), Value::String(reference.clone())))
                    .collect(),
            );
            return Ok(Value::String(format!(
                "Link type is incorrect. You have to use proper relation's reference name NOT relation's name: {}",
                python_repr(&listed)
            )));
        }
        let mut relation = Map::new();
        relation.insert("rel".to_owned(), Value::String(link_type.to_owned()));
        relation.insert(
            "url".to_owned(),
            Value::String(format!(
                "{}/_apis/wit/workItems/{target_id}",
                self.ado.organization_text()
            )),
        );
        if let Some(attributes) = attributes.filter(|attributes| !attributes.is_empty()) {
            relation.insert("attributes".to_owned(), Value::Object(attributes.clone()));
        }
        let source = source_id.to_string();
        // The SDK updates the source without a project route.
        let segments = ["wit", "workitems", source.as_str()];
        self.ado
            .json(
                AdoRequest::new(
                    Method::PATCH,
                    AdoScope::Organization,
                    &segments,
                    WORK_ITEMS_API,
                )
                .json_patch(json!([{"op":"add","path":"/relations/-","value":relation}])),
                true,
            )
            .await?;
        Ok(Value::String(format!(
            "Work item {source_id} linked to {target_id} with link type {link_type}"
        )))
    }

    /// `get_comments`: the first page at the configured limit, then
    /// continuation pages of three until `limit_total` (or the configured
    /// limit) comments are collected.
    pub(crate) async fn get_comments(
        &self,
        request: GetComments<'_>,
    ) -> Result<Value, AdoClientError> {
        let limit_portion = self.limit;
        let limit_all = match request.limit_total {
            None | Some(0) => self.limit,
            Some(limit) => limit,
        };
        // Markdown comments carry images in `text` and HTML ones in
        // `renderedText`; the SDK asks for the rendered form when images are
        // to be processed. Image descriptions need the toolkit LLM, which this
        // runtime does not lend to toolkits, so the comments come back as the
        // SDK returns them when no LLM is configured.
        let expand = if request.process_images && matches!(request.expand, None | Some("none")) {
            Some("renderedText")
        } else {
            request.expand
        };
        let work_item = request.work_item_id.to_string();
        let segments = ["wit", "workItems", work_item.as_str(), "comments"];
        let mut comments: Vec<Value> = Vec::new();
        let mut continuation: Option<String> = None;
        let mut top = limit_portion;
        for _ in 0..MAX_COMMENT_PAGES {
            let page = self
                .ado
                .json(
                    AdoRequest::get(&segments, COMMENTS_API)
                        .query("$top", top.to_string())
                        .query_opt("continuationToken", continuation.take())
                        .query("includeDeleted", request.include_deleted.to_string())
                        .query_opt("$expand", expand)
                        .query_opt("order", request.order),
                    false,
                )
                .await?;
            if let Some(values) = page.get("comments").and_then(Value::as_array) {
                comments.extend(values.iter().map(as_dict));
            }
            let token = page
                .get("continuationToken")
                .and_then(Value::as_str)
                .filter(|token| !token.is_empty());
            let reached = i64::try_from(comments.len()).unwrap_or(i64::MAX) >= limit_all;
            match token {
                Some(token) if !reached => {
                    continuation = Some(token.to_owned());
                    top = 3;
                }
                _ => {
                    if let Ok(limit) = usize::try_from(limit_all) {
                        comments.truncate(limit);
                    } else {
                        // `comments_all[:negative]` drops that many from the end.
                        let drop = usize::try_from(limit_all.unsigned_abs()).unwrap_or(usize::MAX);
                        comments.truncate(comments.len().saturating_sub(drop));
                    }
                    return bounded_output(Value::Array(comments));
                }
            }
        }
        Err(resource_exhausted())
    }

    /// `_get_wiki_artifact_uri`: project id, wiki id and the page's canonical
    /// path, URL-encoded into a `vstfs:///Wiki/WikiPage/` artifact link.
    async fn wiki_artifact_uri(
        &self,
        wiki_identified: &str,
        page_name: &str,
    ) -> Result<String, AdoClientError> {
        let project_segments = ["projects", self.ado.project()];
        let project = self
            .ado
            .json(
                AdoRequest::get(&project_segments, PROJECTS_API).organization(),
                false,
            )
            .await?;
        let project_id = project
            .get("id")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        let wiki_segments = ["wiki", "wikis", wiki_identified];
        let wiki = self
            .ado
            .json(AdoRequest::get(&wiki_segments, WIKIS_API), false)
            .await?;
        let wiki_id = wiki
            .get("id")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        let page_segments = ["wiki", "wikis", wiki_identified, "pages"];
        let page = self
            .ado
            .json(
                AdoRequest::get(&page_segments, WIKI_PAGES_API).query("path", page_name),
                false,
            )
            .await?;
        let page_path = page
            .get("path")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        Ok(format!(
            "vstfs:///Wiki/WikiPage/{}",
            quote_all(&format!("{project_id}/{wiki_id}{page_path}"))
        ))
    }

    pub(crate) async fn link_work_items_to_wiki_page(
        &self,
        work_item_ids: &[u64],
        wiki_identified: &str,
        page_name: &str,
    ) -> Result<Value, AdoClientError> {
        if work_item_ids.is_empty() {
            return Ok(Value::String(
                "No work item IDs provided. No links created.".to_owned(),
            ));
        }
        let artifact_uri = self.wiki_artifact_uri(wiki_identified, page_name).await?;
        let document = json!([{
            "op":0,
            "path":"/relations/-",
            "value":{"rel":"ArtifactLink","url":artifact_uri,"attributes":{"name":"Wiki Page"}}
        }]);
        let mut linked = Vec::new();
        let mut failed = Vec::new();
        for id in work_item_ids {
            let id_text = id.to_string();
            let segments = ["wit", "workitems", id_text.as_str()];
            match self
                .ado
                .json(
                    AdoRequest::new(Method::PATCH, AdoScope::Project, &segments, WORK_ITEMS_API)
                        .json_patch(document.clone()),
                    true,
                )
                .await
            {
                Ok(_) => linked.push(id_text),
                Err(error) => failed.push((id_text, error.to_string())),
            }
        }
        let mut response = String::new();
        if !linked.is_empty() {
            let _ = writeln!(
                response,
                "Successfully linked work items [{}] to wiki page '{page_name}' in wiki '{wiki_identified}'.",
                linked.join(", ")
            );
        }
        if !failed.is_empty() {
            let _ = write!(
                response,
                "Failed to link work items: {}",
                python_json_object(&failed)
            );
        }
        Ok(Value::String(response.trim().to_owned()))
    }

    pub(crate) async fn unlink_work_items_from_wiki_page(
        &self,
        work_item_ids: &[u64],
        wiki_identified: &str,
        page_name: &str,
    ) -> Result<Value, AdoClientError> {
        if work_item_ids.is_empty() {
            return Ok(Value::String(
                "No work item IDs provided. No links removed.".to_owned(),
            ));
        }
        let artifact_uri = self.wiki_artifact_uri(wiki_identified, page_name).await?;
        let mut unlinked = Vec::new();
        let mut not_found = Vec::new();
        let mut failed = Vec::new();
        for id in work_item_ids {
            let id_text = id.to_string();
            let item = match get_work_item(&self.ado, *id, None, None, Some("Relations")).await {
                Ok(item) => item,
                Err(error) => {
                    failed.push((id_text, error.to_string()));
                    continue;
                }
            };
            let index = item
                .get("relations")
                .and_then(Value::as_array)
                .and_then(|relations| {
                    relations.iter().position(|relation| {
                        relation.get("rel").and_then(Value::as_str) == Some("ArtifactLink")
                            && relation.get("url").and_then(Value::as_str)
                                == Some(artifact_uri.as_str())
                    })
                });
            let Some(index) = index else {
                not_found.push(id_text);
                continue;
            };
            let segments = ["wit", "workitems", id_text.as_str()];
            match self
                .ado
                .json(
                    AdoRequest::new(Method::PATCH, AdoScope::Project, &segments, WORK_ITEMS_API)
                        .json_patch(json!([{"op":"remove","path":format!("/relations/{index}")}])),
                    true,
                )
                .await
            {
                Ok(_) => unlinked.push(id_text),
                Err(error) => failed.push((id_text, error.to_string())),
            }
        }
        let mut response = String::new();
        if !unlinked.is_empty() {
            let _ = writeln!(
                response,
                "Successfully unlinked work items [{}] from wiki page '{page_name}' in wiki '{wiki_identified}'.",
                unlinked.join(", ")
            );
        }
        if !not_found.is_empty() {
            let _ = writeln!(
                response,
                "No link to wiki page '{page_name}' found for work items [{}].",
                not_found.join(", ")
            );
        }
        if !failed.is_empty() {
            let _ = write!(
                response,
                "Failed to unlink work items: {}",
                python_json_object(&failed)
            );
        }
        let response = response.trim();
        Ok(Value::String(if response.is_empty() {
            "No action taken or required.".to_owned()
        } else {
            response.to_owned()
        }))
    }

    /// `get_work_item_type_fields`: cached per type until `force_refresh`.
    pub(crate) async fn get_work_item_type_fields(
        &self,
        work_item_type: &str,
        force_refresh: bool,
    ) -> Result<Value, AdoClientError> {
        let mut cache = self.type_fields.lock().await;
        if force_refresh || !cache.contains_key(work_item_type) {
            // The SDK reports a failed read as "unable to retrieve" text.
            // It also caches that empty result until a forced refresh; here a
            // failure is not cached, so the next call tries again.
            match work_item_type_fields(&self.ado, work_item_type).await {
                Ok(definitions) => {
                    cache.insert(work_item_type.to_owned(), definitions);
                }
                Err(_) => {
                    return Ok(Value::String(format_work_item_type_fields(
                        self.ado.project(),
                        work_item_type,
                        &[],
                    )));
                }
            }
        }
        let definitions = cache.get(work_item_type).cloned().unwrap_or_default();
        drop(cache);
        bounded_output(Value::String(format_work_item_type_fields(
            self.ado.project(),
            work_item_type,
            &definitions,
        )))
    }
}

/// Python `urllib.parse.quote(value, safe="")`.
pub(crate) fn quote_all(value: &str) -> String {
    let mut output = String::with_capacity(value.len() * 3);
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-' | b'~') {
            output.push(char::from(byte));
        } else {
            let _ = write!(output, "%{byte:02X}");
        }
    }
    output
}

/// `json.dumps({id: message})` with Python's default separators.
fn python_json_object(entries: &[(String, String)]) -> String {
    let mut output = String::from("{");
    for (index, (key, value)) in entries.iter().enumerate() {
        if index > 0 {
            output.push_str(", ");
        }
        output.push_str(&python_json_string(key));
        output.push_str(": ");
        output.push_str(&python_json_string(value));
    }
    output.push('}');
    output
}
