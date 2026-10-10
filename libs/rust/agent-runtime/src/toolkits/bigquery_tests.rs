//! Focused compatibility and safety tests for the partial `BigQuery` family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::families::bigquery::client::{
    BigQueryApi, BigQueryClient, BigQueryClientError, BigQueryClientErrorCode,
    BigQueryHttpResponse, BigQueryTransport, QueryJob,
};
use super::families::bigquery::config::{BigQueryConfigErrorCode, BigQueryToolkitConfig};
use super::families::bigquery::format::Cell;
use super::families::bigquery::tools::{
    BigQueryToolsetErrorCode, TableDefaults, test_build_with_api, test_raw_tools, where_clause,
};
use super::policy::ToolAdmissionPolicy;

// The same throwaway 2048-bit PKCS#8 test key the GCP family suite signs with.
const TEST_PKCS8_KEY_BODY: &str = "MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDIp4UApaJQ247TbIW43Pg8S+GVMRT6qsdhbg6iSSL6a3qwH4VYLIFcw73rXtRnYrxTasyqi3JwWwDO8xay7FCPuWlyQbnjQjhBnMz3M57riwYhR69PWTL2E9m8CucL9tVtRDLoPhN2dYdTG/qd1WUxdBJEvnXovJImufpEtLihATWNfou3XQxySk8R7Od3diY/rv55YS6x1xZG536JgoZr4UAOr8NYDTE5tBqqc4AYc3LyLjW9VbKISWFlyIHtFU1YESRcUtVswJ1JFtTypQvPWuCiY39M+mv52q/BE9uoODtt19pt2Nsi2FEKjTEVmDMIkJoaAzJReqVeiW4VQkmzAgMBAAECggEAI6TukZDa5rY6BwDOOGq4hi2Moy4W5fiUdpBQdS+80PNq1gKjc2hkipATGs67uKnnfoIIXXtsFt1zpU+1ho9IOF/dhXh7hw1qZO1v07IN1xXZPuw3DkdwMBqSoT7mkE+G1mQ5DtyIJJD4OyFLQeJ4mXJfFGspEvD8nXiIJtBbw+3cMzbUJRYwTWfTxIHfkq7uuXUs1zn3hGm1Ku3WIQo/e3+y1eiecSTqJqrGGWLtZjB6689c59RI0leT6jM4tizOIQ3BkUXAetn/HRFbKZRcNFhh0e7+G6QIVTFX/wXHbLZsJWkPzHxNX2USoWqgpnmgiGZSGTbAt/CJ492NeX0K8QKBgQD4W6jcKVAjlu6SKrhVlhO8RdjYs4IC+Mi4/1eyhvCtgtPhrHxWb/5zHPrlYZrt3E5rdhvcshNkcOM9cS1MxwPCnJshs+eWnjXwkl+tWy3ceroc21xAhu9XHrPqNLuyX04YHV/B0Rg23aC+/C8aQmikq30yeLxFpTiz0jQdSDgnFwKBgQDO1BfuiMQBoDRDYfUx3NfwJXcw5AX81U625OU5aOZc5WBC3I/F4W5S5r3D3CbsiunD+JGxxEuR+xFjSinxQkT9hQ/Vjp5PX53wJ1WmGQmM/VyBlSN6htfCR/Y8ra9nuUiV1qphlTrckdy1wY2VreK/RG3QZcFRlrlv+mFWGXaTxQKBgQC/yYCHq3uQUDChPU4mAYPyAvomtdBzXQ0cF0rwuVXIl9vpTNqjoU6cNEfntM0AW/1O7OEtN3LUQHyq6Ogzfwf/VBJUH2p6nGhJA6/Q3jV3Kmrod9kwl0LiQvpqpRhA8WoMIzrcIA0T6WgFtBbnr1rBtxAyVpwFKEa2TmAiMK/0NwKBgDW7gCQmP9W0Sx+eWVcE6symzxpSgwO2XubA/JQ3nnFP3fxA1NExybmb3Hz/utUFGcohz6gBOSjJszC6Wb8l2kqKwRxYGuTAEIYNkgC+zG5mfBvmJPt2AKOmkmAdN06ZIjRbOpRzcoFPG6nUiPXz4M6T9nuHk/ugTri6sYLuxpGJAoGBALI0mlazlyncjdZYq8GNnN8HaQu6uMahky1cgJjnN5LSq8jC03gEhHwyPlFSmjKVXD0En2YyQC5dEZAtFde76EJMAqtU3ZbEDADY/0H1ajcguEPUXBtey/xQ2y5tWgsXtaF0PeIfamGlgC2pAnH72m5MbRKuM5IiUql/qXNlOreq";
const TOKEN: &[u8] = br#"{"access_token":"ya29.fixture","expires_in":3599,"token_type":"Bearer"}"#;

fn service_account(project_id: Option<&str>) -> String {
    let mut credentials = json!({
        "type": "service_account",
        "client_email": "reader@analytics-prod.iam.gserviceaccount.com",
        "private_key_id": "0123456789abcdef",
        "private_key": format!(
            "-----BEGIN PRIVATE KEY-----\n{TEST_PKCS8_KEY_BODY}\n-----END PRIVATE KEY-----"
        ),
        "token_uri": "https://oauth2.googleapis.com/token"
    });
    if let Some(project_id) = project_id {
        credentials["project_id"] = json!(project_id);
    }
    credentials.to_string()
}

fn settings(configuration: &Value, selected_tools: &Value) -> Map<String, Value> {
    json!({
        "bigquery_configuration": configuration,
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("BigQuery settings fixture is an object")
}

fn configuration() -> Value {
    json!({
        "api_key": service_account(Some("analytics-prod")),
        "project": "analytics-prod",
        "location": "EU",
        "dataset": "docs",
        "table": "chunks"
    })
}

fn config() -> BigQueryToolkitConfig {
    BigQueryToolkitConfig::parse(&settings(&configuration(), &json!([])))
        .expect("valid BigQuery fixture config")
}

fn defaults() -> TableDefaults {
    TableDefaults {
        project: Some("analytics-prod".to_owned()),
        dataset: Some("docs".to_owned()),
        table: Some("chunks".to_owned()),
    }
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("BigQuery fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("bigquery-test")
            .with_session_id("session-1")
            .with_function_call_id("bigquery-call-1"),
    )
}

#[derive(Clone, Debug)]
struct RecordedRequest {
    method: String,
    url: String,
    authorization: Option<String>,
    body: Option<Value>,
    effect: bool,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<BigQueryHttpResponse, BigQueryClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<BigQueryHttpResponse, BigQueryClientError>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        })
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests.lock().expect("request fixture lock").clone()
    }
}

#[async_trait]
impl BigQueryTransport for FixtureTransport {
    async fn execute(
        &self,
        request: reqwest::Request,
        effect: bool,
        _response_limit: usize,
    ) -> Result<BigQueryHttpResponse, BigQueryClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        self.requests
            .lock()
            .expect("request fixture lock")
            .push(RecordedRequest {
                method: request.method().to_string(),
                url: request.url().to_string(),
                authorization: request
                    .headers()
                    .get(reqwest::header::AUTHORIZATION)
                    .and_then(|value| value.to_str().ok())
                    .map(str::to_owned),
                body,
                effect,
            });
        self.responses
            .lock()
            .expect("response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(BigQueryClientError::fixture(
                    BigQueryClientErrorCode::InvalidResponse,
                    false,
                ))
            })
    }
}

fn ok(body: &Value) -> BigQueryHttpResponse {
    BigQueryHttpResponse::fixture(StatusCode::OK, body.to_string().into_bytes())
}

fn token() -> BigQueryHttpResponse {
    BigQueryHttpResponse::fixture(StatusCode::OK, TOKEN)
}

fn client(transport: &Arc<FixtureTransport>) -> BigQueryClient {
    let transport: Arc<dyn BigQueryTransport> = transport.clone();
    BigQueryClient::with_transport(config(), transport)
}

/// A jobs.query response shaped like the real REST API's.
fn query_response(rows: &Value, page_token: Option<&str>) -> Value {
    let mut response = json!({
        "kind": "bigquery#queryResponse",
        "schema": {"fields": [
            {"name": "doc_id", "type": "STRING", "mode": "NULLABLE"},
            {"name": "content", "type": "STRING", "mode": "NULLABLE"},
            {"name": "views", "type": "INTEGER", "mode": "NULLABLE"},
            {"name": "embedding", "type": "FLOAT", "mode": "REPEATED"},
            {"name": "published", "type": "TIMESTAMP", "mode": "NULLABLE"},
            {"name": "meta", "type": "RECORD", "mode": "NULLABLE", "fields": [
                {"name": "lang", "type": "STRING", "mode": "NULLABLE"},
                {"name": "draft", "type": "BOOLEAN", "mode": "NULLABLE"}
            ]}
        ]},
        "jobReference": {"projectId": "analytics-prod", "jobId": "job_Ab12", "location": "EU"},
        "totalRows": "2",
        "rows": rows,
        "jobComplete": true,
        "cacheHit": false
    });
    if let Some(page_token) = page_token {
        response["pageToken"] = json!(page_token);
    }
    response
}

fn row(doc_id: &str, views: &str) -> Value {
    json!({"f": [
        {"v": doc_id},
        {"v": "caf\u{e9} notes"},
        {"v": views},
        {"v": [{"v": "0.5"}, {"v": "1"}]},
        {"v": "1700000000123456"},
        {"v": {"f": [{"v": "en"}, {"v": "false"}]}}
    ]})
}

#[test]
fn configuration_is_nested_strict_and_redacted() {
    let config = config();
    assert!(config.selected_tools().is_empty());
    for (configuration, expected) in [
        (
            json!({"project": "p"}),
            BigQueryConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"api_key": "", "project": "p"}),
            BigQueryConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"api_key": "{not json"}),
            BigQueryConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"api_key": service_account(None), "table": "bad`table"}),
            BigQueryConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"api_key": service_account(None), "dataset": "bad-dataset"}),
            BigQueryConfigErrorCode::InvalidConfiguration,
        ),
        (
            json!({"api_key": "x".repeat(200 * 1_024)}),
            BigQueryConfigErrorCode::ResourceExhausted,
        ),
    ] {
        let Err(error) = BigQueryToolkitConfig::parse(&settings(&configuration, &json!([]))) else {
            panic!("configuration must be refused: {configuration}");
        };
        assert_eq!(error.code(), expected, "{configuration}");
        let rendered = format!("{error:?} {error}");
        assert!(!rendered.contains("PRIVATE KEY"));
    }
    // A cleared UI field is the empty string, which the SDK reads as unset.
    let cleared = json!({
        "api_key": service_account(Some("fallback-project")),
        "project": "",
        "dataset": "",
        "table": "",
        "location": ""
    });
    BigQueryToolkitConfig::parse(&settings(&cleared, &json!(["get_documents"])))
        .expect("empty strings are unset defaults");
    assert!(
        BigQueryToolkitConfig::parse(&json!({"api_key": "x"}).as_object().cloned().unwrap())
            .is_err(),
        "the SDK's credentials live under bigquery_configuration"
    );
}

struct FixtureApi {
    jobs: Mutex<Vec<(String, Vec<Value>, bool)>>,
    rows: Vec<Cell>,
}

impl FixtureApi {
    fn new(rows: Vec<Cell>) -> Arc<Self> {
        Arc::new(Self {
            jobs: Mutex::new(Vec::new()),
            rows,
        })
    }

    fn jobs(&self) -> Vec<(String, Vec<Value>, bool)> {
        self.jobs.lock().expect("job fixture lock").clone()
    }
}

#[async_trait]
impl BigQueryApi for FixtureApi {
    async fn query(&self, job: QueryJob) -> Result<Vec<Cell>, BigQueryClientError> {
        self.jobs
            .lock()
            .expect("job fixture lock")
            .push((job.sql, job.parameters, job.effect));
        Ok(self.rows.clone())
    }

    async fn job_statistics(&self, job_id: &str) -> Result<Value, BigQueryClientError> {
        Ok(json!({"job": job_id, "totalBytesProcessed": "42"}))
    }

    async fn create_table(
        &self,
        project: &str,
        dataset: &str,
        table: &Value,
    ) -> Result<Value, BigQueryClientError> {
        Ok(json!({"project": project, "dataset": dataset, "table": table}))
    }
}

fn fixture_row() -> Cell {
    Cell::Record(vec![
        ("doc_id".to_owned(), Cell::Text("a".to_owned())),
        (
            "embedding".to_owned(),
            Cell::List(vec![Cell::Float(0.5), Cell::Float(1.0)]),
        ),
        ("score".to_owned(), Cell::Float(0.25)),
    ])
}

async fn tools_for(
    api: &Arc<FixtureApi>,
    selected: &[&str],
    defaults: &TableDefaults,
) -> Vec<Arc<dyn Tool>> {
    let api: Arc<dyn BigQueryApi> = api.clone();
    let selected = selected
        .iter()
        .map(|name| (*name).to_owned())
        .collect::<Vec<_>>();
    let toolset = test_build_with_api("warehouse", &selected, defaults, &policy(&[]), &api)
        .expect("BigQuery fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("BigQuery tools")
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, arguments: Value) -> adk_core::Result<Value> {
    let tool = tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("tool {name} is served"));
    tool.execute(context(), arguments).await
}

#[tokio::test]
async fn selection_follows_the_sdk_and_omits_only_the_unserved_tools() {
    let api = FixtureApi::new(Vec::new());
    // The SDK binds only explicitly selected tools.
    assert!(tools_for(&api, &[], &defaults()).await.is_empty());

    let all = super::sdk_conformance::sdk_tool_names("bigquery");
    let all = all.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = tools_for(&api, &all, &defaults()).await;
    let names = tools.iter().map(|tool| tool.name()).collect::<Vec<_>>();
    assert_eq!(
        names,
        [
            "get_documents",
            "batch_search",
            "create_vector_index",
            "job_stats",
            "similarity_search_by_vector",
            "similarity_search_by_vector_with_score",
            "similarity_search_by_vectors",
            "create_delta_lake_table",
        ]
    );
    for tool in &tools {
        assert!(
            tool.description()
                .starts_with("Project: analytics-prod\nToolkit: warehouse\n")
        );
        assert!(tool.description().chars().count() <= 1_000);
        let effect = matches!(
            tool.name(),
            "create_vector_index" | "create_delta_lake_table"
        );
        assert_eq!(tool.is_read_only(), !effect, "{}", tool.name());
    }

    let api_dyn: Arc<dyn BigQueryApi> = api.clone();
    let Err(error) = test_build_with_api(
        "warehouse",
        &["execute".to_owned(), "similarity_search".to_owned()],
        &defaults(),
        &policy(&[]),
        &api_dyn,
    ) else {
        panic!("a selection of only unserved tools must be refused");
    };
    assert_eq!(error.code(), BigQueryToolsetErrorCode::UnsupportedSelection);

    let blocked = test_build_with_api(
        "warehouse",
        &["job_stats".to_owned(), "get_documents".to_owned()],
        &defaults(),
        &policy(&[("bigquery", &["job_stats"])]),
        &api_dyn,
    )
    .expect("blocked definition");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = blocked
        .tools(readonly)
        .await
        .expect("blocked tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names, ["get_documents"]);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api = FixtureApi::new(Vec::new());
    let all = super::sdk_conformance::sdk_tool_names("bigquery");
    let all = all.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = tools_for(&api, &all, &defaults()).await;
    super::sdk_conformance::assert_sdk_conformance("bigquery", &tools);
}

#[test]
fn filters_follow_create_filters_and_cannot_escape_the_condition() {
    assert_eq!(where_clause(None, false).expect("none"), "TRUE");
    assert_eq!(
        where_clause(Some(&json!({})), false).expect("empty"),
        "TRUE"
    );
    assert_eq!(
        where_clause(Some(&json!("")), false).expect("empty"),
        "TRUE"
    );
    assert_eq!(
        where_clause(Some(&json!("views > 3 AND lang = 'en'")), false).expect("text"),
        "views > 3 AND lang = 'en'"
    );
    // Numbers and booleans bare (`str()` of the Python value), text quoted.
    let filter = json!({"views": 3, "ratio": 0.5, "draft": true, "lang": "O'Brien\\"});
    let clause = where_clause(Some(&filter), false).expect("object");
    for part in [
        "views = 3",
        "ratio = 0.5",
        "draft = True",
        "lang = 'O\\'Brien\\\\'",
    ] {
        assert!(clause.contains(part), "{clause} lacks {part}");
    }
    assert_eq!(clause.matches(" AND ").count(), 3);
    // batch_search quotes every value.
    let quoted = where_clause(Some(&json!({"views": 3, "draft": false})), true).expect("quoted");
    assert!(quoted.contains("views = '3'") && quoted.contains("draft = 'False'"));
    for refused in [
        json!("TRUE; DELETE FROM docs.chunks WHERE TRUE"),
        json!({"views = 1 OR 1": 1}),
        json!({"lang": null}),
        json!({"lang": ["en"]}),
        json!(5),
    ] {
        assert!(
            where_clause(Some(&refused), false).is_err(),
            "{refused} must be refused"
        );
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered corpus proves every tool's SQL and output shape.
async fn tools_build_the_sdk_sql_and_format_rows_like_json_dumps() {
    let api = FixtureApi::new(vec![fixture_row()]);
    let all = super::sdk_conformance::sdk_tool_names("bigquery");
    let all = all.iter().map(String::as_str).collect::<Vec<_>>();
    let tools = tools_for(&api, &all, &defaults()).await;

    let result = call(
        &tools,
        "get_documents",
        json!({"ids": ["a", "b"], "filter": {"lang": "en"}}),
    )
    .await
    .expect("get_documents");
    assert_eq!(
        result,
        json!(r#"[{"doc_id": "a", "embedding": [0.5, 1.0], "score": 0.25}]"#)
    );
    let (sql, parameters, effect) = api.jobs()[0].clone();
    assert_eq!(
        sql,
        "SELECT * FROM `analytics-prod.docs.chunks` WHERE doc_id IN UNNEST(@ids) AND lang = 'en'"
    );
    assert_eq!(
        parameters,
        [json!({
            "name": "ids",
            "parameterType": {"type": "ARRAY", "arrayType": {"type": "STRING"}},
            "parameterValue": {"arrayValues": [{"value": "a"}, {"value": "b"}]}
        })]
    );
    assert!(!effect);

    call(
        &tools,
        "similarity_search_by_vector_with_score",
        json!({"embedding": [0.5, 2], "k": 3, "filter": "views > 1"}),
    )
    .await
    .expect("similarity_search_by_vector_with_score");
    let (sql, parameters, _) = api.jobs()[1].clone();
    assert_eq!(
        sql,
        "SELECT *, EUCLIDEAN_DISTANCE(embedding, @query_embedding) AS score\nFROM `analytics-prod.docs.chunks`\nWHERE views > 1\nORDER BY score ASC\nLIMIT 3"
    );
    assert_eq!(
        parameters[0]["parameterValue"]["arrayValues"],
        json!([{"value": "0.5"}, {"value": "2.0"}])
    );

    let vectors = call(
        &tools,
        "similarity_search_by_vectors",
        json!({"embeddings": [[9, 8]], "with_embeddings": true}),
    )
    .await
    .expect("similarity_search_by_vectors");
    // `{**d, "embedding": emb}` keeps the column's position.
    assert_eq!(
        vectors,
        json!(r#"[[{"doc_id": "a", "embedding": [9.0, 8.0], "score": 0.25}]]"#)
    );
    assert!(api.jobs()[2].0.ends_with("LIMIT 5"));

    let index = call(&tools, "create_vector_index", json!({}))
        .await
        .expect("create_vector_index");
    assert_eq!(
        index,
        json!("Vector index 'chunks_langchain_index' created or already exists.")
    );
    let (sql, _, effect) = api.jobs()[3].clone();
    assert!(sql.starts_with(
        "CREATE VECTOR INDEX IF NOT EXISTS\n`chunks_langchain_index`\nON `analytics-prod.docs.chunks`\n(embedding)"
    ));
    assert!(effect);

    let stats = call(&tools, "job_stats", json!({"job_id": "bquxjob_1a2b"}))
        .await
        .expect("job_stats");
    assert_eq!(stats["job"], "bquxjob_1a2b");
    assert!(
        call(&tools, "job_stats", json!({"job_id": "../jobs"}))
            .await
            .is_err()
    );

    let created = call(
        &tools,
        "create_delta_lake_table",
        json!({
            "table_name": "orders",
            "connection_id": "analytics-prod.eu.lake",
            "source_uris": ["gs://lake/orders/"]
        }),
    )
    .await
    .expect("create_delta_lake_table");
    let created: Value = serde_json::from_str(created.as_str().expect("text")).expect("json");
    assert_eq!(created["project"], "analytics-prod");
    assert_eq!(created["dataset"], "docs");
    assert_eq!(
        created["table"]["externalDataConfiguration"],
        json!({
            "sourceFormat": "DELTA_LAKE",
            "autodetect": true,
            "sourceUris": ["gs://lake/orders/"],
            "connectionId": "analytics-prod.eu.lake"
        })
    );
}

#[tokio::test]
async fn batch_search_and_missing_defaults_keep_the_sdk_messages() {
    let api = FixtureApi::new(vec![fixture_row()]);
    let api_dyn: Arc<dyn BigQueryApi> = api.clone();
    let tools = test_raw_tools("warehouse", &defaults(), &api_dyn);
    for (arguments, message) in [
        (
            json!({"queries": ["x"], "embeddings": [[1]]}),
            "Provide only one of 'queries' or 'embeddings'.",
        ),
        (
            json!({"queries": ["x"]}),
            "Embedding model is not set on the wrapper.",
        ),
        (json!({}), "No embeddings or queries provided."),
        (
            json!({"embeddings": []}),
            "No embeddings or queries provided.",
        ),
    ] {
        let error = call(&tools, "batch_search", arguments.clone())
            .await
            .expect_err("refused batch search");
        assert!(error.to_string().contains(message), "{arguments}: {error}");
    }
    let result = call(
        &tools,
        "batch_search",
        json!({"embeddings": [[1], [2]], "k": 1, "filter": {"views": 3}}),
    )
    .await
    .expect("batch_search");
    assert_eq!(
        result.as_str().expect("text").matches("\"doc_id\"").count(),
        2
    );
    assert!(api.jobs()[0].0.contains("WHERE views = '3'"));

    let unconfigured = test_raw_tools("warehouse", &TableDefaults::default(), &api_dyn);
    let error = call(&unconfigured, "get_documents", json!({}))
        .await
        .expect_err("no table");
    assert!(
        error
            .to_string()
            .contains("Project, dataset, and table must be specified.")
    );
    let error = call(
        &unconfigured,
        "create_delta_lake_table",
        json!({"table_name": "t", "connection_id": "p.eu.c", "source_uris": ["gs://b/"]}),
    )
    .await
    .expect_err("no project");
    assert!(
        error
            .to_string()
            .contains("project, dataset, table_name, connection_id, and source_uris are required.")
    );
    assert_eq!(api.jobs().len(), 2, "refusals never reach BigQuery");
}

#[tokio::test]
async fn jobs_query_wire_pages_and_decodes_rows_like_the_python_client() {
    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(ok(&json!({
            "jobReference": {"projectId": "analytics-prod", "jobId": "job_Ab12", "location": "EU"},
            "jobComplete": false
        }))),
        Ok(ok(&query_response(&json!([row("a", "7")]), Some("page-2")))),
        Ok(ok(
            &json!({"rows": [row("b", "-3")], "totalRows": "2", "jobComplete": true}),
        )),
    ]);
    let rows = client(&transport)
        .query(QueryJob {
            sql: "SELECT 1".to_owned(),
            parameters: vec![json!({"name": "ids"})],
            effect: false,
        })
        .await
        .expect("query rows");
    let requests = transport.requests();
    assert_eq!(requests.len(), 4);
    assert_eq!(requests[0].url, "https://oauth2.googleapis.com/token");
    assert_eq!(requests[1].method, "POST");
    assert_eq!(
        requests[1].url,
        "https://bigquery.googleapis.com/bigquery/v2/projects/analytics-prod/queries"
    );
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer ya29.fixture")
    );
    let body = requests[1].body.clone().expect("jobs.query body");
    assert_eq!(body["query"], "SELECT 1");
    assert_eq!(body["useLegacySql"], false);
    assert_eq!(body["location"], "EU");
    assert_eq!(body["parameterMode"], "NAMED");
    assert_eq!(body["formatOptions"]["useInt64Timestamp"], true);
    assert_eq!(requests[2].method, "GET");
    assert!(requests[2].url.starts_with(
        "https://bigquery.googleapis.com/bigquery/v2/projects/analytics-prod/queries/job_Ab12?"
    ));
    assert!(requests[2].url.contains("location=EU"));
    assert!(requests[3].url.contains("pageToken=page-2"));

    assert_eq!(
        super::families::bigquery::format::python_dumps(&Cell::List(rows)),
        concat!(
            r#"[{"doc_id": "a", "content": "caf\u00e9 notes", "views": 7, "embedding": [0.5, 1.0], "#,
            r#""published": "2023-11-14 22:13:20.123456+00:00", "meta": {"lang": "en", "draft": false}}, "#,
            r#"{"doc_id": "b", "content": "caf\u00e9 notes", "views": -3, "embedding": [0.5, 1.0], "#,
            r#""published": "2023-11-14 22:13:20.123456+00:00", "meta": {"lang": "en", "draft": false}}]"#
        )
    );
}

#[tokio::test]
async fn job_stats_and_table_creation_follow_the_rest_api() {
    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(ok(&json!({
            "kind": "bigquery#job",
            "id": "analytics-prod:EU.job_Ab12",
            "statistics": {"creationTime": "1700000000000", "totalBytesProcessed": "1024"}
        }))),
    ]);
    let stats = client(&transport)
        .job_statistics("job_Ab12")
        .await
        .expect("job statistics");
    assert_eq!(
        stats,
        json!({"creationTime": "1700000000000", "totalBytesProcessed": "1024"})
    );
    assert_eq!(
        transport.requests()[1].url,
        "https://bigquery.googleapis.com/bigquery/v2/projects/analytics-prod/jobs/job_Ab12?location=EU"
    );

    // `create_table(exists_ok=True)`: a 409 reads the existing table back.
    let table = json!({"tableReference": {"projectId": "analytics-prod", "datasetId": "docs", "tableId": "orders"}});
    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(BigQueryHttpResponse::fixture(
            StatusCode::CONFLICT,
            br#"{"error":{"code":409,"message":"Already Exists"}}"#.to_vec(),
        )),
        Ok(ok(
            &json!({"kind": "bigquery#table", "id": "analytics-prod:docs.orders"}),
        )),
    ]);
    let existing = client(&transport)
        .create_table("analytics-prod", "docs", &table)
        .await
        .expect("existing table");
    assert_eq!(existing["id"], "analytics-prod:docs.orders");
    let requests = transport.requests();
    assert_eq!(requests[1].method, "POST");
    assert!(requests[1].effect);
    assert_eq!(
        requests[1].url,
        "https://bigquery.googleapis.com/bigquery/v2/projects/analytics-prod/datasets/docs/tables"
    );
    assert_eq!(requests[2].method, "GET");
    assert!(requests[2].url.ends_with("/datasets/docs/tables/orders"));
}

#[tokio::test]
async fn failures_are_typed_and_effect_failures_need_reconciliation() {
    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(BigQueryHttpResponse::fixture(
            StatusCode::BAD_REQUEST,
            br#"{"error":{"message":"Syntax error: secret detail"}}"#.to_vec(),
        )),
    ]);
    let error = client(&transport)
        .query(QueryJob {
            sql: "SELECT".to_owned(),
            parameters: Vec::new(),
            effect: false,
        })
        .await
        .expect_err("rejected query");
    assert_eq!(error.code(), BigQueryClientErrorCode::InvalidInput);
    assert!(!error.into_adk().to_string().contains("secret detail"));

    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(BigQueryHttpResponse::fixture(
            StatusCode::SERVICE_UNAVAILABLE,
            Vec::new(),
        )),
    ]);
    let error = client(&transport)
        .query(QueryJob {
            sql: "CREATE VECTOR INDEX".to_owned(),
            parameters: Vec::new(),
            effect: true,
        })
        .await
        .expect_err("ambiguous DDL");
    assert_eq!(error.code(), BigQueryClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());

    let transport = FixtureTransport::new([
        Ok(token()),
        Ok(BigQueryHttpResponse::fixture(
            StatusCode::SERVICE_UNAVAILABLE,
            Vec::new(),
        )),
    ]);
    let error = client(&transport)
        .job_statistics("job_1")
        .await
        .expect_err("unavailable read");
    assert_eq!(error.code(), BigQueryClientErrorCode::DependencyUnavailable);
    assert!(error.retryable());

    let transport = FixtureTransport::new([Ok(BigQueryHttpResponse::fixture(
        StatusCode::UNAUTHORIZED,
        br#"{"error":"invalid_grant"}"#.to_vec(),
    ))]);
    let error = client(&transport)
        .job_statistics("job_1")
        .await
        .expect_err("token refused");
    assert_eq!(error.code(), BigQueryClientErrorCode::Authentication);
    assert_eq!(transport.requests().len(), 1, "no API call without a token");
}
