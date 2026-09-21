//! Sanitized delegated-authorization control signal shared by tool families.
//!
//! The current SDK names the browser contract `mcp_auth`, but the same
//! `McpAuthorizationRequired` signal is raised by ordinary configured toolkits
//! such as delegated `SharePoint`. Keeping this type outside the MCP transport
//! prevents direct graph nodes from depending on how the guarded tool was
//! materialized. Concrete toolkit families remain responsible for resolving
//! claim-fetched tokens into their own clients during the continuation rebuild.

use std::collections::BTreeMap;

use adk_rust::{AdkError, ErrorCategory, ErrorComponent, ErrorDetails};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

const MAX_AUTH_CHALLENGE_BYTES: usize = 16 * 1_024;
const MAX_TOOLKIT_IDENTITY_BYTES: usize = 1_024;

/// Stable private ADK error code used to carry the current `mcp_auth` control
/// signal through policy wrappers without exposing a provider error body.
pub(crate) const DELEGATED_AUTHORIZATION_ERROR_CODE: &str = "mcp.authorization_required";
pub(crate) const DELEGATED_AUTHORIZATION_METADATA_KEY: &str = "elitea.delegated-authorization.v1";

/// Model-callable tools that are placeholders for one delegated authorization
/// boundary. The catalog is built together with the invocation's toolsets and
/// never contains tokens, tool arguments or provider response bodies.
#[derive(Clone, Default)]
pub(crate) struct DelegatedAuthorizationCatalog {
    requirements: BTreeMap<String, DelegatedAuthorizationRequirement>,
}

impl DelegatedAuthorizationCatalog {
    pub(crate) fn insert(
        &mut self,
        tool_name: &str,
        requirement: DelegatedAuthorizationRequirement,
    ) -> Result<(), ()> {
        if !valid_identity(tool_name) {
            return Err(());
        }
        match self.requirements.get(tool_name) {
            Some(existing) if existing == &requirement => Ok(()),
            Some(_) => Err(()),
            None => {
                self.requirements.insert(tool_name.to_owned(), requirement);
                Ok(())
            }
        }
    }

    /// Re-key every entry this invocation exposes under another function name.
    ///
    /// #983 renames a tool two toolsets both publish. This catalog is keyed by
    /// the model-visible function name — `require_tool_confirmation` and the
    /// projector both look it up by that string — so a catalog left keyed by
    /// the PUBLISHED name after a rename would stop pausing on an MCP tool
    /// whose server demanded authorization, with no error anywhere. `renames`
    /// is one toolset's map; an empty one returns the catalog unchanged.
    pub(crate) fn renamed(self, renames: &BTreeMap<Box<str>, Box<str>>) -> Result<Self, ()> {
        if renames.is_empty() {
            return Ok(self);
        }
        let mut exposed = Self::default();
        for (name, requirement) in self.requirements {
            let name = renames
                .get(name.as_str())
                .map_or(name.as_str(), AsRef::as_ref);
            exposed.insert(name, requirement)?;
        }
        Ok(exposed)
    }

    pub(crate) fn requirement_for(
        &self,
        tool_name: &str,
    ) -> Option<&DelegatedAuthorizationRequirement> {
        self.requirements.get(tool_name)
    }

    pub(crate) fn tool_names(&self) -> impl Iterator<Item = &str> {
        self.requirements.keys().map(String::as_str)
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.requirements.is_empty()
    }

    pub(crate) fn merge(&mut self, other: Self) -> Result<(), ()> {
        for (tool_name, requirement) in other.requirements {
            self.insert(&tool_name, requirement)?;
        }
        Ok(())
    }
}

/// Sanitized OAuth requirement retained long enough to create a native graph
/// interrupt. It contains no token, client secret, tool arguments or response
/// body and deliberately has no `Debug` implementation.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
pub(crate) struct DelegatedAuthorizationRequirement {
    toolkit_name: String,
    toolkit_type: String,
    server_url: String,
    resource_metadata_url: Option<String>,
    www_authenticate: Option<String>,
}

impl DelegatedAuthorizationRequirement {
    pub(crate) fn new(
        toolkit_name: String,
        toolkit_type: String,
        server_url: String,
        resource_metadata_url: Option<String>,
        www_authenticate: Option<String>,
    ) -> Option<Self> {
        let requirement = Self {
            toolkit_name,
            toolkit_type,
            server_url,
            resource_metadata_url,
            www_authenticate,
        };
        valid_requirement(&requirement).then_some(requirement)
    }

    pub(crate) fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    pub(crate) fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    pub(crate) fn server_url(&self) -> &str {
        &self.server_url
    }

    pub(crate) fn resource_metadata_url(&self) -> Option<&str> {
        self.resource_metadata_url.as_deref()
    }

    pub(crate) fn www_authenticate(&self) -> Option<&str> {
        self.www_authenticate.as_deref()
    }

    /// The notice shown when the requirement is discovered while the agent is
    /// being ASSEMBLED, before any tool call exists to authorize against.
    ///
    /// It differs from [`Self::user_message`] on purpose: there is no
    /// authorize/skip pair to offer here, so the text must say which connection
    /// to authorize and what to do afterwards rather than name buttons that are
    /// not on screen.
    pub(crate) fn assembly_notice_message(&self) -> String {
        let family = if self.toolkit_type == "mcp" {
            "MCP"
        } else {
            "toolkit"
        };
        format!(
            "The {family} connection \u{201c}{}\u{201d} ({}) requires authorization before it can be used. Authorize that connection, then send your message again.",
            self.toolkit_name, self.server_url
        )
    }

    pub(crate) fn user_message(&self) -> String {
        let family = if self.toolkit_type == "mcp" {
            "MCP toolkit"
        } else {
            "toolkit"
        };
        format!(
            "Authorization is required to use the {} {family}. Choose Authorize to sign in, or Skip to stop this pipeline safely.",
            self.toolkit_name
        )
    }
}

/// SDK-compatible structured result used to close the original model tool
/// call when the user declines delegated authorization.
pub(crate) fn delegated_authorization_declined_result(
    requirement: &DelegatedAuthorizationRequirement,
    tool_name: &str,
) -> Value {
    json!({
        "type": "mcp_auth_decision",
        "status": "declined",
        "server_url": requirement.server_url(),
        "tool_name": tool_name,
        "toolkit_type": requirement.toolkit_type(),
        "message": format!(
            "Authorization for the {} toolkit was declined; the requested tool was not executed.",
            requirement.toolkit_name()
        ),
        "next_step": "Do not retry this toolkit unless the user explicitly asks to authorize it.",
        "auth_context": {
            "resource_metadata_url": requirement.resource_metadata_url(),
            "www_authenticate": requirement.www_authenticate(),
            "resource_metadata": Value::Null,
        },
        "denial_reason": "user_declined",
    })
}

pub(crate) fn delegated_authorization_requirement(
    error: &AdkError,
) -> Option<DelegatedAuthorizationRequirement> {
    if error.code != DELEGATED_AUTHORIZATION_ERROR_CODE {
        return None;
    }
    error
        .details
        .metadata
        .get(DELEGATED_AUTHORIZATION_METADATA_KEY)
        .cloned()
        .and_then(|value| serde_json::from_value(value).ok())
        .filter(valid_requirement)
}

pub(crate) fn encode_delegated_authorization_requirement(
    requirement: &DelegatedAuthorizationRequirement,
) -> Option<String> {
    serde_json::to_string(requirement).ok()
}

pub(crate) fn decode_delegated_authorization_requirement(
    value: &str,
) -> Option<DelegatedAuthorizationRequirement> {
    serde_json::from_str(value).ok().filter(valid_requirement)
}

pub(super) fn preserve_delegated_authorization_error(error: &AdkError) -> Option<AdkError> {
    let requirement = delegated_authorization_requirement(error)?;
    if error.component != ErrorComponent::Auth
        || error.category != ErrorCategory::Unauthorized
        || error.details.metadata.len() != 1
    {
        return None;
    }
    Some(delegated_authorization_error(&requirement))
}

pub(crate) fn delegated_authorization_error(
    requirement: &DelegatedAuthorizationRequirement,
) -> AdkError {
    let metadata = serde_json::to_value(requirement).unwrap_or(serde_json::Value::Null);
    let mut details = ErrorDetails::default();
    details
        .metadata
        .insert(DELEGATED_AUTHORIZATION_METADATA_KEY.to_owned(), metadata);
    AdkError::unauthorized(
        ErrorComponent::Auth,
        DELEGATED_AUTHORIZATION_ERROR_CODE,
        "the toolkit operation requires delegated authorization",
    )
    .with_details(details)
}

fn valid_requirement(requirement: &DelegatedAuthorizationRequirement) -> bool {
    if !valid_identity(requirement.toolkit_name())
        || !valid_identity(requirement.toolkit_type())
        || valid_https_url(requirement.server_url()).is_none()
        || requirement.www_authenticate().is_some_and(|challenge| {
            challenge.is_empty()
                || challenge.len() > MAX_AUTH_CHALLENGE_BYTES
                || challenge.chars().any(char::is_control)
        })
    {
        return false;
    }
    requirement
        .resource_metadata_url()
        .is_none_or(|url| valid_https_url(url).is_some())
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_TOOLKIT_IDENTITY_BYTES
        && !value.chars().any(char::is_control)
}

fn valid_https_url(value: &str) -> Option<reqwest::Url> {
    let parsed = reqwest::Url::parse(value).ok()?;
    (parsed.scheme() == "https"
        && parsed.host_str().is_some()
        && parsed.username().is_empty()
        && parsed.password().is_none()
        && parsed.fragment().is_none())
    .then_some(parsed)
}

#[cfg(test)]
pub(crate) fn delegated_authorization_error_fixture(toolkit_type: &str) -> AdkError {
    let (server_url, resource_metadata_url) = if toolkit_type == "sharepoint" {
        (
            "https://tenant.sharepoint.example.invalid/sites/support",
            "https://login.microsoftonline.example.invalid/tenant/v2.0/.well-known/openid-configuration",
        )
    } else {
        (
            "https://mcp.example.invalid/v1/mcp",
            "https://mcp.example.invalid/.well-known/oauth-protected-resource",
        )
    };
    delegated_authorization_error(
        &DelegatedAuthorizationRequirement::new(
            "Customer Support".to_owned(),
            toolkit_type.to_owned(),
            server_url.to_owned(),
            Some(resource_metadata_url.to_owned()),
            Some(format!(
                "Bearer resource_metadata=\"{resource_metadata_url}\""
            )),
        )
        .expect("authorization fixture"),
    )
}
