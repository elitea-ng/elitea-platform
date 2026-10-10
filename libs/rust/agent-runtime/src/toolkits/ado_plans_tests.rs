use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::{Value, json};

use super::ado_test_support::{
    FixtureTransport, client, collection, context, ok, policy, settings, status, tool, tools,
};
use super::families::ado::client::AdoClientErrorCode;
use super::families::ado_plans::client::{AdoPlansClient, NewTestCase};
use super::families::ado_plans::config::AdoPlansToolkitConfig;
use super::families::ado_plans::steps::{steps_from_json, steps_from_xml};
use super::families::ado_plans::tools::{build_ado_plans_toolset, build_with_client};

fn plans(transport: Arc<FixtureTransport>) -> AdoPlansClient {
    let (ado, _) = client(&json!({}), transport);
    AdoPlansClient::with_client(ado)
}

fn test_case(id: u64) -> Value {
    json!({
        "testPlan":{"id":1,"name":"Release 1"},
        "testSuite":{"id":2,"name":"Smoke"},
        "workItem":{
            "id":id,"name":"Login works",
            "workItemFields":[{"System.State":"Design"},{"Microsoft.VSTS.TCM.Steps":"<steps />"}]
        },
        "pointAssignments":[{"id":7,"configurationId":3,"configurationName":"Windows"}],
        "order":1
    })
}

#[test]
fn json_steps_serialize_like_element_tree() {
    assert_eq!(
        steps_from_json(
            r#"[{"stepNumber":1,"action":"Open <login> & \"wait\"","expectedResult":"Form"},{"stepNumber":"2","action":"Submit"}]"#
        )
        .expect("steps"),
        "<steps><step id=\"1\" type=\"Action\"><parameterizedString isformatted=\"true\">Open &lt;login&gt; &amp; \"wait\"</parameterizedString><parameterizedString isformatted=\"true\">Form</parameterizedString></step><step id=\"2\" type=\"Action\"><parameterizedString isformatted=\"true\">Submit</parameterizedString><parameterizedString isformatted=\"true\" /></step></steps>"
    );
    assert_eq!(steps_from_json("[]").expect("no steps"), "<steps />");
    assert!(steps_from_json("{\"action\":\"x\"}").is_err());
    assert!(steps_from_json("not json").is_err());
}

#[test]
fn xml_steps_are_read_with_findtext_defaults() {
    let converted = steps_from_xml(
        "<?xml version=\"1.0\"?>\n<Steps>\n  <!-- first -->\n  <Step>\n    <StepNumber>4</StepNumber>\n    <Action>Use &amp;&#x41;<![CDATA[<raw>]]></Action>\n    <ExpectedResult/>\n  </Step>\n  <Step><Action>No number</Action></Step>\n  <Other><Action>ignored</Action></Other>\n</Steps>",
    )
    .expect("xml steps");
    assert_eq!(
        converted,
        "<steps><step id=\"4\" type=\"Action\"><parameterizedString isformatted=\"true\">Use &amp;A&lt;raw&gt;</parameterizedString><parameterizedString isformatted=\"true\" /></step><step id=\"1\" type=\"Action\"><parameterizedString isformatted=\"true\">No number</parameterizedString><parameterizedString isformatted=\"true\" /></step></steps>"
    );
    for invalid in [
        "<Steps><Step></Steps>",
        "<!DOCTYPE x [<!ENTITY a \"b\">]><Steps/>",
        "<Steps>&unknown;</Steps>",
        "<Steps/><Steps/>",
    ] {
        assert!(steps_from_xml(invalid).is_err(), "{invalid}");
    }
}

#[tokio::test]
async fn plans_and_suites_are_created_from_python_attribute_json() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.method, Method::POST);
            assert_eq!(request.path, "/contoso/Fabrikam Fiber/_apis/testplan/plans");
            assert_eq!(request.query_value("api-version"), Some("7.0"));
            assert_eq!(
                request.body,
                Some(json!({
                    "name":"Release 1","areaPath":"Fabrikam Fiber","iteration":"Fabrikam Fiber\\Sprint 1",
                    "owner":{"displayName":"Ana","id":"abc"}
                }))
            );
            ok(json!({"id":11,"name":"Release 1"}))
        }
        1 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/testplan/Plans/11/suites"
            );
            assert_eq!(
                request.body,
                Some(json!({
                    "name":"Smoke","suiteType":"staticTestSuite",
                    "parentSuite":{"id":12,"name":"Release 1"},"requirementId":5
                }))
            );
            ok(json!({"id":13}))
        }
        _ => panic!("unexpected request"),
    });
    let plans = plans(transport);
    assert_eq!(
        plans
            .create_test_plan(
                r#"{"name":"Release 1","area_path":"Fabrikam Fiber","iteration":"Fabrikam Fiber\\Sprint 1","owner":{"display_name":"Ana","id":"abc"}}"#
            )
            .await
            .expect("plan"),
        json!("Test plan 11 created successfully.")
    );
    assert_eq!(
        plans
            .create_test_suite(
                r#"{"name":"Smoke","suite_type":"staticTestSuite","parent_suite":{"id":12,"name":"Release 1"},"requirement_id":"5"}"#,
                11
            )
            .await
            .expect("suite"),
        json!("Test suite 13 created successfully.")
    );
    assert_eq!(
        plans
            .create_test_plan(r#"{"name":"x","areaPath":"y"}"#)
            .await
            .expect("an unknown attribute is model-visible"),
        json!(
            "Error creating test plan: TestPlanCreateParams.__init__() got an unexpected keyword argument 'areaPath'"
        )
    );
}

#[tokio::test]
async fn test_cases_are_added_and_listed_as_msrest_dicts() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/testplan/Plans/1/Suites/2/TestCase"
            );
            assert_eq!(request.body, Some(json!([{"workItem":{"id":23}}])));
            collection(&json!([test_case(23)]))
        }
        _ => panic!("unexpected request"),
    });
    let added = plans(transport)
        .add_test_case(r#"[{"work_item":{"id":"23"}}]"#, 1, 2)
        .await
        .expect("added");
    assert_eq!(
        added,
        json!([{
            "test_plan":{"id":1,"name":"Release 1"},
            "test_suite":{"id":2,"name":"Smoke"},
            "work_item":{
                "id":23,"name":"Login works",
                "work_item_fields":[{"System.State":"Design"},{"Microsoft.VSTS.TCM.Steps":"<steps />"}]
            },
            "point_assignments":[{"id":7,"configuration_id":3,"configuration_name":"Windows"}],
            "order":1
        }])
    );
}

#[tokio::test]
async fn create_test_case_creates_the_work_item_then_adds_it() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/wit/workitems/$Test Case"
            );
            let mut body = request.body.clone().expect("patch");
            body.as_array_mut()
                .expect("patch document")
                .sort_by(|left, right| left["path"].as_str().cmp(&right["path"].as_str()));
            assert_eq!(
                body,
                json!([
                    {"op":"add","path":"/fields/Custom.SDLC","value":"Development"},
                    {"op":"add","path":"/fields/Microsoft.VSTS.TCM.Steps","value":"<steps><step id=\"1\" type=\"Action\"><parameterizedString isformatted=\"true\">Open</parameterizedString><parameterizedString isformatted=\"true\">Shown</parameterizedString></step></steps>"},
                    {"op":"add","path":"/fields/System.Description","value":"Checks login"},
                    {"op":"add","path":"/fields/System.Title","value":"Login works"}
                ])
            );
            ok(json!({"id":23,"fields":{}}))
        }
        1 => {
            assert_eq!(request.body, Some(json!([{"workItem":{"id":23}}])));
            collection(&json!([test_case(23)]))
        }
        _ => panic!("unexpected request"),
    });
    let created = plans(transport)
        .create_test_case(&NewTestCase {
            plan_id: 1,
            suite_id: 2,
            title: "Login works".to_owned(),
            description: "Checks login".to_owned(),
            test_steps: r#"[{"stepNumber":1,"action":"Open","expectedResult":"Shown"}]"#.to_owned(),
            test_steps_format: "json".to_owned(),
            additional_fields: Some(
                r#"{"Custom.SDLC":"Development","System.Title":"ignored"}"#.to_owned(),
            ),
        })
        .await
        .expect("created");
    assert_eq!(created[0]["work_item"]["id"], json!(23));
}

#[tokio::test]
async fn a_rule_error_on_create_points_at_the_field_discovery_tool() {
    let transport = FixtureTransport::new(|_, _| {
        status(
            StatusCode::BAD_REQUEST,
            Some("TF401320: Rule Error for field SDLC. Error code: Required, InvalidEmpty."),
        )
    });
    let result = plans(transport)
        .create_test_case(&NewTestCase {
            plan_id: 1,
            suite_id: 2,
            title: "t".to_owned(),
            description: "d".to_owned(),
            test_steps: "[]".to_owned(),
            test_steps_format: "json".to_owned(),
            additional_fields: None,
        })
        .await
        .expect("model-visible");
    let text = result.as_str().expect("text");
    assert!(text.starts_with("Error creating work item: TF401320: Rule Error for field SDLC."));
    assert!(text.contains("get_all_test_case_fields_for_project()"));

    let unknown = FixtureTransport::new(|_, _| ok(json!({})));
    assert_eq!(
        plans(unknown)
            .create_test_case(&NewTestCase {
                plan_id: 1,
                suite_id: 2,
                title: "t".to_owned(),
                description: "d".to_owned(),
                test_steps: "[]".to_owned(),
                test_steps_format: "yaml".to_owned(),
                additional_fields: None,
            })
            .await
            .expect("format text"),
        json!("Unknown test steps format: yaml")
    );
}

#[tokio::test]
async fn get_test_case_adds_the_full_work_item_or_reports_none() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/testplan/Plans/1/Suites/2/TestCase/23"
            );
            collection(&json!([test_case(23)]))
        }
        1 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/wit/workitems/23"
            );
            assert_eq!(request.query_value("$expand"), Some("Relations"));
            ok(json!({"id":23,"fields":{"System.Title":"Login works"}}))
        }
        2 => collection(&json!([])),
        _ => panic!("unexpected request"),
    });
    let plans = plans(transport);
    let case = plans
        .get_test_case(1, 2, "23", None)
        .await
        .expect("test case");
    assert_eq!(
        case["work_item_full_details"],
        json!({
            "id":23,"url":"https://dev.azure.com/contoso/_workitems/edit/23",
            "System.Title":"Login works"
        })
    );
    assert_eq!(
        plans.get_test_case(1, 2, "99", None).await.expect("none"),
        json!(
            "No test cases found per given criteria: project Fabrikam Fiber, plan 1, suite 2, test case id 99"
        )
    );
}

#[tokio::test]
async fn reads_list_plans_and_effects_fail_closed() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.path, "/contoso/Fabrikam Fiber/_apis/testplan/plans");
        collection(&json!([{"id":1,"name":"P","areaPath":"A","rootSuite":{"id":2}}]))
    });
    assert_eq!(
        plans(transport).get_test_plan(None).await.expect("plans"),
        json!([{"id":1,"name":"P","area_path":"A","root_suite":{"id":2}}])
    );
    let failing = FixtureTransport::new(|_, _| status(StatusCode::INTERNAL_SERVER_ERROR, None));
    let error = plans(failing)
        .delete_test_suite(1, 2)
        .await
        .expect_err("ambiguous delete");
    assert_eq!(error.code(), AdoClientErrorCode::UnknownOutcome);
}

#[tokio::test]
async fn tool_arguments_follow_the_sdk_shapes() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(request.method, Method::GET);
        collection(&json!([]))
    });
    let toolset =
        build_with_client("plans", &[], &policy(&[]), Arc::new(plans(transport))).expect("toolset");
    let served = tools(&toolset).await;
    assert_eq!(served.len(), 12);
    assert_eq!(
        tool(&served, "get_test_plan")
            .execute(context(), json!({"plan_id":0}))
            .await
            .expect("plan_id 0 lists plans"),
        json!([])
    );
    for (name, arguments) in [
        (
            "create_test_cases",
            json!({"create_test_cases_parameters":"[]"}),
        ),
        (
            "create_test_cases",
            json!({"create_test_cases_parameters":"[{\"plan_id\":1,\"suite_id\":2,\"title\":\"t\"}]"}),
        ),
        ("delete_test_plan", json!({"plan_id":"x"})),
        ("get_test_case", json!({"plan_id":1,"suite_id":2})),
    ] {
        assert!(
            tool(&served, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} {arguments}"
        );
    }
    build_ado_plans_toolset(
        "plans",
        AdoPlansToolkitConfig::parse(&settings(&json!({}))).expect("config"),
        &policy(&[]),
    )
    .expect("production constructor");
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_, _| ok(json!({})));
    let toolset = build_with_client("gate", &[], &policy(&[]), Arc::new(plans(transport)))
        .expect("complete ado_plans toolset");
    super::sdk_conformance::assert_sdk_conformance("ado_plans", &tools(&toolset).await);
}
