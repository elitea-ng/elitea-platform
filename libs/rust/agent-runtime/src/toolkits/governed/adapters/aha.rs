//! Aha!: `static_secret` (an API key) and `oauth_delegated` (an Aha! `OAuth2`
//! access token); both are a Bearer credential. Target: the account origin.
//!
//! The catalogue names products and features only. The feature-specific
//! tools map directly; the generic record tools map only when the kind field
//! the family reads (`record_type`, or `resource_type` for comments) is a
//! feature, and are not mapped otherwise: a release, idea or initiative has
//! no governed operation, and a call whose other kind field disagrees is
//! ambiguous.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value};

use super::atlassian::row;
use super::{base_url_target, with_credential};
use crate::toolkits::families::https_base_url::BasePath;
use crate::toolkits::governed::{
    AuthAdapter, BoundOperation, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError,
    OperationNotMapped, TargetKind, ToolOperation, admit_material, table_operation,
};
use crate::toolkits::materialize::governed_family_toolset;

// Each row binds the ONE argument the family reads for the resource it
// touches; `None` where that resource cannot be told from the arguments.
const AHA_TOOLS: &[ToolOperation] = &[
    row("list_products", "aha.product.read", "product", None),
    row(
        "get_product",
        "aha.product.read",
        "product",
        Some("reference_or_id"),
    ),
    row("find_project", "aha.product.read", "product", None),
    row(
        "get_feature",
        "aha.feature.read",
        "feature",
        Some("reference_or_id"),
    ),
    row(
        "get_feature_gql",
        "aha.feature.read",
        "feature",
        Some("reference"),
    ),
    row(
        "get_requirement",
        "aha.feature.read",
        "feature",
        Some("reference_or_id"),
    ),
    row(
        "get_requirement_gql",
        "aha.feature.read",
        "feature",
        Some("reference"),
    ),
    // A release filter lists another record's features.
    row(
        "list_features",
        "aha.feature.search",
        "product",
        Some("product_id"),
    )
    .unbound_with(&["release_id"]),
    // Lists a feature's requirements: not a product the search names.
    row("list_requirements", "aha.feature.search", "product", None),
    // The generic record tools, when the record is a feature
    // (`AhaAdapter::operation_for`).
    row(
        "read_records",
        "aha.feature.read",
        "feature",
        Some("reference_or_id"),
    ),
    row("search_records", "aha.feature.search", "product", None),
    row(
        "list_comments",
        "aha.feature.read",
        "feature",
        Some("resource_id"),
    ),
    row("create_record", "aha.feature.write", "feature", None),
    row(
        "update_record",
        "aha.feature.write",
        "feature",
        Some("record_id"),
    ),
    row(
        "delete_record",
        "aha.feature.write",
        "feature",
        Some("record_id"),
    ),
    row(
        "manage_record",
        "aha.feature.write",
        "feature",
        Some("record_id"),
    ),
    row(
        "copy_record",
        "aha.feature.write",
        "feature",
        Some("record_id"),
    ),
    row(
        "add_comment",
        "aha.feature.write",
        "feature",
        Some("resource_id"),
    ),
];

/// The tools that act on whatever record kind `record_type` names.
const RECORD_TYPE_TOOLS: [&str; 7] = [
    "read_records",
    "search_records",
    "create_record",
    "update_record",
    "delete_record",
    "manage_record",
    "copy_record",
];

/// The tools that act on whatever record kind `resource_type` names.
const RESOURCE_TYPE_TOOLS: [&str; 2] = ["list_comments", "add_comment"];

/// The Aha! adapter.
pub struct AhaAdapter;

impl AuthAdapter for AhaAdapter {
    fn provider_type(&self) -> &'static str {
        "aha"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::BaseUrl
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        match class {
            CredentialClass::StaticSecret => &["token"],
            CredentialClass::OauthDelegated => &["access_token"],
        }
    }

    fn probe_operation(&self) -> &'static str {
        "aha.product.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        AHA_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        base_url_target(settings, "aha_configuration", BasePath::OriginOnly)
    }

    fn operation_for(
        &self,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<BoundOperation, OperationNotMapped> {
        let kind_field = if RECORD_TYPE_TOOLS.contains(&tool_name) {
            Some(("record_type", "resource_type"))
        } else if RESOURCE_TYPE_TOOLS.contains(&tool_name) {
            Some(("resource_type", "record_type"))
        } else {
            None
        };
        if let Some((read, other)) = kind_field
            && !names_feature(arguments, read, other)
        {
            return Err(OperationNotMapped);
        }
        table_operation(self.provider_type(), AHA_TOOLS, tool_name, arguments)
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        let key = match admit_material(self, envelope)? {
            CredentialClass::StaticSecret => envelope.material("token"),
            CredentialClass::OauthDelegated => envelope.material("access_token"),
        }
        .ok_or(GovernedError::MaterialNotAllowed)?;
        let settings = with_credential(
            build.settings(),
            "aha_configuration",
            &["api_key", "token", "access_token"],
            &[("api_key", key)],
        )?;
        governed_family_toolset("aha", build, &settings, &Map::new())
    }
}

/// Whether a generic record call names a feature (or a requirement, a
/// feature's child) in `read`, the field the family reads for the record
/// kind. A call whose `other` kind field names a different kind is refused:
/// the record the call touches must not be ambiguous.
fn names_feature(arguments: &Value, read: &str, other: &str) -> bool {
    let kind = |name: &str| {
        arguments
            .get(name)
            .filter(|value| !value.is_null())
            .map(|value| {
                value
                    .as_str()
                    .map(|kind| {
                        let kind = kind.trim().to_ascii_lowercase();
                        kind.strip_suffix('s')
                            .map_or_else(|| kind.clone(), str::to_owned)
                    })
                    .unwrap_or_default()
            })
    };
    let Some(read_kind) = kind(read) else {
        return false;
    };
    if kind(other).is_some_and(|other_kind| other_kind != read_kind) {
        return false;
    }
    matches!(read_kind.as_str(), "feature" | "requirement")
}
