use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::ado_test_support::{
    CapturedRequest, FixtureTransport, TOKEN, client, collection, context, expected_authorization,
    file, ok, policy, settings, status, tool, tools,
};
use super::attachment_test_support::{FakePlatform, PNG};
use super::families::ado::client::{AdoClientError, AdoClientErrorCode, IntoAdk};
use super::families::ado::config::{AdoConfigErrorCode, AdoToolkitConfig};
use super::families::ado::toolset::AdoToolsetErrorCode;
use super::families::ado_boards::client::{AdoBoardsClient, GetComments, SearchWorkItems};
use super::families::ado_boards::config::AdoBoardsToolkitConfig;
use super::families::ado_boards::tools::{build_ado_boards_toolset, build_with_client};
use super::families::office_host::OfficeFailure;

fn boards(transport: Arc<FixtureTransport>) -> AdoBoardsClient {
    let (ado, family) = client(&json!({}), transport);
    AdoBoardsClient::with_client(ado, family.limit)
}

fn work_item(id: u64, fields: &Value) -> Value {
    json!({
        "id":id,"rev":3,"fields":fields,
        "_links":{"self":{"href":format!("https://dev.azure.com/contoso/_apis/wit/workItems/{id}")}},
        "url":format!("https://dev.azure.com/contoso/_apis/wit/workItems/{id}")
    })
}

#[test]
fn configuration_normalizes_the_organization_and_never_renders_the_token() {
    let parsed = AdoToolkitConfig::parse(&settings(&json!({"limit":null}))).expect("valid");
    assert_eq!(
        parsed.connection().organization_text(),
        "https://dev.azure.com/contoso"
    );
    assert_eq!(parsed.connection().project(), "Fabrikam Fiber");
    assert_eq!(parsed.limit(), 5);
    let server = AdoToolkitConfig::parse(&settings(&json!({
        "ado_configuration":{"organization_url":"https://ado.example.test/tfs/DefaultCollection","token":TOKEN}
    })))
    .expect("an Azure DevOps Server collection URL is valid");
    assert_eq!(
        server.connection().organization_text(),
        "https://ado.example.test/tfs/DefaultCollection"
    );
    for invalid in [
        json!({"ado_configuration":{"organization_url":"http://dev.azure.com/contoso","token":TOKEN}}),
        json!({"ado_configuration":{"organization_url":"https://user@dev.azure.com/contoso","token":TOKEN}}),
        json!({"ado_configuration":{"organization_url":"https://dev.azure.com/contoso?x=1","token":TOKEN}}),
        json!({"ado_configuration":{"organization_url":"https://dev.azure.com/contoso"}}),
        json!({"ado_configuration":{"organization_url":"https://dev.azure.com/contoso","token":"  "}}),
        json!({"project":""}),
        json!({"limit":0}),
    ] {
        let Err(error) = AdoToolkitConfig::parse(&settings(&invalid)) else {
            panic!("invalid ADO configuration must fail: {invalid}");
        };
        assert_eq!(error.code(), AdoConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?} {error}").contains(TOKEN));
    }
    let Err(error) = AdoToolkitConfig::parse(&settings(&json!({
        "ado_configuration":{"organization_url":"https://dev.azure.com/contoso","token":"x".repeat(16 * 1_024 + 1)}
    }))) else {
        panic!("an oversized PAT must fail");
    };
    assert_eq!(error.code(), AdoConfigErrorCode::ResourceExhausted);
}

#[tokio::test]
async fn search_runs_wiql_then_reads_each_item_with_the_sdk_default_fields() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.method, Method::POST);
            assert_eq!(request.path, "/contoso/Fabrikam Fiber/_apis/wit/wiql");
            ok(json!({
                "queryType":"flat","queryResultType":"workItem","asOf":"2026-10-01T00:00:00Z",
                "workItems":[
                    {"id":300,"url":"https://dev.azure.com/contoso/_apis/wit/workItems/300"},
                    {"id":301,"url":"https://dev.azure.com/contoso/_apis/wit/workItems/301"}
                ]
            }))
        }
        1 => ok(work_item(
            300,
            &json!({"System.Title":"Login fails","System.State":"Active"}),
        )),
        2 => ok(work_item(301, &json!({"System.Title":"Logout fails"}))),
        _ => panic!("unexpected request {index}"),
    });
    let result = boards(transport.clone())
        .search_work_items(SearchWorkItems {
            query: "SELECT [System.Id] FROM workitems",
            limit: None,
            fields: None,
        })
        .await
        .expect("search");
    assert_eq!(
        result,
        json!([
            {
                "id":300,"url":"https://dev.azure.com/contoso/_workitems/edit/300",
                "System.Title":"Login fails","System.State":"Active","System.AssignedTo":"N/A",
                "System.CreatedDate":"N/A","System.ChangedDate":"N/A"
            },
            {
                "id":301,"url":"https://dev.azure.com/contoso/_workitems/edit/301",
                "System.Title":"Logout fails","System.State":"N/A","System.AssignedTo":"N/A",
                "System.CreatedDate":"N/A","System.ChangedDate":"N/A"
            }
        ])
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 3);
    assert_eq!(requests[0].query_value("$top"), Some("5"));
    assert_eq!(
        requests[0].query_value("api-version"),
        Some("7.1-preview.2")
    );
    assert_eq!(
        requests[0].body,
        Some(json!({"query":"SELECT [System.Id] FROM workitems"}))
    );
    assert_eq!(
        requests[1].path,
        "/contoso/Fabrikam Fiber/_apis/wit/workitems/300"
    );
    assert_eq!(
        requests[1].query_value("fields"),
        Some("System.Title,System.State,System.AssignedTo,System.CreatedDate,System.ChangedDate")
    );
    assert!(requests.iter().all(|request| {
        request.authorization.as_deref() == Some(expected_authorization().as_str())
            && request.authorization_sensitive
            && !request.effect
    }));
}

#[tokio::test]
async fn search_without_matches_and_with_all_items_keeps_the_sdk_texts() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.query_value("$top"), None, "limit -1 means no $top");
        ok(json!({"workItems":[]}))
    });
    let result = boards(transport)
        .search_work_items(SearchWorkItems {
            query: "SELECT [System.Id] FROM workitems",
            limit: Some(-1),
            fields: Some(vec!["System.Id".to_owned(), "System.Title".to_owned()]),
        })
        .await
        .expect("empty search");
    assert_eq!(result, json!("No work items found."));
}

#[tokio::test]
async fn create_sends_a_json_patch_to_the_typed_route_and_reports_the_sdk_message() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.method, Method::POST);
        assert_eq!(
            request.path,
            "/contoso/Fabrikam Fiber/_apis/wit/workitems/$User Story"
        );
        assert_eq!(
            request.content_type.as_deref(),
            Some("application/json-patch+json")
        );
        assert!(request.effect);
        ok(work_item(42, &json!({"System.Title":"New"})))
    });
    let boards = boards(transport.clone());
    let created = boards
        .create_work_item(
            r#"{"fields":{"System.Title":"New","Microsoft.VSTS.Common.Priority":2}}"#,
            "User Story",
        )
        .await
        .expect("create");
    assert_eq!(
        created,
        json!({
            "id":42,
            "message":"Work item 42 created successfully. View it at https://dev.azure.com/contoso/_apis/wit/workItems/42."
        })
    );
    let mut body = transport.requests()[0].body.clone().expect("patch body");
    // One `add` per field; their order follows the build's map order.
    body.as_array_mut()
        .expect("patch document")
        .sort_by(|left, right| left["path"].as_str().cmp(&right["path"].as_str()));
    assert_eq!(
        body,
        json!([
            {"op":"add","path":"/fields/Microsoft.VSTS.Common.Priority","value":2},
            {"op":"add","path":"/fields/System.Title","value":"New"}
        ])
    );
    assert_eq!(
        boards
            .create_work_item(r#"{"title":"x"}"#, "Task")
            .await
            .expect("missing fields is a model-visible result"),
        json!(
            "Issues during attempt to parse work_item_json: The 'fields' property is missing from the work_item_json."
        )
    );
}

#[tokio::test]
async fn a_refused_create_shows_the_provider_rule_and_a_failed_effect_is_unknown() {
    let refused = FixtureTransport::new(|_, _| {
        status(
            StatusCode::BAD_REQUEST,
            Some("TF401320: Rule Error for field Severity. Error code: Required."),
        )
    });
    assert_eq!(
        boards(refused)
            .create_work_item(r#"{"fields":{"System.Title":"x"}}"#, "Bug")
            .await
            .expect("validation failure is model-visible"),
        json!(
            "Error creating work item: TF401320: Rule Error for field Severity. Error code: Required."
        )
    );
    let unavailable = FixtureTransport::new(|_, _| status(StatusCode::BAD_GATEWAY, None));
    let error = boards(unavailable)
        .delete_work_item(7)
        .await
        .expect_err("a 502 after a delete is ambiguous");
    assert_eq!(error.code(), AdoClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    let read = FixtureTransport::new(|_, _| status(StatusCode::SERVICE_UNAVAILABLE, None));
    let error = boards(read)
        .get_work_item(7, None, None, None)
        .await
        .expect_err("a 503 read is retryable");
    assert_eq!(error.code(), AdoClientErrorCode::DependencyUnavailable);
    assert!(error.retryable());
    assert!(!format!("{error:?} {error}").contains(TOKEN));
}

#[tokio::test]
async fn get_work_item_projects_fields_and_relations_like_the_sdk() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.query_value("$expand"), Some("Relations"));
        assert_eq!(request.query_value("api-version"), Some("7.1-preview.3"));
        ok(json!({
            "id":9,"fields":{"System.Title":"Crash","System.State":"New"},
            "relations":[{
                "rel":"AttachedFile",
                "url":"https://dev.azure.com/contoso/_apis/wit/attachments/abc",
                "attributes":{"name":"log.txt","resourceSize":12,"isLocked":false}
            }]
        }))
    });
    let result = boards(transport)
        .get_work_item(9, None, None, Some("Relations"))
        .await
        .expect("work item");
    assert_eq!(
        result,
        json!({
            "id":9,"url":"https://dev.azure.com/contoso/_workitems/edit/9",
            "System.Title":"Crash","System.State":"New",
            "relations":[{
                "rel":"AttachedFile",
                "url":"https://dev.azure.com/contoso/_apis/wit/attachments/abc",
                "attributes":{"name":"log.txt","resourceSize":12,"isLocked":false}
            }]
        })
    );
}

#[tokio::test]
async fn relation_types_are_read_once_and_a_wrong_link_type_is_explained() {
    let transport = FixtureTransport::new(|request, _| {
        if request.path.ends_with("workitemrelationtypes") {
            assert_eq!(request.path, "/contoso/_apis/wit/workitemrelationtypes");
            return collection(&json!([
                {"name":"Parent","referenceName":"System.LinkTypes.Hierarchy-Reverse","attributes":{}},
                {"name":"Related","referenceName":"System.LinkTypes.Related","attributes":{}}
            ]));
        }
        assert_eq!(request.method, Method::PATCH);
        assert_eq!(request.path, "/contoso/_apis/wit/workitems/1");
        assert_eq!(
            request.body,
            Some(json!([{
                "op":"add","path":"/relations/-",
                "value":{
                    "rel":"System.LinkTypes.Related",
                    "url":"https://dev.azure.com/contoso/_apis/wit/workItems/2",
                    "attributes":{"comment":"same root cause"}
                }
            }]))
        );
        ok(work_item(1, &json!({})))
    });
    let boards = boards(transport.clone());
    let attributes = json!({"comment":"same root cause"});
    assert_eq!(
        boards
            .link_work_items(1, 2, "System.LinkTypes.Related", attributes.as_object())
            .await
            .expect("link"),
        json!("Work item 1 linked to 2 with link type System.LinkTypes.Related")
    );
    assert_eq!(
        boards
            .link_work_items(1, 2, "Related", None)
            .await
            .expect("wrong type is model-visible"),
        json!(
            "Link type is incorrect. You have to use proper relation's reference name NOT relation's name: {'Parent': 'System.LinkTypes.Hierarchy-Reverse', 'Related': 'System.LinkTypes.Related'}"
        )
    );
    assert_eq!(
        transport
            .requests()
            .iter()
            .filter(|request| request.path.ends_with("workitemrelationtypes"))
            .count(),
        1
    );
}

#[tokio::test]
async fn comments_page_by_continuation_three_at_a_time_up_to_the_total() {
    let transport = FixtureTransport::new(|request, index| {
        assert_eq!(
            request.path,
            "/contoso/Fabrikam Fiber/_apis/wit/workItems/5/comments"
        );
        assert_eq!(request.query_value("api-version"), Some("7.1-preview.4"));
        assert_eq!(request.query_value("includeDeleted"), Some("false"));
        assert_eq!(request.query_value("$expand"), Some("none"));
        let comment = |id: u64| {
            json!({
                "workItemId":5,"id":id,"version":1,"text":format!("comment {id}"),
                "createdBy":{"displayName":"Ana","uniqueName":"ana@contoso.test"},
                "createdDate":"2026-10-01T10:00:00.000Z"
            })
        };
        match index {
            0 => {
                assert_eq!(request.query_value("$top"), Some("5"));
                ok(json!({
                    "totalCount":9,"count":5,"continuationToken":"page-2",
                    "comments":[comment(1),comment(2),comment(3),comment(4),comment(5)]
                }))
            }
            1 => {
                assert_eq!(request.query_value("$top"), Some("3"));
                assert_eq!(request.query_value("continuationToken"), Some("page-2"));
                ok(json!({
                    "totalCount":9,"count":3,"continuationToken":"page-3",
                    "comments":[comment(6),comment(7),comment(8)]
                }))
            }
            _ => panic!("the total was reached after two pages"),
        }
    });
    let result = boards(transport)
        .get_comments(GetComments {
            work_item_id: 5,
            limit_total: Some(7),
            include_deleted: false,
            expand: Some("none"),
            order: None,
            process_images: false,
        })
        .await
        .expect("comments");
    let comments = result.as_array().expect("comment list");
    assert_eq!(comments.len(), 7);
    assert_eq!(
        comments[0],
        json!({
            "work_item_id":5,"id":1,"version":1,"text":"comment 1",
            "created_by":{"display_name":"Ana","unique_name":"ana@contoso.test"},
            "created_date":"2026-10-01T10:00:00.000Z"
        })
    );
}

#[tokio::test]
async fn wiki_links_use_the_artifact_uri_and_report_partial_failures() {
    let transport = FixtureTransport::new(|request, _| {
        match (request.method.clone(), request.path.as_str()) {
            (Method::GET, "/contoso/_apis/projects/Fabrikam Fiber") => {
                ok(json!({"id":"proj-guid","name":"Fabrikam Fiber"}))
            }
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wiki/wikis/Team.wiki") => {
                ok(json!({"id":"wiki-guid","name":"Team.wiki"}))
            }
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wiki/wikis/Team.wiki/pages") => {
                assert_eq!(request.query_value("path"), Some("/Release notes"));
                ok(json!({"id":12,"path":"/Release notes"}))
            }
            (Method::PATCH, "/contoso/Fabrikam Fiber/_apis/wit/workitems/10") => {
                assert_eq!(
                    request.body,
                    Some(json!([{
                        "op":0,"path":"/relations/-",
                        "value":{
                            "rel":"ArtifactLink",
                            "url":"vstfs:///Wiki/WikiPage/proj-guid%2Fwiki-guid%2FRelease%20notes",
                            "attributes":{"name":"Wiki Page"}
                        }
                    }]))
                );
                ok(work_item(10, &json!({})))
            }
            (Method::PATCH, "/contoso/Fabrikam Fiber/_apis/wit/workitems/11") => {
                status(StatusCode::FORBIDDEN, Some("denied"))
            }
            other => panic!("unexpected request {other:?}"),
        }
    });
    let result = boards(transport)
        .link_work_items_to_wiki_page(&[10, 11], "Team.wiki", "/Release notes")
        .await
        .expect("link to wiki");
    assert_eq!(
        result,
        json!(
            "Successfully linked work items [10] to wiki page '/Release notes' in wiki 'Team.wiki'.\nFailed to link work items: {\"11\": \"Azure DevOps authorization failed\"}"
        )
    );
}

#[tokio::test]
async fn unlink_removes_the_matching_relation_by_index() {
    let uri = "vstfs:///Wiki/WikiPage/proj-guid%2Fwiki-guid%2FHome";
    let transport = FixtureTransport::new(move |request, _| {
        match (request.method.clone(), request.path.as_str()) {
            (Method::GET, "/contoso/_apis/projects/Fabrikam Fiber") => {
                ok(json!({"id":"proj-guid"}))
            }
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W") => {
                ok(json!({"id":"wiki-guid"}))
            }
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages") => {
                ok(json!({"path":"/Home"}))
            }
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wit/workitems/3") => ok(json!({
                "id":3,"fields":{},
                "relations":[
                    {"rel":"System.LinkTypes.Related","url":"https://dev.azure.com/contoso/_apis/wit/workItems/4"},
                    {"rel":"ArtifactLink","url":uri,"attributes":{"name":"Wiki Page"}}
                ]
            })),
            (Method::GET, "/contoso/Fabrikam Fiber/_apis/wit/workitems/4") => {
                ok(json!({"id":4,"fields":{}}))
            }
            (Method::PATCH, "/contoso/Fabrikam Fiber/_apis/wit/workitems/3") => {
                assert_eq!(
                    request.body,
                    Some(json!([{"op":"remove","path":"/relations/1"}]))
                );
                ok(json!({"id":3}))
            }
            other => panic!("unexpected request {other:?}"),
        }
    });
    assert_eq!(
        boards(transport)
            .unlink_work_items_from_wiki_page(&[3, 4], "W", "/Home")
            .await
            .expect("unlink"),
        json!(
            "Successfully unlinked work items [3] from wiki page '/Home' in wiki 'W'.\nNo link to wiki page '/Home' found for work items [4]."
        )
    );
}

#[tokio::test]
async fn type_fields_render_the_sdk_report() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(
            request.path,
            "/contoso/Fabrikam Fiber/_apis/wit/workitemtypes/Bug"
        );
        ok(json!({
            "name":"Bug",
            "fields":[
                {"referenceName":"System.Title","name":"Title","alwaysRequired":true},
                {"referenceName":"System.State","name":"State","alwaysRequired":false,"allowedValues":["New","Active"]},
                {"referenceName":"Custom.Hidden","name":"Hidden","alwaysRequired":false}
            ]
        }))
    });
    let boards = boards(transport.clone());
    let report = boards
        .get_work_item_type_fields("Bug", false)
        .await
        .expect("fields");
    let expected = format!(
        "Available Fields for Work Item Type 'Bug' in Project 'Fabrikam Fiber':\n\n{rule}\n\n📋 REQUIRED FIELDS:\n{dash}\n\n✓ Title (Reference: System.Title)\n  Type: Unknown\n\n\n📝 OPTIONAL FIELDS (Common):\n{dash}\n\n  State (Reference: System.State)\n    Type: Unknown\n    Allowed Values: New, Active\n\n\n{rule}\n\n💡 Usage Instructions:\n  • Use the 'Reference' name (e.g., 'System.Title') as the field key in work_item_json\n  • Provide all required fields when creating work items\n  • For fields with allowed values, use exact value from the list\n  • Example for Bug: {{\"fields\": {{\"System.Title\": \"My title\", \"CustomField\": \"Value\"}}}}",
        rule = "=".repeat(80),
        dash = "-".repeat(80)
    );
    assert_eq!(report, Value::String(expected));
    boards
        .get_work_item_type_fields("Bug", false)
        .await
        .expect("cached");
    assert_eq!(transport.requests().len(), 1, "the second read is cached");
}

#[tokio::test]
async fn selection_omits_unserved_sdk_tools_and_policy_still_applies() {
    let transport = FixtureTransport::new(|_, _| ok(json!({})));
    let make = || Arc::new(boards(transport.clone()));
    let toolset = build_with_client(
        "boards",
        &[
            "get_work_item".into(),
            "get_image_by_url".into(),
            "index_data".into(),
        ],
        &policy(&[]),
        make(),
    )
    .expect("partial selection");
    let served = tools(&toolset).await;
    assert_eq!(
        served.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_work_item"]
    );
    assert!(served[0].description().contains("Toolkit: boards"));
    assert!(served[0].is_read_only());

    let Err(error) = build_with_client(
        "boards",
        &["attach_file_to_work_item".into()],
        &policy(&[]),
        make(),
    ) else {
        panic!("a selection with nothing served is unsupported");
    };
    assert_eq!(error.code(), AdoToolsetErrorCode::UnsupportedSelection);

    let blocked = build_with_client(
        "boards",
        &[],
        &policy(&[("ado_boards", &["delete_work_item"])]),
        make(),
    )
    .expect("policy-filtered toolset");
    let served = tools(&blocked).await;
    assert_eq!(served.len(), 10);
    assert!(served.iter().all(|tool| tool.name() != "delete_work_item"));
    assert!(!tool(&served, "create_work_item").is_read_only());

    let Ok(_) = build_ado_boards_toolset(
        "boards",
        AdoBoardsToolkitConfig::parse(&settings(&json!({}))).expect("config"),
        &policy(&[]),
        None,
    ) else {
        panic!("the production constructor builds without network");
    };
}

#[tokio::test]
async fn arguments_are_validated_before_any_provider_call() {
    let transport = FixtureTransport::new(|_, _| panic!("no request for invalid arguments"));
    let toolset = build_with_client("boards", &[], &policy(&[]), Arc::new(boards(transport)))
        .expect("toolset");
    let served = tools(&toolset).await;
    for (name, arguments) in [
        ("get_work_item", json!({})),
        ("get_work_item", json!({"id":"abc"})),
        ("get_work_item", json!({"id":0})),
        ("delete_work_item", json!({"id":1,"extra":true})),
        (
            "link_work_items",
            json!({"source_id":1,"target_id":2,"link_type":"x","attributes":"no"}),
        ),
        ("update_work_item", json!({"id":"x1","work_item_json":"{}"})),
        (
            "link_work_items_to_wiki_page",
            json!({"work_item_ids":"1","wiki_identified":"w","page_name":"/p"}),
        ),
    ] {
        assert!(
            tool(&served, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} {arguments}"
        );
    }
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_, _| ok(json!({})));
    let platform = FakePlatform::new();
    let client = Arc::new(boards(transport).with_attachments(Some(platform.host())));
    let toolset =
        build_with_client("gate", &[], &policy(&[]), client).expect("complete ado_boards toolset");
    super::sdk_conformance::assert_sdk_conformance("ado_boards", &tools(&toolset).await);
}

#[test]
fn client_errors_never_render_provider_text() {
    let error = AdoClientError::fixture(AdoClientErrorCode::NotFound, false);
    assert_eq!(error.into_adk().code, "ado.resource.not_found");
}

// ---- attach_file_to_work_item and get_image_by_url -------------------------

const ATTACHMENT_URL: &str =
    "https://dev.azure.com/contoso/_apis/wit/attachments/a-1?fileName=x.png";
const WI: &str = "/contoso/Fabrikam Fiber/_apis/wit/workitems/42";

fn host_tools(
    transport: &Arc<FixtureTransport>,
    platform: &Arc<FakePlatform>,
) -> Arc<AdoBoardsClient> {
    Arc::new(boards(Arc::clone(transport)).with_attachments(Some(platform.host())))
}

async fn host_toolset(
    transport: &Arc<FixtureTransport>,
    platform: &Arc<FakePlatform>,
) -> Vec<Arc<dyn adk_core::Tool>> {
    let toolset = build_with_client("boards", &[], &policy(&[]), host_tools(transport, platform))
        .expect("toolset with a host");
    tools(&toolset).await
}

/// A provider that accepts an upload, a link, a field read and a comment.
fn attach_provider(
    request: &CapturedRequest,
    _: usize,
) -> Result<super::families::ado::client::AdoHttpResponse, AdoClientError> {
    match (request.method.as_str(), request.path.as_str()) {
        ("POST", "/contoso/Fabrikam Fiber/_apis/wit/attachments") => {
            ok(json!({"id":"a-1","url":ATTACHMENT_URL}))
        }
        ("PATCH", WI) => ok(json!({"id":42,"rev":4})),
        ("GET", WI) => ok(work_item(42, &json!({"System.Description":"<p>old</p>"}))),
        ("POST", "/contoso/Fabrikam Fiber/_apis/wit/workItems/42/comments") => {
            ok(json!({"id":7,"text":"x"}))
        }
        other => panic!("unexpected request {other:?}"),
    }
}

async fn attach(tools: &[Arc<dyn adk_core::Tool>], arguments: Value) -> Value {
    tool(tools, "attach_file_to_work_item")
        .execute(context(), arguments)
        .await
        .expect("a result")
}

#[tokio::test]
async fn the_host_tools_are_served_only_with_a_host_and_classified() {
    let transport = FixtureTransport::new(attach_provider);
    let none = build_with_client(
        "boards",
        &[],
        &policy(&[]),
        Arc::new(boards(transport.clone())),
    )
    .expect("toolset");
    assert_eq!(tools(&none).await.len(), 11);
    let platform = FakePlatform::new();
    let served = host_toolset(&transport, &platform).await;
    assert_eq!(served.len(), 13);
    let attach = tool(&served, "attach_file_to_work_item");
    assert!(!attach.is_read_only() && !attach.is_concurrency_safe());
    assert!(attach.description().contains("50 MiB"));
    let image = tool(&served, "get_image_by_url");
    assert!(image.is_read_only() && image.is_concurrency_safe());
    super::sdk_conformance::assert_sdk_conformance("ado_boards", &served);
    // Selecting only the host tools builds with a host and not without.
    let only = ["get_image_by_url".into(), "attach_file_to_work_item".into()];
    assert!(build_with_client("b", &only, &policy(&[]), host_tools(&transport, &platform)).is_ok());
    assert!(build_with_client("b", &only, &policy(&[]), Arc::new(boards(transport))).is_err());
}

#[tokio::test]
async fn attaching_uploads_the_bytes_then_adds_the_relation() {
    let platform = FakePlatform::new();
    platform.put("bucket", "shots/x.png", PNG);
    let transport = FixtureTransport::new(attach_provider);
    let served = host_toolset(&transport, &platform).await;
    let result = attach(
        &served,
        json!({"work_item_id":42,"filepath":"/bucket/shots/x.png","comment":"login screen"}),
    )
    .await;
    assert_eq!(
        result,
        json!({
            "work_item_id":42,"attachment_id":"a-1","attachment_url":ATTACHMENT_URL,
            "filename":"x.png","mime_type":"image/png","inline_field":null,
            "message":"File 'x.png' attached to work item 42."
        })
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    let upload = &requests[0];
    assert_eq!(upload.raw, PNG);
    assert_eq!(
        upload.content_type.as_deref(),
        Some("application/octet-stream")
    );
    assert_eq!(upload.query_value("fileName"), Some("x.png"));
    assert_eq!(upload.query_value("uploadType"), Some("Simple"));
    assert_eq!(upload.query_value("api-version"), Some("7.1-preview.3"));
    assert!(upload.effect);
    assert_eq!(
        upload.authorization.as_deref(),
        Some(expected_authorization().as_str())
    );
    assert_eq!(
        requests[1].body,
        Some(json!([{"op":"add","path":"/relations/-","value":{
            "rel":"AttachedFile","url":ATTACHMENT_URL,
            "attributes":{"name":"x.png","comment":"login screen"}
        }}]))
    );
    // The same file again is a second upload and a second relation, as in the SDK.
    attach(
        &served,
        json!({"work_item_id":42,"filepath":"/bucket/shots/x.png","filename":"y.png"}),
    )
    .await;
    assert_eq!(transport.requests().len(), 4);
    assert_eq!(
        transport.requests()[2].query_value("fileName"),
        Some("y.png")
    );
}

#[tokio::test]
async fn the_inline_field_keeps_its_content_and_the_comment_follows() {
    let platform = FakePlatform::new();
    platform.put("bucket", "n.txt", b"notes");
    let transport = FixtureTransport::new(attach_provider);
    let served = host_toolset(&transport, &platform).await;
    let result = attach(
        &served,
        json!({"work_item_id":"42","filepath":"/bucket/n.txt","filename":"a<b>\"c.txt","inline_field":"System.Description","add_as_comment":true}),
    )
    .await;
    let markup = format!("<div><a href=\"{ATTACHMENT_URL}\">a&lt;b&gt;&quot;c.txt</a></div>");
    assert_eq!(result["inline_field"], "System.Description");
    assert_eq!(result["comment_added"], true);
    assert_eq!(result["mime_type"], "text/plain");
    assert!(result.get("warnings").is_none());
    let requests = transport.requests();
    let patch = requests
        .iter()
        .find(|request| request.method == Method::PATCH)
        .expect("patch");
    assert_eq!(
        patch.body.as_ref().expect("body")[1],
        json!({"op":"add","path":"/fields/System.Description","value":format!("<p>old</p>{markup}")})
    );
    let comment = requests.last().expect("comment");
    assert_eq!(comment.body, Some(json!({"text":markup})));
    assert_eq!(comment.query_value("api-version"), Some("7.1-preview.4"));
    // An image is an <img>.
    platform.put("bucket", "p.png", PNG);
    let image = attach(
        &served,
        json!({"work_item_id":42,"filepath":"/bucket/p.png","inline_field":"System.Description"}),
    )
    .await;
    assert_eq!(image["inline_field"], "System.Description");
    let patch = transport
        .requests()
        .into_iter()
        .rfind(|r| r.method == Method::PATCH)
        .expect("patch");
    assert!(
        patch.body.expect("body")[1]["value"]
            .as_str()
            .expect("text")
            .contains("<img src=")
    );
}

#[tokio::test]
async fn an_unreadable_field_is_left_alone_and_a_failed_comment_is_reported() {
    let platform = FakePlatform::new();
    platform.put("bucket", "n.txt", b"notes");
    let transport = FixtureTransport::new(|request, index| {
        match (request.method.as_str(), request.path.as_str()) {
            ("GET", WI) => status(StatusCode::FORBIDDEN, None),
            ("POST", path) if path.ends_with("/comments") => {
                status(StatusCode::INTERNAL_SERVER_ERROR, None)
            }
            _ => attach_provider(request, index),
        }
    });
    let served = host_toolset(&transport, &platform).await;
    let result = attach(
        &served,
        json!({"work_item_id":42,"filepath":"/bucket/n.txt","inline_field":"System.Description","add_as_comment":true}),
    )
    .await;
    assert_eq!(result["inline_field"], Value::Null);
    assert_eq!(result["comment_added"], false);
    let warnings = result["warnings"].as_array().expect("warnings");
    assert_eq!(warnings.len(), 2);
    assert!(
        warnings[0]
            .as_str()
            .expect("text")
            .contains("'System.Description' was not updated")
    );
    assert!(
        warnings[1]
            .as_str()
            .expect("text")
            .contains("may or may not have been added")
    );
    let message = result["message"].as_str().expect("message");
    assert!(
        message.starts_with("File 'n.txt' attached to work item 42. Warning: "),
        "{message}"
    );
    let patch = transport
        .requests()
        .into_iter()
        .find(|r| r.method == Method::PATCH)
        .expect("patch");
    assert_eq!(
        patch.body.expect("body").as_array().map(Vec::len),
        Some(1),
        "only the relation"
    );
}

#[tokio::test]
async fn an_upload_that_is_not_linked_says_so_precisely() {
    let platform = FakePlatform::new();
    platform.put("bucket", "n.txt", b"notes");
    for (patch, expected) in [
        (
            status(
                StatusCode::BAD_REQUEST,
                Some("TF401320: Rule Error for field X"),
            ),
            "'n.txt' was uploaded as attachment a-1 but NOT linked to work item 42, which is unchanged; retrying uploads the file again: Azure DevOps refused the request: TF401320: Rule Error for field X.",
        ),
        (
            status(StatusCode::NOT_FOUND, None),
            "'n.txt' was uploaded as attachment a-1 but NOT linked to work item 42, which is unchanged; retrying uploads the file again: Azure DevOps did not find the work item (or the attachment).",
        ),
        (
            status(StatusCode::INTERNAL_SERVER_ERROR, None),
            "'n.txt' was uploaded as attachment a-1 and may or may not be linked to work item 42; check the work item's attachments before retrying: Azure DevOps may have applied the requested effect; reconcile it before retrying.",
        ),
    ] {
        let patch = std::sync::Mutex::new(Some(patch));
        let transport = FixtureTransport::new(move |request, index| {
            if request.method == Method::PATCH {
                return patch.lock().expect("lock").take().expect("one patch");
            }
            attach_provider(request, index)
        });
        let served = host_toolset(&transport, &platform).await;
        let text = attach(
            &served,
            json!({"work_item_id":42,"filepath":"/bucket/n.txt"}),
        )
        .await;
        assert_eq!(text, Value::String(expected.to_owned()));
        assert_eq!(transport.requests().len(), 2, "nothing was retried");
    }
}

#[tokio::test]
async fn upload_failures_say_nothing_was_attached() {
    let platform = FakePlatform::new();
    platform.put("bucket", "n.txt", b"notes");
    for (code, expected) in [
        (
            StatusCode::UNAUTHORIZED,
            "Could not upload 'n.txt' to Azure DevOps: Azure DevOps rejected the credentials (the PAT); do not retry, tell the user; nothing was attached to work item 42.",
        ),
        (
            StatusCode::FORBIDDEN,
            "Could not upload 'n.txt' to Azure DevOps: Azure DevOps did not authorize the request; nothing was attached to work item 42.",
        ),
        (
            StatusCode::BAD_REQUEST,
            "Could not upload 'n.txt' to Azure DevOps: Azure DevOps refused the request as invalid; nothing was attached to work item 42.",
        ),
        (
            StatusCode::BAD_GATEWAY,
            "Could not upload 'n.txt' to Azure DevOps: Azure DevOps may have applied the requested effect; reconcile it before retrying; the file may or may not have been uploaded, and it was not linked to work item 42.",
        ),
    ] {
        let transport = FixtureTransport::new(move |_, _| status(code, None));
        let served = host_toolset(&transport, &platform).await;
        let text = attach(
            &served,
            json!({"work_item_id":42,"filepath":"/bucket/n.txt"}),
        )
        .await;
        assert_eq!(text, Value::String(expected.to_owned()));
        assert_eq!(transport.requests().len(), 1);
    }
    // No URL back, or one outside the organization: not linked.
    for (reply, expected) in [
        (
            json!({"id":"a-1"}),
            "Azure DevOps stored the upload but returned no attachment URL, so 'n.txt' could not be linked to work item 42; nothing was attached.",
        ),
        (
            json!({"id":"a-1","url":"https://evil.example/x"}),
            "Azure DevOps returned an attachment URL outside the configured organization, so 'n.txt' was uploaded but NOT linked to work item 42.",
        ),
    ] {
        let transport = FixtureTransport::new(move |_, _| ok(reply.clone()));
        let served = host_toolset(&transport, &platform).await;
        let text = attach(
            &served,
            json!({"work_item_id":42,"filepath":"/bucket/n.txt"}),
        )
        .await;
        assert_eq!(text, Value::String(expected.to_owned()));
        assert_eq!(transport.requests().len(), 1);
    }
}

#[tokio::test]
async fn attaching_validates_and_reports_host_failures_before_any_request() {
    let platform = FakePlatform::new();
    platform.put("bucket", "n.txt", b"notes");
    platform.put("bucket", "empty.txt", b"");
    platform.put("bucket", "big.bin", &vec![1_u8; (50 << 20) + 1]);
    let transport = FixtureTransport::new(|_, _| panic!("no request before the file is read"));
    let served = host_toolset(&transport, &platform).await;
    for arguments in [
        json!({"filepath":"/bucket/n.txt"}),
        json!({"work_item_id":0,"filepath":"/bucket/n.txt"}),
        json!({"work_item_id":42,"filepath":"/bucket/n.txt","extra":1}),
        json!({"work_item_id":42,"filepath":"/bucket/n.txt","inline_field":"System.Description/../x"}),
        json!({"work_item_id":42,"filepath":"/bucket/n.txt","filename":"a\nb"}),
        json!({"work_item_id":42,"filepath":"/bucket/n.txt","add_as_comment":"yes"}),
    ] {
        assert!(
            tool(&served, "attach_file_to_work_item")
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{arguments}"
        );
    }
    for (path, expected) in [
        ("/bucket/../x", "artifact path is not valid"),
        ("/bucket/missing", "was not found"),
        ("/bucket/empty.txt", "empty"),
        ("/bucket/big.bin", "50 MiB"),
    ] {
        let text = attach(&served, json!({"work_item_id":42,"filepath":path})).await;
        assert!(
            text.as_str().expect("text").contains(expected),
            "{path}: {text}"
        );
    }
    platform.fail_reads(OfficeFailure::AccessRefused);
    let denied = attach(
        &served,
        json!({"work_item_id":42,"filepath":"/bucket/n.txt"}),
    )
    .await;
    assert!(
        denied.as_str().expect("text").contains("refused access"),
        "{denied}"
    );
}

fn image_provider(
    request: &CapturedRequest,
    _: usize,
) -> Result<super::families::ado::client::AdoHttpResponse, AdoClientError> {
    match request.path.as_str() {
        "/contoso/_apis/wit/attachments/guid-1" => file(PNG),
        "/contoso/_apis/wit/attachments/missing" => status(StatusCode::NOT_FOUND, None),
        "/contoso/_apis/wit/attachments/big" => {
            Err(super::families::ado::client::resource_exhausted())
        }
        "/contoso/_apis/wit/attachments/junk" => file(b"not an image at all"),
        "/contoso/_apis/wit/attachments/huge" => {
            let mut header = PNG[..24].to_vec();
            header[16..20].copy_from_slice(&7_000_u32.to_be_bytes());
            header[20..24].copy_from_slice(&7_000_u32.to_be_bytes());
            file(&header)
        }
        other => panic!("unexpected request {other}"),
    }
}

async fn describe(tools: &[Arc<dyn adk_core::Tool>], arguments: Value) -> String {
    tool(tools, "get_image_by_url")
        .execute(context(), arguments)
        .await
        .expect("a result")
        .as_str()
        .expect("text")
        .to_owned()
}

#[tokio::test]
async fn an_image_is_read_through_the_connection_and_described_by_the_model() {
    let platform = FakePlatform::new();
    platform.reply("A login form.");
    platform.reply("A second reply.");
    platform.reply("A third reply.");
    platform.reply("   ");
    let transport = FixtureTransport::new(image_provider);
    let served = host_toolset(&transport, &platform).await;
    let url = "https://dev.azure.com/contoso/Fabrikam/_apis/wit/attachments/guid-1?fileName=screen%20one.PNG";
    assert_eq!(
        describe(&served, json!({"attachment_url":url})).await,
        "A login form."
    );
    let request = &transport.requests()[0];
    assert_eq!(request.method, Method::GET);
    assert_eq!(
        request.path, "/contoso/_apis/wit/attachments/guid-1",
        "the connection's organization, not the URL's project"
    );
    assert_eq!(request.query_value("download"), Some("true"));
    assert_eq!(request.accept.as_deref(), Some("application/octet-stream"));
    assert_eq!(
        request.authorization.as_deref(),
        Some(expected_authorization().as_str())
    );
    assert!(!request.effect);
    let calls = platform.model_calls();
    assert!(
        calls[0]
            .prompt
            .trim_start()
            .starts_with("You are an AI model designed for analyzing images.")
    );
    assert_eq!(&*calls[0].image_media_types, ["image/png"]);
    assert_eq!(calls[0].images_png[0], PNG);
    // A custom prompt, a name override for a URL without one, the other address
    // form and a relative URL.
    for (url, reply) in [
        (
            "https://dev.azure.com/contoso/_apis/wit/attachments/guid-1",
            "A second reply.",
        ),
        (
            "https://contoso.visualstudio.com/_apis/wit/attachments/guid-1",
            "A third reply.",
        ),
    ] {
        let text = describe(
            &served,
            json!({"attachment_url":url,"file_name":"s.png","prompt":"What is shown?"}),
        )
        .await;
        assert_eq!(text, reply);
    }
    assert_eq!(platform.model_calls()[1].prompt, "What is shown?");
    assert_eq!(
        describe(
            &served,
            json!({"attachment_url":"/_apis/wit/attachments/guid-1?fileName=a.png"})
        )
        .await,
        "[No readable text detected by LLM]"
    );
}

#[tokio::test]
async fn an_image_that_cannot_be_described_says_why_without_a_model_call() {
    let platform = FakePlatform::new();
    let transport = FixtureTransport::new(image_provider);
    let served = host_toolset(&transport, &platform).await;
    let prefix = "Error analyzing attachment image: ";
    for (arguments, expected) in [
        (
            json!({"attachment_url":"data:image/png;base64,AAAA"}),
            "data: URIs are not supported; provide an Azure DevOps attachment URL.",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/git/x?fileName=a.png"}),
            "URL must be an Azure DevOps work item attachment URL (containing /_apis/wit/attachments/).",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/?fileName=a.png"}),
            "Could not determine the attachment id from the URL.",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/guid-1"}),
            "Provide file_name; the URL has no fileName parameter.",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/guid-1?fileName=a.pdf"}),
            "Unsupported image format '.pdf'; supported: png, jpg, jpeg, gif, webp, bmp, svg",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/guid-1?fileName=a.svg"}),
            ".svg images cannot be described in this runtime (it takes png, jpg, jpeg, gif and webp).",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/other/_apis/wit/attachments/guid-1?fileName=a.png"}),
            "The attachment URL is not on the configured Azure DevOps organization; nothing was fetched.",
        ),
        (
            json!({"attachment_url":"https://evil.example/contoso/_apis/wit/attachments/guid-1?fileName=a.png"}),
            "The attachment URL is not on the configured Azure DevOps organization; nothing was fetched.",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/missing?fileName=a.png"}),
            "Could not download the attachment: Azure DevOps did not find the work item (or the attachment)",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/big?fileName=a.png"}),
            "Image exceeds the 5 MB processing limit",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/junk?fileName=a.png"}),
            "Could not read image dimensions",
        ),
        (
            json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/huge?fileName=a.png"}),
            "Image dimensions 7000x7000 exceed the processing limit",
        ),
    ] {
        assert_eq!(
            describe(&served, arguments.clone()).await,
            format!("{prefix}{expected}"),
            "{arguments}"
        );
    }
    assert!(platform.model_calls().is_empty());
    // Argument errors.
    for arguments in [
        json!({}),
        json!({"attachment_url":"x","extra":1}),
        json!({"attachment_url":5}),
    ] {
        assert!(
            tool(&served, "get_image_by_url")
                .execute(context(), arguments)
                .await
                .is_err()
        );
    }
}

#[tokio::test]
async fn model_failures_are_named_and_never_retried() {
    let platform = FakePlatform::new();
    let transport = FixtureTransport::new(image_provider);
    let served = host_toolset(&transport, &platform).await;
    let url = json!({"attachment_url":"https://dev.azure.com/contoso/_apis/wit/attachments/guid-1?fileName=a.png"});
    platform.fail_model(OfficeFailure::BudgetRefused);
    let text = describe(&served, url.clone()).await;
    assert!(text.contains("The model budget is exhausted"), "{text}");
    platform.fail_model(OfficeFailure::NoModel);
    let text = describe(&served, url).await;
    assert!(text.contains("none is bound in this run"), "{text}");
    assert_eq!(platform.model_calls().len(), 2);
}
