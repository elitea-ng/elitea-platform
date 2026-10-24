//! The key-less `direct` fetch backend: the one place a model-chosen URL is
//! dialled, so the one place the worker's SSRF rule applies.
//!
//! - Every hop's URL is checked ([`super::capability::fetch::url_violation`])
//!   and its host resolved through the egress guard; the client's resolver is
//!   the guard too, so the connection goes to a checked address (no DNS
//!   rebinding). Redirects are followed by hand, at most five, each hop
//!   checked again: a 302 to `http://169.254.169.254/` fails.
//! - No cookies, no auth headers, no proxy; `User-Agent: EliteaFetch/1`.
//! - At most 5 MiB is downloaded; only text, JSON and XHTML are read (PDF is
//!   refused). HTML becomes Markdown (or plain text) with scripts, styles,
//!   forms, navigation and hidden elements stripped.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use elitea_egress_guard::{GuardedResolver, Lookup, PermitSet, resolve_checked};
use html_to_markdown_rs::{
    ConversionOptions, OutputFormat, PreprocessingOptions, PreprocessingPreset,
};
use reqwest::header::{ACCEPT, HeaderValue};
use reqwest::{Method, Request, StatusCode};

use super::WebState;
use super::capability::fetch::url_violation;
use super::capability::{FetchFormat, FetchRequest, FetchResult, ProviderFailure};
use super::meter::Units;
use super::transport::{ProviderError, map_reqwest, read_answer_truncating};

const MAX_REDIRECTS: usize = 5;
const MAX_DOWNLOAD_BYTES: usize = 5 * 1_024 * 1_024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const FETCH_TIMEOUT: Duration = Duration::from_secs(20);
const ACCEPTED: &str =
    "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.9,text/*;q=0.8";

pub(crate) struct DirectFetcher {
    lookup: Arc<dyn Lookup>,
    permits: PermitSet,
    user_agent: String,
    /// One client per port: the guarded resolver checks permits per port.
    clients: Mutex<BTreeMap<(u16, bool), reqwest::Client>>,
}

impl DirectFetcher {
    pub(crate) fn new(lookup: Arc<dyn Lookup>, permits: PermitSet, user_agent: String) -> Self {
        Self {
            lookup,
            permits,
            user_agent,
            clients: Mutex::new(BTreeMap::new()),
        }
    }

    fn client(&self, port: u16, https_only: bool) -> Result<reqwest::Client, ProviderError> {
        let mut clients = self
            .clients
            .lock()
            .map_err(|_| ProviderError::Unavailable)?;
        if let Some(client) = clients.get(&(port, https_only)) {
            return Ok(client.clone());
        }
        let client = reqwest::Client::builder()
            .https_only(https_only)
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .dns_resolver(Arc::new(GuardedResolver::new(
                Arc::clone(&self.lookup),
                port,
                self.permits.clone(),
            )))
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(FETCH_TIMEOUT)
            .user_agent(self.user_agent.clone())
            .build()
            .map_err(|_| ProviderError::Unavailable)?;
        clients.insert((port, https_only), client.clone());
        Ok(client)
    }

    pub(crate) async fn fetch(
        &self,
        request: &FetchRequest,
        state: &WebState,
    ) -> Result<FetchResult, ProviderFailure> {
        let allow_http = state.config.allow_http;
        let mut url = request.url.clone();
        let mut hops = 0;
        let answer = loop {
            if let Some(violation) = url_violation(&url, allow_http, state) {
                return Err(violation.into());
            }
            let host = url.host_str().ok_or(ProviderError::UrlRefused)?.to_owned();
            let port = url
                .port_or_known_default()
                .ok_or(ProviderError::UrlRefused)?;
            // Checked here for a clear refusal; the client's resolver checks
            // again and is what the connection actually uses.
            resolve_checked(self.lookup.as_ref(), &host, port, &self.permits)
                .await
                .map_err(|error| match error {
                    elitea_egress_guard::ResolveError::NotPublic => ProviderError::AddressRefused,
                    _ => ProviderError::Unavailable,
                })?;
            let client = self.client(port, !allow_http)?;
            let mut outgoing = Request::new(Method::GET, url.clone());
            outgoing
                .headers_mut()
                .insert(ACCEPT, HeaderValue::from_static(ACCEPTED));
            *outgoing.timeout_mut() = Some(FETCH_TIMEOUT);
            let response = client
                .execute(outgoing)
                .await
                .map_err(|error| map_reqwest(&error.without_url()))?;
            let answer = read_answer_truncating(response, MAX_DOWNLOAD_BYTES).await?;
            if answer.0.status.is_redirection()
                && let Some(location) = answer.0.location.as_deref()
            {
                if hops == MAX_REDIRECTS {
                    return Err(ProviderError::TooManyRedirects.into());
                }
                hops += 1;
                url = url.join(location).map_err(|_| ProviderError::UrlRefused)?;
                continue;
            }
            break answer;
        };
        let (answer, download_truncated) = answer;
        if let Some(error) = answer.error() {
            return Err(error.into());
        }
        if answer.status != StatusCode::OK && !answer.status.is_success() {
            return Err(ProviderError::Unavailable.into());
        }
        let media = answer
            .media_type()
            .unwrap_or_else(|| "text/plain".to_owned());
        let html = matches!(media.as_str(), "text/html" | "application/xhtml+xml");
        if !(media.starts_with("text/") || media == "application/json" || html) {
            return Err(ProviderError::UnsupportedContent.into());
        }
        let text = String::from_utf8_lossy(&answer.body);
        let (title, content, format) = if html {
            let (title, content) = html_to_text(&text, request.format);
            (title, content, request.format)
        } else {
            (None, text.into_owned(), FetchFormat::Text)
        };
        Ok(FetchResult {
            url: request.url.to_string(),
            final_url: url.to_string(),
            status: answer.status.as_u16(),
            content_type: answer.media_type(),
            title,
            content,
            format: format.as_str(),
            truncated: download_truncated,
            bytes: answer.body.len(),
            units: Units::pages(1),
        })
    }
}

/// Readability-lite: the page's `<main>` (else its first `<article>`, else
/// the whole page), converted with the scripts, styles, forms, navigation
/// and hidden elements stripped.
pub(crate) fn html_to_text(html: &str, format: FetchFormat) -> (Option<String>, String) {
    let title = element_inner(html, "title")
        .map(|title| html_escape::decode_html_entities(title.trim()).into_owned())
        .filter(|title| !title.is_empty());
    let body = element_inner(html, "main")
        .or_else(|| element_inner(html, "article"))
        .unwrap_or(html);
    let options = ConversionOptions {
        extract_metadata: false,
        skip_images: true,
        strip_tags: [
            "script", "style", "noscript", "iframe", "svg", "form", "template", "object", "embed",
            "canvas", "button", "input", "select", "textarea", "nav", "aside", "footer",
        ]
        .iter()
        .map(|tag| (*tag).to_owned())
        .collect(),
        preprocessing: PreprocessingOptions {
            enabled: true,
            preset: PreprocessingPreset::Standard,
            remove_navigation: true,
            remove_forms: true,
        },
        output_format: match format {
            FetchFormat::Markdown => OutputFormat::Markdown,
            FetchFormat::Text => OutputFormat::Plain,
        },
        ..ConversionOptions::default()
    };
    let content = html_to_markdown_rs::convert(body, options)
        .ok()
        .and_then(|result| result.content)
        .unwrap_or_default();
    (title, content.trim().to_owned())
}

/// The inner HTML of the first `<tag …>…</tag>`, ASCII case ignored.
fn element_inner<'a>(html: &'a str, tag: &str) -> Option<&'a str> {
    let lower = html.to_ascii_lowercase();
    let open = format!("<{tag}");
    let mut from = 0;
    let start = loop {
        let at = from + lower[from..].find(&open)?;
        let after = lower.as_bytes().get(at + open.len()).copied();
        if matches!(after, Some(b'>' | b' ' | b'\t' | b'\n' | b'\r' | b'/')) {
            break at;
        }
        from = at + open.len();
    };
    let content_start = start + lower[start..].find('>')? + 1;
    let close = format!("</{tag}");
    let end = content_start + lower[content_start..].find(&close)?;
    html.get(content_start..end)
}
