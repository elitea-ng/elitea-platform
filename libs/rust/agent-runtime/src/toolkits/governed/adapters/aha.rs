//! Aha!: `static_secret` (an API key) and `oauth_delegated` (an Aha! `OAuth2`
//! access token); both are a Bearer credential. Target: the account origin.
//!
//! The catalogue names products and features only. The feature-specific
//! tools map directly; the generic record tools map only when their
//! `record_type` (or `resource_type`) is a feature, and are not mapped
//! otherwise: a release, idea or initiative has no governed operation.

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

const FEATURE: &[&str] = &["feature_id", "reference", "record_id", "reference_or_id"];
const PRODUCT: &[&str] = &["product_id", "id"];
const RESOURCE: &[&str] = &["resource_id"];

const AHA_TOOLS: &[ToolOperation] = &[
    row("list_products", "aha.product.read", "product", &[]),
    row("get_product", "aha.product.read", "product", PRODUCT),
    row("find_project", "aha.product.read", "product", &[]),
    row("get_feature", "aha.feature.read", "feature", FEATURE),
    row("get_feature_gql", "aha.feature.read", "feature", FEATURE),
    row("get_requirement", "aha.feature.read", "feature", FEATURE),
    row(
        "get_requirement_gql",
        "aha.feature.read",
        "feature",
        FEATURE,
    ),
    row("list_features", "aha.feature.search", "product", PRODUCT),
    row(
        "list_requirements",
        "aha.feature.search",
        "product",
        PRODUCT,
    ),
    // The generic record tools, when the record is a feature
    // (`AhaAdapter::operation_for`).
    row("read_records", "aha.feature.read", "feature", FEATURE),
    row("search_records", "aha.feature.search", "product", PRODUCT),
    row("list_comments", "aha.feature.read", "feature", RESOURCE),
    row("create_record", "aha.feature.write", "feature", &[]),
    row("update_record", "aha.feature.write", "feature", FEATURE),
    row("delete_record", "aha.feature.write", "feature", FEATURE),
    row("manage_record", "aha.feature.write", "feature", FEATURE),
    row("copy_record", "aha.feature.write", "feature", FEATURE),
    row("add_comment", "aha.feature.write", "feature", RESOURCE),
];

/// The tools that act on whatever record kind their arguments name.
const GENERIC_RECORD_TOOLS: [&str; 8] = [
    "read_records",
    "search_records",
    "create_record",
    "update_record",
    "delete_record",
    "manage_record",
    "copy_record",
    "list_comments",
];

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
        let generic = GENERIC_RECORD_TOOLS.contains(&tool_name) || tool_name == "add_comment";
        if generic && !names_feature(arguments) {
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
/// feature's child) as its record or comment resource.
fn names_feature(arguments: &Value) -> bool {
    ["record_type", "resource_type"]
        .iter()
        .filter_map(|name| arguments.get(*name).and_then(Value::as_str))
        .any(|kind| {
            matches!(
                kind.trim().to_ascii_lowercase().as_str(),
                "feature" | "features" | "requirement" | "requirements"
            )
        })
}
