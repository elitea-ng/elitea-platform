use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, UNIX_EPOCH};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use reqwest::StatusCode;
use reqwest::header::{AUTHORIZATION, HeaderMap, HeaderValue, RETRY_AFTER};
use serde_json::{Map, Value, json};

use super::families::github::client::{
    GitHubApi, GitHubClient, GitHubClientError, GitHubClientErrorCode, GitHubCodeSearchQuery,
    GitHubCommitQuery, GitHubFileScope, GitHubRequestKind, test_map_status, test_project_branches,
    test_project_issue_detail, test_project_issue_list, test_project_issue_search,
    test_project_text_file, test_project_tree_files, test_project_tree_sha, test_project_user,
    test_validate_file_path,
};
use super::families::github::code_search::{
    test_project_code_search, test_scope_code_search_query,
};
use super::families::github::commits::{
    test_project_commit_changes, test_project_commit_comparison, test_project_commit_list,
};
use super::families::github::config::{GitHubAuthKind, GitHubToolkitConfig};
use super::families::github::projects::{test_project_project_issues, test_project_query_payload};
use super::families::github::pull_requests::{
    test_project_pull_request_detail, test_project_pull_request_files,
    test_project_pull_request_list,
};
use super::families::github::tools::{
    GitHubToolsetErrorCode, build_github_read_only_toolset, test_build_with_api,
};
use super::families::github::workflow_runs::test_project_workflow_status;
use super::materialize::materialize_configured_toolsets_with_tokens_and_authorization;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;

const TEST_PKCS8_KEY_BODY: &str = "MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDIp4UApaJQ247TbIW43Pg8S+GVMRT6qsdhbg6iSSL6a3qwH4VYLIFcw73rXtRnYrxTasyqi3JwWwDO8xay7FCPuWlyQbnjQjhBnMz3M57riwYhR69PWTL2E9m8CucL9tVtRDLoPhN2dYdTG/qd1WUxdBJEvnXovJImufpEtLihATWNfou3XQxySk8R7Od3diY/rv55YS6x1xZG536JgoZr4UAOr8NYDTE5tBqqc4AYc3LyLjW9VbKISWFlyIHtFU1YESRcUtVswJ1JFtTypQvPWuCiY39M+mv52q/BE9uoODtt19pt2Nsi2FEKjTEVmDMIkJoaAzJReqVeiW4VQkmzAgMBAAECggEAI6TukZDa5rY6BwDOOGq4hi2Moy4W5fiUdpBQdS+80PNq1gKjc2hkipATGs67uKnnfoIIXXtsFt1zpU+1ho9IOF/dhXh7hw1qZO1v07IN1xXZPuw3DkdwMBqSoT7mkE+G1mQ5DtyIJJD4OyFLQeJ4mXJfFGspEvD8nXiIJtBbw+3cMzbUJRYwTWfTxIHfkq7uuXUs1zn3hGm1Ku3WIQo/e3+y1eiecSTqJqrGGWLtZjB6689c59RI0leT6jM4tizOIQ3BkUXAetn/HRFbKZRcNFhh0e7+G6QIVTFX/wXHbLZsJWkPzHxNX2USoWqgpnmgiGZSGTbAt/CJ492NeX0K8QKBgQD4W6jcKVAjlu6SKrhVlhO8RdjYs4IC+Mi4/1eyhvCtgtPhrHxWb/5zHPrlYZrt3E5rdhvcshNkcOM9cS1MxwPCnJshs+eWnjXwkl+tWy3ceroc21xAhu9XHrPqNLuyX04YHV/B0Rg23aC+/C8aQmikq30yeLxFpTiz0jQdSDgnFwKBgQDO1BfuiMQBoDRDYfUx3NfwJXcw5AX81U625OU5aOZc5WBC3I/F4W5S5r3D3CbsiunD+JGxxEuR+xFjSinxQkT9hQ/Vjp5PX53wJ1WmGQmM/VyBlSN6htfCR/Y8ra9nuUiV1qphlTrckdy1wY2VreK/RG3QZcFRlrlv+mFWGXaTxQKBgQC/yYCHq3uQUDChPU4mAYPyAvomtdBzXQ0cF0rwuVXIl9vpTNqjoU6cNEfntM0AW/1O7OEtN3LUQHyq6Ogzfwf/VBJUH2p6nGhJA6/Q3jV3Kmrod9kwl0LiQvpqpRhA8WoMIzrcIA0T6WgFtBbnr1rBtxAyVpwFKEa2TmAiMK/0NwKBgDW7gCQmP9W0Sx+eWVcE6symzxpSgwO2XubA/JQ3nnFP3fxA1NExybmb3Hz/utUFGcohz6gBOSjJszC6Wb8l2kqKwRxYGuTAEIYNkgC+zG5mfBvmJPt2AKOmkmAdN06ZIjRbOpRzcoFPG6nUiPXz4M6T9nuHk/ugTri6sYLuxpGJAoGBALI0mlazlyncjdZYq8GNnN8HaQu6uMahky1cgJjnN5LSq8jC03gEhHwyPlFSmjKVXD0En2YyQC5dEZAtFde76EJMAqtU3ZbEDADY/0H1ajcguEPUXBtey/xQ2y5tWgsXtaF0PeIfamGlgC2pAnH72m5MbRKuM5IiUql/qXNlOreq";

fn settings(configuration: &Value, selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "github_configuration": configuration,
        "repository": "https://github.example.test/EliteaAI/elitea-platform.git",
        "active_branch": "feature/runtime",
        "base_branch": "main",
        "selected_tools": selected_tools,
        "pgvector_configuration": null,
        "embedding_model": null
    })
    .as_object()
    .expect("GitHub settings fixture")
    .clone()
}

fn token_config(selected_tools: &[&str]) -> GitHubToolkitConfig {
    GitHubToolkitConfig::parse(&settings(
        &json!({
            "configuration_type": "github",
            "base_url": "https://github.example.test/api/v3/",
            "access_token": "fixture-token",
            "username": "ignored-user",
            "password": "ignored-password"
        }),
        selected_tools,
    ))
    .expect("valid GitHub token configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(ToString::to_string).collect::<Vec<_>>(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("GitHub policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("github-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn current_materialized_configuration_preserves_auth_precedence_and_repository_shape() {
    let config = token_config(&["get_me", "list_branches_in_repo", "get_me"]);

    assert_eq!(config.auth_kind(), GitHubAuthKind::Token);
    assert_eq!(
        config.base_url().as_str(),
        "https://github.example.test/api/v3"
    );
    assert_eq!(config.repository(), "EliteaAI/elitea-platform");
    assert_eq!(config.active_branch(), "feature/runtime");
    assert_eq!(config.base_branch(), "main");
    assert_eq!(
        config.selected_tools(),
        [
            Box::<str>::from("get_me"),
            Box::<str>::from("list_branches_in_repo")
        ]
    );
}

#[test]
fn malformed_auth_origin_repository_and_bounds_fail_without_sensitive_diagnostics() {
    let secret = "fixture-secret-that-must-not-render";
    let cases = [
        settings(
            &json!({"base_url": "http://api.github.com", "access_token": secret}),
            &["get_me"],
        ),
        settings(
            &json!({"base_url": "https://user@api.github.com", "access_token": secret}),
            &["get_me"],
        ),
        settings(
            &json!({"base_url": "https://api.github.com", "username": "user"}),
            &["get_me"],
        ),
        settings(
            &json!({"base_url": "https://api.github.com", "app_private_key": secret}),
            &["get_me"],
        ),
    ];
    for mut case in cases {
        if case
            .get("github_configuration")
            .and_then(Value::as_object)
            .is_some_and(|configuration| configuration.contains_key("access_token"))
        {
            case.insert("repository".to_owned(), json!("too/many/segments"));
        }
        let Err(error) = GitHubToolkitConfig::parse(&case) else {
            panic!("invalid GitHub configuration was accepted");
        };
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(secret));
        assert!(!diagnostic.contains("github.com"));
    }

    let oversized = settings(
        &json!({"base_url": "https://api.github.com", "access_token": "x".repeat(64 * 1_024 + 1)}),
        &["get_me"],
    );
    assert!(GitHubToolkitConfig::parse(&oversized).is_err());
}

#[test]
fn request_builder_is_origin_bound_and_uses_the_current_auth_contract() {
    let client =
        GitHubClient::new(token_config(&["list_branches_in_repo"])).expect("GitHub token client");
    let request = client
        .test_request(
            GitHubRequestKind::Repository,
            &["repos", "EliteaAI", "elitea-platform", "branches"],
            &[("per_page", "17".to_owned())],
            UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        )
        .expect("origin-bound GitHub request");

    assert_eq!(
        request.url().as_str(),
        "https://github.example.test/api/v3/repos/EliteaAI/elitea-platform/branches?per_page=17"
    );
    assert_eq!(
        request
            .headers()
            .get(AUTHORIZATION)
            .expect("authorization header"),
        "token fixture-token"
    );
    assert_eq!(
        request
            .headers()
            .get("x-github-api-version")
            .expect("GitHub version header"),
        "2022-11-28"
    );
}

#[test]
fn code_search_request_uses_one_origin_bound_provider_page() {
    let client =
        GitHubClient::new(token_config(&["search_code"])).expect("GitHub code-search client");
    let query = test_scope_code_search_query("fn main", "EliteaAI/elitea-platform")
        .expect("configured code-search scope");
    let request = client
        .test_request(
            GitHubRequestKind::Repository,
            &["search", "code"],
            &[
                ("q", query),
                ("sort", "indexed".to_owned()),
                ("order", "desc".to_owned()),
                ("per_page", "30".to_owned()),
                ("page", "2".to_owned()),
            ],
            UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        )
        .expect("one code-search request");
    assert_eq!(
        request.url().as_str(),
        "https://github.example.test/api/v3/search/code?q=fn+main+repo%3AEliteaAI%2Felitea-platform&sort=indexed&order=desc&per_page=30&page=2"
    );
}

#[test]
fn workflow_status_requests_are_origin_bound_and_job_paging_is_explicit() {
    let client = GitHubClient::new(token_config(&["get_workflow_status"]))
        .expect("GitHub workflow-status client");
    let run = client
        .test_request(
            GitHubRequestKind::Repository,
            &[
                "repos",
                "EliteaAI",
                "elitea-platform",
                "actions",
                "runs",
                "9001",
            ],
            &[],
            UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        )
        .expect("origin-bound workflow-run request");
    assert_eq!(
        run.url().as_str(),
        "https://github.example.test/api/v3/repos/EliteaAI/elitea-platform/actions/runs/9001"
    );

    let jobs = client
        .test_request(
            GitHubRequestKind::Repository,
            &[
                "repos",
                "EliteaAI",
                "elitea-platform",
                "actions",
                "runs",
                "9001",
                "jobs",
            ],
            &[("per_page", "100".to_owned()), ("page", "1".to_owned())],
            UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        )
        .expect("origin-bound workflow-jobs request");
    assert_eq!(
        jobs.url().as_str(),
        "https://github.example.test/api/v3/repos/EliteaAI/elitea-platform/actions/runs/9001/jobs?per_page=100&page=1"
    );
}

#[test]
fn project_query_uses_the_enterprise_graphql_endpoint_and_one_bounded_body() {
    let client =
        GitHubClient::new(token_config(&["list_project_issues"])).expect("GitHub project client");
    let payload = test_project_query_payload("EliteaAI", "elitea-platform", 7, 25);
    let request = client
        .test_graphql_request(&payload, UNIX_EPOCH + Duration::from_secs(1_700_000_000))
        .expect("origin-bound GraphQL request");

    assert_eq!(request.method().as_str(), "POST");
    assert_eq!(
        request.url().as_str(),
        "https://github.example.test/api/graphql"
    );
    assert_eq!(
        request
            .headers()
            .get(AUTHORIZATION)
            .expect("GraphQL authorization header"),
        "token fixture-token"
    );
    assert_eq!(
        request
            .headers()
            .get("content-type")
            .expect("GraphQL content type"),
        "application/json"
    );
    let encoded = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .expect("bounded GraphQL request body");
    assert_eq!(
        serde_json::from_slice::<Value>(encoded).expect("GraphQL request JSON"),
        payload
    );

    let public_config = GitHubToolkitConfig::parse(&settings(
        &json!({
            "configuration_type": "github",
            "base_url": "https://api.github.com",
            "access_token": "fixture-token"
        }),
        &["list_project_issues"],
    ))
    .expect("public GitHub project configuration");
    let public = GitHubClient::new(public_config).expect("public GitHub project client");
    let public_request = public
        .test_graphql_request(&payload, UNIX_EPOCH + Duration::from_secs(1_700_000_000))
        .expect("public GraphQL request");
    assert_eq!(
        public_request.url().as_str(),
        "https://api.github.com/graphql"
    );
}

#[test]
fn github_app_probe_accepts_pkcs8_and_creates_a_short_lived_rs256_jwt() {
    let config = GitHubToolkitConfig::parse(&settings(
        &json!({
            "base_url": "https://api.github.com",
            "app_id": "123456",
            "app_private_key": format!("-----BEGIN PRIVATE KEY-----\n{TEST_PKCS8_KEY_BODY}\n-----END PRIVATE KEY-----")
        }),
        &["get_me"],
    ))
    .expect("valid GitHub App configuration");
    let client = GitHubClient::new(config).expect("GitHub App client");
    let now = UNIX_EPOCH + Duration::from_secs(1_700_000_000);
    let request = client
        .test_request(GitHubRequestKind::Probe, &[], &[], now)
        .expect("GitHub App probe request");

    assert_eq!(request.url().as_str(), "https://api.github.com/app");
    let authorization = request
        .headers()
        .get(AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .expect("GitHub App authorization");
    let token = authorization
        .strip_prefix("Bearer ")
        .expect("GitHub App bearer token");
    let parts = token.split('.').collect::<Vec<_>>();
    assert_eq!(parts.len(), 3);
    let header: Value = serde_json::from_slice(
        &URL_SAFE_NO_PAD
            .decode(parts[0])
            .expect("GitHub App JWT header"),
    )
    .expect("GitHub App JWT header JSON");
    let payload: Value = serde_json::from_slice(
        &URL_SAFE_NO_PAD
            .decode(parts[1])
            .expect("GitHub App JWT payload"),
    )
    .expect("GitHub App JWT payload JSON");
    assert_eq!(header, json!({"alg": "RS256", "typ": "JWT"}));
    assert_eq!(payload["iss"], "123456");
    assert_eq!(payload["iat"], 1_700_000_000_u64);
    assert_eq!(payload["exp"], 1_700_000_600_u64);
    assert!(!parts[2].is_empty());
}

#[test]
fn response_projection_is_bounded_and_omits_unknown_or_null_fields() {
    let user = test_project_user(&json!({
        "login": "octocat",
        "id": 1,
        "name": null,
        "company": "GitHub",
        "raw_secret_field": "must not project"
    }))
    .expect("valid GitHub user response");
    assert_eq!(
        user,
        json!({"login": "octocat", "id": 1, "company": "GitHub"})
    );

    let branches = test_project_branches(
        &json!([
            {"name": "main", "protected": true, "commit": {"sha": "abc"}},
            {"name": "feature", "protected": false}
        ]),
        2,
    )
    .expect("valid GitHub branches response");
    assert_eq!(
        branches,
        json!([
            {"name": "main", "protected": true},
            {"name": "feature", "protected": false}
        ])
    );
    assert!(test_project_branches(&json!([{"name": "main", "protected": true}]), 0).is_err());

    let content = base64::engine::general_purpose::STANDARD.encode("alpha\nbeta\n");
    assert_eq!(
        test_project_text_file(&json!({
            "type": "file",
            "encoding": "base64",
            "size": 11,
            "content": format!("{}\n", content)
        }))
        .expect("valid GitHub text file"),
        "alpha\nbeta\n"
    );
    assert!(
        test_project_text_file(&json!({
            "type": "file",
            "encoding": "base64",
            "size": 2,
            "content": "/w=="
        }))
        .is_err()
    );
    assert!(test_validate_file_path("src/lib.rs").is_ok());
    for invalid_path in ["/etc/passwd", "../secret", "src/../secret", "src//lib.rs"] {
        assert!(test_validate_file_path(invalid_path).is_err());
    }

    let tree = json!({
        "truncated": false,
        "tree": [
            {"path": "README.md", "type": "blob"},
            {"path": "src", "type": "tree"},
            {"path": "src/lib.rs", "type": "blob"},
            {"path": "src/nested/mod.rs", "type": "blob"},
            {"path": "vendor/dependency", "type": "commit"}
        ]
    });
    assert_eq!(
        test_project_tree_files(&tree, "src").expect("bounded recursive tree"),
        json!(["src/lib.rs", "src/nested/mod.rs"])
    );
    assert!(test_project_tree_files(&json!({"truncated": true, "tree": []}), "").is_err());
    assert_eq!(
        test_project_tree_sha(&json!({
            "commit": {"commit": {"tree": {"sha": "A".repeat(40)}}}
        }))
        .expect("branch tree SHA"),
        "a".repeat(40)
    );
}

#[test]
fn issue_projection_matches_the_current_sdk_fields_and_is_bounded() {
    let issue = json!({
        "number": 42,
        "title": "Bound the worker",
        "body": "Issue body",
        "state": "open",
        "html_url": "https://github.example.test/EliteaAI/elitea-platform/issues/42",
        "created_at": "2026-08-18T10:00:00Z",
        "updated_at": "2026-08-18T11:00:00Z",
        "comments": 3,
        "labels": [{"name": "rust"}],
        "assignees": [{"login": "octocat"}],
        "raw_secret_field": "must not project"
    });
    assert_eq!(
        test_project_issue_detail(&issue).expect("bounded issue detail"),
        json!({
            "number": 42,
            "title": "Bound the worker",
            "body": "Issue body",
            "state": "open",
            "url": "https://github.example.test/EliteaAI/elitea-platform/issues/42",
            "created_at": "2026-08-18T10:00:00+00:00",
            "updated_at": "2026-08-18T11:00:00+00:00",
            "comments": 3,
            "labels": ["rust"],
            "assignees": ["octocat"]
        })
    );
    assert_eq!(
        test_project_issue_list(&json!([issue.clone()])).expect("bounded issue list"),
        json!([{
            "number": 42,
            "title": "Bound the worker",
            "state": "open",
            "created_at": "2026-08-18T10:00:00+00:00",
            "updated_at": "2026-08-18T11:00:00+00:00",
            "url": "https://github.example.test/EliteaAI/elitea-platform/issues/42",
            "labels": ["rust"],
            "assignees": ["octocat"]
        }])
    );
    assert_eq!(
        test_project_issue_search(
            &json!({"total_count": 1, "items": [{
                "number": 42,
                "title": "Bound the worker",
                "body": null,
                "state": "open",
                "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/42",
                "pull_request": {"url": "ignored"}
            }]}),
            30,
        )
        .expect("bounded issue search"),
        json!([{
            "id": 42,
            "title": "Bound the worker",
            "description": null,
            "status": "open",
            "url": "https://github.example.test/EliteaAI/elitea-platform/pull/42",
            "entity_type": "PR"
        }])
    );
    assert_eq!(
        test_project_issue_search(&json!({"total_count": 0, "items": []}), 30)
            .expect("empty issue search"),
        Value::String("No issues or PRs found matching your query.".to_owned())
    );
    assert!(
        test_project_issue_detail(&json!({
            "number": 1,
            "title": "x",
            "body": "x".repeat(128 * 1_024 + 1),
            "state": "open",
            "html_url": "https://github.example.test/issue/1",
            "created_at": "2026-08-18T10:00:00Z",
            "updated_at": "2026-08-18T10:00:00Z",
            "comments": 0,
            "labels": [],
            "assignees": []
        }))
        .is_err()
    );
}

#[test]
fn pull_request_projection_is_complete_typed_and_bounded() {
    let pull = pull_request_fixture();
    assert_eq!(
        test_project_pull_request_list(&json!([pull.clone()]), 1)
            .expect("bounded pull-request list"),
        json!([{
            "number": 42,
            "title": "Bound the worker",
            "state": "open",
            "created_at": "2026-08-18T10:00:00+00:00",
            "updated_at": null,
            "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/42",
            "user": null,
            "head": "feature/runtime",
            "base": "main"
        }])
    );
    assert_eq!(
        test_project_pull_request_detail(
            &pull,
            &json!([{"body": "Looks good", "user": {"login": "reviewer"}}]),
            &json!([{"commit": {"message": "feat: bound output"}}]),
            42,
        )
        .expect("bounded typed pull-request detail"),
        json!({
            "title": "Bound the worker",
            "number": 42,
            "body": null,
            "pr_url": "https://github.example.test/EliteaAI/elitea-platform/pull/42",
            "state": "open",
            "head": "feature/runtime",
            "base": "main",
            "comments": [{"body": "Looks good", "user": "reviewer"}],
            "commits": [{"message": "feat: bound output"}]
        })
    );
}

#[test]
fn pull_request_file_projection_is_complete_and_bounded() {
    let pull = pull_request_fixture();
    assert_eq!(
        test_project_pull_request_files(
            &pull,
            &[json!([
                {
                    "filename": "src/lib.rs",
                    "status": "modified",
                    "additions": 2,
                    "deletions": 1,
                    "changes": 3,
                    "patch": "@@ -1 +1 @@"
                },
                {
                    "filename": "src/new.rs",
                    "status": "added",
                    "additions": 1,
                    "deletions": 0,
                    "changes": 1,
                    "patch": null
                }
            ])],
            42,
        )
        .expect("bounded pull-request files"),
        json!([
            {
                "path": "src/lib.rs",
                "patch": "@@ -1 +1 @@",
                "filename": "src/lib.rs",
                "status": "modified",
                "additions": 2,
                "deletions": 1,
                "changes": 3
            },
            {
                "path": "src/new.rs",
                "patch": null,
                "filename": "src/new.rs",
                "status": "added",
                "additions": 1,
                "deletions": 0,
                "changes": 1
            }
        ])
    );
    let mut oversized = pull;
    oversized["changed_files"] = json!(301);
    assert!(test_project_pull_request_files(&oversized, &[], 42).is_err());
    let mut incomplete = pull_request_fixture();
    incomplete["changed_files"] = json!(1);
    assert!(test_project_pull_request_files(&incomplete, &[json!([])], 42).is_err());
}

#[test]
fn pull_request_detail_rejects_an_over_limit_collection() {
    assert!(
        test_project_pull_request_detail(
            &pull_request_fixture(),
            &Value::Array(
                (0..11)
                    .map(|_| json!({"body": null, "user": null}))
                    .collect(),
            ),
            &json!([]),
            42,
        )
        .is_err()
    );
}

fn pull_request_fixture() -> Value {
    json!({
        "number": 42,
        "title": "Bound the worker",
        "body": null,
        "state": "open",
        "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/42",
        "created_at": "2026-08-18T10:00:00Z",
        "updated_at": null,
        "user": null,
        "head": {"ref": "feature/runtime"},
        "base": {"ref": "main"},
        "changed_files": 2
    })
}

#[test]
fn commit_projection_preserves_the_sdk_fields_and_safe_fallbacks() {
    let commit = commit_fixture();
    assert_eq!(
        test_project_commit_list(&json!([commit.clone()]), 1).expect("bounded commit list"),
        json!([{
            "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "author": "Commit Author",
            "date": "2026-08-18T10:00:00+00:00",
            "message": "feat: bound commit inspection",
            "url": "https://github.example.test/EliteaAI/elitea-platform/commit/aaaaaaaa"
        }])
    );
    assert_eq!(
        test_project_commit_changes(std::slice::from_ref(&commit)).expect("bounded commit changes"),
        json!({
            "commit_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "commit_message": "feat: bound commit inspection",
            "author": "Commit Author",
            "date": "2026-08-18T10:00:00+00:00",
            "total_files_changed": 1,
            "total_additions": 2,
            "total_deletions": 1,
            "files": [{
                "filename": "src/lib.rs",
                "status": "modified",
                "additions": 2,
                "deletions": 1,
                "changes": 3,
                "patch": "@@ -1 +1 @@",
                "blob_url": "https://github.example.test/blob/src/lib.rs",
                "raw_url": null
            }]
        })
    );

    let mut fallback = commit;
    fallback["commit"]["author"] = Value::Null;
    fallback["author"] = json!({"login": "fallback-login"});
    assert_eq!(
        test_project_commit_list(&json!([fallback]), 1).expect("safe commit author fallback")[0]
            .clone(),
        json!({
            "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "author": "fallback-login",
            "date": null,
            "message": "feat: bound commit inspection",
            "url": "https://github.example.test/EliteaAI/elitea-platform/commit/aaaaaaaa"
        })
    );
}

#[test]
fn commit_comparison_requires_complete_bounded_collections() {
    let base = commit_fixture_with_sha('a');
    let head = commit_fixture_with_sha('b');
    let file = head["files"][0].clone();
    let comparison = json!({
        "base_commit": base,
        "status": "ahead",
        "ahead_by": 1,
        "behind_by": 0,
        "total_commits": 1,
        "commits": [head.clone()],
        "files": [file]
    });
    assert_eq!(
        test_project_commit_comparison(&comparison).expect("bounded comparison"),
        json!({
            "base_commit": {
                "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "message": "feat: bound commit inspection",
                "author": "Commit Author",
                "date": "2026-08-18T10:00:00+00:00"
            },
            "head_commit": {
                "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
                "message": "feat: bound commit inspection",
                "author": "Commit Author",
                "date": "2026-08-18T10:00:00+00:00"
            },
            "status": "ahead",
            "ahead_by": 1,
            "behind_by": 0,
            "total_commits": 1,
            "commits": [{
                "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
                "author": "Commit Author",
                "date": "2026-08-18T10:00:00+00:00",
                "message": "feat: bound commit inspection",
                "url": "https://github.example.test/EliteaAI/elitea-platform/commit/bbbbbbbb"
            }],
            "files": [{
                "filename": "src/lib.rs",
                "status": "modified",
                "additions": 2,
                "deletions": 1,
                "changes": 3,
                "patch": "@@ -1 +1 @@",
                "blob_url": "https://github.example.test/blob/src/lib.rs",
                "raw_url": null
            }],
            "summary": {
                "total_files_changed": 1,
                "total_additions": 2,
                "total_deletions": 1
            }
        })
    );

    let identical = json!({
        "base_commit": commit_fixture_with_sha('a'),
        "status": "identical",
        "ahead_by": 0,
        "behind_by": 0,
        "total_commits": 0,
        "commits": [],
        "files": []
    });
    assert!(test_project_commit_comparison(&identical).is_ok());

    let behind = json!({
        "base_commit": commit_fixture_with_sha('a'),
        "merge_base_commit": commit_fixture_with_sha('b'),
        "status": "behind",
        "ahead_by": 0,
        "behind_by": 1,
        "total_commits": 0,
        "commits": [],
        "files": []
    });
    assert_eq!(
        test_project_commit_comparison(&behind).expect("same-snapshot behind comparison")["head_commit"]
            ["sha"],
        json!("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
    );

    let mut too_many = comparison;
    too_many["total_commits"] = json!(101);
    assert!(test_project_commit_comparison(&too_many).is_err());
}

fn commit_fixture() -> Value {
    commit_fixture_with_sha('a')
}

fn commit_fixture_with_sha(character: char) -> Value {
    let sha = character.to_string().repeat(40);
    json!({
        "sha": sha,
        "html_url": format!(
            "https://github.example.test/EliteaAI/elitea-platform/commit/{}",
            character.to_string().repeat(8)
        ),
        "commit": {
            "message": "feat: bound commit inspection",
            "author": {
                "name": "Commit Author",
                "date": "2026-08-18T10:00:00Z"
            }
        },
        "author": null,
        "files": [{
            "filename": "src/lib.rs",
            "status": "modified",
            "additions": 2,
            "deletions": 1,
            "changes": 3,
            "patch": "@@ -1 +1 @@",
            "blob_url": "https://github.example.test/blob/src/lib.rs",
            "raw_url": null
        }]
    })
}

#[test]
fn code_search_scope_and_projection_are_bounded_and_provider_truthful() {
    assert_eq!(
        test_scope_code_search_query("fn main language:rust", "EliteaAI/elitea-platform")
            .expect("configured repository scope"),
        "fn main language:rust repo:EliteaAI/elitea-platform"
    );
    for explicitly_scoped in [
        "symbol repo:EliteaAI/other",
        "symbol ORG:EliteaAI",
        "symbol user:octocat",
    ] {
        assert_eq!(
            test_scope_code_search_query(explicitly_scoped, "EliteaAI/elitea-platform")
                .expect("explicit search scope"),
            explicitly_scoped
        );
    }
    assert!(test_scope_code_search_query("   ", "EliteaAI/elitea-platform").is_err());

    let response = json!({
        "total_count": 37,
        "incomplete_results": true,
        "items": [{
            "name": "lib.rs",
            "path": "src/lib.rs",
            "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "html_url": "https://github.example.test/EliteaAI/elitea-platform/blob/main/src/lib.rs",
            "repository": {
                "full_name": "EliteaAI/elitea-platform",
                "html_url": "https://github.example.test/EliteaAI/elitea-platform",
                "description": null,
                "private": true,
                "owner": {"raw": "not projected"}
            },
            "text_matches": [{
                "fragment": "fn main() {\n}",
                "matches": [{"text": "main", "indices": [3, 7]}],
                "object_url": "not projected"
            }],
            "raw_internal": "not projected"
        }]
    });
    assert_eq!(
        test_project_code_search(&response, 2, 30).expect("bounded code-search response"),
        json!({
            "total_count": 37,
            "incomplete_results": true,
            "items": [{
                "name": "lib.rs",
                "path": "src/lib.rs",
                "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "html_url": "https://github.example.test/EliteaAI/elitea-platform/blob/main/src/lib.rs",
                "repository": {
                    "full_name": "EliteaAI/elitea-platform",
                    "html_url": "https://github.example.test/EliteaAI/elitea-platform",
                    "description": null,
                    "private": true
                },
                "text_matches": [{
                    "fragment": "fn main() {\n}",
                    "matches": [{"text": "main", "indices": [3, 7]}]
                }]
            }],
            "page": 2,
            "per_page": 30
        })
    );
    assert!(test_project_code_search(&response, 11, 100).is_err());
    let mut oversized = response;
    oversized["items"] = Value::Array((0..101).map(|_| json!({})).collect());
    assert!(test_project_code_search(&oversized, 1, 100).is_err());
}

#[test]
fn workflow_status_projection_preserves_sdk_fields_and_exposes_job_truncation() {
    let run = json!({
        "id": 9001,
        "name": "Rust worker checks",
        "workflow_id": 71,
        "event": "pull_request",
        "status": "in_progress",
        "conclusion": null,
        "created_at": "2026-08-18T10:00:00Z",
        "updated_at": "2026-08-18T10:01:00Z",
        "head_branch": "feature/runtime",
        "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "html_url": "https://github.example.test/EliteaAI/elitea-platform/actions/runs/9001",
        "upstream_only": "not projected"
    });
    let jobs = json!({
        "total_count": 2,
        "jobs": [{
            "id": 44,
            "run_id": 9001,
            "name": "rust-1.95",
            "status": "completed",
            "conclusion": "success",
            "started_at": "2026-08-18T10:00:05Z",
            "completed_at": "2026-08-18T10:00:55Z",
            "html_url": "https://github.example.test/EliteaAI/elitea-platform/actions/runs/9001/job/44",
            "steps": [{"name": "not projected"}]
        }]
    });
    assert_eq!(
        test_project_workflow_status(&run, &jobs, 9001).expect("bounded workflow-status response"),
        json!({
            "id": 9001,
            "name": "Rust worker checks",
            "workflow_id": 71,
            "event": "pull_request",
            "status": "in_progress",
            "conclusion": null,
            "created_at": "2026-08-18T10:00:00+00:00",
            "updated_at": "2026-08-18T10:01:00+00:00",
            "head_branch": "feature/runtime",
            "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "jobs": [{
                "id": 44,
                "name": "rust-1.95",
                "status": "completed",
                "conclusion": "success",
                "started_at": "2026-08-18T10:00:05+00:00",
                "completed_at": "2026-08-18T10:00:55+00:00",
                "url": "https://github.example.test/EliteaAI/elitea-platform/actions/runs/9001/job/44"
            }],
            "jobs_total_count": 2,
            "jobs_truncated": true,
            "url": "https://github.example.test/EliteaAI/elitea-platform/actions/runs/9001"
        })
    );
    assert!(test_project_workflow_status(&run, &jobs, 9002).is_err());

    let mut nullable_run = run.clone();
    nullable_run["name"] = Value::Null;
    nullable_run["status"] = Value::Null;
    let mut nullable_jobs = jobs.clone();
    nullable_jobs["jobs"][0]["html_url"] = Value::Null;
    let nullable = test_project_workflow_status(&nullable_run, &nullable_jobs, 9001)
        .expect("provider-nullable workflow fields");
    assert_eq!(nullable["name"], Value::Null);
    assert_eq!(nullable["status"], Value::Null);
    assert_eq!(nullable["jobs"][0]["url"], Value::Null);

    let mut wrong_run_jobs = jobs.clone();
    wrong_run_jobs["jobs"][0]["run_id"] = json!(9002);
    assert!(test_project_workflow_status(&run, &wrong_run_jobs, 9001).is_err());

    let job = jobs["jobs"][0].clone();
    let oversized = json!({
        "total_count": 101,
        "jobs": vec![job; 101]
    });
    assert!(test_project_workflow_status(&run, &oversized, 9001).is_err());
}

fn project_response() -> Value {
    json!({
        "data": {
            "repository": {
                "projectV2": {
                    "id": "PVT_project",
                    "title": "Runtime delivery",
                    "url": "https://github.example.test/orgs/EliteaAI/projects/7",
                    "fields": {
                        "nodes": [{
                            "id": "field-status",
                            "name": "Status",
                            "dataType": "SINGLE_SELECT",
                            "options": [{"id": "todo", "name": "Todo", "color": "GRAY"}]
                        }]
                    },
                    "items": {
                        "totalCount": 2,
                        "pageInfo": {"hasNextPage": true},
                        "nodes": [{
                            "id": "item-1",
                            "type": "ISSUE",
                            "fieldValues": {"nodes": [{
                                "field": {"id": "field-status", "name": "Status"},
                                "name": "Todo",
                                "optionId": "todo"
                            }]},
                            "content": {
                                "id": "issue-1",
                                "number": 42,
                                "title": "Make recovery boring",
                                "state": "OPEN",
                                "url": "https://github.example.test/EliteaAI/elitea-platform/issues/42",
                                "labels": {"nodes": [{"id": "label-1", "name": "runtime", "color": "0052cc"}]},
                                "assignees": {"nodes": [{"id": "user-1", "login": "octocat", "name": null}]}
                            }
                        }]
                    }
                }
            }
        }
    })
}

#[test]
fn project_projection_preserves_sdk_fields_and_reports_the_bounded_window() {
    let response = project_response();
    let projected = test_project_project_issues(&response, 1).expect("bounded project projection");
    assert_eq!(
        projected,
        json!({
            "id": "PVT_project",
            "title": "Runtime delivery",
            "url": "https://github.example.test/orgs/EliteaAI/projects/7",
            "fields": [{
                "id": "field-status",
                "name": "Status",
                "dataType": "SINGLE_SELECT",
                "options": [{"id": "todo", "name": "Todo", "color": "GRAY"}]
            }],
            "items": [{
                "id": "item-1",
                "type": "ISSUE",
                "fieldValues": [{
                    "field": {"id": "field-status", "name": "Status"},
                    "optionId": "todo",
                    "optionName": "Todo"
                }],
                "content": {
                    "id": "issue-1",
                    "number": 42,
                    "title": "Make recovery boring",
                    "state": "OPEN",
                    "url": "https://github.example.test/EliteaAI/elitea-platform/issues/42",
                    "labels": [{"id": "label-1", "name": "runtime", "color": "0052cc"}],
                    "assignees": [{"id": "user-1", "login": "octocat", "name": null}]
                }
            }],
            "items_total_count": 2,
            "items_truncated": true
        })
    );
}

#[test]
fn project_projection_rejects_partial_or_wrong_windows_and_preserves_draft_nulls() {
    let response = project_response();
    let provider_limit = json!({
        "errors": [{"extensions": {"type": "RATE_LIMITED"}}],
        "data": response["data"].clone()
    });
    let rate_limit = test_project_project_issues(&provider_limit, 1)
        .expect_err("GraphQL errors must not return partial project data");
    assert_eq!(rate_limit.code(), GitHubClientErrorCode::RateLimited);
    assert!(rate_limit.retryable());

    let mut wrong_window = response;
    wrong_window["data"]["repository"]["projectV2"]["items"]["nodes"] = json!([{}, {}]);
    assert!(test_project_project_issues(&wrong_window, 1).is_err());

    let draft = json!({
        "data": {"repository": {"projectV2": {
            "id": "PVT_project",
            "title": "Runtime delivery",
            "url": null,
            "fields": {"nodes": []},
            "items": {
                "totalCount": 1,
                "pageInfo": {"hasNextPage": false},
                "nodes": [{
                    "id": "draft-item",
                    "type": "DRAFT_ISSUE",
                    "fieldValues": {"nodes": []},
                    "content": {"id": "draft-1", "title": "Untriaged"}
                }]
            }
        }}}
    });
    let draft = test_project_project_issues(&draft, 1).expect("nullable draft projection");
    assert_eq!(draft["url"], Value::Null);
    assert_eq!(draft["items"][0]["content"]["number"], Value::Null);
    assert_eq!(draft["items"][0]["content"]["url"], Value::Null);
    assert_eq!(draft["items"][0]["content"]["state"], Value::Null);
}

#[test]
fn project_projection_classifies_mixed_graphql_errors_independently_of_order() {
    for errors in [
        json!([
            {"extensions": {"type": "RATE_LIMITED"}},
            {"extensions": {"type": "FORBIDDEN"}}
        ]),
        json!([
            {"extensions": {"type": "FORBIDDEN"}},
            {"extensions": {"type": "RATE_LIMITED"}}
        ]),
    ] {
        let response = json!({
            "errors": errors,
            "data": project_response()["data"].clone()
        });
        let failure = test_project_project_issues(&response, 1)
            .expect_err("mixed GraphQL error categories must fail closed");
        assert_eq!(failure.code(), GitHubClientErrorCode::InvalidResponse);
        assert!(!failure.retryable());
    }
}

#[test]
fn github_rate_limits_remain_distinct_from_authorization_failures() {
    let permission = test_map_status(StatusCode::FORBIDDEN, &HeaderMap::new())
        .expect_err("plain forbidden must be an authorization failure");
    assert_eq!(permission.code(), GitHubClientErrorCode::Authorization);
    assert!(!permission.retryable());

    let invalid_query = test_map_status(StatusCode::UNPROCESSABLE_ENTITY, &HeaderMap::new())
        .expect_err("unprocessable search must be invalid input");
    assert_eq!(invalid_query.code(), GitHubClientErrorCode::InvalidInput);
    assert!(!invalid_query.retryable());

    let mut primary_limit = HeaderMap::new();
    primary_limit.insert("x-ratelimit-remaining", HeaderValue::from_static("0"));
    let primary = test_map_status(StatusCode::FORBIDDEN, &primary_limit)
        .expect_err("exhausted primary limit must be rate limited");
    assert_eq!(primary.code(), GitHubClientErrorCode::RateLimited);
    assert!(primary.retryable());

    let mut secondary_limit = HeaderMap::new();
    secondary_limit.insert(RETRY_AFTER, HeaderValue::from_static("60"));
    let secondary = test_map_status(StatusCode::FORBIDDEN, &secondary_limit)
        .expect_err("secondary limit must be rate limited");
    assert_eq!(secondary.code(), GitHubClientErrorCode::RateLimited);
    assert!(secondary.retryable());
}

#[tokio::test]
async fn native_adk_tools_preserve_selection_policy_and_argument_contracts() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec!["get_me".to_owned(), "list_branches_in_repo".to_owned()];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[("github", &["get_me"])]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native GitHub toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let mut tools = toolset.tools(readonly).await.expect("native GitHub tools");

    assert_eq!(tools.len(), 1);
    let tool = tools.pop().expect("list branches tool");
    assert_eq!(tool.name(), "list_branches_in_repo");
    assert!(tool.is_read_only());
    assert!(tool.is_concurrency_safe());
    assert!(
        tool.description()
            .starts_with("Toolkit: team-github\nRepository:")
    );
    let invocation: Arc<dyn ToolContext> = context();
    let result = tool
        .execute(invocation, json!({"max_count": 7, "unused": null}))
        .await
        .expect("GitHub branch result");
    assert_eq!(result, json!([{"name": "main", "protected": true}]));
    assert_eq!(
        client
            .branch_limits
            .lock()
            .expect("branch limit calls")
            .as_slice(),
        &[7]
    );

    let invalid = tool
        .execute(context(), json!({"max_count": 101}))
        .await
        .expect_err("over-limit branch count");
    assert_eq!(invalid.code, "tool.execution.invalid_input");
}

async fn native_file_tools(client: &Arc<FixtureGitHubApi>) -> Vec<Arc<dyn Tool>> {
    let selected = vec![
        "read_file".to_owned(),
        "read_multiple_files".to_owned(),
        "grep_file".to_owned(),
    ];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native GitHub file toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset
        .tools(readonly)
        .await
        .expect("native GitHub file tools")
}

fn insert_file(client: &FixtureGitHubApi, path: &str, content: String) {
    client
        .files
        .lock()
        .expect("file fixture lock")
        .insert(path.to_owned(), content);
}

#[tokio::test]
async fn native_read_file_preserves_python_line_slicing_and_guidance() {
    let client = Arc::new(FixtureGitHubApi::default());
    insert_file(
        &client,
        "src/main.py",
        "alpha\r\nbeta\u{2028}gamma\ndelta".to_owned(),
    );
    insert_file(&client, "large.py", "x\n".repeat(100_001));
    let tools = native_file_tools(&client).await;

    let read_file = tools
        .iter()
        .find(|tool| tool.name() == "read_file")
        .expect("read_file tool");
    assert_eq!(
        read_file
            .execute(
                context(),
                json!({
                    "file_path": "src/main.py",
                    "branch": "release/v2",
                    "repo_name": "EliteaAI/other",
                    "start_line": 2,
                    "end_line": 3
                }),
            )
            .await
            .expect("bounded GitHub file read"),
        Value::String("beta\u{2028}gamma\n".to_owned())
    );
    let guidance = read_file
        .execute(context(), json!({"file_path": "large.py"}))
        .await
        .expect("large-file guidance");
    assert_eq!(
        guidance,
        json!({
            "__result_status__": "content_too_large",
            "context": {
                "actual_chars": 200_002,
                "limit_chars": 200_000,
                "requested": "full file read"
            },
            "extension": ".py",
            "filename": "large.py",
            "instruction_for_readFile": {
                "extra_params": {},
                "first_class_params": {
                    "end_line": "integer (1-indexed, inclusive) — last line to read. Valid range 1..100001. Omit to read to the end.",
                    "start_line": "integer (1-indexed, inclusive) — first line to read. Valid range 1..100001. Omit to read from the beginning."
                },
                "notes": "Use start_line/end_line together to read a bounded slice of a large file and keep tokens bounded."
            },
            "read_limits": {
                "full_read_allowed": false,
                "max_output_chars": 200_000
            },
            "schema_version": "1.0",
            "total_lines": 100_001,
            "type": "text/x-python",
            "unit": "lines"
        })
    );
    assert!(
        client
            .file_calls
            .lock()
            .expect("file call fixture lock")
            .iter()
            .any(|call| {
                call == &(
                    "src/main.py".to_owned(),
                    Some("release/v2".to_owned()),
                    Some("EliteaAI/other".to_owned()),
                )
            })
    );
}

#[tokio::test]
async fn native_grep_and_batch_reads_are_bounded_before_network_use() {
    let client = Arc::new(FixtureGitHubApi::default());
    insert_file(
        &client,
        "search.rs",
        "before\nfn Main() {}\nafter\nlast".to_owned(),
    );
    insert_file(&client, "budget.txt", "x".repeat(200_000));
    insert_file(&client, "must-not-fetch.txt", "unreachable".to_owned());
    let tools = native_file_tools(&client).await;
    let grep = tools
        .iter()
        .find(|tool| tool.name() == "grep_file")
        .expect("grep_file tool");
    assert_eq!(
        grep.execute(
            context(),
            json!({
                "file_path": "search.rs",
                "pattern": "fn main\\(\\)",
                "is_regex": true,
                "context_lines": 1
            }),
        )
        .await
        .expect("bounded regex search"),
        Value::String(
            "Found 1 match(es) for pattern 'fn main\\(\\)' in search.rs:\n\n\n--- Match 1 at line 2 ---\n  before\n> fn Main() {}\n  after"
                .to_owned()
        )
    );
    let calls_before_invalid_regex = client
        .file_calls
        .lock()
        .expect("file call fixture lock")
        .len();
    assert!(
        grep.execute(
            context(),
            json!({"file_path": "search.rs", "pattern": "[", "is_regex": true}),
        )
        .await
        .is_err()
    );
    assert_eq!(
        client
            .file_calls
            .lock()
            .expect("file call fixture lock")
            .len(),
        calls_before_invalid_regex
    );

    let read_multiple = tools
        .iter()
        .find(|tool| tool.name() == "read_multiple_files")
        .expect("read_multiple_files tool");
    assert_eq!(
        read_multiple
            .execute(
                context(),
                json!({"file_paths": ["budget.txt", "must-not-fetch.txt"]}),
            )
            .await
            .expect("capped batch read"),
        json!({
            "budget.txt": "x".repeat(200_000),
            "must-not-fetch.txt": "Skipped: the batch's cumulative 200000-character read limit was already reached by earlier files in this call. Read this file individually with read_file."
        })
    );
    let calls = client.file_calls.lock().expect("file call fixture lock");
    assert!(
        !calls
            .iter()
            .any(|(path, _, _)| path == "must-not-fetch.txt")
    );
}

#[tokio::test]
async fn native_repository_navigation_uses_the_admitted_base_and_active_scopes() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec![
        "list_files_in_main_branch".to_owned(),
        "list_files_in_bot_branch".to_owned(),
        "get_files_from_directory".to_owned(),
    ];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native GitHub navigation toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("navigation tools");

    for (name, arguments) in [
        ("list_files_in_main_branch", json!({})),
        ("list_files_in_bot_branch", json!({})),
        (
            "get_files_from_directory",
            json!({"directory_path": "/src/nested/"}),
        ),
    ] {
        let tool = tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("selected navigation tool");
        assert_eq!(
            tool.execute(context(), arguments)
                .await
                .expect("repository file list"),
            json!(["src/lib.rs", "src/nested/mod.rs"])
        );
    }
    assert_eq!(
        client
            .file_list_calls
            .lock()
            .expect("file list call fixture lock")
            .as_slice(),
        &[
            (GitHubFileScope::BaseBranch, None),
            (GitHubFileScope::ActiveBranch, None),
            (
                GitHubFileScope::ActiveBranch,
                Some("/src/nested/".to_owned())
            )
        ]
    );
}

#[tokio::test]
async fn native_issue_reads_preserve_the_current_sdk_schemas_and_bounds() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec![
        "get_issues".to_owned(),
        "get_issue".to_owned(),
        "search_issues".to_owned(),
    ];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native GitHub issue toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("issue tools");

    for (name, arguments) in [
        ("get_issues", json!({})),
        (
            "get_issue",
            json!({"issue_number": 42, "repo_name": "EliteaAI/other"}),
        ),
        (
            "search_issues",
            json!({
                "search_query": "is:open label:rust",
                "repo_name": "EliteaAI/other",
                "max_count": 17
            }),
        ),
    ] {
        let tool = tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("selected issue tool");
        assert!(tool.execute(context(), arguments).await.is_ok());
    }
    assert_eq!(
        client
            .issue_calls
            .lock()
            .expect("issue calls fixture lock")
            .as_slice(),
        &[
            "list".to_owned(),
            "get:42:EliteaAI/other".to_owned(),
            "search:is:open label:rust:EliteaAI/other:17".to_owned(),
        ]
    );

    let search = tools
        .iter()
        .find(|tool| tool.name() == "search_issues")
        .expect("search issues tool");
    let calls_before = client
        .issue_calls
        .lock()
        .expect("issue calls fixture lock")
        .len();
    assert!(
        search
            .execute(context(), json!({"search_query": "<script>alert(1)"}))
            .await
            .is_err()
    );
    assert_eq!(
        client
            .issue_calls
            .lock()
            .expect("issue calls fixture lock")
            .len(),
        calls_before
    );
}

#[tokio::test]
async fn native_pull_request_inspection_preserves_public_scope_and_limits() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec![
        "list_open_pull_requests".to_owned(),
        "get_pull_request".to_owned(),
        "list_pull_request_diffs".to_owned(),
    ];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native pull-request toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("pull-request tools");

    for (name, arguments) in [
        ("list_open_pull_requests", json!({"max_count": 17})),
        (
            "get_pull_request",
            json!({"pr_number": 42, "repo_name": "EliteaAI/other"}),
        ),
        (
            "list_pull_request_diffs",
            json!({"pr_number": 42, "repo_name": "EliteaAI/other"}),
        ),
    ] {
        let tool = tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("selected pull-request tool");
        assert!(tool.execute(context(), arguments).await.is_ok());
    }
    assert_eq!(
        client
            .pull_request_calls
            .lock()
            .expect("pull-request calls fixture lock")
            .as_slice(),
        &[
            "list:17".to_owned(),
            "get:42:EliteaAI/other".to_owned(),
            "files:42:EliteaAI/other".to_owned(),
        ]
    );
    let list = tools
        .iter()
        .find(|tool| tool.name() == "list_open_pull_requests")
        .expect("list pull requests tool");
    assert_eq!(
        list.parameters_schema()
            .and_then(|schema| schema.pointer("/properties/max_count/maximum").cloned()),
        Some(json!(100))
    );
    let detail = tools
        .iter()
        .find(|tool| tool.name() == "get_pull_request")
        .expect("get pull request tool");
    assert_eq!(
        detail
            .parameters_schema()
            .and_then(|schema| schema.get("required").cloned()),
        Some(json!(["pr_number"]))
    );
    assert!(
        list.execute(context(), json!({"max_count": null}))
            .await
            .is_ok()
    );
    assert_eq!(
        client
            .pull_request_calls
            .lock()
            .expect("pull-request calls fixture lock")
            .last(),
        Some(&"list:100".to_owned())
    );
    assert!(
        list.execute(context(), json!({"max_count": 101}))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn native_commit_inspection_normalizes_filters_and_accepts_general_refs() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec![
        "get_commits".to_owned(),
        "get_commit_changes".to_owned(),
        "get_commits_diff".to_owned(),
    ];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native commit toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("commit tools");

    for (name, arguments) in [
        (
            "get_commits",
            json!({
                "repo_name": "EliteaAI/other",
                "sha": "release/v2",
                "path": "docs/",
                "since": "2026-08-18T12:00:00+03:00",
                "until": "2026-08-19",
                "author": "octocat",
                "max_count": 17
            }),
        ),
        (
            "get_commit_changes",
            json!({"sha": "release/v2", "repo_name": "EliteaAI/other"}),
        ),
        (
            "get_commits_diff",
            json!({
                "base_sha": "release/v1",
                "head_sha": "release/v2",
                "repo_name": "EliteaAI/other"
            }),
        ),
    ] {
        let tool = tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("selected commit tool");
        assert!(tool.execute(context(), arguments).await.is_ok());
    }
    assert_eq!(
        client
            .commit_calls
            .lock()
            .expect("commit calls fixture lock")
            .as_slice(),
        &[
            "list:EliteaAI/other:release/v2:docs/:2026-08-18T09:00:00Z:2026-08-19T00:00:00Z:octocat:17".to_owned(),
            "changes:release/v2:EliteaAI/other".to_owned(),
            "compare:release/v1:release/v2:EliteaAI/other".to_owned(),
        ]
    );

    let list = tools
        .iter()
        .find(|tool| tool.name() == "get_commits")
        .expect("get commits tool");
    let calls_before = client
        .commit_calls
        .lock()
        .expect("commit calls fixture lock")
        .len();
    assert!(
        list.execute(
            context(),
            json!({"since": "2026-08-19", "until": "2026-08-18"}),
        )
        .await
        .is_err()
    );
    assert!(
        list.execute(context(), json!({"max_count": 101}))
            .await
            .is_err()
    );
    assert_eq!(
        client
            .commit_calls
            .lock()
            .expect("commit calls fixture lock")
            .len(),
        calls_before
    );
    assert_commit_tool_schemas(&tools);
}

fn assert_commit_tool_schemas(tools: &[Arc<dyn Tool>]) {
    let list = tools
        .iter()
        .find(|tool| tool.name() == "get_commits")
        .expect("get commits tool");
    assert_eq!(
        list.parameters_schema()
            .and_then(|schema| schema.pointer("/properties/max_count/maximum").cloned()),
        Some(json!(100))
    );
    let comparison = tools
        .iter()
        .find(|tool| tool.name() == "get_commits_diff")
        .expect("compare commits tool");
    assert_eq!(
        comparison
            .parameters_schema()
            .and_then(|schema| schema.get("required").cloned()),
        Some(json!(["base_sha", "head_sha"]))
    );
}

fn assert_code_search_model_contract(search: &dyn Tool) {
    assert_eq!(
        search
            .parameters_schema()
            .and_then(|schema| schema.get("required").cloned()),
        Some(json!(["query"]))
    );
    assert!(search.description().contains("native qualifiers"));
    let schema = search
        .parameters_schema()
        .expect("code search has an argument schema");
    let query_description = schema["properties"]["query"]["description"]
        .as_str()
        .expect("query description is text");
    assert!(query_description.contains("language:rust"));
    assert!(query_description.contains("path:src"));
    assert!(query_description.contains("repo:owner/name"));
    assert!(
        schema["properties"]["order"]["description"]
            .as_str()
            .expect("order description is text")
            .contains("default descending order")
    );
    for property in ["page", "per_page"] {
        assert!(
            schema["properties"][property]["description"]
                .as_str()
                .expect("window description is text")
                .contains("(page - 1) * per_page + per_page <= 1000")
        );
    }
}

#[tokio::test]
async fn native_code_search_preserves_scopes_defaults_and_one_page_window() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec!["search_code".to_owned()];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native code-search toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("code-search tool");
    let search = tools
        .iter()
        .find(|tool| tool.name() == "search_code")
        .expect("selected code-search tool");

    assert!(
        search
            .execute(
                context(),
                json!({
                    "query": "struct Worker repo:EliteaAI/other",
                    "sort": "indexed",
                    "order": "asc",
                    "per_page": 17,
                    "page": 3
                }),
            )
            .await
            .is_ok()
    );
    assert!(
        search
            .execute(
                context(),
                json!({
                    "query": "fn main",
                    "sort": null,
                    "order": null,
                    "per_page": null,
                    "page": null
                }),
            )
            .await
            .is_ok()
    );
    assert_eq!(
        client
            .code_search_calls
            .lock()
            .expect("code-search calls fixture lock")
            .as_slice(),
        &[
            "struct Worker repo:EliteaAI/other:indexed:asc:17:3".to_owned(),
            "fn main:none:none:30:1".to_owned(),
        ]
    );
    assert_code_search_model_contract(search.as_ref());

    let calls_before = client
        .code_search_calls
        .lock()
        .expect("code-search calls fixture lock")
        .len();
    for invalid in [
        json!({"query": " "}),
        json!({"query": "x", "sort": "created"}),
        json!({"query": "x", "order": "sideways"}),
        json!({"query": "x", "per_page": 100, "page": 11}),
        json!({"query": "x", "unknown": true}),
    ] {
        assert!(search.execute(context(), invalid).await.is_err());
    }
    assert_eq!(
        client
            .code_search_calls
            .lock()
            .expect("code-search calls fixture lock")
            .len(),
        calls_before
    );
}

#[tokio::test]
async fn native_workflow_status_preserves_the_string_id_and_optional_repository_contract() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec!["get_workflow_status".to_owned()];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native workflow-status toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("workflow-status tool");
    let status = tools
        .iter()
        .find(|tool| tool.name() == "get_workflow_status")
        .expect("selected workflow-status tool");

    assert!(
        status
            .execute(
                context(),
                json!({"run_id": "9001", "repo_name": "EliteaAI/other"}),
            )
            .await
            .is_ok()
    );
    assert_eq!(
        client
            .workflow_status_calls
            .lock()
            .expect("workflow-status calls fixture lock")
            .as_slice(),
        &["9001:EliteaAI/other".to_owned()]
    );
    assert_eq!(
        status
            .parameters_schema()
            .and_then(|schema| schema.get("required").cloned()),
        Some(json!(["run_id"]))
    );

    let calls_before = client
        .workflow_status_calls
        .lock()
        .expect("workflow-status calls fixture lock")
        .len();
    for invalid in [
        json!({"run_id": 9001}),
        json!({"run_id": "0"}),
        json!({"run_id": "-1"}),
        json!({"run_id": "9223372036854775808"}),
        json!({"run_id": "9001", "unknown": true}),
    ] {
        assert!(status.execute(context(), invalid).await.is_err());
    }
    assert_eq!(
        client
            .workflow_status_calls
            .lock()
            .expect("workflow-status calls fixture lock")
            .len(),
        calls_before
    );
}

#[tokio::test]
async fn native_project_read_preserves_the_sdk_schema_with_a_visible_hard_bound() {
    let client = Arc::new(FixtureGitHubApi::default());
    let selected = vec!["list_project_issues".to_owned()];
    let toolset = test_build_with_api(
        "team-github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &(client.clone() as Arc<dyn GitHubApi>),
    )
    .expect("native project toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("project tool");
    let projects = tools
        .iter()
        .find(|tool| tool.name() == "list_project_issues")
        .expect("selected project tool");

    assert!(
        projects
            .execute(
                context(),
                json!({
                    "board_repo": "EliteaAI/elitea-platform",
                    "project_number": 7,
                    "items_count": 25
                }),
            )
            .await
            .is_ok()
    );
    assert!(
        projects
            .execute(
                context(),
                json!({
                    "board_repo": "EliteaAI/elitea-platform",
                    "project_number": 7,
                    "items_count": null
                }),
            )
            .await
            .is_ok()
    );
    assert_eq!(
        client
            .project_calls
            .lock()
            .expect("project calls fixture lock")
            .as_slice(),
        &[
            "EliteaAI/elitea-platform:7:25".to_owned(),
            "EliteaAI/elitea-platform:7:100".to_owned()
        ]
    );
    assert_eq!(
        projects
            .parameters_schema()
            .and_then(|schema| schema.pointer("/properties/items_count/maximum").cloned()),
        Some(json!(100))
    );

    let calls_before = client
        .project_calls
        .lock()
        .expect("project calls fixture lock")
        .len();
    for invalid in [
        json!({"board_repo": "missing-slash", "project_number": 7}),
        json!({"board_repo": "EliteaAI/elitea-platform", "project_number": 0}),
        json!({"board_repo": "EliteaAI/elitea-platform", "project_number": 2_147_483_648_u64}),
        json!({"board_repo": "EliteaAI/elitea-platform", "project_number": 7, "items_count": 101}),
        json!({"board_repo": "EliteaAI/elitea-platform", "project_number": 7, "unknown": true}),
    ] {
        assert!(projects.execute(context(), invalid).await.is_err());
    }
    assert_eq!(
        client
            .project_calls
            .lock()
            .expect("project calls fixture lock")
            .len(),
        calls_before
    );
}

#[test]
fn partial_profile_rejects_empty_or_wholly_unported_selection_before_network_use() {
    let Err(empty) = build_github_read_only_toolset("github", token_config(&[]), &policy(&[]))
    else {
        panic!("empty selection means the full unported SDK catalog");
    };
    assert_eq!(empty.code(), GitHubToolsetErrorCode::UnsupportedSelection);

    let Err(unsupported) =
        build_github_read_only_toolset("github", token_config(&["create_issue"]), &policy(&[]))
    else {
        panic!("write tool is not in the first read-only profile");
    };
    assert_eq!(
        unsupported.code(),
        GitHubToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn configured_materializer_keeps_supported_reads_from_mixed_sdk_selection() {
    let version = json!({
        "tools":[{
            "id":1,
            "type":"github",
            "toolkit_name":"configurations",
            "settings":settings(
                &json!({
                    "configuration_type":"github",
                    "base_url":"https://github.example.test/api/v3/",
                    "access_token":"fixture-token"
                }),
                &["list_branches_in_repo", "create_issue"]
            )
        }]
    });
    let version = version.as_object().expect("version details");
    let snapshot = FrozenToolSnapshot::from_version_details(version)
        .expect("GitHub snapshot")
        .apply_policy(policy(&[]).as_ref());

    let (toolsets, authorization) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy(&[]),
        &Map::new(),
    )
    .await
    .expect("GitHub materialization");

    assert_eq!(toolsets.len(), 1);
    assert!(authorization.is_empty());
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolsets[0].tools(readonly).await.expect("GitHub tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "list_branches_in_repo");
}

#[tokio::test]
async fn provider_tool_schemas_do_not_emit_an_unportable_integer_ceiling() {
    let selected = vec!["get_issue".to_owned(), "get_pull_request".to_owned()];
    let client: Arc<dyn GitHubApi> = Arc::new(FixtureGitHubApi::default());
    let toolset = test_build_with_api(
        "github",
        "EliteaAI/elitea-platform",
        &selected,
        &policy(&[]),
        &client,
    )
    .expect("GitHub tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("GitHub tools");

    for tool in tools {
        let schema = tool.parameters_schema().expect("parameter schema");
        let number = if tool.name() == "get_issue" {
            "issue_number"
        } else {
            "pr_number"
        };
        let property = &schema["properties"][number];
        assert_eq!(property["minimum"], 1);
        assert!(property.get("maximum").is_none());
    }
}

#[derive(Default)]
struct FixtureGitHubApi {
    branch_limits: Mutex<Vec<usize>>,
    files: Mutex<BTreeMap<String, String>>,
    file_calls: Mutex<Vec<FileCall>>,
    file_list_calls: Mutex<Vec<FileListCall>>,
    issue_calls: Mutex<Vec<String>>,
    pull_request_calls: Mutex<Vec<String>>,
    commit_calls: Mutex<Vec<String>>,
    code_search_calls: Mutex<Vec<String>>,
    workflow_status_calls: Mutex<Vec<String>>,
    project_calls: Mutex<Vec<String>>,
}

type FileCall = (String, Option<String>, Option<String>);
type FileListCall = (GitHubFileScope, Option<String>);

#[async_trait]
impl GitHubApi for FixtureGitHubApi {
    async fn get_authenticated_user(&self) -> Result<Value, GitHubClientError> {
        Ok(json!({"login": "octocat", "id": 1}))
    }

    async fn list_branches(&self, max_count: usize) -> Result<Value, GitHubClientError> {
        self.branch_limits
            .lock()
            .expect("branch limit fixture lock")
            .push(max_count);
        Ok(json!([{"name": "main", "protected": true}]))
    }

    async fn read_text_file(
        &self,
        file_path: &str,
        branch: Option<&str>,
        repository: Option<&str>,
    ) -> Result<String, GitHubClientError> {
        self.file_calls
            .lock()
            .expect("file call fixture lock")
            .push((
                file_path.to_owned(),
                branch.map(ToOwned::to_owned),
                repository.map(ToOwned::to_owned),
            ));
        Ok(self
            .files
            .lock()
            .expect("file fixture lock")
            .get(file_path)
            .cloned()
            .unwrap_or_default())
    }

    async fn list_repository_files(
        &self,
        scope: GitHubFileScope,
        directory_path: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.file_list_calls
            .lock()
            .expect("file list call fixture lock")
            .push((scope, directory_path.map(ToOwned::to_owned)));
        Ok(json!(["src/lib.rs", "src/nested/mod.rs"]))
    }

    async fn list_open_issues(&self) -> Result<Value, GitHubClientError> {
        self.issue_calls
            .lock()
            .expect("issue calls fixture lock")
            .push("list".to_owned());
        Ok(json!([]))
    }

    async fn get_issue(
        &self,
        issue_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.issue_calls
            .lock()
            .expect("issue calls fixture lock")
            .push(format!(
                "get:{issue_number}:{}",
                repository.unwrap_or("default")
            ));
        Ok(json!({"number": issue_number}))
    }

    async fn search_issues(
        &self,
        search_query: &str,
        repository: Option<&str>,
        max_count: usize,
    ) -> Result<Value, GitHubClientError> {
        self.issue_calls
            .lock()
            .expect("issue calls fixture lock")
            .push(format!(
                "search:{search_query}:{}:{max_count}",
                repository.unwrap_or("default")
            ));
        Ok(json!([]))
    }

    async fn list_open_pull_requests(&self, max_count: usize) -> Result<Value, GitHubClientError> {
        self.pull_request_calls
            .lock()
            .expect("pull-request calls fixture lock")
            .push(format!("list:{max_count}"));
        Ok(json!([]))
    }

    async fn get_pull_request(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.pull_request_calls
            .lock()
            .expect("pull-request calls fixture lock")
            .push(format!(
                "get:{pull_request_number}:{}",
                repository.unwrap_or("default")
            ));
        Ok(json!({"number": pull_request_number}))
    }

    async fn list_pull_request_files(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.pull_request_calls
            .lock()
            .expect("pull-request calls fixture lock")
            .push(format!(
                "files:{pull_request_number}:{}",
                repository.unwrap_or("default")
            ));
        Ok(json!([]))
    }

    async fn list_commits(&self, query: GitHubCommitQuery) -> Result<Value, GitHubClientError> {
        self.commit_calls
            .lock()
            .expect("commit calls fixture lock")
            .push(format!(
                "list:{}:{}:{}:{}:{}:{}:{}",
                query.repository.as_deref().unwrap_or("default"),
                query.reference.as_deref().unwrap_or("default"),
                query.path.as_deref().unwrap_or("none"),
                query.since.as_deref().unwrap_or("none"),
                query.until.as_deref().unwrap_or("none"),
                query.author.as_deref().unwrap_or("none"),
                query.max_count,
            ));
        Ok(json!([]))
    }

    async fn get_commit_changes(
        &self,
        reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.commit_calls
            .lock()
            .expect("commit calls fixture lock")
            .push(format!(
                "changes:{reference}:{}",
                repository.unwrap_or("default")
            ));
        Ok(json!({"commit_sha": reference}))
    }

    async fn compare_commits(
        &self,
        base_reference: &str,
        head_reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.commit_calls
            .lock()
            .expect("commit calls fixture lock")
            .push(format!(
                "compare:{base_reference}:{head_reference}:{}",
                repository.unwrap_or("default")
            ));
        Ok(json!({"status": "ahead"}))
    }

    async fn search_code(&self, query: GitHubCodeSearchQuery) -> Result<Value, GitHubClientError> {
        self.code_search_calls
            .lock()
            .expect("code-search calls fixture lock")
            .push(format!(
                "{}:{}:{}:{}:{}",
                query.query,
                query.sort.as_deref().unwrap_or("none"),
                query.order.as_deref().unwrap_or("none"),
                query.per_page,
                query.page,
            ));
        Ok(json!({
            "total_count": 0,
            "incomplete_results": false,
            "items": [],
            "page": query.page,
            "per_page": query.per_page
        }))
    }

    async fn get_workflow_status(
        &self,
        run_id: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        self.workflow_status_calls
            .lock()
            .expect("workflow-status calls fixture lock")
            .push(format!("{run_id}:{}", repository.unwrap_or("default")));
        Ok(json!({"id": run_id, "jobs": []}))
    }

    async fn list_project_issues(
        &self,
        board_repository: &str,
        project_number: u32,
        items_count: usize,
    ) -> Result<Value, GitHubClientError> {
        self.project_calls
            .lock()
            .expect("project calls fixture lock")
            .push(format!("{board_repository}:{project_number}:{items_count}"));
        Ok(json!({
            "id": "project-1",
            "title": "Delivery",
            "url": "https://github.example.test/orgs/EliteaAI/projects/7",
            "fields": [],
            "items": [],
            "items_total_count": 0,
            "items_truncated": false
        }))
    }
}
