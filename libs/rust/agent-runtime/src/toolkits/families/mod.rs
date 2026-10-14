pub(crate) mod ado;
pub(crate) mod ado_boards;
pub(crate) mod ado_plans;
pub(crate) mod ado_repos;
pub(crate) mod ado_wiki;
pub(crate) mod aha;
pub(crate) mod attachment_host;
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
pub(crate) mod connector_client;
#[cfg(feature = "toolkit-data-analysis")]
pub mod data_analysis;
#[cfg(feature = "toolkit-office")]
pub(crate) mod docx;
pub(crate) mod elastic;
pub(crate) mod figma;
pub(crate) mod gcp;
pub(crate) mod github;
pub(crate) mod gitlab;
pub(crate) mod gitlab_org;
pub(crate) mod google_places;
pub(crate) mod https_base_url;
pub(crate) mod imagegen;
#[cfg(feature = "toolkit-office")]
pub(crate) mod office_common;
pub mod office_host;
// Public only to the worker's composition suites (`test-support`).
#[cfg(any(test, feature = "test-support"))]
pub mod index_tools;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod index_tools;
pub(crate) mod jira;
pub(crate) mod keycloak;
// Public only to the worker's composition suites (`test-support`).
pub(crate) mod kubernetes;
#[cfg(any(test, feature = "test-support"))]
pub mod memory;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod memory;
// Public only to the worker's composition suites (`test-support`).
pub(crate) mod integer_like;
#[cfg(any(test, feature = "test-support"))]
pub mod openapi;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod openapi;
pub(crate) mod postman;
#[cfg(feature = "toolkit-office")]
pub(crate) mod pptx;
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
#[cfg(feature = "toolkit-office")]
pub(crate) mod xlsx;
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
    /// A `SharePoint` credential with no OAuth discovery endpoint: the
    /// app-only (client secret) mode.
    SharePointAppOnly,
    /// A `memory` toolkit whose `namespace` is blank, too long or holds a
    /// control character.
    MemoryNamespace,
    /// A `memory` toolkit whose `memory_scope` is not `personal`, `project`
    /// or `both`.
    MemoryScope,
    /// A `memory` toolkit whose `selected_tools` is not a list of names.
    MemorySelection,
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
            Self::MemoryNamespace => {
                "the memory toolkit's namespace must be a non-blank text of at most 256 bytes without control characters; edit the toolkit and save it again"
            }
            Self::MemoryScope => {
                "the memory toolkit's memory scope must be personal, project or both; edit the toolkit and save it again"
            }
            Self::MemorySelection => {
                "the memory toolkit's selected tools must be a list of tool names; edit the toolkit and save it again"
            }
            Self::SharePointAppOnly => {
                "a SharePoint credential without an OAuth discovery endpoint (app-only access with a client secret) is not supported by the Rust worker: its SharePoint tools act as the signed-in person with a delegated token, so set the credential's OAuth discovery endpoint. Microsoft retired app-only access to the OneNote Graph API (MC1011142), so the OneNote tools cannot run on an app-only credential at all"
            }
        }
    }
}

/// The Office families' tools as elitea-main's `rust_only_toolkit_tools.json`
/// carries them (ADR-0032 P3, P4): the `pptx` tools the SDK does not declare
/// under `types`, and the whole `docx` and `xlsx` types (they have no SDK
/// counterpart) under `new_types`. The Go catalogue merges both into its pinned
/// SDK snapshot; the test that compares this document with the file there
/// (`ELITEA_WRITE_RUST_ONLY_TOOLS=1` rewrites it) is what keeps the two equal.
#[cfg(all(test, feature = "toolkit-office"))]
pub(crate) fn rust_only_catalogue_document() -> serde_json::Value {
    serde_json::json!({
        "schema_version": "elitea.rust-only-toolkit-tools.v1",
        "types": {"pptx": serde_json::Value::Object(pptx::schema::rust_only_catalogue_tools())},
        "new_types": {"docx": docx::schema::type_entry(), "xlsx": xlsx::schema::type_entry()},
    })
}
