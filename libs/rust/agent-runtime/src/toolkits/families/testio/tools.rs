use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    MAX_OUTPUT_BYTES, TestIoApi, TestIoClient, TestIoClientError, invalid_response,
    resource_exhausted,
};
use super::config::{TestIoConfigError, TestIoConfigErrorCode, TestIoToolkitConfig};

const MAX_DESCRIPTION_CHARS: usize = 1_000;
const MAX_ARGUMENT_BYTES: usize = 64 * 1_024;
const MAX_LIST_ITEMS: usize = 256;
const MAX_FIELD_BYTES: usize = 256;
const MAX_FILTER_BYTES: usize = 4 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestIoToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the Test IO family.
pub(crate) struct TestIoToolsetError {
    code: TestIoToolsetErrorCode,
}

impl TestIoToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestIoToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for TestIoToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestIoToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestIoToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestIoToolsetErrorCode::InvalidConfiguration => {
                "the TestIO toolkit configuration is invalid"
            }
            TestIoToolsetErrorCode::ResourceExhausted => {
                "the TestIO toolkit configuration exceeds its approved limit"
            }
            TestIoToolsetErrorCode::UnsupportedSelection => {
                "the selected TestIO tool profile is not supported"
            }
            TestIoToolsetErrorCode::Client => "the TestIO client could not be created",
            TestIoToolsetErrorCode::InvalidDefinition => {
                "the TestIO ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for TestIoToolsetError {}

impl From<TestIoConfigError> for TestIoToolsetError {
    fn from(source: TestIoConfigError) -> Self {
        Self {
            code: match source.code() {
                TestIoConfigErrorCode::InvalidConfiguration => {
                    TestIoToolsetErrorCode::InvalidConfiguration
                }
                TestIoConfigErrorCode::ResourceExhausted => {
                    TestIoToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<TestIoClientError> for TestIoToolsetError {
    fn from(_: TestIoClientError) -> Self {
        Self {
            code: TestIoToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for TestIoToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: TestIoToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the thirteen Test IO reads.
///
/// An empty selection serves every read. A selection keeps the reads it
/// names; the two unserved SDK writes (and any name the SDK never declared)
/// are left out, as the catalogue marks them unavailable. A selection that
/// names no served read is `UnsupportedSelection`, which skips the toolkit.
pub(crate) fn build_testio_toolset(
    toolkit_name: &str,
    config: TestIoToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, TestIoToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    let client: Arc<dyn TestIoApi> = Arc::new(TestIoClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, TestIoToolsetError> {
    let served = selected
        .iter()
        .filter(|name| {
            TestIoToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
        })
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if !selected.is_empty() && served.is_empty() {
        return Err(TestIoToolsetError {
            code: TestIoToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn TestIoApi>,
) -> Result<BasicToolset, TestIoToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(TestIoToolKind::ALL.len());
    for kind in TestIoToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(TestIoTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "testio", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn TestIoApi>,
) -> Result<BasicToolset, TestIoToolsetError> {
    let selected = served_selection(
        &selected
            .iter()
            .map(|name| (*name).into())
            .collect::<Vec<Box<str>>>(),
    )?;
    build_with_api(toolkit_name, &selected, policy, client)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TestIoToolKind {
    ListProducts,
    GetProduct,
    ListFeatures,
    GetFeature,
    ListUserStories,
    GetUserStory,
    ListExploratoryTests,
    GetExploratoryTest,
    ListTestCases,
    GetTestCase,
    GetTestCasesForTest,
    GetTestCasesStatusesForTest,
    ListBugsForTestWithFilter,
}

impl TestIoToolKind {
    /// The SDK's `get_available_tools` order, writes removed.
    const ALL: [Self; 13] = [
        Self::ListProducts,
        Self::GetProduct,
        Self::ListFeatures,
        Self::GetFeature,
        Self::ListUserStories,
        Self::GetUserStory,
        Self::ListExploratoryTests,
        Self::GetExploratoryTest,
        Self::ListTestCases,
        Self::GetTestCase,
        Self::GetTestCasesForTest,
        Self::GetTestCasesStatusesForTest,
        Self::ListBugsForTestWithFilter,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::ListProducts => "list_products",
            Self::GetProduct => "get_product",
            Self::ListFeatures => "list_features",
            Self::GetFeature => "get_feature",
            Self::ListUserStories => "list_user_stories",
            Self::GetUserStory => "get_user_story",
            Self::ListExploratoryTests => "list_exploratory_tests",
            Self::GetExploratoryTest => "get_exploratory_test",
            Self::ListTestCases => "list_test_cases",
            Self::GetTestCase => "get_test_case",
            Self::GetTestCasesForTest => "get_test_cases_for_test",
            Self::GetTestCasesStatusesForTest => "get_test_cases_statuses_for_test",
            Self::ListBugsForTestWithFilter => "list_bugs_for_test_with_filter",
        }
    }

    /// The SDK's descriptions, kept so a model chooses the same tool.
    const fn description(self) -> &'static str {
        match self {
            Self::ListProducts => {
                "Retrieve a list of all available products with optional filtering by product IDs."
            }
            Self::GetProduct => "Retrieve detailed information about a specific product by its ID.",
            Self::ListFeatures => {
                "Retrieve a comprehensive list of features across all products with optional filtering by feature IDs."
            }
            Self::GetFeature => "Retrieve detailed information about a specific feature by its ID.",
            Self::ListUserStories => {
                "Retrieve a list of user stories with optional filtering by story IDs."
            }
            Self::GetUserStory => {
                "Retrieve detailed information about a specific user story by its ID."
            }
            Self::ListExploratoryTests => {
                "Retrieve a list of exploratory tests with optional filtering by product ID. TestIO lists exploratory tests per product, so product_id is required."
            }
            Self::GetExploratoryTest => {
                "Retrieve detailed information about a specific exploratory test by its ID. TestIO lists exploratory tests per product, so pass the product_id that owns the test."
            }
            Self::ListTestCases => {
                "Retrieve a list of test cases for a specific product with optional filtering by section."
            }
            Self::GetTestCase => {
                "Retrieve detailed information about a specific test case by its ID and product ID."
            }
            Self::GetTestCasesForTest => {
                "Retrieve detailed information about test cases for a particular launch (test)\nincluding test cases description, steps and expected result.\n"
            }
            Self::GetTestCasesStatusesForTest => {
                "Fetch information regarding statuses of executed test cases within a particular launch (test),\ne.g. Passed, Failed, Pending.\n"
            }
            Self::ListBugsForTestWithFilter => {
                "Retrieve detailed information about bugs associated with test cases\nexecuted within a particular launch (test) with optional filters.\n"
            }
        }
    }
}

struct TestIoTool {
    kind: TestIoToolKind,
    client: Arc<dyn TestIoApi>,
    description: Box<str>,
}

impl TestIoTool {
    fn new(kind: TestIoToolKind, toolkit_name: &str, client: Arc<dyn TestIoApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_CHARS)
                .collect::<String>()
                .into_boxed_str(),
        }
    }

    async fn run(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let schema = schema(self.kind);
        let allowed = schema["properties"]
            .as_object()
            .ok_or_else(invalid_arguments)?;
        if arguments.keys().any(|key| !allowed.contains_key(key)) {
            return Err(invalid_arguments());
        }
        let fields = client_fields(arguments)?;
        let data = match self.kind {
            TestIoToolKind::ListProducts
            | TestIoToolKind::GetProduct
            | TestIoToolKind::ListFeatures
            | TestIoToolKind::GetFeature
            | TestIoToolKind::ListUserStories
            | TestIoToolKind::GetUserStory => self.product_read(arguments).await?,
            _ => self.test_read(arguments).await?,
        };
        let projected = match fields {
            Some(fields) => filter_fields(data, &fields),
            None => data,
        };
        bounded(projected)
    }

    async fn product_read(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        Ok(match self.kind {
            TestIoToolKind::ListProducts => {
                let ids = optional_id_list(arguments, "filter_product_ids")?;
                let body = self.get(&["products".to_owned()], &[]).await?;
                let mut products = list_member(&body, "products")?;
                // The SDK sends `filter={"product_ids": [...]}` through
                // `requests`, which encodes a dict as its keys
                // (`filter=product_ids`), so the provider never filters.
                // The documented filter is applied here instead.
                if let (Some(ids), Value::Array(items)) = (ids, &mut products) {
                    items.retain(|item| {
                        item.get("id")
                            .and_then(Value::as_u64)
                            .is_some_and(|id| ids.contains(&id))
                    });
                }
                products
            }
            TestIoToolKind::GetProduct => {
                let product = required_id(arguments, "product_id")?;
                let body = self.get(&segments(&["products"], &[product]), &[]).await?;
                object_member(&body, "product")?
            }
            TestIoToolKind::ListFeatures => {
                let product = required_id(arguments, "product_id")?;
                let ids = optional_id_list(arguments, "filter_ids")?;
                self.features(product, ids.as_deref()).await?
            }
            TestIoToolKind::GetFeature => {
                let product = required_id(arguments, "product_id")?;
                let feature = required_id(arguments, "feature_id")?;
                first_or_empty(self.features(product, Some(&[feature])).await?)
            }
            TestIoToolKind::ListUserStories => {
                let product = required_id(arguments, "product_id")?;
                let ids = optional_id_list(arguments, "filter_ids")?;
                self.user_stories(product, ids.as_deref()).await?
            }
            TestIoToolKind::GetUserStory => {
                let product = required_id(arguments, "product_id")?;
                let story = required_id(arguments, "story_id")?;
                // The SDK returns the filtered list, not its first element.
                self.user_stories(product, Some(&[story])).await?
            }
            _ => return Err(invalid_arguments()),
        })
    }

    async fn test_read(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        Ok(match self.kind {
            TestIoToolKind::ListExploratoryTests => {
                let product =
                    optional_id(arguments, "product_id")?.ok_or_else(product_id_required)?;
                self.exploratory_tests(product).await?
            }
            TestIoToolKind::GetExploratoryTest => {
                let test = required_id(arguments, "exploratory_test_id")?;
                let product =
                    optional_id(arguments, "product_id")?.ok_or_else(product_id_required)?;
                let tests = self.exploratory_tests(product).await?;
                tests
                    .as_array()
                    .and_then(|items| {
                        items
                            .iter()
                            .find(|item| item.get("id").and_then(Value::as_u64) == Some(test))
                    })
                    .cloned()
                    .unwrap_or(Value::Null)
            }
            TestIoToolKind::ListTestCases => {
                let product = required_id(arguments, "product_id")?;
                let cycle = required_id(arguments, "cycle_id")?;
                let mut query = Vec::new();
                // The SDK sends the section only when it is truthy.
                if let Some(section) = optional_id(arguments, "section_id")?.filter(|id| *id != 0) {
                    query.push(("filter_section_ids", section.to_string()));
                }
                let body = self
                    .get(
                        &segments(&["products", "test_case_tests"], &[product, cycle]),
                        &query,
                    )
                    .await?;
                list_member(&body, "test_cases")?
            }
            TestIoToolKind::GetTestCase => {
                let product = required_id(arguments, "product_id")?;
                let test_case = required_id(arguments, "test_case_id")?;
                let body = self
                    .get(
                        &segments(&["products", "test_cases"], &[product, test_case]),
                        &[],
                    )
                    .await?;
                object_member(&body, "test_case")?
            }
            TestIoToolKind::GetTestCasesForTest => {
                let product = required_id(arguments, "product_id")?;
                let test = required_id(arguments, "test_case_test_id")?;
                let body = self
                    .get(
                        &segments(&["products", "test_case_tests"], &[product, test]),
                        &[],
                    )
                    .await?;
                body.as_object()
                    .ok_or_else(|| invalid_response().into_adk())?
                    .get("test_case_test")
                    .cloned()
                    .unwrap_or(Value::Null)
            }
            TestIoToolKind::GetTestCasesStatusesForTest => {
                let product = required_id(arguments, "product_id")?;
                let test = required_id(arguments, "test_case_test_id")?;
                let mut path = segments(&["products", "test_case_tests"], &[product, test]);
                path.push("results".to_owned());
                self.get(&path, &[]).await?
            }
            TestIoToolKind::ListBugsForTestWithFilter => {
                let mut query = Vec::new();
                for name in ["filter_product_ids", "filter_test_cycle_ids"] {
                    if let Some(value) = optional_filter(arguments, name)? {
                        query.push((name, value.to_owned()));
                    }
                }
                let body = self.get(&["bugs".to_owned()], &query).await?;
                list_member(&body, "bugs")?
            }
            _ => return Err(invalid_arguments()),
        })
    }

    async fn get(
        &self,
        segments: &[String],
        query: &[(&'static str, String)],
    ) -> Result<Value, AdkError> {
        self.client
            .get(segments, query)
            .await
            .map_err(TestIoClientError::into_adk)
    }

    async fn features(&self, product: u64, ids: Option<&[u64]>) -> Result<Value, AdkError> {
        let mut query = Vec::new();
        if let Some(ids) = ids.filter(|ids| !ids.is_empty()) {
            query.push(("filter_feature_ids", join_ids(ids)));
        }
        let mut path = segments(&["products"], &[product]);
        path.push("features".to_owned());
        let body = self.get(&path, &query).await?;
        list_member(&body, "features")
    }

    async fn user_stories(&self, product: u64, ids: Option<&[u64]>) -> Result<Value, AdkError> {
        let mut query = Vec::new();
        if let Some(ids) = ids.filter(|ids| !ids.is_empty()) {
            query.push(("filter_user_story_ids", join_ids(ids)));
        }
        let mut path = segments(&["products"], &[product]);
        path.push("user_stories".to_owned());
        let body = self.get(&path, &query).await?;
        list_member(&body, "user_stories")
    }

    async fn exploratory_tests(&self, product: u64) -> Result<Value, AdkError> {
        let mut path = segments(&["products"], &[product]);
        path.push("exploratory_tests".to_owned());
        let body = self.get(&path, &[]).await?;
        list_member(&body, "exploratory_tests")
    }
}

#[async_trait]
impl Tool for TestIoTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        self.run(arguments).await
    }
}

fn schema(kind: TestIoToolKind) -> Value {
    let (properties, required) = match kind {
        TestIoToolKind::ListProducts
        | TestIoToolKind::GetProduct
        | TestIoToolKind::ListFeatures
        | TestIoToolKind::GetFeature
        | TestIoToolKind::ListUserStories
        | TestIoToolKind::GetUserStory => product_properties(kind),
        _ => test_properties(kind),
    };
    let mut schema = json!({
        "type":"object",
        "properties":properties,
        "additionalProperties":false
    });
    if !required.is_empty() {
        schema["required"] = json!(required);
    }
    schema
}

fn id_property(description: &str) -> Value {
    json!({"type":"integer","minimum":0,"description":description})
}

fn optional_id_property(description: &str) -> Value {
    json!({"type":["integer","null"],"minimum":0,"default":null,"description":description})
}

fn id_list_property(description: &str) -> Value {
    json!({
        "type":["array","null"], "items":{"type":"integer","minimum":0},
        "maxItems":MAX_LIST_ITEMS, "default":null, "description":description
    })
}

fn fields_property() -> Value {
    json!({
        "type":["array","null"], "items":{"type":"string","minLength":1,"maxLength":MAX_FIELD_BYTES},
        "maxItems":MAX_LIST_ITEMS, "default":null,
        "description":"Fields to include in the response"
    })
}

fn product_properties(kind: TestIoToolKind) -> (Value, &'static [&'static str]) {
    let product = id_property("The ID of the product");
    let fields = fields_property();
    match kind {
        TestIoToolKind::ListProducts => (
            json!({
                "filter_product_ids":id_list_property("List of product IDs to filter by"),
                "client_fields":fields
            }),
            &[],
        ),
        TestIoToolKind::GetProduct => (
            json!({"product_id":product, "client_fields":fields}),
            &["product_id"],
        ),
        TestIoToolKind::ListFeatures => (
            json!({
                "product_id":product,
                "filter_ids":id_list_property("Filter by feature IDs"),
                "client_fields":fields
            }),
            &["product_id"],
        ),
        TestIoToolKind::GetFeature => (
            json!({
                "product_id":product,
                "feature_id":id_property("The ID of the feature"),
                "client_fields":fields
            }),
            &["product_id", "feature_id"],
        ),
        TestIoToolKind::ListUserStories => (
            json!({
                "product_id":product,
                "filter_ids":id_list_property("Filter by user story IDs"),
                "client_fields":fields
            }),
            &["product_id"],
        ),
        _ => (
            json!({
                "product_id":product,
                "story_id":id_property("The ID of the user story"),
                "client_fields":fields
            }),
            &["product_id", "story_id"],
        ),
    }
}

fn test_properties(kind: TestIoToolKind) -> (Value, &'static [&'static str]) {
    let product = id_property("The ID of the product");
    let fields = fields_property();
    match kind {
        TestIoToolKind::ListExploratoryTests => (
            json!({
                "product_id":optional_id_property("Filter by product ID. Required in practice: TestIO lists exploratory tests per product."),
                "client_fields":fields
            }),
            &[],
        ),
        TestIoToolKind::GetExploratoryTest => (
            json!({
                "exploratory_test_id":id_property("The ID of the exploratory test"),
                "product_id":optional_id_property("The ID of the product that owns the exploratory test. Required in practice: TestIO lists exploratory tests per product."),
                "client_fields":fields
            }),
            &["exploratory_test_id"],
        ),
        TestIoToolKind::ListTestCases => (
            json!({
                "product_id":product,
                "cycle_id":id_property("The ID of the test cycle"),
                "section_id":optional_id_property("Filter by section ID"),
                "client_fields":fields
            }),
            &["product_id", "cycle_id"],
        ),
        TestIoToolKind::GetTestCase => (
            json!({
                "product_id":product,
                "test_case_id":id_property("The ID of the test case"),
                "client_fields":fields
            }),
            &["product_id", "test_case_id"],
        ),
        TestIoToolKind::GetTestCasesForTest => (
            json!({
                "product_id":product,
                "test_case_test_id":id_property("The ID of the test case test")
            }),
            &["product_id", "test_case_test_id"],
        ),
        TestIoToolKind::GetTestCasesStatusesForTest => (
            json!({
                "product_id":product,
                "test_case_test_id":id_property("The ID of the test case test"),
                "client_fields":fields
            }),
            &["product_id", "test_case_test_id"],
        ),
        _ => (
            json!({
                "filter_product_ids":{
                    "type":["string","null"], "maxLength":MAX_FILTER_BYTES, "default":null,
                    "description":"Comma-separated list of product IDs to filter by"
                },
                "filter_test_cycle_ids":{
                    "type":["string","null"], "maxLength":MAX_FILTER_BYTES, "default":null,
                    "description":"Comma-separated list of test cycle IDs to filter by"
                },
                "client_fields":fields
            }),
            &[],
        ),
    }
}

fn segments(names: &[&str], ids: &[u64]) -> Vec<String> {
    let mut path = Vec::with_capacity(names.len() + ids.len());
    for (index, name) in names.iter().enumerate() {
        path.push((*name).to_owned());
        if let Some(id) = ids.get(index) {
            path.push(id.to_string());
        }
    }
    path
}

fn join_ids(ids: &[u64]) -> String {
    ids.iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>()
        .join(",")
}

/// `response.json().get(name, [])`: a missing member is an empty list.
fn list_member(body: &Value, name: &str) -> Result<Value, AdkError> {
    let object = body
        .as_object()
        .ok_or_else(|| invalid_response().into_adk())?;
    Ok(object
        .get(name)
        .cloned()
        .unwrap_or_else(|| Value::Array(Vec::new())))
}

/// `response.json().get(name, {})`.
fn object_member(body: &Value, name: &str) -> Result<Value, AdkError> {
    let object = body
        .as_object()
        .ok_or_else(|| invalid_response().into_adk())?;
    Ok(object
        .get(name)
        .cloned()
        .unwrap_or_else(|| Value::Object(Map::new())))
}

fn first_or_empty(list: Value) -> Value {
    match list {
        Value::Array(mut items) if !items.is_empty() => items.swap_remove(0),
        _ => Value::Object(Map::new()),
    }
}

/// The SDK's `filter_fields`: keep only the named keys of a dict or of each
/// dict in a list; anything else passes through.
fn filter_fields(data: Value, fields: &[String]) -> Value {
    let filter = |item: Map<String, Value>| {
        Value::Object(
            item.into_iter()
                .filter(|(key, _)| fields.iter().any(|field| field == key))
                .collect(),
        )
    };
    match data {
        Value::Array(items) => Value::Array(
            items
                .into_iter()
                .map(|item| match item {
                    Value::Object(object) => filter(object),
                    other => other,
                })
                .collect(),
        ),
        Value::Object(object) => filter(object),
        other => other,
    }
}

fn bounded(value: Value) -> Result<Value, AdkError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response().into_adk())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted().into_adk());
    }
    Ok(value)
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    let size = serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len();
    if size > MAX_ARGUMENT_BYTES {
        return Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "testio.arguments.resource_exhausted",
            "the TestIO tool arguments exceed the approved limit",
        ));
    }
    Ok(())
}

/// An integer ID, also accepted as a decimal string the way the SDK's
/// pydantic models coerce one.
fn id_value(value: &Value) -> Option<u64> {
    match value {
        Value::Number(number) => number.as_u64(),
        Value::String(text) if !text.is_empty() && text.len() <= 20 => {
            text.trim().parse::<u64>().ok()
        }
        _ => None,
    }
}

fn required_id(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    optional_id(arguments, name)?.ok_or_else(invalid_arguments)
}

fn optional_id(arguments: &Map<String, Value>, name: &str) -> Result<Option<u64>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => id_value(value).map(Some).ok_or_else(invalid_arguments),
    }
}

fn optional_id_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<Vec<u64>>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.len() <= MAX_LIST_ITEMS => values
            .iter()
            .map(|value| id_value(value).ok_or_else(invalid_arguments))
            .collect::<Result<Vec<_>, _>>()
            .map(|ids| (!ids.is_empty()).then_some(ids)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn client_fields(arguments: &Map<String, Value>) -> Result<Option<Vec<String>>, AdkError> {
    match arguments.get("client_fields") {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.len() <= MAX_LIST_ITEMS => {
            let fields = values
                .iter()
                .map(|value| {
                    value
                        .as_str()
                        .filter(|field| field.len() <= MAX_FIELD_BYTES)
                        .map(ToOwned::to_owned)
                        .ok_or_else(invalid_arguments)
                })
                .collect::<Result<Vec<_>, _>>()?;
            // `if client_fields:` — an empty list means no projection.
            Ok((!fields.is_empty()).then_some(fields))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

/// A comma-separated provider filter, sent only when non-empty.
fn optional_filter<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value))
            if value.len() <= MAX_FILTER_BYTES && !value.chars().any(char::is_control) =>
        {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "testio.arguments.invalid",
        "the TestIO tool arguments are invalid",
    )
}

fn product_id_required() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "testio.arguments.product_id_required",
        "product_id is required: TestIO lists exploratory tests per product",
    )
}
