use reqwest::Method;
use serde_json::Value;

use crate::toolkits::families::attachment_host::AttachmentHost;
use crate::toolkits::families::zephyr_rest::client::{
    MAX_FILE_BYTES, MultipartFile, ZephyrReply, ZephyrRestClient, ZephyrRestError,
    ZephyrRestErrorCode, ZephyrRestFamily, bounded_value, invalid_response, resource_exhausted,
};

/// Page size and page ceiling of the folder-name lookup.
const FOLDER_PAGE_SIZE: u32 = 100;
const MAX_FOLDER_PAGES: u32 = 50;

pub(in crate::toolkits) static FAMILY: ZephyrRestFamily = ZephyrRestFamily {
    label: "Zephyr Essential",
    code: error_code,
};

const fn error_code(code: ZephyrRestErrorCode) -> &'static str {
    match code {
        ZephyrRestErrorCode::InvalidConfiguration => "zephyr_essential.configuration.invalid",
        ZephyrRestErrorCode::InvalidInput => "zephyr_essential.request.invalid",
        ZephyrRestErrorCode::Authentication => "zephyr_essential.authentication.failed",
        ZephyrRestErrorCode::Authorization => "zephyr_essential.authorization.failed",
        ZephyrRestErrorCode::NotFound => "zephyr_essential.resource.not_found",
        ZephyrRestErrorCode::Conflict => "zephyr_essential.resource.conflict",
        ZephyrRestErrorCode::RateLimited => "zephyr_essential.rate_limited",
        ZephyrRestErrorCode::Timeout => "zephyr_essential.timeout",
        ZephyrRestErrorCode::DependencyUnavailable => "zephyr_essential.unavailable",
        ZephyrRestErrorCode::InvalidResponse => "zephyr_essential.response.invalid",
        ZephyrRestErrorCode::ResourceExhausted => "zephyr_essential.response.resource_exhausted",
        ZephyrRestErrorCode::UnknownOutcome => "zephyr_essential.effect.unknown_outcome",
    }
}

/// The SDK's `ZephyrEssentialAPI` over one bearer client.
pub(crate) struct ZephyrEssentialClient {
    rest: ZephyrRestClient,
    attachments: Option<AttachmentHost>,
}

impl ZephyrEssentialClient {
    pub(crate) const fn new(rest: ZephyrRestClient) -> Self {
        Self {
            rest,
            attachments: None,
        }
    }

    /// Lend the artifact store: it enables the result uploads and the BDD
    /// archive download, which read and write artifacts.
    #[must_use]
    pub(crate) fn with_attachments(mut self, attachments: AttachmentHost) -> Self {
        self.attachments = Some(attachments);
        self
    }

    pub(in crate::toolkits) const fn attachments(&self) -> Option<&AttachmentHost> {
        self.attachments.as_ref()
    }

    /// `POST /automations/executions/{kind}` with one multipart `file`. Sent
    /// once; an unreadable answer after the send is an unknown outcome.
    pub(in crate::toolkits) async fn upload(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
        file: &MultipartFile<'_>,
    ) -> Result<Value, ZephyrRestError> {
        let reply: ZephyrReply = self.rest.upload(segments, query, file).await?;
        bounded_value(reply.into_value(), true)
    }

    /// `GET /automations/testcases?projectKey=` as `application/zip`: the raw
    /// archive, refused over 50 MiB before it is buffered.
    pub(in crate::toolkits) async fn download_bdd(
        &self,
        project_key: &str,
    ) -> Result<Vec<u8>, ZephyrRestError> {
        self.rest
            .get_bytes(
                &["automations", "testcases"],
                &[("projectKey", project_key.to_owned())],
                "application/zip",
                MAX_FILE_BYTES,
            )
            .await
    }

    /// The SDK's `_do_request`: the decoded JSON, the text, or `""`.
    pub(in crate::toolkits) async fn request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<Value, ZephyrRestError> {
        let effect = method != Method::GET;
        let reply = self.rest.call(method, segments, query, body).await?;
        bounded_value(reply.into_value(), effect)
    }

    /// `find_folder_by_name`: the first folder whose name equals `name`
    /// ignoring case, or `None`.
    ///
    /// The SDK reads only the first `/folders` page at the server's default
    /// size, so a folder past it is never found; this walks the pages
    /// (`isLast`) under a fixed ceiling instead.
    pub(in crate::toolkits) async fn find_folder_by_name(
        &self,
        name: &str,
        project_key: Option<&str>,
        folder_type: Option<&str>,
    ) -> Result<Option<Value>, ZephyrRestError> {
        let wanted = name.to_lowercase();
        let mut start_at = 0_u64;
        for _ in 0..MAX_FOLDER_PAGES {
            let mut query = Vec::with_capacity(4);
            if let Some(project_key) = project_key {
                query.push(("projectKey", project_key.to_owned()));
            }
            if let Some(folder_type) = folder_type {
                query.push(("folderType", folder_type.to_owned()));
            }
            query.push(("maxResults", FOLDER_PAGE_SIZE.to_string()));
            query.push(("startAt", start_at.to_string()));
            let page = self.rest.get_json(&["folders"], &query).await?;
            let values = page
                .get("values")
                .and_then(Value::as_array)
                .ok_or_else(invalid_response)?;
            if let Some(found) = values.iter().find(|folder| {
                folder
                    .get("name")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_lowercase()
                    == wanted
            }) {
                return Ok(Some(found.clone()));
            }
            if values.is_empty() || page.get("isLast").and_then(Value::as_bool) != Some(false) {
                return Ok(None);
            }
            start_at = start_at.saturating_add(u64::try_from(values.len()).unwrap_or(u64::MAX));
        }
        Err(resource_exhausted(false))
    }
}
