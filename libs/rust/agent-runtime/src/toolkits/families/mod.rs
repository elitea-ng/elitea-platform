pub(crate) mod ado;
pub(crate) mod ado_boards;
pub(crate) mod ado_plans;
pub(crate) mod ado_repos;
pub(crate) mod ado_wiki;
pub(crate) mod aha;
// Public only to the worker's composition suites (`test-support`).
#[cfg(any(test, feature = "test-support"))]
pub mod artifact;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod artifact;
pub(crate) mod azure;
pub(crate) mod azure_search;
pub(crate) mod bigquery;
pub(crate) mod bitbucket;
pub(crate) mod carrier;
pub(crate) mod confluence;
pub(crate) mod elastic;
pub(crate) mod figma;
pub(crate) mod gcp;
pub(crate) mod github;
pub(crate) mod gitlab;
pub(crate) mod gitlab_org;
pub(crate) mod google_places;
pub(crate) mod https_base_url;
pub(crate) mod jira;
pub(crate) mod keycloak;
pub(crate) mod kubernetes;
// Public only to the worker's composition suites (`test-support`).
#[cfg(any(test, feature = "test-support"))]
pub mod openapi;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod openapi;
pub(crate) mod postman;
pub(crate) mod python_repr;
pub(crate) mod qtest;
pub(crate) mod rally;
pub(crate) mod report_portal;
pub(crate) mod salesforce;
pub(crate) mod service_now;
pub(crate) mod sharepoint;
pub(crate) mod slack;
pub(crate) mod sonar;
#[cfg(feature = "toolkit-sql")]
pub(crate) mod sql;
pub(crate) mod testio;
pub(crate) mod testrail;
pub(crate) mod vcs_text;
pub(crate) mod xray_cloud;
pub(crate) mod yagmail;
pub(crate) mod zephyr;
pub(crate) mod zephyr_enterprise;
pub(crate) mod zephyr_essential;
pub(crate) mod zephyr_rest;
pub(crate) mod zephyr_scale;
pub(crate) mod zephyr_squad;

/// A configured setting the SDK honours and this runtime deliberately
/// refuses (#1207 review round 2). A family that refuses one names it, so the
/// refusal reaches the user as a sentence rather than as a toolkit that is
/// listed as enabled and silently does nothing (see
/// `materialize::RefusedToolkit`). The security policy itself is unchanged:
/// these stay refused until the owner decides otherwise.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum UnsupportedSetting {
    /// A plain `http://` base URL.
    PlainHttp,
    /// `verify_ssl: false`.
    VerifySslDisabled,
    /// A custom header that would replace the credential, carry platform
    /// identity or change request framing.
    ReservedHeader,
}

impl UnsupportedSetting {
    /// The reason, as the user and the model read it. Carries no
    /// configuration value.
    #[must_use]
    pub(crate) const fn reason(self) -> &'static str {
        match self {
            Self::PlainHttp => {
                "a plain http:// base URL is not supported by the Rust worker; configure an https:// URL"
            }
            Self::VerifySslDisabled => {
                "verify_ssl=false is not supported by the Rust worker; TLS certificates are always verified"
            }
            Self::ReservedHeader => {
                "a custom header that overrides the credential or request framing (Authorization, Cookie, Host, Content-Type, Accept or a platform header) is not supported by the Rust worker"
            }
        }
    }
}
