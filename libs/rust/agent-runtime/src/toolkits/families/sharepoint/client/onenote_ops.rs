//! `OneNote` of the site over Graph: creating notebooks, sections and pages,
//! patching and deleting pages, and one embedded resource's bytes.
//!
//! Every change is sent once on a transport that never retries; a change
//! whose answer was lost is an unknown outcome, never a failure to repeat.
//! These calls act as the signed-in person (a delegated token): Microsoft
//! retired app-only access to the `OneNote` Graph API (MC1011142), and the
//! family does not run on an app-only credential at all.

use super::{
    GraphCall, Map, Method, SharePointClient, SharePointClientError, Url, Value, content,
    graph_url, invalid_input, json, require_effect_success, resource_exhausted,
};

/// The most HTML a new or replaced page may carry.
pub(in crate::toolkits) const MAX_PAGE_HTML_BYTES: usize = 1_024 * 1_024;
/// The most bytes of one embedded resource the readers fetch.
pub(in crate::toolkits) const MAX_RESOURCE_BYTES: usize = 20 * 1_024 * 1_024;

impl SharePointClient {
    async fn onenote_url(&self, tail: &[&str]) -> Result<Url, SharePointClientError> {
        let mut segments = self.onenote_prefix().await?;
        segments.extend(tail.iter().map(|segment| (*segment).to_owned()));
        let refs = segments.iter().map(String::as_str).collect::<Vec<_>>();
        graph_url(&refs)
    }

    /// One change that creates something. Its 2xx status is read before its
    /// size or its body, so a confirmed change is never reported as a failure.
    async fn onenote_create(
        &self,
        tail: &[&str],
        content_type: &'static str,
        body: Vec<u8>,
        project: impl FnOnce(&Map<String, Value>) -> Value,
    ) -> Result<Value, SharePointClientError> {
        let url = self.onenote_url(tail).await?;
        let response = self
            .send(
                GraphCall::new(Method::POST, url, "application/json")
                    .body(content_type, body)
                    .tolerate_oversize(),
            )
            .await
            .map_err(SharePointClientError::for_effect)?;
        require_effect_success(response.status)?;
        Ok(content::confirmed_echo(&response, project))
    }

    pub(super) async fn onenote_create_notebook_impl(
        &self,
        display_name: &str,
    ) -> Result<Value, SharePointClientError> {
        let body = serde_json::to_vec(&json!({"displayName": display_name}))
            .map_err(|_| invalid_input())?;
        self.onenote_create(&["notebooks"], "application/json", body, |created| {
            json!({
                "id": created.get("id").cloned().unwrap_or(Value::Null),
                "displayName": created.get("displayName").cloned().unwrap_or(Value::Null),
                "createdDateTime": created.get("createdDateTime").cloned().unwrap_or(Value::Null),
                "webUrl": created
                    .get("links")
                    .and_then(|links| links.get("oneNoteWebUrl"))
                    .and_then(|link| link.get("href"))
                    .cloned()
                    .unwrap_or(Value::Null),
            })
        })
        .await
    }

    pub(super) async fn onenote_create_section_impl(
        &self,
        notebook_id: &str,
        display_name: &str,
    ) -> Result<Value, SharePointClientError> {
        let body = serde_json::to_vec(&json!({"displayName": display_name}))
            .map_err(|_| invalid_input())?;
        self.onenote_create(
            &["notebooks", notebook_id, "sections"],
            "application/json",
            body,
            |created| {
                json!({
                    "id": created.get("id").cloned().unwrap_or(Value::Null),
                    "displayName": created.get("displayName").cloned().unwrap_or(Value::Null),
                    "createdDateTime": created.get("createdDateTime").cloned().unwrap_or(Value::Null),
                    "pagesUrl": created.get("pagesUrl").cloned().unwrap_or(Value::Null),
                })
            },
        )
        .await
    }

    pub(super) async fn onenote_create_page_impl(
        &self,
        section_id: &str,
        html_content: &str,
    ) -> Result<Value, SharePointClientError> {
        if html_content.len() > MAX_PAGE_HTML_BYTES {
            return Err(resource_exhausted());
        }
        self.onenote_create(
            &["sections", section_id, "pages"],
            "text/html",
            html_content.as_bytes().to_vec(),
            |created| {
                json!({
                    "id": created.get("id").cloned().unwrap_or(Value::Null),
                    "title": created.get("title").cloned().unwrap_or(Value::Null),
                    "createdDateTime": created.get("createdDateTime").cloned().unwrap_or(Value::Null),
                    "webUrl": created.get("webUrl").cloned().unwrap_or(Value::Null),
                    "contentUrl": created.get("contentUrl").cloned().unwrap_or(Value::Null),
                })
            },
        )
        .await
    }

    pub(super) async fn onenote_update_page_impl(
        &self,
        page_id: &str,
        patch_commands: &[Value],
    ) -> Result<(), SharePointClientError> {
        let body = serde_json::to_vec(patch_commands).map_err(|_| invalid_input())?;
        if body.len() > MAX_PAGE_HTML_BYTES {
            return Err(resource_exhausted());
        }
        let url = self.onenote_url(&["pages", page_id, "content"]).await?;
        let response = self
            .send(
                GraphCall::new(Method::PATCH, url, "application/json")
                    .body("application/json", body)
                    .tolerate_oversize(),
            )
            .await
            .map_err(SharePointClientError::for_effect)?;
        require_effect_success(response.status)
    }

    pub(super) async fn onenote_delete_page_impl(
        &self,
        page_id: &str,
    ) -> Result<(), SharePointClientError> {
        let url = self.onenote_url(&["pages", page_id]).await?;
        let response = self
            .send(GraphCall::new(Method::DELETE, url, "application/json").tolerate_oversize())
            .await
            .map_err(SharePointClientError::for_effect)?;
        require_effect_success(response.status)
    }

    pub(super) async fn onenote_download_resource_impl(
        &self,
        resource_id: &str,
        max_bytes: usize,
    ) -> Result<Vec<u8>, SharePointClientError> {
        let url = self
            .onenote_url(&["resources", resource_id, "content"])
            .await?;
        self.fetch_content(url, max_bytes.min(MAX_RESOURCE_BYTES))
            .await
    }
}
