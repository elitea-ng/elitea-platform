//! Sanitized delegated-authorization control signal shared by tool families.
//!
//! The current SDK names the browser contract `mcp_auth`, but the same
//! `McpAuthorizationRequired` signal is raised by ordinary configured toolkits
//! such as delegated `SharePoint`. Keeping this type outside the MCP transport
//! prevents direct graph nodes from depending on how the guarded tool was
//! materialized. Concrete toolkit families remain responsible for resolving
//! claim-fetched tokens into their own clients during the continuation rebuild.

use std::collections::{BTreeMap, BTreeSet};

use adk_rust::{AdkError, ErrorCategory, ErrorComponent, ErrorDetails};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::tool_binding::ToolBindingPlan;

mod discovery;
mod model_tools;

pub(crate) use model_tools::{bind_authorization_model_tools, hide_model_tools};

const MAX_AUTH_CHALLENGE_BYTES: usize = 16 * 1_024;
const MAX_AUTH_METADATA_BYTES: usize = 64 * 1_024;
const MAX_AUTH_METADATA_LIST_ITEMS: usize = 64;
const MAX_AUTH_METADATA_STRING_BYTES: usize = 4 * 1_024;
const MAX_TOOLKIT_IDENTITY_BYTES: usize = 1_024;

/// Stable private ADK error code used to carry the current `mcp_auth` control
/// signal through policy wrappers without exposing a provider error body.
pub(crate) const DELEGATED_AUTHORIZATION_ERROR_CODE: &str = "mcp.authorization_required";
pub(crate) const DELEGATED_AUTHORIZATION_METADATA_KEY: &str = "elitea.delegated-authorization.v1";
pub(crate) const DELEGATED_AUTHORIZATION_SCOPE_KEY: &str =
    "elitea.delegated-authorization-scope.v1";

/// Model-callable tools that are placeholders for one delegated authorization
/// boundary. The catalog is built together with the invocation's toolsets and
/// never contains tokens, tool arguments or provider response bodies.
#[derive(Clone, Default)]
pub(crate) struct DelegatedAuthorizationCatalog {
    scoped_requirements: BTreeMap<DelegatedToolIdentity, DelegatedAuthorizationRequirement>,
    // A server may reject discovery before any operation names are known.
    // These requirements authorize discovery, not an invented remote operation.
    discovery_requirements: BTreeMap<String, DelegatedAuthorizationRequirement>,
    provider_requirements: BTreeMap<String, DelegatedAuthorizationRequirement>,
    declined_tools: BTreeSet<String>,
}

#[derive(Clone, Eq, Ord, PartialEq, PartialOrd)]
struct DelegatedToolIdentity {
    toolkit_name: String,
    tool_name: String,
}

impl DelegatedAuthorizationCatalog {
    pub(crate) fn insert_discovery_requirement(
        &mut self,
        requirement: DelegatedAuthorizationRequirement,
    ) -> Result<(), ()> {
        let name = requirement.authorization_tool_name();
        match self.discovery_requirements.get(&name) {
            Some(existing) if existing == &requirement => Ok(()),
            Some(_) => Err(()),
            None => {
                self.discovery_requirements.insert(name, requirement);
                Ok(())
            }
        }
    }

    pub(crate) fn insert(
        &mut self,
        tool_name: &str,
        requirement: DelegatedAuthorizationRequirement,
    ) -> Result<(), ()> {
        if !valid_identity(tool_name) {
            return Err(());
        }
        let identity = DelegatedToolIdentity {
            toolkit_name: requirement.toolkit_name().to_owned(),
            tool_name: tool_name.to_owned(),
        };
        match self.scoped_requirements.get(&identity) {
            Some(existing) if existing == &requirement => Ok(()),
            Some(_) => Err(()),
            None => {
                self.scoped_requirements.insert(identity, requirement);
                self.rebuild_unqualified_provider_requirements();
                Ok(())
            }
        }
    }

    pub(crate) fn requirement_for(
        &self,
        tool_name: &str,
    ) -> Option<&DelegatedAuthorizationRequirement> {
        self.provider_requirements
            .get(tool_name)
            .or_else(|| self.discovery_requirements.get(tool_name))
            .or_else(|| {
                self.provider_requirements
                    .values()
                    .find(|requirement| requirement.authorization_tool_name() == tool_name)
            })
    }

    pub(crate) fn requirement_for_scoped(
        &self,
        toolkit_name: &str,
        tool_name: &str,
    ) -> Option<&DelegatedAuthorizationRequirement> {
        self.scoped_requirements.get(&DelegatedToolIdentity {
            toolkit_name: toolkit_name.to_owned(),
            tool_name: tool_name.to_owned(),
        })
    }

    pub(crate) fn tool_names(&self) -> impl Iterator<Item = &str> {
        self.provider_requirements
            .keys()
            .filter_map(|name| (!self.declined_tools.contains(name)).then_some(name.as_str()))
    }

    /// Apply an already validated decision to this invocation's exact toolkit.
    pub(crate) fn decline(&mut self, requirement: &DelegatedAuthorizationRequirement) {
        for (name, candidate) in &self.provider_requirements {
            if candidate.same_authority(requirement) {
                self.declined_tools.insert(name.clone());
            }
        }
        self.declined_tools
            .insert(requirement.authorization_tool_name());
    }

    pub(crate) fn is_declined(&self, tool_name: &str) -> bool {
        self.declined_tools.contains(tool_name)
    }

    pub(crate) fn encode_declined_scope(&self) -> Result<Option<String>, ()> {
        let requirements: BTreeMap<_, _> = self
            .provider_requirements
            .iter()
            .filter(|(name, _)| self.is_declined(name))
            .chain(
                self.discovery_requirements
                    .iter()
                    .filter(|(name, _)| self.is_declined(name)),
            )
            .map(|(_, requirement)| (requirement.authorization_tool_name(), requirement))
            .collect();
        if requirements.is_empty() {
            return Ok(None);
        }
        let encoded =
            serde_json::to_string(&requirements.values().collect::<Vec<_>>()).map_err(|_| ())?;
        decode_declined_authorization_scope(&encoded).ok_or(())?;
        Ok(Some(encoded))
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.scoped_requirements.is_empty() && self.discovery_requirements.is_empty()
    }

    pub(crate) fn merge(&mut self, other: Self) -> Result<(), ()> {
        for requirement in other.discovery_requirements.into_values() {
            self.insert_discovery_requirement(requirement)?;
        }
        for (identity, requirement) in other.scoped_requirements {
            match self.scoped_requirements.get(&identity) {
                Some(existing) if existing == &requirement => {}
                Some(_) => return Err(()),
                None => {
                    self.scoped_requirements.insert(identity, requirement);
                }
            }
        }
        self.rebuild_unqualified_provider_requirements();
        Ok(())
    }

    pub(crate) fn bind_provider_names(mut self, binding: &ToolBindingPlan) -> Result<Self, ()> {
        if binding
            .bindings()
            .any(|(_, _, name)| self.discovery_requirements.contains_key(name))
        {
            return Err(());
        }
        let mut provider_requirements = BTreeMap::new();
        for (identity, requirement) in &self.scoped_requirements {
            let provider_name = binding
                .provider_name(&identity.toolkit_name, &identity.tool_name)
                .ok_or(())?;
            if provider_requirements
                .insert(provider_name.to_owned(), requirement.clone())
                .is_some()
            {
                return Err(());
            }
        }
        self.provider_requirements = provider_requirements;
        Ok(self)
    }

    fn rebuild_unqualified_provider_requirements(&mut self) {
        let mut counts = BTreeMap::<&str, usize>::new();
        for identity in self.scoped_requirements.keys() {
            *counts.entry(identity.tool_name.as_str()).or_default() += 1;
        }
        self.provider_requirements = self
            .scoped_requirements
            .iter()
            .filter(|(identity, _)| counts.get(identity.tool_name.as_str()) == Some(&1))
            .map(|(identity, requirement)| (identity.tool_name.clone(), requirement.clone()))
            .collect();
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    resource_metadata: Option<Value>,
}

impl DelegatedAuthorizationRequirement {
    /// Stable model namespace for one frozen toolkit, independent of its tools.
    pub(crate) fn authorization_tool_name(&self) -> String {
        use std::fmt::Write as _;
        let identity = json!([
            self.toolkit_name,
            self.toolkit_type,
            self.server_url,
            discovery::configured_metadata(self.resource_metadata.as_ref())
        ]);
        let hash = ring::digest::digest(&ring::digest::SHA256, identity.to_string().as_bytes());
        let mut name = String::from("mcp_authorize_");
        for byte in &hash.as_ref()[..16] {
            let _ = write!(name, "{byte:02x}");
        }
        name
    }

    /// Admit a storage identity, not an egress URL. Resolution binds it exactly.
    pub(crate) fn valid_token_key(value: &str) -> bool {
        !value.is_empty()
            && value.len() <= MAX_AUTH_METADATA_STRING_BYTES
            && !value.chars().any(char::is_control)
    }

    /// Match the UI's configuration-scoped key against this frozen requirement.
    pub(crate) fn matches_token_key(&self, key: &str) -> bool {
        if key == self.server_url {
            return true;
        }
        let Some(metadata) = self.resource_metadata.as_ref() else {
            return false;
        };
        let Some(issuer) = metadata["authorization_servers"]
            .as_array()
            .and_then(|servers| servers.first())
            .and_then(Value::as_str)
        else {
            return false;
        };
        match metadata["configuration_uuid"].as_str() {
            Some(uuid) => {
                key.strip_prefix(uuid)
                    .and_then(|rest| rest.strip_prefix(':'))
                    == Some(issuer)
            }
            None => metadata.get("provided_settings").is_some() && key == issuer,
        }
    }

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
            resource_metadata: None,
        };
        valid_requirement(&requirement).then_some(requirement)
    }

    pub(crate) fn with_resource_metadata(mut self, resource_metadata: Value) -> Option<Self> {
        self.resource_metadata = Some(resource_metadata);
        valid_requirement(&self).then_some(self)
    }

    /// Compare the frozen authority, not the optional discovered display data.
    pub(crate) fn same_authority(&self, other: &Self) -> bool {
        self.toolkit_name == other.toolkit_name
            && self.toolkit_type == other.toolkit_type
            && self.server_url == other.server_url
            && self.resource_metadata_url == other.resource_metadata_url
            && self.www_authenticate == other.www_authenticate
            && discovery::configured_metadata(self.resource_metadata.as_ref())
                == discovery::configured_metadata(other.resource_metadata.as_ref())
    }

    pub(crate) fn with_toolkit_id(mut self, id: Option<u64>) -> Self {
        if let Some(id) = id
            && let Some(metadata) = self
                .resource_metadata
                .as_mut()
                .and_then(Value::as_object_mut)
        {
            metadata.insert("toolkit_id".to_owned(), Value::String(id.to_string()));
        }
        self
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

    pub(crate) fn resource_metadata(&self) -> Option<&Value> {
        self.resource_metadata.as_ref()
    }

    pub(crate) fn authorization_servers(&self) -> Option<&[Value]> {
        self.resource_metadata()
            .and_then(Value::as_object)
            .and_then(|metadata| metadata.get("authorization_servers"))
            .and_then(Value::as_array)
            .map(Vec::as_slice)
    }

    pub(crate) fn user_message(&self) -> String {
        let family = if self.toolkit_type == "mcp" {
            "MCP toolkit"
        } else {
            "toolkit"
        };
        format!(
            "Authorization is required to use the {} {family}. Choose Authorize to sign in, or Skip to leave this tool unused.",
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
        "scope": "current_run",
        "tool_name": tool_name,
        "toolkit_name": requirement.toolkit_name(),
        "toolkit_type": requirement.toolkit_type(),
        "message": format!(
            "The user skipped the {} toolkit for this run. No protected operation was executed. Use other tools or report the limitation. A later user turn can request this toolkit again through its authorization tool.",
            requirement.toolkit_name()
        ),
        "next_step": "use_other_tools_or_report",
        "denial_reason": "user_declined",
    })
}

pub(crate) fn delegated_authorization_granted_result(
    requirement: &DelegatedAuthorizationRequirement,
    tool_name: &str,
) -> Value {
    json!({
        "type": "mcp_auth_decision",
        "status": "authorized",
        "tool_name": tool_name,
        "toolkit_type": requirement.toolkit_type(),
        "message": "Authorization is available. Use the toolkit operations to continue the original task.",
        "next_step": "use_authorized_tools",
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

pub(crate) fn decode_declined_authorization_scope(
    value: &str,
) -> Option<Vec<DelegatedAuthorizationRequirement>> {
    if value.len() > MAX_AUTH_METADATA_BYTES {
        return None;
    }
    let requirements: Vec<DelegatedAuthorizationRequirement> = serde_json::from_str(value).ok()?;
    (requirements.len() <= MAX_AUTH_METADATA_LIST_ITEMS
        && requirements.iter().all(valid_requirement))
    .then_some(requirements)
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
        && requirement
            .resource_metadata()
            .is_none_or(valid_resource_metadata)
}

fn valid_resource_metadata(value: &Value) -> bool {
    if serde_json::to_vec(value)
        .ok()
        .is_none_or(|encoded| encoded.len() > MAX_AUTH_METADATA_BYTES)
    {
        return false;
    }
    let Some(metadata) = value.as_object() else {
        return false;
    };
    if metadata.keys().any(|key| {
        !matches!(
            key.as_str(),
            "authorization_servers"
                | "oauth_authorization_server"
                | "scopes_supported"
                | "resource_name"
                | "configuration_uuid"
                | "toolkit_id"
                | "provided_settings"
        )
    }) {
        return false;
    }
    let Some(servers) = metadata
        .get("authorization_servers")
        .and_then(Value::as_array)
        .filter(|servers| !servers.is_empty() && servers.len() <= MAX_AUTH_METADATA_LIST_ITEMS)
    else {
        return false;
    };
    if !valid_url_list(servers) {
        return false;
    }
    if !discovery::valid_configured_metadata(metadata) {
        return false;
    }
    if metadata
        .get("scopes_supported")
        .is_some_and(|scopes| !valid_string_list(scopes))
    {
        return false;
    }
    let Some(server) = metadata.get("oauth_authorization_server") else {
        // A configured toolkit can defer public discovery until its guard runs.
        return metadata.contains_key("provided_settings");
    };
    let Some(server) = server.as_object() else {
        return false;
    };
    valid_authorization_server(server)
}

fn valid_authorization_server(server: &serde_json::Map<String, Value>) -> bool {
    if server.keys().any(|key| {
        !matches!(
            key.as_str(),
            "authorization_endpoint"
                | "token_endpoint"
                | "registration_endpoint"
                | "issuer"
                | "jwks_uri"
                | "revocation_endpoint"
                | "userinfo_endpoint"
                | "scopes_supported"
                | "response_types_supported"
                | "grant_types_supported"
                | "token_endpoint_auth_methods_supported"
                | "code_challenge_methods_supported"
        )
    }) {
        return false;
    }
    for required in ["authorization_endpoint", "token_endpoint"] {
        if server
            .get(required)
            .and_then(Value::as_str)
            .and_then(valid_https_url)
            .is_none()
        {
            return false;
        }
    }
    for optional in [
        "registration_endpoint",
        "issuer",
        "jwks_uri",
        "revocation_endpoint",
        "userinfo_endpoint",
    ] {
        if server
            .get(optional)
            .is_some_and(|url| url.as_str().and_then(valid_https_url).is_none())
        {
            return false;
        }
    }
    for list in [
        "scopes_supported",
        "response_types_supported",
        "grant_types_supported",
        "token_endpoint_auth_methods_supported",
        "code_challenge_methods_supported",
    ] {
        if server
            .get(list)
            .is_some_and(|values| !valid_string_list(values))
        {
            return false;
        }
    }
    true
}

fn valid_url_list(values: &[Value]) -> bool {
    values.iter().all(|value| {
        value
            .as_str()
            .filter(|value| value.len() <= MAX_AUTH_METADATA_STRING_BYTES)
            .and_then(valid_https_url)
            .is_some()
    })
}

fn valid_string_list(value: &Value) -> bool {
    value.as_array().is_some_and(|values| {
        !values.is_empty()
            && values.len() <= MAX_AUTH_METADATA_LIST_ITEMS
            && values.iter().all(|value| {
                value.as_str().is_some_and(|value| {
                    !value.is_empty()
                        && value.len() <= MAX_AUTH_METADATA_STRING_BYTES
                        && !value.chars().any(char::is_control)
                })
            })
    })
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

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::sync::Arc;

    use adk_rust::tool::{BasicToolset, FunctionTool};
    use adk_rust::{Tool, Toolset};
    use serde_json::{Value, json};

    use super::{
        DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement,
        decode_delegated_authorization_requirement, encode_delegated_authorization_requirement,
    };
    use crate::toolkits::bind_toolsets;

    #[tokio::test]
    async fn provider_aliases_retain_exact_delegated_authorization_identity() {
        let mut catalog = DelegatedAuthorizationCatalog::default();
        let mut toolsets = Vec::new();
        for (toolkit_name, endpoint) in [
            (
                "release intelligence",
                "https://release.example.invalid/mcp",
            ),
            ("audit intelligence", "https://audit.example.invalid/mcp"),
        ] {
            let requirement = DelegatedAuthorizationRequirement::new(
                toolkit_name.to_owned(),
                "mcp".to_owned(),
                endpoint.to_owned(),
                None,
                None,
            )
            .expect("delegated authorization fixture");
            catalog
                .insert("lookup", requirement)
                .expect("scoped authorization requirement");
            let tool: Arc<dyn Tool> = Arc::new(FunctionTool::new(
                "lookup",
                "Read one exact source",
                |_context, _arguments| async { Ok(json!({})) },
            ));
            toolsets
                .push(Arc::new(BasicToolset::new(toolkit_name, vec![tool])) as Arc<dyn Toolset>);
        }
        let binding = bind_toolsets(toolsets, &BTreeSet::new(), "authorization_alias_test")
            .await
            .expect("provider binding");
        let catalog = catalog
            .bind_provider_names(&binding)
            .expect("bound authorization catalog");
        assert_eq!(
            catalog
                .requirement_for("release_intelligence__lookup")
                .map(DelegatedAuthorizationRequirement::toolkit_name),
            Some("release intelligence")
        );
        assert_eq!(
            catalog
                .requirement_for("audit_intelligence__lookup")
                .map(DelegatedAuthorizationRequirement::toolkit_name),
            Some("audit intelligence")
        );
        assert!(catalog.requirement_for("lookup").is_none());
    }

    #[test]
    fn authorization_metadata_round_trip_preserves_only_the_validated_shape() {
        let requirement = DelegatedAuthorizationRequirement::new(
            "release intelligence".to_owned(),
            "mcp".to_owned(),
            "https://mcp.example.invalid/v1/mcp".to_owned(),
            Some("https://mcp.example.invalid/.well-known/oauth-protected-resource".to_owned()),
            None,
        )
        .expect("base requirement")
        .with_resource_metadata(json!({
            "authorization_servers": ["https://login.example.invalid"],
            "oauth_authorization_server": {
                "issuer": "https://login.example.invalid",
                "authorization_endpoint": "https://login.example.invalid/authorize",
                "token_endpoint": "https://login.example.invalid/token",
                "registration_endpoint": "https://login.example.invalid/register",
                "grant_types_supported": ["authorization_code", "refresh_token"],
                "code_challenge_methods_supported": ["S256"]
            },
            "scopes_supported": ["mcp:read"]
        }))
        .expect("validated resource metadata");

        let encoded = encode_delegated_authorization_requirement(&requirement)
            .expect("encoded delegated authorization");
        let decoded = decode_delegated_authorization_requirement(&encoded)
            .expect("decoded delegated authorization");
        assert!(decoded == requirement);
        assert_eq!(
            decoded
                .authorization_servers()
                .and_then(|servers| servers.first())
                .and_then(Value::as_str),
            Some("https://login.example.invalid")
        );
    }

    #[test]
    fn authorization_metadata_rejects_unowned_or_unsafe_fields() {
        let base = || {
            DelegatedAuthorizationRequirement::new(
                "release intelligence".to_owned(),
                "mcp".to_owned(),
                "https://mcp.example.invalid/v1/mcp".to_owned(),
                None,
                None,
            )
            .expect("base requirement")
        };
        for invalid in [
            json!({
                "authorization_servers": ["https://login.example.invalid"],
                "oauth_authorization_server": {
                    "authorization_endpoint": "https://login.example.invalid/authorize",
                    "token_endpoint": "https://login.example.invalid/token",
                    "client_secret": "must-not-cross-the-boundary"
                }
            }),
            json!({
                "authorization_servers": ["http://login.example.invalid"],
                "oauth_authorization_server": {
                    "authorization_endpoint": "https://login.example.invalid/authorize",
                    "token_endpoint": "https://login.example.invalid/token"
                }
            }),
            json!({
                "authorization_servers": [],
                "oauth_authorization_server": {
                    "authorization_endpoint": "https://login.example.invalid/authorize",
                    "token_endpoint": "https://login.example.invalid/token"
                }
            }),
        ] {
            assert!(base().with_resource_metadata(invalid).is_none());
        }
    }
}
