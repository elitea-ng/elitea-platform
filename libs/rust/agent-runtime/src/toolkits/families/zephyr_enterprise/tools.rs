use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};

use crate::toolkits::families::zephyr_rest::client::{ZephyrRestClient, ZephyrRestFamily};
use crate::toolkits::families::zephyr_rest::tools::{
    Arguments, ZephyrArgumentCodes, ZephyrRestToolsetError, ZephyrToolSpec, build_toolset,
    identifier_property, json_text_property, object_schema, text_property,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{EnterpriseStep, FAMILY, MAX_STEPS_PER_CALL, ZephyrEnterpriseClient};
use super::config::ZephyrEnterpriseToolkitConfig;

const TOOLKIT_TYPE: &str = "zephyr_enterprise";

static ARGUMENTS: ZephyrArgumentCodes = ZephyrArgumentCodes {
    label: "Zephyr Enterprise",
    invalid: "zephyr_enterprise.arguments.invalid",
    exhausted: "zephyr_enterprise.arguments.resource_exhausted",
};

const ZQL_DESCRIPTION: &str = "ZQL query to search for test cases. Supported: estimatedTime, testcaseId, creator, release, project, priority, altId, version, versionId, automated, folder, contents, name, comment, tag. It has to follow the syntax in examples: \"folder=\\\"TestToolkit\\\"\", \"name~\\\"TestToolkit5\\\"\"";

/// Build the five Zephyr Enterprise business tools.
pub(crate) fn build_zephyr_enterprise_toolset(
    toolkit_name: &str,
    config: ZephyrEnterpriseToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let (base_url, token, selected) = config.into_parts();
    let client = Arc::new(ZephyrEnterpriseClient::new(ZephyrRestClient::new(
        base_url, token,
    )?));
    build_with_client(toolkit_name, &selected, policy, &client)
}

fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<ZephyrEnterpriseClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    // The SDK appends the instance to every description.
    let suffix = format!("\nZephyr Enterprise instance: {}", client.instance());
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &EnterpriseTool::ALL,
        selected,
        Some(&suffix),
        policy,
        client,
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_client(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<ZephyrEnterpriseClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(*name))
        .collect::<Vec<_>>();
    build_with_client(toolkit_name, &selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, bool)> {
    EnterpriseTool::ALL
        .iter()
        .map(|kind| (kind.name(), kind.is_read_only()))
        .collect()
}

#[derive(Clone, Copy)]
enum EnterpriseTool {
    GetTestCase,
    SearchZql,
    CreateTestcase,
    AddSteps,
    GetTestcasesByZql,
}

impl EnterpriseTool {
    const ALL: [Self; 5] = [
        Self::GetTestCase,
        Self::SearchZql,
        Self::CreateTestcase,
        Self::AddSteps,
        Self::GetTestcasesByZql,
    ];
}

#[async_trait]
impl ZephyrToolSpec for EnterpriseTool {
    type Api = ZephyrEnterpriseClient;

    fn name(self) -> &'static str {
        match self {
            Self::GetTestCase => "get_test_case",
            Self::SearchZql => "search_zql",
            Self::CreateTestcase => "create_testcase",
            Self::AddSteps => "add_steps",
            Self::GetTestcasesByZql => "get_testcases_by_zql",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::GetTestCase => {
                "Retrieve test case data by id. Returns the provider's test case object for one test case tree id."
            }
            Self::SearchZql => {
                "Retrieve Zephyr entities by zql. Sends one ZQL search document (entitytype, word and optional releaseid/projectid) and returns the provider's search result. Read-only."
            }
            Self::CreateTestcase => {
                "Creates test case per given test case properties as JSON.\nNOTE: steps cannot be added from this method use method `add_steps` instead. Creation is a remote effect and can duplicate on retry."
            }
            Self::AddSteps => {
                "Adds steps to the last test case version.\n:param testcase_tree_id: The ID of the test case.\n:param steps: List of steps to add ([{\"step\": \"some_step\", \"data\": \"test step data\", \"result\": \"expected result\"}, ...])\n:return: one 'Step added: ...' line per step, joined by ';'. Steps are appended one at a time; after a failure part of the list may exist, so read the test case before retrying."
            }
            Self::GetTestcasesByZql => {
                "Retrieve testcases by zql.\n:param zql: ZQL query to search for test cases.\n:return: one 'Test case ID: ..., Test case: ...' line per match, or a sentence saying none matched."
            }
        }
    }

    fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetTestCase | Self::SearchZql | Self::GetTestcasesByZql
        )
    }

    fn schema(self) -> Value {
        match self {
            Self::GetTestCase => object_schema(
                "GetTestCasesModel",
                &[("testcase_id", identifier_property("The ID of the testcase"))],
                &["testcase_id"],
            ),
            Self::SearchZql => object_schema(
                "SearchZqlModel",
                &[(
                    "zql_json",
                    json_text_property(
                        "Search for Zephyr entities using ZQL (Zephyr Query Language), as a JSON document. Supports only search by name and id. By name: {\"entitytype\": \"testcase\", \"word\": \"name ~ \\\"Desktop.AEM.Booking\\\"\"} (always escape the name with double quotes). By id: {\"entitytype\": \"testcase\", \"word\": \"id = 358380\"}, optionally with \"releaseid\" and \"projectid\". entitytype is testcase, requirement or execution.",
                    ),
                )],
                &["zql_json"],
            ),
            Self::CreateTestcase => object_schema(
                "CreateTestcaseModel",
                &[(
                    "create_testcase_json",
                    json_text_property(
                        "JSON body of create test case query, i.e. { \"tcrCatalogTreeId\": 137973, \"testcase\": { \"name\": \"TestToolkit\", \"description\": \"some description\", \"projectId\": 75 } }",
                    ),
                )],
                &["create_testcase_json"],
            ),
            Self::AddSteps => object_schema(
                "AddStepsModel",
                &[
                    (
                        "testcase_tree_id",
                        identifier_property("The ID of the testcase"),
                    ),
                    (
                        "steps",
                        serde_json::json!({
                            "type":["array","null"],
                            "items":{"type":"object"},
                            "maxItems":MAX_STEPS_PER_CALL,
                            "description":"List of steps to add per format: [{\"step\": \"some_step\", \"data\": \"test step data\", \"result\": \"expected result\"}, ...]"
                        }),
                    ),
                ],
                &["testcase_tree_id", "steps"],
            ),
            Self::GetTestcasesByZql => object_schema(
                "TestCaseByZqlModel",
                &[("zql", text_property(ZQL_DESCRIPTION))],
                &["zql"],
            ),
        }
    }

    fn family() -> &'static ZephyrRestFamily {
        &FAMILY
    }

    fn arguments() -> &'static ZephyrArgumentCodes {
        &ARGUMENTS
    }

    async fn execute(
        self,
        api: &ZephyrEnterpriseClient,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        let into_adk = |error: crate::toolkits::families::zephyr_rest::client::ZephyrRestError| {
            error.into_adk(&FAMILY)
        };
        match self {
            Self::GetTestCase => {
                let args = Arguments::new(arguments, &ARGUMENTS, &["testcase_id"])?;
                api.get_test_case(args.identifier("testcase_id")?)
                    .await
                    .map_err(into_adk)
            }
            Self::SearchZql => {
                let args = Arguments::new(arguments, &ARGUMENTS, &["zql_json"])?;
                let zql = args.json("zql_json")?;
                api.search_by_zql(&zql).await.map_err(into_adk)
            }
            Self::CreateTestcase => {
                let args = Arguments::new(arguments, &ARGUMENTS, &["create_testcase_json"])?;
                let body = args.json("create_testcase_json")?;
                api.create_testcase(&body).await.map_err(into_adk)
            }
            Self::AddSteps => {
                let args = Arguments::new(arguments, &ARGUMENTS, &["testcase_tree_id", "steps"])?;
                let tree_id = args.identifier("testcase_tree_id")?;
                let steps = parse_steps(&args)?;
                if steps.is_empty() {
                    // The SDK returns this ToolException as the tool's text.
                    return Ok(Value::String("Steps cannot be empty.".to_owned()));
                }
                api.add_steps(tree_id, &steps).await.map_err(into_adk)
            }
            Self::GetTestcasesByZql => {
                let args = Arguments::new(arguments, &ARGUMENTS, &["zql"])?;
                api.get_testcases_by_zql(args.text("zql")?)
                    .await
                    .map_err(into_adk)
            }
        }
    }
}

/// The SDK's `steps: Optional[List[dict]]`, read with its defaults: a missing
/// `step`, `data` or `result` is the empty string.
fn parse_steps(args: &Arguments<'_>) -> adk_core::Result<Vec<EnterpriseStep>> {
    let Some(value) = args.raw("steps") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(|| args.invalid())?;
    if values.len() > MAX_STEPS_PER_CALL {
        return Err(args.exhausted());
    }
    let mut nodes = 0_usize;
    crate::toolkits::families::zephyr_rest::tools::bound_json(value, 0, &mut nodes, &ARGUMENTS)?;
    values
        .iter()
        .map(|step| {
            let step = step.as_object().ok_or_else(|| args.invalid())?;
            let field = |name: &str| {
                step.get(name)
                    .cloned()
                    .unwrap_or_else(|| Value::String(String::new()))
            };
            Ok(EnterpriseStep {
                step: field("step"),
                data: field("data"),
                result: field("result"),
            })
        })
        .collect()
}
