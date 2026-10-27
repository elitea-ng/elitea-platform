//! `SharePoint`: `oauth_delegated` only (a Microsoft Graph access token, an
//! Entra on-behalf-of exchange on Main, #1117). Target: the configured site
//! URL with its site path applied.
//!
//! The catalogue names sites, lists and files. List writes, `OneNote` and
//! `read_file_from_sharing_link` (a link can name a file on another site)
//! have no governed operation and are not mapped.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value, json};

use super::atlassian::row;
use super::{GOVERNED_UNUSED_SECRET, with_credential};
use crate::toolkits::governed::{
    AuthAdapter, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError, TargetKind,
    ToolOperation, admit_material, canonical_target,
};
use crate::toolkits::materialize::governed_family_toolset;

// Each row binds the ONE argument the family reads for the resource it
// touches; `None` where that resource cannot be told from the arguments.
const SHAREPOINT_TOOLS: &[ToolOperation] = &[
    row("get_lists", "sharepoint.site.read", "site", None),
    row(
        "read_list",
        "sharepoint.list.read",
        "list",
        Some("list_title"),
    ),
    row(
        "get_list_columns",
        "sharepoint.list.read",
        "list",
        Some("list_title"),
    ),
    // A form filter lists a form library's files, not the folder's.
    row(
        "get_files_list",
        "sharepoint.file.read",
        "file",
        Some("folder_name"),
    )
    .unbound_with(&["form_name"]),
    row(
        "read_document",
        "sharepoint.file.read",
        "file",
        Some("path"),
    ),
    // Writes `folder_path` + `filename` (or the source file's name): a new
    // file whose path no single argument names.
    row("upload_file", "sharepoint.file.write", "file", None),
];

/// The `SharePoint` adapter.
pub struct SharePointAdapter;

impl AuthAdapter for SharePointAdapter {
    fn provider_type(&self) -> &'static str {
        "sharepoint"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::TenantSite
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        match class {
            CredentialClass::StaticSecret => &[],
            CredentialClass::OauthDelegated => &["access_token"],
        }
    }

    fn probe_operation(&self) -> &'static str {
        "sharepoint.site.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        SHAREPOINT_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        let site_url = settings
            .get("sharepoint_configuration")
            .and_then(|value| value.get("site_url"))
            .and_then(Value::as_str)
            .ok_or(GovernedError::InvalidConfiguration)?;
        let site_path = match settings.get("site_path") {
            None | Some(Value::Null) => None,
            Some(Value::String(path)) => Some(path.as_str()),
            Some(_) => return Err(GovernedError::InvalidConfiguration),
        };
        let url = elitea_connectors::sharepoint_site::resolve_site_url(site_url, site_path)
            .ok_or(GovernedError::InvalidConfiguration)?;
        Ok(canonical_target(&url))
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        admit_material(self, envelope)?;
        let token = envelope
            .material("access_token")
            .ok_or(GovernedError::MaterialNotAllowed)?;
        // The family insists on a client secret it never uses for a
        // delegated token: whatever the settings carry is replaced.
        let settings = with_credential(
            build.settings(),
            "sharepoint_configuration",
            &["client_secret"],
            &[("client_secret", GOVERNED_UNUSED_SECRET)],
        )?;
        // The only token the family can resolve is the envelope's, under the
        // canonical site (one of its lookup keys).
        let tokens = Map::from_iter([(
            envelope.target_identity().to_owned(),
            json!({ "access_token": token }),
        )]);
        governed_family_toolset("sharepoint", build, &settings, &tokens)
    }
}
