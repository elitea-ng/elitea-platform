use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::ado_test_support::{
    FixtureTransport, TOKEN, client, collection, context, expected_authorization, ok, policy,
    settings, status, tool, tools,
};
use super::families::ado::client::{AdoClientError, AdoClientErrorCode, IntoAdk};
use super::families::ado::config::{AdoConfigErrorCode, AdoToolkitConfig};
use super::families::ado::toolset::AdoToolsetErrorCode;
use super::families::ado_boards::client::{AdoBoardsClient, GetComments, SearchWorkItems};
use super::families::ado_boards::config::AdoBoardsToolkitConfig;
use super::families::ado_boards::tools::{build_ado_boards_toolset, build_with_client};

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
    let toolset = build_with_client("gate", &[], &policy(&[]), Arc::new(boards(transport)))
        .expect("complete ado_boards toolset");
    super::sdk_conformance::assert_sdk_conformance("ado_boards", &tools(&toolset).await);
}

#[test]
fn client_errors_never_render_provider_text() {
    let error = AdoClientError::fixture(AdoClientErrorCode::NotFound, false);
    assert_eq!(error.into_adk().code, "ado.resource.not_found");
}
