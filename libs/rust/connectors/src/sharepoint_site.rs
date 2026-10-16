#![allow(clippy::doc_markdown)] // `SharePoint` is a product name.
//! The one rule for the SharePoint site a toolkit names, shared by the
//! agent-runtime toolkit family (delegated token) and the indexing
//! connector (app-only credential): both read `site_url` and the optional
//! `site_path` setting the same way.

use url::Url;

/// The site `site_url` names, with the toolkit's `site_path` (`sites/<name>`
/// or `teams/<name>`) replacing the URL's own path when given, and no
/// trailing slash. `None` for anything unsafe: not `https`, a port,
/// credentials, a query or fragment, `..` segments, a `site_path` that is
/// not `sites|teams/<name…>`, or a character that would change meaning once
/// put into a Graph address.
#[must_use]
pub fn resolve_site_url(value: &str, site_path: Option<&str>) -> Option<Url> {
    let mut site_url = parse_site_url(value)?;
    if let Some(site_path) = site_path {
        let normalized = site_path.trim().trim_matches('/');
        let mut parts = normalized.splitn(2, '/');
        let prefix = parts.next().unwrap_or_default();
        let suffix = parts.next().unwrap_or_default();
        if !matches!(prefix, "sites" | "teams")
            || suffix.is_empty()
            || suffix
                .split('/')
                .any(|part| part.is_empty() || part == "..")
            || normalized.contains(['?', '#', '\\', '%'])
            || normalized.chars().any(char::is_control)
        {
            return None;
        }
        site_url.set_path(&format!("/{normalized}"));
    }
    if site_url.path().ends_with('/') && site_url.path() != "/" {
        let path = site_url.path().trim_end_matches('/').to_owned();
        site_url.set_path(&path);
    }
    Some(site_url)
}

/// A site URL as written: the checks of [`resolve_site_url`] on the URL
/// alone.
#[must_use]
pub fn parse_site_url(value: &str) -> Option<Url> {
    if value.contains(['\\', '%']) {
        return None;
    }
    let url = Url::parse(value.trim()).ok()?;
    if url.scheme() != "https"
        || url.host_str().is_none_or(str::is_empty)
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.cannot_be_a_base()
        || url.path().split('/').any(|part| part == "..")
    {
        return None;
    }
    Some(url)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_site_path_replaces_the_url_path_and_unsafe_values_are_refused() {
        let site =
            resolve_site_url("https://contoso.sharepoint.com/", Some("/sites/eng/")).unwrap();
        assert_eq!(site.as_str(), "https://contoso.sharepoint.com/sites/eng");
        assert!(resolve_site_url("http://contoso.sharepoint.com", None).is_none());
        assert!(resolve_site_url("https://contoso.sharepoint.com:8443", None).is_none());
        assert!(resolve_site_url("https://contoso.sharepoint.com", Some("other/x")).is_none());
        assert!(resolve_site_url("https://contoso.sharepoint.com", Some("sites/a/../b")).is_none());
        assert!(resolve_site_url("https://contoso.sharepoint.com/a%2Fb", None).is_none());
    }
}
