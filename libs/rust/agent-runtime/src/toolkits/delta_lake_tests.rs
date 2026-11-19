//! The `delta_lake` family against a real Delta table (a log and Parquet
//! files written into an in-process object store), the SDK argument schemas,
//! the structured filter, the scan bounds and the S3 request signing.

use std::collections::BTreeMap;
use std::io::{Read as _, Write as _};
use std::net::TcpListener;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, Toolset};
use adk_tool::SimpleToolContext;
use delta_kernel::arrow::array::{
    ArrayRef, Float32Builder, Float64Array, Int64Array, ListBuilder, StringArray,
};
use delta_kernel::arrow::datatypes::{DataType, Field, Schema};
use delta_kernel::arrow::record_batch::RecordBatch;
use delta_kernel::object_store::memory::InMemory;
use delta_kernel::object_store::path::Path;
use delta_kernel::object_store::{DynObjectStore, ObjectStoreExt as _, PutPayload};
use delta_kernel::parquet::arrow::ArrowWriter;
use serde_json::{Map, Value, json};

use super::families::delta_lake::config::{
    DeltaLakeConfigErrorCode, DeltaLakeToolkitConfig, TableLocation, s3_endpoint,
};
use super::families::delta_lake::table::DeltaTable;
use super::families::delta_lake::tools::{build_with_table, tool_catalog};
use super::policy::ToolAdmissionPolicy;

const URL: &str = "s3://fixture-bucket/tables/events/";
const PREFIX: &str = "S3 Path: s3://fixture-bucket/tables/events Table Path: ";

const SCHEMA_STRING: &str = r#"{"type":"struct","fields":[{"name":"id","type":"long","nullable":false,"metadata":{}},{"name":"name","type":"string","nullable":true,"metadata":{}},{"name":"score","type":"double","nullable":true,"metadata":{}},{"name":"embedding","type":{"type":"array","elementType":"float","containsNull":true},"nullable":true,"metadata":{}}]}"#;

type Row = (i64, &'static str, Option<f64>, Option<Vec<f32>>);

fn rows() -> (Vec<Row>, Vec<Row>) {
    (
        vec![
            (1, "alice", Some(10.5), Some(vec![1.0, 0.0, 0.0])),
            (2, "bob", Some(20.0), Some(vec![0.0, 1.0, 0.0])),
            (3, "carol", None, Some(vec![0.9, 0.1, 0.0])),
        ],
        vec![
            (4, "dave", Some(40.5), None),
            (5, "erin", Some(50.0), Some(vec![0.0, 0.0, 1.0])),
        ],
    )
}

/// One Parquet file the way a Delta writer lays it out: the list element is
/// named `element`.
fn parquet(rows: &[Row]) -> Vec<u8> {
    let embedding_field = Field::new(
        "embedding",
        DataType::List(Arc::new(Field::new("element", DataType::Float32, true))),
        true,
    );
    let schema = Arc::new(Schema::new(vec![
        Field::new("id", DataType::Int64, false),
        Field::new("name", DataType::Utf8, true),
        Field::new("score", DataType::Float64, true),
        embedding_field,
    ]));
    let mut embeddings = ListBuilder::new(Float32Builder::new()).with_field(Arc::new(Field::new(
        "element",
        DataType::Float32,
        true,
    )));
    for (_, _, _, embedding) in rows {
        match embedding {
            Some(values) => {
                embeddings.values().append_slice(values);
                embeddings.append(true);
            }
            None => embeddings.append(false),
        }
    }
    let columns: Vec<ArrayRef> = vec![
        Arc::new(Int64Array::from_iter_values(rows.iter().map(|row| row.0))),
        Arc::new(StringArray::from_iter(rows.iter().map(|row| Some(row.1)))),
        Arc::new(Float64Array::from_iter(rows.iter().map(|row| row.2))),
        Arc::new(embeddings.finish()),
    ];
    let batch = RecordBatch::try_new(Arc::clone(&schema), columns).expect("fixture batch");
    let mut bytes = Vec::new();
    let mut writer = ArrowWriter::try_new(&mut bytes, schema, None).expect("parquet writer");
    writer.write(&batch).expect("write batch");
    writer.close().expect("close parquet");
    bytes
}

/// A Delta table at `tables/events` with one commit adding every file.
async fn write_table(store: &Arc<DynObjectStore>, files: &[Vec<Row>]) {
    let mut commit = vec![
        json!({"protocol": {"minReaderVersion": 1, "minWriterVersion": 2}}).to_string(),
        json!({"metaData": {
            "id": "5fba94ed-9794-4965-ba6e-6ee3c0d22af9",
            "format": {"provider": "parquet", "options": {}},
            "schemaString": SCHEMA_STRING,
            "partitionColumns": [],
            "configuration": {},
            "createdTime": 1_700_000_000_000_i64
        }})
        .to_string(),
    ];
    for (index, file) in files.iter().enumerate() {
        let name = format!("part-{index}.parquet");
        let bytes = parquet(file);
        commit.push(
            json!({"add": {
                "path": name,
                "partitionValues": {},
                "size": bytes.len(),
                "modificationTime": 1_700_000_000_000_i64,
                "dataChange": true,
                "stats": json!({"numRecords": file.len()}).to_string()
            }})
            .to_string(),
        );
        store
            .put(
                &Path::from(format!("tables/events/{name}")),
                PutPayload::from(bytes),
            )
            .await
            .expect("put data file");
    }
    store
        .put(
            &Path::from("tables/events/_delta_log/00000000000000000000.json"),
            PutPayload::from(commit.join("\n")),
        )
        .await
        .expect("put commit");
}

async fn fixture_store() -> Arc<DynObjectStore> {
    let store: Arc<DynObjectStore> = Arc::new(InMemory::new());
    let (first, second) = rows();
    write_table(&store, &[first, second]).await;
    store
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(SimpleToolContext::new("delta-lake-test").with_session_id("session-1"))
}

async fn tools_for(table: DeltaTable, selected: &[&str]) -> Vec<Arc<dyn Tool>> {
    let selected: Vec<String> = selected.iter().map(|name| (*name).to_owned()).collect();
    let toolset =
        build_with_table("lake", &selected, &policy(), &Arc::new(table), PREFIX).expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("tools")
}

async fn fixture_tools() -> Vec<Arc<dyn Tool>> {
    let table = DeltaTable::with_store(fixture_store().await, URL.to_owned());
    tools_for(table, &[]).await
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, arguments: Value) -> Result<Value, String> {
    let tool = tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("tool {name} is not served"));
    tool.execute(context(), arguments)
        .await
        .map_err(|error| error.to_string())
}

fn ids(value: &Value) -> Vec<i64> {
    value
        .as_array()
        .expect("rows")
        .iter()
        .map(|row| row["id"].as_i64().expect("id"))
        .collect()
}

fn settings(configuration: &Value) -> Map<String, Value> {
    json!({"delta_lake_configuration": configuration, "selected_tools": []})
        .as_object()
        .cloned()
        .expect("settings")
}

fn good_configuration() -> Value {
    json!({
        "aws_access_key_id": "AKIAFIXTURE",
        "aws_secret_access_key": "secret-fixture-value",
        "aws_session_token": "token-fixture-value",
        "aws_region": "us-east-1",
        "s3_path": "s3://fixture-bucket/tables/events",
        "table_path": null
    })
}

// ---- the SDK contract ------------------------------------------------------

#[tokio::test]
async fn the_family_serves_the_sdk_tools_in_order_and_keeps_its_contract() {
    let tools = fixture_tools().await;
    let names: Vec<&str> = tools.iter().map(|tool| tool.name()).collect();
    assert_eq!(names, ["query_table", "vector_search", "get_table_schema"]);
    assert_eq!(
        tool_catalog(),
        [
            ("query_table", false),
            ("vector_search", false),
            ("get_table_schema", false)
        ]
    );
    super::sdk_conformance::assert_sdk_conformance("delta_lake", &tools);
    for tool in &tools {
        assert!(tool.is_read_only(), "{}", tool.name());
        let description = tool.description();
        assert!(description.starts_with(PREFIX), "{description}");
        assert!(description.contains("Toolkit: lake\n"), "{description}");
        assert!(description.len() <= 1_000);
    }
}

#[tokio::test]
async fn a_selection_serves_only_what_it_names() {
    let table = DeltaTable::with_store(fixture_store().await, URL.to_owned());
    let tools = tools_for(table, &["get_table_schema", "not_a_tool"]).await;
    let names: Vec<&str> = tools.iter().map(|tool| tool.name()).collect();
    assert_eq!(names, ["get_table_schema"]);
}

// ---- get_table_schema ------------------------------------------------------

#[tokio::test]
async fn the_schema_is_printed_the_way_pyarrow_prints_it() {
    let tools = fixture_tools().await;
    let schema = call(&tools, "get_table_schema", json!({}))
        .await
        .expect("schema");
    assert_eq!(
        schema,
        json!("id: int64 not null\nname: string\nscore: double\nembedding: list<item: float>")
    );
}

// ---- query_table -----------------------------------------------------------

#[tokio::test]
async fn an_unfiltered_query_returns_every_row_of_every_file_in_order() {
    let tools = fixture_tools().await;
    let rows = call(&tools, "query_table", json!({})).await.expect("rows");
    assert_eq!(ids(&rows), [1, 2, 3, 4, 5]);
    let alice = &rows[0];
    assert_eq!(alice["name"], "alice");
    assert_eq!(alice["score"], 10.5);
    assert_eq!(alice["embedding"], json!([1.0, 0.0, 0.0]));
    assert_eq!(rows[2]["score"], Value::Null);
    assert_eq!(rows[3]["embedding"], Value::Null);
}

#[tokio::test]
async fn equality_filters_work_as_in_the_sdk() {
    let tools = fixture_tools().await;
    let rows = call(
        &tools,
        "query_table",
        json!({"filters": {"name": "bob"}, "columns": ["id", "name"]}),
    )
    .await
    .expect("rows");
    assert_eq!(rows, json!([{"id": 2, "name": "bob"}]));
}

#[tokio::test]
async fn structured_filters_cover_in_range_null_and_not_equal() {
    let tools = fixture_tools().await;
    let query = |filters: Value| {
        let tools = &tools;
        async move {
            ids(&call(tools, "query_table", json!({"filters": filters}))
                .await
                .expect("rows"))
        }
    };
    assert_eq!(query(json!({"id": [1, 4, 9]})).await, [1, 4]);
    assert_eq!(query(json!({"id": {"in": [2, 3]}})).await, [2, 3]);
    assert_eq!(query(json!({"score": {"gt": 15}})).await, [2, 4, 5]);
    assert_eq!(query(json!({"score": {"gte": 20, "lt": 45}})).await, [2, 4]);
    assert_eq!(query(json!({"score": {"lte": 10.5}})).await, [1]);
    assert_eq!(query(json!({"score": null})).await, [3]);
    assert_eq!(query(json!({"score": {"is_null": true}})).await, [3]);
    assert_eq!(
        query(json!({"score": {"is_null": false}})).await,
        [1, 2, 4, 5]
    );
    assert_eq!(query(json!({"name": {"ne": "alice"}})).await, [2, 3, 4, 5]);
    assert_eq!(query(json!({"name": {"gt": "c", "lt": "e"}})).await, [3, 4]);
    // Columns are ANDed.
    assert_eq!(
        query(json!({"score": {"gt": 15}, "name": {"in": ["bob", "carol"]}})).await,
        [2]
    );
    // A value of another kind never matches an ordering operator.
    assert_eq!(query(json!({"name": {"gt": 1}})).await, Vec::<i64>::new());
}

#[tokio::test]
async fn the_filter_column_need_not_be_selected() {
    let tools = fixture_tools().await;
    let rows = call(
        &tools,
        "query_table",
        json!({"filters": {"score": {"gt": 45}}, "columns": ["name"]}),
    )
    .await
    .expect("rows");
    assert_eq!(rows, json!([{"name": "erin"}]));
}

#[tokio::test]
async fn bad_columns_and_filters_are_refused_with_a_sentence() {
    let tools = fixture_tools().await;
    for (arguments, fragment) in [
        (
            json!({"columns": ["nope"]}),
            "Column 'nope' not found in table.",
        ),
        (
            json!({"filters": {"nope": 1}}),
            "Column 'nope' not found in table.",
        ),
        (
            json!({"filters": {"id": {"like": "1%"}}}),
            "Unsupported filter operator 'like'",
        ),
        (json!({"filters": {"id": {}}}), "must name between 1 and"),
        (
            json!({"filters": {"id": {"gt": [1]}}}),
            "needs a number or a string",
        ),
        (
            json!({"filters": {"id": {"in": 3}}}),
            "needs a list of values",
        ),
        (
            json!({"filters": {"id": {"is_null": "yes"}}}),
            "needs true or false",
        ),
        (json!({"filters": ["id"]}), "must be an object"),
        (json!({"columns": "id"}), "columns must be a list"),
    ] {
        let error = call(&tools, "query_table", arguments.clone())
            .await
            .expect_err("refused");
        assert!(error.contains(fragment), "{arguments}: {error}");
    }
}

#[tokio::test]
async fn a_pandas_query_expression_is_refused_not_evaluated() {
    let tools = fixture_tools().await;
    for query in ["id > 2", "name == 'bob' or id == 5", "@__import__('os')"] {
        let error = call(&tools, "query_table", json!({"query": query}))
            .await
            .expect_err("refused");
        assert!(error.contains("query is not supported"), "{query}: {error}");
        assert!(error.contains("filters"), "{error}");
    }
    // An empty or null query is the SDK's "return everything".
    for query in [json!(null), json!(""), json!("  ")] {
        let rows = call(&tools, "query_table", json!({"query": query}))
            .await
            .expect("rows");
        assert_eq!(ids(&rows), [1, 2, 3, 4, 5]);
    }
}

#[tokio::test]
async fn a_large_answer_is_cut_at_the_row_cap_and_says_so() {
    let store: Arc<DynObjectStore> = Arc::new(InMemory::new());
    let big: Vec<Row> = (0..1_200).map(|id| (id, "row", Some(1.0), None)).collect();
    write_table(&store, &[big]).await;
    let tools = tools_for(DeltaTable::with_store(store, URL.to_owned()), &[]).await;
    let rows = call(&tools, "query_table", json!({"columns": ["id"]}))
        .await
        .expect("rows");
    let rows = rows.as_array().expect("array");
    assert_eq!(rows.len(), 1_001, "1000 rows and the marker");
    assert_eq!(rows[0], json!({"id": 0}));
    assert_eq!(rows[999], json!({"id": 999}));
    assert_eq!(rows[1_000]["_truncated"], true);
    assert!(
        rows[1_000]["_message"]
            .as_str()
            .expect("message")
            .contains("Narrow the read")
    );
    // A filter that matches few rows is not cut.
    let few = call(
        &tools,
        "query_table",
        json!({"columns": ["id"], "filters": {"id": {"gte": 1_198}}}),
    )
    .await
    .expect("rows");
    assert_eq!(few, json!([{"id": 1198}, {"id": 1199}]));
}

#[tokio::test]
async fn a_table_larger_than_the_scan_budget_is_refused_not_half_read() {
    let store: Arc<DynObjectStore> = Arc::new(InMemory::new());
    let big: Vec<Row> = (0..1_200).map(|id| (id, "row", Some(1.0), None)).collect();
    write_table(&store, &[big]).await;
    let table = DeltaTable::with_store(store, URL.to_owned()).with_max_scan_rows(500);
    let tools = tools_for(table, &[]).await;
    let error = call(
        &tools,
        "query_table",
        json!({"filters": {"id": {"gt": 100_000}}}),
    )
    .await
    .expect_err("refused");
    assert!(error.contains("too large to scan"), "{error}");
    assert!(error.contains("500 rows"), "{error}");
}

// ---- vector_search ---------------------------------------------------------

#[tokio::test]
async fn vector_search_returns_the_most_similar_rows_best_first() {
    let tools = fixture_tools().await;
    let rows = call(
        &tools,
        "vector_search",
        json!({"embedding": [1.0, 0.0, 0.0], "k": 2}),
    )
    .await
    .expect("rows");
    assert_eq!(ids(&rows), [1, 3]);
    // Whole rows, as the SDK returns them.
    assert_eq!(rows[0]["name"], "alice");
    assert_eq!(rows[1]["embedding"], json!([0.9, 0.1, 0.0]));
}

#[tokio::test]
async fn vector_search_skips_rows_without_an_embedding_and_defaults_k_to_five() {
    let tools = fixture_tools().await;
    let rows = call(
        &tools,
        "vector_search",
        json!({"embedding": [0.0, 0.0, 1.0]}),
    )
    .await
    .expect("rows");
    // dave has no embedding; the four others are all returned (k defaults
    // to 5), the exact match first.
    let found = ids(&rows);
    assert_eq!(found.len(), 4);
    assert_eq!(found[0], 5);
    assert!(!found.contains(&4));
}

#[tokio::test]
async fn vector_search_checks_its_arguments_and_the_column() {
    let tools = fixture_tools().await;
    for (arguments, fragment) in [
        (
            json!({"embedding": [1.0, 0.0, 0.0], "embedding_column": "nope"}),
            "Embedding column 'nope' not found in table.",
        ),
        (
            json!({"embedding": [1.0, 0.0, 0.0], "embedding_column": "name"}),
            "cannot hold embeddings",
        ),
        (
            json!({"embedding": [1.0, 0.0]}),
            "has 2 values but the rows of 'embedding' have 3",
        ),
        (json!({"embedding": [0.0, 0.0, 0.0]}), "non-zero"),
        (
            json!({"embedding": [1.0, 0.0, 0.0], "k": 0}),
            "k must be at least 1",
        ),
        (json!({"embedding": []}), "between 1 and"),
        (json!({"embedding": "abc"}), "list of numbers"),
        (json!({"embedding": [1.0, "x", 0.0]}), "list of numbers"),
        (json!({}), "list of numbers"),
    ] {
        let error = call(&tools, "vector_search", arguments.clone())
            .await
            .expect_err("refused");
        assert!(error.contains(fragment), "{arguments}: {error}");
    }
    // k is read the way pydantic reads an integer, and capped.
    let rows = call(
        &tools,
        "vector_search",
        json!({"embedding": [1.0, 0.0, 0.0], "k": "1"}),
    )
    .await
    .expect("rows");
    assert_eq!(ids(&rows), [1]);
    let all = call(
        &tools,
        "vector_search",
        json!({"embedding": [1.0, 0.0, 0.0], "k": 100_000}),
    )
    .await
    .expect("rows");
    assert_eq!(all.as_array().expect("rows").len(), 4);
}

// ---- hostile and missing tables -------------------------------------------

#[tokio::test]
async fn a_missing_or_broken_table_is_an_error_not_a_panic() {
    // No table at the path.
    let empty: Arc<DynObjectStore> = Arc::new(InMemory::new());
    let tools = tools_for(DeltaTable::with_store(empty, URL.to_owned()), &[]).await;
    for name in ["query_table", "get_table_schema"] {
        let error = call(&tools, name, json!({})).await.expect_err("no table");
        assert!(error.contains("Error initializing DeltaTable"), "{error}");
    }

    // A truncated commit.
    let broken: Arc<DynObjectStore> = Arc::new(InMemory::new());
    broken
        .put(
            &Path::from("tables/events/_delta_log/00000000000000000000.json"),
            PutPayload::from("{\"protocol\":{\"minReader"),
        )
        .await
        .expect("put");
    let tools = tools_for(DeltaTable::with_store(broken, URL.to_owned()), &[]).await;
    assert!(call(&tools, "get_table_schema", json!({})).await.is_err());

    // A data file that is not Parquet.
    let hostile = fixture_store().await;
    hostile
        .put(
            &Path::from("tables/events/part-0.parquet"),
            PutPayload::from("PAR1 this is not a parquet file"),
        )
        .await
        .expect("put");
    let tools = tools_for(DeltaTable::with_store(hostile, URL.to_owned()), &[]).await;
    assert!(call(&tools, "query_table", json!({})).await.is_err());
}

// ---- configuration ---------------------------------------------------------

#[test]
fn the_configuration_reads_the_credential_and_the_table_path() {
    let config = DeltaLakeToolkitConfig::parse(&settings(&good_configuration())).expect("config");
    assert_eq!(config.access_key_id(), "AKIAFIXTURE");
    assert_eq!(config.session_token(), Some("token-fixture-value"));
    assert_eq!(config.region(), "us-east-1");
    assert_eq!(
        config.location().url(),
        "s3://fixture-bucket/tables/events/"
    );
    assert_eq!(
        config.description_prefix(),
        "S3 Path: s3://fixture-bucket/tables/events Table Path: "
    );

    // The SDK reads `table_path or s3_path`; the session token is optional.
    let mut other = good_configuration();
    other["table_path"] = json!("s3a://other-bucket//deep/table/");
    other["aws_session_token"] = Value::Null;
    let config = DeltaLakeToolkitConfig::parse(&settings(&other)).expect("config");
    assert_eq!(config.location().url(), "s3://other-bucket/deep/table/");
    assert_eq!(config.session_token(), None);
}

#[test]
fn the_configuration_refuses_what_the_sdk_refuses_and_what_is_not_s3() {
    let with = |key: &str, value: Value| {
        let mut configuration = good_configuration();
        configuration[key] = value;
        settings(&configuration)
    };
    let refused = [
        with("aws_access_key_id", json!(null)),
        with("aws_secret_access_key", json!("")),
        with("aws_region", json!(null)),
        with("aws_region", json!("us-east-1.evil.test")),
        with("aws_region", json!("localhost")),
        // A local path, another scheme, a traversal, a malformed bucket.
        with("s3_path", json!("/var/lib/tables/events")),
        with("s3_path", json!("file:///etc")),
        with("s3_path", json!("https://bucket.s3.amazonaws.com/t")),
        with("s3_path", json!("s3://fixture-bucket/../other")),
        with("s3_path", json!("s3://UPPER/t")),
        with("s3_path", json!("s3://ab/t")),
        with("s3_path", json!("s3://192.168.0.1/t")),
        with("s3_path", json!("s3://fixture-bucket/t?x=1")),
        with("s3_path", json!(null)),
        json!({"selected_tools": []})
            .as_object()
            .cloned()
            .expect("object"),
    ];
    for settings in refused {
        let error = DeltaLakeToolkitConfig::parse(&settings)
            .err()
            .expect("refused");
        assert_eq!(error.code(), DeltaLakeConfigErrorCode::InvalidConfiguration);
        let shown = format!("{error} {error:?}");
        for secret in ["AKIAFIXTURE", "secret-fixture-value", "token-fixture-value"] {
            assert!(!shown.contains(secret), "{shown}");
        }
    }
    // Neither path set: the SDK's "either s3_path or table_path".
    let mut none = good_configuration();
    none["s3_path"] = Value::Null;
    assert!(DeltaLakeToolkitConfig::parse(&settings(&none)).is_err());
}

#[test]
fn the_endpoint_is_the_regions_own() {
    assert_eq!(
        s3_endpoint("us-east-1"),
        "https://s3.us-east-1.amazonaws.com"
    );
    assert_eq!(
        s3_endpoint("us-gov-west-1"),
        "https://s3.us-gov-west-1.amazonaws.com"
    );
    assert_eq!(
        s3_endpoint("cn-north-1"),
        "https://s3.cn-north-1.amazonaws.com.cn"
    );
    let location = TableLocation::parse("S3://my.bucket-1/a/b").expect("location");
    assert_eq!(location.bucket(), "my.bucket-1");
    assert_eq!(location.url(), "s3://my.bucket-1/a/b/");
    assert_eq!(
        TableLocation::parse("s3://my-bucket").expect("root").url(),
        "s3://my-bucket/"
    );
}

// ---- S3 signing, against a local stub -------------------------------------

/// A table whose bucket answers 404: the request the toolkit makes is signed
/// with the credential's key and region, goes to the bucket as a path on the
/// endpoint, carries the session token, and the credential never reaches the
/// answer the model reads.
#[tokio::test]
async fn s3_requests_are_signed_and_the_secret_never_reaches_the_answer() {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind stub");
    let port = listener.local_addr().expect("addr").port();
    let heads: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let seen = Arc::clone(&heads);
    std::thread::spawn(move || {
        for stream in listener.incoming() {
            let Ok(mut stream) = stream else { return };
            let mut buffer = [0_u8; 8_192];
            let read = stream.read(&mut buffer).unwrap_or(0);
            seen.lock()
                .expect("heads")
                .push(String::from_utf8_lossy(&buffer[..read]).to_lowercase());
            let _ = stream.write_all(
                b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            );
        }
    });
    let config = DeltaLakeToolkitConfig::parse(&settings(&good_configuration())).expect("config");
    let table = DeltaTable::connect_endpoint(&config, &format!("http://127.0.0.1:{port}"), true)
        .expect("table");
    let tools = tools_for(table, &["get_table_schema"]).await;
    let error = call(&tools, "get_table_schema", json!({}))
        .await
        .expect_err("a 404 table");
    assert!(error.contains("Error initializing DeltaTable"), "{error}");
    for secret in ["secret-fixture-value", "token-fixture-value"] {
        assert!(!error.contains(secret), "{error}");
    }

    let heads = heads.lock().expect("heads").clone();
    let first = heads.first().expect("the stub saw a request");
    assert!(first.starts_with("get /fixture-bucket"), "{first}");
    assert!(
        first.contains("authorization: aws4-hmac-sha256 credential=akiafixture/"),
        "{first}"
    );
    assert!(first.contains("/us-east-1/s3/aws4_request"), "{first}");
    assert!(
        first.contains("x-amz-security-token: token-fixture-value"),
        "{first}"
    );
    assert!(!first.contains("secret-fixture-value"), "{first}");
}

/// The materializer entry: a toolkit's settings build the toolset, and a
/// configuration the SDK refuses does not.
#[tokio::test]
async fn the_toolset_builds_from_settings_without_touching_the_network() {
    let config = DeltaLakeToolkitConfig::parse(&settings(&good_configuration())).expect("config");
    let toolset =
        super::families::delta_lake::tools::build_delta_lake_toolset("lake", &config, &policy())
            .expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    assert_eq!(tools.len(), 3);
    assert!(
        tools[0].description().starts_with(
            "S3 Path: s3://fixture-bucket/tables/events Table Path: \nToolkit: lake\n"
        )
    );
}
