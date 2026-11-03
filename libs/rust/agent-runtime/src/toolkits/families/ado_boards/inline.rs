//! Inline image description for `get_comments` and `get_work_item` (#985):
//! the SDK's `_embed_comment_image_descriptions` and the `parse_attachments`
//! field pass of `get_work_item`.
//!
//! Each picture is read as `get_image_by_url` reads it: by its attachment id,
//! through the configured connection, and only on the configured
//! organization. It is described by the run's chat model. A picture that
//! cannot be read or described gets an `[image unavailable: ...]` note; the
//! call still returns every comment and field.

use serde_json::{Map, Value};

use crate::toolkits::families::ado::inline_images::{
    Picture, annotate, describe_references, html_image_sources, markdown_image_urls,
};

use super::client::AdoBoardsClient;

/// The comment fields that carry markup (`_COMMENT_TEXT_FIELDS`, after the
/// snake-case conversion every comment goes through).
const COMMENT_TEXT_FIELDS: [&str; 3] = ["rendered_text", "renderedText", "text"];

impl AdoBoardsClient {
    /// Describe the pictures of `comments` in place. Without the run's chat
    /// model (no attachment host), the comments stay as they are, as the SDK
    /// returns them when no LLM is configured.
    pub(super) async fn describe_comment_images(
        &self,
        comments: &mut [Value],
        prompt: Option<&str>,
    ) {
        let texts = comments
            .iter()
            .filter_map(Value::as_object)
            .flat_map(|comment| {
                COMMENT_TEXT_FIELDS
                    .iter()
                    .filter_map(|field| comment.get(*field).and_then(Value::as_str))
            })
            .map(ToOwned::to_owned)
            .collect::<Vec<_>>();
        let Some(annotations) = self.describe_texts(&texts, prompt).await else {
            return;
        };
        for comment in comments.iter_mut().filter_map(Value::as_object_mut) {
            for field in COMMENT_TEXT_FIELDS {
                if let Some(Value::String(text)) = comment.get_mut(field) {
                    *text = annotate(text, &annotations);
                }
            }
        }
    }

    /// Describe the pictures of every text field of a `get_work_item`
    /// result in place (`id` and `url` are not fields).
    pub(super) async fn describe_work_item_images(
        &self,
        item: &mut Map<String, Value>,
        prompt: Option<&str>,
    ) {
        let texts = item
            .iter()
            .filter(|(name, _)| !matches!(name.as_str(), "id" | "url"))
            .filter_map(|(_, value)| value.as_str().map(ToOwned::to_owned))
            .collect::<Vec<_>>();
        let Some(annotations) = self.describe_texts(&texts, prompt).await else {
            return;
        };
        for (name, value) in item.iter_mut() {
            if matches!(name.as_str(), "id" | "url") {
                continue;
            }
            if let Value::String(text) = value {
                *text = annotate(text, &annotations);
            }
        }
    }

    /// The annotations for the pictures `texts` reference, or `None` when
    /// there is nothing to do: no picture, or no chat model lent to the run.
    async fn describe_texts(
        &self,
        texts: &[String],
        prompt: Option<&str>,
    ) -> Option<
        std::collections::BTreeMap<
            String,
            crate::toolkits::families::ado::inline_images::Annotation,
        >,
    > {
        let references = texts
            .iter()
            .flat_map(|text| {
                let mut found = html_image_sources(text);
                found.extend(markdown_image_urls(text));
                found
            })
            .collect::<Vec<_>>();
        if references.is_empty() {
            return None;
        }
        let Some(host) = &self.attachments else {
            tracing::debug!(
                event = "ado_boards_images_not_described",
                reason = "this run lends no chat model to toolkits",
                "work item images are returned without descriptions"
            );
            return None;
        };
        Some(
            describe_references(references, host, prompt, |url| async move {
                match self.fetch_attachment_picture(&url, None).await {
                    Ok((bytes, media_type)) => Picture::Read(bytes, media_type),
                    Err(reason) => Picture::Unavailable(reason),
                }
            })
            .await,
        )
    }
}
