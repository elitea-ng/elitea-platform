//! qTest: `static_secret` only (an API token; qTest has no delegated
//! OAuth). Target: the configured site base URL.
//!
//! The catalogue names projects and test cases. Test runs, defects,
//! requirements read on their own and the generic entity tools have no
//! governed operation and are not mapped.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value};

use super::atlassian::row;
use super::{base_url_target, with_credential};
use crate::toolkits::families::https_base_url::BasePath;
use crate::toolkits::governed::{
    AuthAdapter, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError, TargetKind,
    ToolOperation, admit_material,
};
use crate::toolkits::materialize::governed_family_toolset;

const TEST_CASE: &[&str] = &["test_case_id", "id"];

const QTEST_TOOLS: &[ToolOperation] = &[
    row("get_modules", "qtest.project.read", "project", &[]),
    row(
        "get_all_test_cases_fields_for_project",
        "qtest.project.read",
        "project",
        &[],
    ),
    row(
        "find_test_case_by_id",
        "qtest.test_case.read",
        "test_case",
        TEST_CASE,
    ),
    row(
        "get_test_case_versions",
        "qtest.test_case.read",
        "test_case",
        TEST_CASE,
    ),
    row("search_by_dql", "qtest.test_case.search", "project", &[]),
    row(
        "find_test_cases_by_requirement_id",
        "qtest.test_case.search",
        "project",
        &[],
    ),
    row(
        "create_test_cases",
        "qtest.test_case.write",
        "test_case",
        &[],
    ),
    row(
        "update_test_case",
        "qtest.test_case.write",
        "test_case",
        TEST_CASE,
    ),
    row(
        "delete_test_case",
        "qtest.test_case.write",
        "test_case",
        TEST_CASE,
    ),
    row(
        "add_file_to_test_case",
        "qtest.test_case.write",
        "test_case",
        TEST_CASE,
    ),
    row(
        "link_tests_to_jira_requirement",
        "qtest.test_case.write",
        "test_case",
        &[],
    ),
    row(
        "link_tests_to_qtest_requirement",
        "qtest.test_case.write",
        "test_case",
        &[],
    ),
];

/// The qTest adapter.
pub struct QtestAdapter;

impl AuthAdapter for QtestAdapter {
    fn provider_type(&self) -> &'static str {
        "qtest"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::BaseUrl
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        match class {
            CredentialClass::StaticSecret => &["token"],
            CredentialClass::OauthDelegated => &[],
        }
    }

    fn probe_operation(&self) -> &'static str {
        "qtest.project.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        QTEST_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        base_url_target(settings, "qtest_configuration", BasePath::Prefix)
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        admit_material(self, envelope)?;
        let token = envelope
            .material("token")
            .ok_or(GovernedError::MaterialNotAllowed)?;
        let settings = with_credential(
            build.settings(),
            "qtest_configuration",
            &["qtest_api_token", "token"],
            &[("qtest_api_token", token)],
        )?;
        governed_family_toolset("qtest", build, &settings, &Map::new())
    }
}
