//! Inline image description for the wiki page reads (#985): the SDK's
//! `_process_images` for `get_wiki_page_by_path`, `get_wiki_page_by_id` and
//! `get_wiki_page` with content.
//!
//! A wiki page links its pictures as `![name](/.attachments/<file>)`. The
//! file is read from the wiki's own git repository (the SDK's
//! `ReposApiWrapper.download_file` on the wiki branch), through the
//! configured connection. Azure DevOps also links non-image attachments
//! with image syntax; a link whose file has no image extension is left as
//! it is and is never read. The SDK also fetched external `http(s)` images
//! without credentials; this runtime sends no request to a host the toolkit
//! is not configured for, so such a link gets a note instead.

use serde_json::Value;

use crate::toolkits::families::ado::client::{AdoClientError, AdoRequest};
use crate::toolkits::families::ado::inline_images::{
    Target, annotate_wiki, describe_references, wiki_image_links,
};
use crate::toolkits::families::ado::pictures::checked_picture;

use super::client::AdoWikiClient;

const WIKI_API: &str = "7.0";
const GIT_API: &str = "7.0";
/// The extensions the SDK treats as pictures in a wiki link
/// (`_IMAGE_EXTENSIONS`).
const IMAGE_EXTENSIONS: [&str; 10] = [
    ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".svg", ".tiff", ".tif", ".ico",
];
const ATTACHMENTS_PREFIX: &str = "/.attachments/";

/// Where a wiki keeps its files: the repository, the branch and the folder
/// the wiki shows (empty for a project wiki).
struct WikiRepository {
    id: String,
    version: String,
    prefix: String,
}

/// The link target without Azure DevOps' ` =WxH` size suffix, `?query` and
/// `#fragment`.
fn link_target(url: &str) -> &str {
    let mut target = url.trim();
    if let Some(at) = target.rfind(" =")
        && target[at + 2..]
            .split_once('x')
            .is_some_and(|(width, height)| {
                width.bytes().all(|byte| byte.is_ascii_digit())
                    && height.bytes().all(|byte| byte.is_ascii_digit())
            })
    {
        target = target[..at].trim_end();
    }
    target.split(['?', '#']).next().unwrap_or(target)
}

fn has_image_extension(path: &str) -> bool {
    let lower = path.to_ascii_lowercase();
    IMAGE_EXTENSIONS
        .iter()
        .any(|extension| lower.ends_with(extension))
}

impl AdoWikiClient {
    /// `content` with each picture link described, or as it is when no
    /// picture link is in it, the wiki cannot be resolved, or the run lends
    /// no chat model to toolkits.
    pub(crate) async fn describe_page_images(
        &self,
        wiki_identified: Option<&str>,
        content: &str,
        prompt: Option<&str>,
    ) -> String {
        let links = wiki_image_links(content);
        if !links
            .iter()
            .any(|(_, url)| has_image_extension(link_target(url)))
        {
            return content.to_owned();
        }
        let Some(host) = &self.attachments else {
            tracing::debug!(
                event = "ado_wiki_images_not_described",
                reason = "this run lends no chat model to toolkits",
                "wiki images are returned without descriptions"
            );
            return content.to_owned();
        };
        let Some(wiki) = self.resolve(wiki_identified) else {
            return content.to_owned();
        };
        // Read once per call, and only when a picture is to be read.
        let repository = tokio::sync::OnceCell::new();
        let annotations = describe_references(
            links.into_iter().map(|(_, url)| url),
            host,
            prompt,
            |url| {
                let target = link_target(url);
                if !has_image_extension(target) {
                    Target::Skip
                } else if target.starts_with(ATTACHMENTS_PREFIX) {
                    Target::Fetch(target.to_owned())
                } else {
                    Target::Unavailable(
                        "only wiki attachments are read; external images are not fetched"
                            .to_owned(),
                    )
                }
            },
            |target| {
                let repository = &repository;
                async move {
                    let Ok(repository) = repository
                        .get_or_try_init(|| self.wiki_repository(wiki))
                        .await
                    else {
                        return Err("the wiki's repository could not be read".to_owned());
                    };
                    self.read_attachment(repository, &target).await
                }
            },
        )
        .await;
        annotate_wiki(content, &annotations)
    }

    async fn wiki_repository(&self, wiki: &str) -> Result<WikiRepository, AdoClientError> {
        let segments = ["wiki", "wikis", wiki];
        let value = self
            .ado
            .json(AdoRequest::get(&segments, WIKI_API), false)
            .await?;
        let text = |name: &str| value.get(name).and_then(Value::as_str).map(str::to_owned);
        let id = text("repositoryId")
            .filter(|id| !id.is_empty())
            .ok_or_else(crate::toolkits::families::ado::client::invalid_response)?;
        let version = value
            .get("versions")
            .and_then(Value::as_array)
            .and_then(|versions| versions.first())
            .and_then(|version| version.get("version"))
            .and_then(Value::as_str)
            .filter(|version| !version.is_empty())
            .unwrap_or("wikiMaster")
            .to_owned();
        let prefix = text("mappedPath")
            .unwrap_or_default()
            .trim_matches('/')
            .to_owned();
        Ok(WikiRepository {
            id,
            version,
            prefix,
        })
    }

    async fn read_attachment(
        &self,
        repository: &WikiRepository,
        target: &str,
    ) -> Result<(Vec<u8>, &'static str), String> {
        let relative = percent_encoding::percent_decode_str(target.trim_start_matches('/'))
            .decode_utf8_lossy()
            .into_owned();
        if relative.split('/').any(|segment| segment == "..") {
            return Err("the attachment path is not valid".to_owned());
        }
        let path = if repository.prefix.is_empty() {
            format!("/{relative}")
        } else {
            format!("/{}/{relative}", repository.prefix)
        };
        let segments = ["git", "repositories", repository.id.as_str(), "items"];
        let request = AdoRequest::get(&segments, GIT_API)
            .query("path", path)
            .query("versionDescriptor.versionType", "branch")
            .query("versionDescriptor.version", repository.version.clone())
            .query("download", "true")
            .octet_stream();
        let bytes = match self.ado.bytes(request, false).await {
            Ok(bytes) => bytes,
            Err(error) => {
                return Err(match error.code() {
                    crate::toolkits::families::ado::client::AdoClientErrorCode::ResourceExhausted => {
                        "image exceeds the 5 MB processing limit".to_owned()
                    }
                    crate::toolkits::families::ado::client::AdoClientErrorCode::NotFound => {
                        "the attachment was not found in the wiki".to_owned()
                    }
                    _ => "could not download the attachment".to_owned(),
                });
            }
        };
        let name = relative.rsplit('/').next().unwrap_or(relative.as_str());
        let media_type = checked_picture(&bytes, name)?;
        Ok((bytes, media_type))
    }
}

/// `value` (a page read's result) with its page content described: the
/// string itself, or the `page.content` of a `get_wiki_page` result.
pub(crate) async fn describe_result(
    client: &AdoWikiClient,
    wiki_identified: Option<&str>,
    value: Value,
    prompt: Option<&str>,
) -> Value {
    match value {
        Value::String(content) => Value::String(
            client
                .describe_page_images(wiki_identified, &content, prompt)
                .await,
        ),
        Value::Object(mut object) => {
            let content = object
                .get("page")
                .and_then(|page| page.get("content"))
                .and_then(Value::as_str)
                .map(ToOwned::to_owned);
            if let Some(content) = content {
                let described = client
                    .describe_page_images(wiki_identified, &content, prompt)
                    .await;
                if let Some(page) = object.get_mut("page").and_then(Value::as_object_mut) {
                    page.insert("content".to_owned(), Value::String(described));
                }
            }
            Value::Object(object)
        }
        other => other,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_target_drops_the_size_query_and_fragment() {
        assert_eq!(
            link_target("/.attachments/a.png =300x200"),
            "/.attachments/a.png"
        );
        assert_eq!(
            link_target("/.attachments/a.png?x=1#f"),
            "/.attachments/a.png"
        );
        assert!(has_image_extension("/.attachments/A.PNG"));
        assert!(!has_image_extension("/.attachments/spec.pdf"));
    }

    #[test]
    fn unavailable_reads_as_the_sdk_note() {
        assert_eq!(
            crate::toolkits::families::ado::inline_images::unavailable(
                "Could not read image dimensions"
            ),
            crate::toolkits::families::ado::inline_images::Annotation::Note(
                "[image unavailable: could not read image dimensions]".to_owned()
            )
        );
    }
}
