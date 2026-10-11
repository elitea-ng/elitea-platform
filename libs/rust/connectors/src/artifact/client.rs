//! The platform object API one invocation may read:
//! `GET {platform}/api/v2/artifacts/objects/{project}/{bucket}?prefix=&cursor=`
//! lists (`objects[].key`, `size_bytes`, `modified_at`, `etag`,
//! `next_cursor`) and `GET …/{bucket}/{key}` downloads, each with the
//! invocation's bearer as a sensitive `Authorization` header.
//!
//! The project in the URL is the bearer's own (elitea-main checks the
//! bearer against it), so a source can only name a bucket of the caller's
//! project. Nothing a caller or a listing supplies reaches the URL except as
//! a percent-encoded path segment or query value.

use std::fmt;

use zeroize::Zeroizing;

use crate::transport::header::{ACCEPT, AUTHORIZATION};
use crate::transport::{HeaderValue, Method, Request, Url};

const OBJECTS_ROUTE: [&str; 4] = ["api", "v2", "artifacts", "objects"];
const MAX_BEARER_BYTES: usize = 16 * 1_024;
const MAX_NAME_BYTES: usize = 1_024;

/// Why the platform settings were refused. Carries no value.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum ArtifactConfigError {
    #[error(
        "the platform base must be an absolute http(s) URL without credentials, query or fragment"
    )]
    Base,
    #[error("the artifact source needs the invocation's bearer")]
    Bearer,
    #[error("the artifact source needs the invoking project")]
    Project,
    #[error("the bucket name is not a single path segment")]
    Bucket,
}

/// The base URL, bearer and project of the object API.
///
/// `Debug` never prints the bearer.
pub struct ArtifactPlatform {
    base: Url,
    bearer: Zeroizing<String>,
    project_id: String,
}

impl fmt::Debug for ArtifactPlatform {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ArtifactPlatform")
            .field("base", &self.base.as_str())
            .field("project_id", &self.project_id)
            .finish_non_exhaustive()
    }
}

impl ArtifactPlatform {
    /// The platform at `base` (no `/llm` suffix), for `project_id`, with the
    /// invocation's `bearer`.
    ///
    /// # Errors
    ///
    /// The base is not an absolute http(s) URL without credentials, query
    /// or fragment; the bearer is empty or not a header value; the project
    /// is empty or not one path segment.
    pub fn new(base: &str, bearer: &str, project_id: &str) -> Result<Self, ArtifactConfigError> {
        let base = Url::parse(base.trim()).map_err(|_| ArtifactConfigError::Base)?;
        if !matches!(base.scheme(), "http" | "https")
            || base.host_str().is_none()
            || !base.username().is_empty()
            || base.password().is_some()
            || base.query().is_some()
            || base.fragment().is_some()
        {
            return Err(ArtifactConfigError::Base);
        }
        let bearer = bearer.trim();
        if bearer.is_empty()
            || bearer.len() > MAX_BEARER_BYTES
            || !bearer.bytes().all(|byte| (0x21..=0x7e).contains(&byte))
        {
            return Err(ArtifactConfigError::Bearer);
        }
        let project_id = project_id.trim();
        if !valid_segment(project_id) {
            return Err(ArtifactConfigError::Project);
        }
        Ok(Self {
            base,
            bearer: Zeroizing::new(bearer.to_owned()),
            project_id: project_id.to_owned(),
        })
    }

    /// The platform base URL (no credential in it).
    #[must_use]
    pub fn base(&self) -> &Url {
        &self.base
    }

    #[must_use]
    pub fn project_id(&self) -> &str {
        &self.project_id
    }

    /// `{base}/api/v2/artifacts/objects/{project}/{bucket}[/{key…}]`, every
    /// segment percent-encoded on its own (a key's `/` stays a separator).
    #[must_use]
    pub fn objects_url(&self, bucket: &str, key: Option<&str>) -> Url {
        let mut url = self.base.clone();
        if let Ok(mut segments) = url.path_segments_mut() {
            segments.pop_if_empty();
            segments.extend(OBJECTS_ROUTE);
            segments.push(&self.project_id);
            segments.push(bucket);
            if let Some(key) = key {
                segments.extend(key.split('/'));
            }
        }
        url
    }

    /// A GET of `url` with the bearer.
    ///
    /// # Errors
    ///
    /// Never for a bearer `new` admitted; kept fallible so a header rule
    /// change cannot panic.
    pub fn get(&self, url: Url) -> Result<Request, ArtifactConfigError> {
        let mut request = Request::new(Method::GET, url);
        let value = Zeroizing::new(format!("Bearer {}", self.bearer.as_str()));
        let mut authorization =
            HeaderValue::from_str(&value).map_err(|_| ArtifactConfigError::Bearer)?;
        authorization.set_sensitive(true);
        request.headers_mut().insert(AUTHORIZATION, authorization);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("*/*"));
        Ok(request)
    }
}

/// A bucket (or project) name: one non-empty path segment.
#[must_use]
pub fn valid_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_NAME_BYTES
        && !matches!(value, "." | "..")
        && !value.contains(['/', '\\', '\0'])
        && !value.chars().any(char::is_control)
}

/// elitea-main's object-key rules: no NUL, no backslash, no empty, `.` or
/// `..` segment (so no absolute path).
#[must_use]
pub fn valid_object_key(key: &str) -> bool {
    crate::git_id::valid_key(key)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_platform_base_and_bearer_are_checked() {
        for base in [
            "ftp://p",
            "https://u:p@p",
            "https://p/?q",
            "https://p/#f",
            "nope",
        ] {
            assert_eq!(
                ArtifactPlatform::new(base, "t", "7").err(),
                Some(ArtifactConfigError::Base),
                "{base}"
            );
        }
        assert_eq!(
            ArtifactPlatform::new("https://p", "", "7").err(),
            Some(ArtifactConfigError::Bearer)
        );
        assert_eq!(
            ArtifactPlatform::new("https://p", "t", "a/b").err(),
            Some(ArtifactConfigError::Project)
        );
        let platform = ArtifactPlatform::new("https://p/main/", "s3cret", "7").expect("platform");
        assert!(!format!("{platform:?}").contains("s3cret"));
        assert_eq!(
            platform.objects_url("b k", Some("dir/a b.md")).as_str(),
            "https://p/main/api/v2/artifacts/objects/7/b%20k/dir/a%20b.md"
        );
        let request = platform
            .get(platform.objects_url("b", None))
            .expect("request");
        assert!(
            request
                .headers()
                .get(AUTHORIZATION)
                .expect("bearer")
                .is_sensitive()
        );
    }

    #[test]
    fn object_keys_follow_the_platform_rules() {
        assert!(valid_object_key("docs/a.md"));
        for key in ["", "/a", "a//b", "a/../b", "a\\b", "a\0b", "."] {
            assert!(!valid_object_key(key), "{key:?}");
        }
    }
}
