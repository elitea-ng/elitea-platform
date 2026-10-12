//! The one base-URL rule the provider clients apply to a configured
//! origin (#1207 review round 2).
//!
//! Ten configs used to carry their own copy of this check, and the copies had
//! drifted: some refused `%`, some refused dot segments, two answered a plain
//! `http://` origin differently from every other malformed URL. The base URL
//! is an authority boundary — every request path is appended beneath it,
//! segment by segment — so the rule is the strictest of those copies, applied
//! once:
//!
//! * `https` only. A plain `http://` origin is reported as its own error,
//!   [`BaseUrlError::PlainHttp`], so a family that tells the user "plain HTTP
//!   is not supported by the Rust worker" (Jira, Confluence) can, while every
//!   other family folds it into its invalid-configuration answer.
//! * a host, and no userinfo, query or fragment (an empty `?` or `#` counts).
//! * no whitespace, control characters or backslashes anywhere.
//! * no `%` at all: a percent-encoded byte in a base is either an encoded
//!   dot segment, an encoded separator or an encoded credential, and none of
//!   those belongs in an origin with a context path.
//! * no `.` or `..` path segment in the text as written. The URL parser
//!   resolves them away, so the check runs on the input, before parsing.
//!
//! The stored URL never ends in `/`; [`BasePath::OriginOnly`] additionally
//! refuses any path (a family whose API lives at a fixed root).

use url::Url;

/// The longest base URL any family accepts, in bytes.
pub const MAX_BASE_URL_BYTES: usize = 2 * 1_024;

/// Why a base URL was refused. Carries no part of the URL.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BaseUrlError {
    /// Not a URL this rule admits.
    Invalid,
    /// Longer than [`MAX_BASE_URL_BYTES`].
    TooLong,
    /// A well-formed `http://` origin: refused like [`Self::Invalid`], but
    /// distinguishable so a family can name the reason.
    PlainHttp,
}

/// Whether the base may carry a context path.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BasePath {
    /// `https://host/prefix` — a server under a context path.
    Prefix,
    /// `https://host` only.
    OriginOnly,
}

/// Parse `value` (surrounding whitespace ignored) under the rule above.
///
/// # Errors
///
/// The value breaks the rule ([`BaseUrlError`] says how, without the URL).
pub fn parse(value: &str, path: BasePath) -> Result<Url, BaseUrlError> {
    let value = value.trim();
    if value.len() > MAX_BASE_URL_BYTES {
        return Err(BaseUrlError::TooLong);
    }
    if value.is_empty()
        || value
            .chars()
            .any(|character| character.is_whitespace() || character.is_control())
        || value.contains(['%', '\\'])
        || value
            .split('/')
            .any(|segment| matches!(segment, "." | ".."))
    {
        return Err(BaseUrlError::Invalid);
    }
    let mut url = Url::parse(value).map_err(|_| BaseUrlError::Invalid)?;
    if url.scheme() == "http" {
        return Err(BaseUrlError::PlainHttp);
    }
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(BaseUrlError::Invalid);
    }
    let trimmed = url.path().trim_end_matches('/').to_owned();
    if path == BasePath::OriginOnly && !trimmed.is_empty() {
        return Err(BaseUrlError::Invalid);
    }
    url.set_path(&trimmed);
    Ok(url)
}

/// `url_host_matches_domain`: the host is `domain` or a subdomain of it
/// (ASCII case-insensitive).
#[must_use]
pub fn host_matches_domain(url: &Url, domain: &str) -> bool {
    url.host_str().is_some_and(|host| {
        let host = host.to_ascii_lowercase();
        host == domain
            || host
                .strip_suffix(domain)
                .is_some_and(|prefix| prefix.ends_with('.'))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ok(value: &str) -> String {
        parse(value, BasePath::Prefix)
            .expect("admitted base")
            .to_string()
    }

    fn refused(value: &str) -> BaseUrlError {
        parse(value, BasePath::Prefix).expect_err("refused base")
    }

    #[test]
    fn admits_https_origins_and_context_paths_without_a_trailing_slash() {
        assert_eq!(ok("https://example.com"), "https://example.com/");
        assert_eq!(ok("  https://example.com/  "), "https://example.com/");
        assert_eq!(ok("https://example.com/jira/"), "https://example.com/jira");
        assert_eq!(
            ok("https://example.com:8443/a/b//"),
            "https://example.com:8443/a/b"
        );
        assert_eq!(
            parse("https://gitlab.example.com/", BasePath::OriginOnly)
                .expect("origin")
                .as_str(),
            "https://gitlab.example.com/"
        );
    }

    #[test]
    fn plain_http_is_its_own_refusal() {
        assert_eq!(refused("http://example.com"), BaseUrlError::PlainHttp);
        assert_eq!(refused("HTTP://example.com/jira"), BaseUrlError::PlainHttp);
        // Malformed http text is not "plain http": it is not a URL at all.
        assert_eq!(refused("http://"), BaseUrlError::Invalid);
    }

    #[test]
    fn refuses_everything_the_strictest_copy_refused() {
        for value in [
            "",
            "   ",
            "ftp://example.com",
            "https://",
            "https://user@example.com",
            "https://user:secret@example.com",
            "https://example.com?",
            "https://example.com/?a=1",
            "https://example.com#",
            "https://example.com/#top",
            "https://example.com/a/../b",
            "https://example.com/./b",
            "https://example.com/..",
            "https://example.com/%2e%2e/admin",
            "https://example.com/a%20b",
            "https://example.com\\evil",
            "https://exa mple.com",
            "https://example.com/a\tb",
            "https://example.com/\u{0}",
            "not a url",
        ] {
            assert_eq!(refused(value), BaseUrlError::Invalid, "{value:?}");
        }
        assert_eq!(
            parse("https://example.com/api", BasePath::OriginOnly),
            Err(BaseUrlError::Invalid)
        );
    }

    #[test]
    fn length_is_capped_before_parsing() {
        let long = format!("https://example.com/{}", "a".repeat(MAX_BASE_URL_BYTES));
        assert_eq!(refused(&long), BaseUrlError::TooLong);
        let fits = format!(
            "https://example.com/{}",
            "a".repeat(MAX_BASE_URL_BYTES - "https://example.com/".len())
        );
        assert!(parse(&fits, BasePath::Prefix).is_ok());
    }

    #[test]
    fn host_matches_the_domain_or_a_subdomain_only() {
        let url = |value: &str| Url::parse(value).expect("url");
        assert!(host_matches_domain(
            &url("https://atlassian.net"),
            "atlassian.net"
        ));
        assert!(host_matches_domain(
            &url("https://Acme.Atlassian.NET/wiki"),
            "atlassian.net"
        ));
        assert!(!host_matches_domain(
            &url("https://evilatlassian.net"),
            "atlassian.net"
        ));
        assert!(!host_matches_domain(
            &url("https://atlassian.net.evil.com"),
            "atlassian.net"
        ));
    }
}
