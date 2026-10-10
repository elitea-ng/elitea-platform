use std::fmt::Write as _;
use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::ado_test_support::{
    FixtureTransport, collection, context, ok, policy, settings, status, text, tool, tools,
};
use super::families::ado::client::AdoClientErrorCode;
use super::families::ado::config::AdoConfigErrorCode;
use super::families::ado_repos::client::{
    AdoReposClient, GetCommits, inline_comments, test_line_slice, test_python_datetime_str,
};
use super::families::ado_repos::config::AdoReposToolkitConfig;
use super::families::ado_repos::diff::generate_diff;
use super::families::ado_repos::tools::{build_ado_repos_toolset, build_with_client};

const REPO: &str = "/contoso/Fabrikam Fiber/_apis/git/repositories/web";

fn repos(transport: Arc<FixtureTransport>) -> AdoReposClient {
    let config = AdoReposToolkitConfig::parse(&settings(&json!({
        "repository_id":"web","base_branch":"main","active_branch":null
    })))
    .expect("repos configuration");
    let (connection, repository) = config.into_parts();
    AdoReposClient::with_client(
        super::families::ado::client::AdoClient::with_transport(connection, transport),
        repository,
    )
}

fn branch_stats(name: &str, commit: &str) -> Value {
    json!({"name":name,"aheadCount":0,"behindCount":0,"isBaseVersion":false,"commit":{"commitId":commit}})
}

#[test]
fn repository_configuration_requires_a_repository_and_defaults_branches() {
    let Err(error) = AdoReposToolkitConfig::parse(&settings(&json!({}))) else {
        panic!("repository_id is required");
    };
    assert_eq!(error.code(), AdoConfigErrorCode::InvalidConfiguration);
    let (_, repository) = AdoReposToolkitConfig::parse(&settings(
        &json!({"repository_id":"web","base_branch":"","active_branch":"develop"}),
    ))
    .expect("config")
    .into_parts();
    assert_eq!(repository.base_branch.as_ref(), "main");
    assert_eq!(repository.active_branch.as_ref(), "develop");
}

#[test]
fn unified_diffs_match_python_difflib() {
    let base = (1..=20).fold(String::new(), |mut text, index| {
        let _ = writeln!(text, "line {index}");
        text
    });
    let target = base
        .replace("line 2\n", "line two\n")
        .replace("line 15\n", "")
        .replace("line 20\n", "line 20\nline 21");
    assert_eq!(
        generate_diff(&base, &target, "src/f.py"),
        "--- a/src/f.py\n+++ b/src/f.py\n@@ -1,5 +1,5 @@\n line 1\n-line 2\n+line two\n line 3\n line 4\n line 5\n@@ -12,9 +12,9 @@\n line 12\n line 13\n line 14\n-line 15\n line 16\n line 17\n line 18\n line 19\n line 20\n+line 21"
    );
    assert_eq!(
        generate_diff("a\nb\nc", "a\nB\nc", "src/f.py"),
        "--- a/src/f.py\n+++ b/src/f.py\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c"
    );
    assert_eq!(
        generate_diff("", "new\n", "src/f.py"),
        "--- a/src/f.py\n+++ b/src/f.py\n@@ -0,0 +1 @@\n+new\n"
    );
    assert_eq!(generate_diff("same\n", "same\n", "src/f.py"), "");
    // 250 lines: the blank line is "popular" under difflib's autojunk rule.
    let big = (0..250).fold(String::new(), |mut text, index| {
        if index % 3 == 0 {
            text.push('\n');
        } else {
            let _ = writeln!(text, "x{index}");
        }
        text
    });
    let changed = big.replace("x100\n", "y100\n").replace("x7\n", "");
    assert_eq!(
        generate_diff(&big, &changed, "src/f.py"),
        "--- a/src/f.py\n+++ b/src/f.py\n@@ -5,7 +5,6 @@\n x4\n x5\n \n-x7\n x8\n \n x10\n@@ -98,7 +97,7 @@\n x97\n x98\n \n-x100\n+y100\n x101\n \n x103\n"
    );
}

#[test]
fn dates_and_line_slices_follow_python() {
    assert_eq!(
        test_python_datetime_str("2023-01-01T00:00:00Z").as_deref(),
        Some("2023-01-01 00:00:00+00:00")
    );
    assert_eq!(
        test_python_datetime_str("2023-06-30").as_deref(),
        Some("2023-06-30 00:00:00")
    );
    assert_eq!(
        test_python_datetime_str("2023-06-30T10:20:30.5+02:00").as_deref(),
        Some("2023-06-30 10:20:30.500000+02:00")
    );
    assert_eq!(test_python_datetime_str("yesterday"), None);
    assert_eq!(test_line_slice("a\nb\nc\nd\n", 2, Some(2)), "b\nc\n");
    assert_eq!(test_line_slice("a\nb\nc", 3, None), "c");
    assert_eq!(test_line_slice("a\nb", 9, Some(1)), "");
}

#[tokio::test]
async fn read_file_slices_lines_and_moves_the_active_branch() {
    let transport = FixtureTransport::new(|request, index| {
        assert_eq!(request.path, format!("{REPO}/items"));
        assert_eq!(
            request.query_value("versionDescriptor.versionType"),
            Some("branch")
        );
        assert_eq!(request.query_value("api-version"), Some("7.0"));
        match index {
            0 => {
                assert_eq!(request.query_value("path"), Some("src/app.py"));
                assert_eq!(
                    request.query_value("versionDescriptor.version"),
                    Some("feature")
                );
                text("one\ntwo\nthree\n")
            }
            1 => status(
                StatusCode::NOT_FOUND,
                Some("TF401174: The item could not be found"),
            ),
            _ => panic!("unexpected request"),
        }
    });
    let repos = repos(transport);
    assert_eq!(
        repos
            .read_file("src/app.py", "feature", Some(2), Some(1))
            .await
            .expect("slice"),
        json!("two\n")
    );
    assert_eq!(repos.active_branch().await, "feature");
    assert_eq!(
        repos
            .read_file("missing.py", "feature", None, None)
            .await
            .expect("missing"),
        json!("File not found `missing.py` on branch `feature`.")
    );
}

#[tokio::test]
async fn an_oversized_read_returns_offset_limit_guidance() {
    let transport = FixtureTransport::new(|_, _| {
        let line = format!("{}\n", "x".repeat(99));
        text(&line.repeat(2_500))
    });
    let guidance = repos(transport)
        .read_file("big.py", "main", None, None)
        .await
        .expect("guidance");
    assert_eq!(guidance["__result_status__"], json!("content_too_large"));
    assert_eq!(guidance["total_lines"], json!(2_500));
    assert_eq!(guidance["type"], json!("text/x-python"));
    assert_eq!(guidance["context"]["requested"], json!("full file read"));
    assert_eq!(guidance["context"]["actual_chars"], json!(250_000));
    assert_eq!(
        guidance["instruction_for_readFile"]["first_class_params"]["offset"],
        json!(
            "integer (1-indexed, inclusive) — first line to read. Valid range 1..2500. Omit to read from the beginning."
        )
    );
    assert!(
        guidance["instruction_for_readFile"]["first_class_params"]
            .get("start_line")
            .is_none()
    );
}

#[tokio::test]
async fn create_file_refuses_the_base_branch_and_pushes_an_add() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.path, format!("{REPO}/items"));
            status(StatusCode::NOT_FOUND, None)
        }
        1 => {
            assert_eq!(request.path, format!("{REPO}/stats/branches"));
            assert_eq!(request.query_value("name"), Some("feature"));
            ok(branch_stats("feature", "abc123"))
        }
        2 => {
            assert_eq!(request.method, Method::POST);
            assert_eq!(request.path, format!("{REPO}/pushes"));
            assert!(request.effect);
            assert_eq!(
                request.body,
                Some(json!({
                    "commits":[{"comment":"Create docs/a.md","changes":[{
                        "changeType":"add","item":{"path":"docs/a.md"},
                        "newContent":{"content":"# A\n","contentType":"rawtext"}
                    }]}],
                    "refUpdates":[{"name":"refs/heads/feature","oldObjectId":"abc123"}]
                }))
            );
            ok(json!({"pushId":7}))
        }
        3 => ok(json!({"objectId":"x","path":"/docs/a.md"})),
        _ => panic!("unexpected request"),
    });
    let repos = repos(transport);
    assert_eq!(
        repos
            .create_file("docs/a.md", "# A\n", None)
            .await
            .expect("protected"),
        json!(
            "You're attempting to commit directly to the main branch, which is protected. Please create a new branch and try again."
        )
    );
    assert_eq!(
        repos
            .create_file("docs/a.md", "# A\n", Some("feature"))
            .await
            .expect("created"),
        json!("Created file docs/a.md")
    );
    assert_eq!(
        repos
            .create_file("docs/a.md", "# A\n", Some("feature"))
            .await
            .expect("exists"),
        json!(
            "File already exists at `docs/a.md` on branch `feature`. You must use `update_file` to modify it."
        )
    );
}

#[tokio::test]
async fn update_file_applies_markers_and_pushes_an_edit() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => text("def main():\n    return 1\n"),
        1 => ok(branch_stats("feature", "head1")),
        2 => {
            assert_eq!(
                request.body.as_ref().expect("push")["commits"][0]["changes"][0],
                json!({
                    "changeType":"edit","item":{"path":"app.py"},
                    "newContent":{"content":"def main():\n    return 2\n","contentType":"rawtext"}
                })
            );
            ok(json!({"pushId":8}))
        }
        3 => text("plain\n"),
        _ => panic!("unexpected request"),
    });
    let repos = repos(transport);
    assert_eq!(
        repos
            .update_file(
                "feature",
                "app.py",
                "OLD <<<<\n    return 1\n>>>> OLD\nNEW <<<<\n    return 2\n>>>> NEW"
            )
            .await
            .expect("updated"),
        json!("Updated file app.py")
    );
    assert_eq!(
        repos
            .update_file("feature", "app.py", "no markers")
            .await
            .expect("marker text"),
        json!(
            "No OLD/NEW marker pairs found in file_query. Format: Each marker must be on its own line:\nOLD <<<<\nold text\n>>>> OLD\nNEW <<<<\nnew text\n>>>> NEW"
        )
    );
}

#[tokio::test]
async fn branches_are_listed_switched_and_created_from_the_active_head() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 | 1 => collection(&json!([
            branch_stats("main", "m1"),
            branch_stats("dev", "d1")
        ])),
        2 => {
            assert_eq!(request.query_value("name"), Some("feature/x"));
            status(StatusCode::NOT_FOUND, None)
        }
        3 => {
            assert_eq!(request.query_value("name"), Some("dev"));
            ok(branch_stats("dev", "d1"))
        }
        4 => {
            assert_eq!(request.path, format!("{REPO}/refs"));
            assert_eq!(
                request.body,
                Some(json!([{
                    "name":"refs/heads/feature/x",
                    "oldObjectId":"0000000000000000000000000000000000000000",
                    "newObjectId":"d1"
                }]))
            );
            collection(
                &json!([{"name":"refs/heads/feature/x","success":true,"updateStatus":"succeeded"}]),
            )
        }
        _ => panic!("unexpected request"),
    });
    let repos = repos(transport);
    assert_eq!(
        repos.list_branches_in_repo().await.expect("branches"),
        json!("Found 2 branches in the repository:\nmain\ndev")
    );
    assert_eq!(
        repos.set_active_branch("dev").await.expect("switch"),
        json!("Switched to branch `dev`")
    );
    assert_eq!(
        repos.create_branch("feature/x").await.expect("created"),
        json!("Branch 'feature/x' created successfully, and set as current active branch.")
    );
    assert_eq!(repos.active_branch().await, "feature/x");
    assert_eq!(
        repos.create_branch("has space").await.expect("spaces"),
        json!("Branch 'has space' contains spaces. Please remove them or use special characters")
    );
}

#[tokio::test]
async fn open_pull_requests_print_the_sdk_python_list() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.path, format!("{REPO}/pullrequests"));
            assert_eq!(request.query_value("searchCriteria.status"), Some("active"));
            assert_eq!(
                request.query_value("searchCriteria.repositoryId"),
                Some("web")
            );
            collection(&json!([{
                "pullRequestId":12,"title":"Fix it's bug","description":null,
                "sourceRefName":"refs/heads/fix","targetRefName":"refs/heads/main"
            }]))
        }
        1 => {
            assert_eq!(request.path, format!("{REPO}/pullRequests/12/threads"));
            collection(&json!([{
                "id":1,"status":"active",
                "comments":[{"id":1,"author":{"displayName":"Ana"},"content":"LGTM","publishedDate":"2026-10-01T10:00:00.123Z"}]
            }]))
        }
        2 => {
            assert_eq!(request.path, format!("{REPO}/pullRequests/12/commits"));
            collection(&json!([{"commitId":"abc","comment":"fix"}]))
        }
        _ => panic!("unexpected request"),
    });
    assert_eq!(
        repos(transport)
            .list_open_pull_requests()
            .await
            .expect("prs"),
        json!(
            "Found 1 open pull requests:\n[{'title': \"Fix it's bug\", 'description': '', 'pull_request_id': 12, 'commits': [{'commit_id': 'abc', 'comment': 'fix'}], 'comments': [{'id': 1, 'author': 'Ana', 'content': 'LGTM', 'published_date': '2026-10-01 10:00:00 UTC', 'status': 'active'}], 'source_branch': 'refs/heads/fix', 'target_branch': 'refs/heads/main'}]"
        )
    );
}

#[tokio::test]
async fn pull_request_files_diff_edits_between_the_iteration_commits() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => collection(&json!([
            {"id":1,"sourceRefCommit":{"commitId":"s1"},"targetRefCommit":{"commitId":"t1"}},
            {"id":2,"sourceRefCommit":{"commitId":"s2"},"targetRefCommit":{"commitId":"t2"}}
        ])),
        1 => {
            assert_eq!(
                request.path,
                format!("{REPO}/pullRequests/5/iterations/2/changes")
            );
            ok(json!({"changeEntries":[
                {"changeTrackingId":1,"changeId":1,"item":{"objectId":"o","path":"/app.py"},"changeType":"edit"},
                {"changeTrackingId":2,"changeId":2,"item":{"path":"/new.txt"},"changeType":"add"}
            ]}))
        }
        2 => {
            assert_eq!(request.query_value("versionDescriptor.version"), Some("t2"));
            assert_eq!(
                request.query_value("versionDescriptor.versionType"),
                Some("commit")
            );
            text("a\nb\n")
        }
        3 => {
            assert_eq!(request.query_value("versionDescriptor.version"), Some("s2"));
            text("a\nc\n")
        }
        _ => panic!("unexpected request"),
    });
    assert_eq!(
        repos(transport)
            .list_pull_request_files("5")
            .await
            .expect("diffs"),
        json!(
            "[{\"path\": \"/app.py\", \"diff\": \"--- a//app.py\\n+++ b//app.py\\n@@ -1,2 +1,2 @@\\n a\\n-b\\n+c\\n\"}, {\"path\": \"/new.txt\", \"diff\": \"Change Type: add\"}]"
        )
    );
    let none = FixtureTransport::new(|_, _| panic!("no request"));
    assert_eq!(
        repos(none)
            .list_pull_request_files("abc")
            .await
            .expect("text"),
        json!(
            "Passed argument is not INT type: abc.\nError: invalid literal for int() with base 10: 'abc'"
        )
    );
}

#[tokio::test]
async fn comments_post_threads_with_positions_or_from_the_query() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.path, format!("{REPO}/pullRequests/3/threads"));
            assert_eq!(
                request.body,
                Some(json!({
                    "comments":[{"commentType":"text","content":"Rename this"}],
                    "status":"active",
                    "threadContext":{
                        "filePath":"src/main.py",
                        "rightFileStart":{"line":35,"offset":1},"rightFileEnd":{"line":37,"offset":1},
                        "leftFileStart":{"line":20,"offset":1},"leftFileEnd":{"line":20,"offset":1}
                    }
                }))
            );
            ok(json!({"id":1}))
        }
        1 => {
            assert_eq!(request.path, format!("{REPO}/pullRequests/7/threads"));
            assert_eq!(
                request.body,
                Some(
                    json!({"comments":[{"commentType":"text","content":"Looks good\n\nreally"}],"status":"active"})
                )
            );
            ok(json!({"id":2}))
        }
        _ => panic!("unexpected request"),
    });
    let repos = repos(transport);
    let comments = inline_comments(&[json!({
        "file_path":"src/main.py","comment_text":"Rename this","left_line":20,"right_range":[35,37]
    })])
    .expect("valid inline comment");
    assert_eq!(
        repos.comment_inline(3, &comments).await.expect("inline"),
        json!(
            "Successfully added 1 comments:\nComment added to file 'src/main.py' (right file lines 35-37) (left file line 20)"
        )
    );
    assert_eq!(
        repos
            .comment_query("7\n\nLooks good\n\nreally")
            .await
            .expect("query"),
        json!("Commented on pull request 7")
    );
    assert_eq!(
        inline_comments(&[json!({"file_path":"a","comment_text":"b"})])
            .err()
            .as_deref(),
        Some(
            "Invalid input parameters: Comment must specify either `left_line`, `right_line`, `left_range`, or `right_range`."
        )
    );
    assert_eq!(
        inline_comments(&[json!({"file_path":"a","comment_text":"b","right_range":[1]})])
            .err()
            .as_deref(),
        Some("Invalid input parameters: `right_range` must be a tuple (line_start, line_end)")
    );
}

#[tokio::test]
async fn commits_filter_by_version_path_dates_and_author() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.path, format!("{REPO}/commits"));
        assert_eq!(
            request.query_value("searchCriteria.itemVersion.version"),
            Some("abc")
        );
        assert_eq!(
            request.query_value("searchCriteria.itemVersion.versionType"),
            Some("commit")
        );
        assert_eq!(request.query_value("searchCriteria.itemPath"), Some("src"));
        assert_eq!(
            request.query_value("searchCriteria.fromDate"),
            Some("2023-01-01 00:00:00+00:00")
        );
        assert_eq!(request.query_value("searchCriteria.author"), Some("Ana"));
        collection(&json!([{
            "commitId":"abc","comment":"init",
            "author":{"name":"Ana","email":"ana@contoso.test","date":"2023-02-01T10:00:00Z"},
            "remoteUrl":"https://dev.azure.com/contoso/Fabrikam/_git/web/commit/abc"
        }]))
    });
    let repos = repos(transport);
    assert_eq!(
        repos
            .get_commits(GetCommits {
                sha: Some("abc"),
                path: Some("src"),
                since: Some("2023-01-01T00:00:00Z"),
                until: None,
                author: Some("Ana"),
            })
            .await
            .expect("commits"),
        json!([{
            "sha":"abc","author":"Ana","createdAt":"2023-02-01 10:00:00+00:00","message":"init",
            "url":"https://dev.azure.com/contoso/Fabrikam/_git/web/commit/abc"
        }])
    );
    assert_eq!(
        repos
            .get_commits(GetCommits {
                sha: None,
                path: None,
                since: Some("last week"),
                until: None,
                author: None,
            })
            .await
            .expect("date text"),
        json!("Unable to retrieve commits due to error:\nInvalid isoformat string: 'last week'")
    );
}

#[tokio::test]
async fn effects_fail_closed_and_tools_validate_arguments() {
    let failing = FixtureTransport::new(|request, _| {
        if request.method == Method::GET {
            ok(branch_stats("feature", "h"))
        } else {
            status(StatusCode::SERVICE_UNAVAILABLE, None)
        }
    });
    let error = repos(failing)
        .delete_file("feature", "a.txt")
        .await
        .expect_err("ambiguous push");
    assert_eq!(error.code(), AdoClientErrorCode::UnknownOutcome);

    let transport = FixtureTransport::new(|_, _| panic!("no request for invalid arguments"));
    let toolset =
        build_with_client("repos", &[], &policy(&[]), Arc::new(repos(transport))).expect("toolset");
    let served = tools(&toolset).await;
    assert_eq!(served.len(), 15);
    assert_eq!(
        tool(&served, "comment_on_pull_request")
            .execute(context(), json!({"inline_comments":[{"file_path":"a"}]}))
            .await
            .expect("model-visible"),
        json!(
            "Invalid input parameters: `pull_request_id` must be provided when using `comments` for inline commenting."
        )
    );
    assert_eq!(
        tool(&served, "get_pull_request")
            .execute(context(), json!({"pull_request_id":"x"}))
            .await
            .expect("model-visible"),
        json!("Failed to find pull request with 'x' ID.")
    );
    for (name, arguments) in [
        (
            "read_file",
            json!({"file_path":"a","branch":"main","offset":0}),
        ),
        ("read_file", json!({"file_path":"a"})),
        ("delete_file", json!({"branch_name":"","file_path":"a"})),
        ("get_work_items", json!({"pull_request_id":"x"})),
    ] {
        assert!(
            tool(&served, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} {arguments}"
        );
    }
    build_ado_repos_toolset(
        "repos",
        AdoReposToolkitConfig::parse(&settings(&json!({"repository_id":"web"}))).expect("config"),
        &policy(&[]),
    )
    .expect("production constructor");
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_, _| ok(json!({})));
    let toolset = build_with_client("gate", &[], &policy(&[]), Arc::new(repos(transport)))
        .expect("complete ado_repos toolset");
    super::sdk_conformance::assert_sdk_conformance("ado_repos", &tools(&toolset).await);
}
