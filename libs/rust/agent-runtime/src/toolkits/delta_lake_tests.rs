//! The `delta_lake` family against a real Delta table (a log and Parquet
//! files written into an in-process object store), the SDK argument schemas,
//! the structured filter, the scan bounds and the S3 request signing.

use std::collections::BTreeMap;
use std::io::{Read as _, Write as _};
use std::net::TcpListener;
use std::sync::atomic::AtomicBool;
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
use super::families::delta_lake::table::{DeltaTable, QueryRequest};
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
async fn query_expressions_follow_pandas_query_for_the_literal_subset() {
    let tools = fixture_tools().await;
    let query = |text: &'static str| {
        let tools = &tools;
        async move {
            ids(&call(tools, "query_table", json!({"query": text}))
                .await
                .unwrap_or_else(|error| panic!("{text}: {error}")))
        }
    };
    // Parity cases in the style of pandas' own DataFrame.query examples.
    assert_eq!(query("id > 2").await, [3, 4, 5]);
    assert_eq!(query("id >= 2 and id < 5").await, [2, 3, 4]);
    assert_eq!(query("id == 1 or id == 5").await, [1, 5]);
    // The default `parser='pandas'`: `&` and `|` rank with and/or, below comparisons.
    assert_eq!(query("id == 1 | id == 2").await, [1, 2]);
    assert_eq!(query("id > 3 & name != 'dave'").await, [5]);
    assert_eq!(query("(id > 1) & (id < 4)").await, [2, 3]);
    assert_eq!(query("not id > 2").await, [1, 2]);
    assert_eq!(query("~(id > 2)").await, [1, 2]);
    assert_eq!(query("1 < id <= 3").await, [2, 3]);
    assert_eq!(query("name == 'bob'").await, [2]);
    assert_eq!(query("name == \"bob\"").await, [2]);
    assert_eq!(query("name != 'bob' and score > 20").await, [4, 5]);
    assert_eq!(query("name in ['alice', 'erin']").await, [1, 5]);
    assert_eq!(query("id in (2, 3)").await, [2, 3]);
    assert_eq!(query("id not in [2, 3]").await, [1, 4, 5]);
    assert_eq!(query("score is null").await, [3]);
    assert_eq!(query("score is not null").await, [1, 2, 4, 5]);
    assert_eq!(query("score is None").await, [3]);
    assert_eq!(query("score > 15.5").await, [2, 4, 5]);
    assert_eq!(query("score >= 2e1").await, [2, 4, 5]);
    assert_eq!(query("id * 2 > 7").await, [4, 5]);
    assert_eq!(query("id % 2 == 0").await, [2, 4]);
    assert_eq!(query("score / 10 < 2").await, [1]);
    assert_eq!(query("-id < -3").await, [4, 5]);
    assert_eq!(query("id > score / 10").await, Vec::<i64>::new());
    assert_eq!(query("`id` == 4").await, [4]);
    assert_eq!(
        query("id == 1 and name == 'alice' and score == 10.5").await,
        [1]
    );
    // A comparison with a missing value is false, `!=` is true (pandas).
    assert_eq!(query("score != 20").await, [1, 3, 4, 5]);
    assert_eq!(query("name > 1").await, Vec::<i64>::new());
    // query and filters both apply, and the filter column need not be selected.
    let rows = call(
        &tools,
        "query_table",
        json!({"query": "id > 1", "filters": {"score": {"lt": 45}}, "columns": ["name"]}),
    )
    .await
    .expect("rows");
    assert_eq!(rows, json!([{"name": "bob"}, {"name": "dave"}]));
    // An empty or null query is the SDK's "return everything".
    for empty in [json!(null), json!(""), json!("  ")] {
        let rows = call(&tools, "query_table", json!({"query": empty}))
            .await
            .expect("rows");
        assert_eq!(ids(&rows), [1, 2, 3, 4, 5]);
    }
}

/// Everything outside the subset fails, naming the token, and never runs.
#[tokio::test]
async fn query_expressions_outside_the_subset_name_the_offending_token() {
    let tools = fixture_tools().await;
    for (query, fragment) in [
        (
            "name.str.contains('a')",
            "Unsupported token '.' at position 4",
        ),
        ("abs(id) > 1", "Unsupported token 'abs(' at position 0"),
        ("id > @limit", "Unsupported token '@' at position 5"),
        ("id = 1", "Unsupported token '=' at position 3"),
        ("id ** 2 > 1", "Unsupported token '**' at position 3"),
        ("id // 2 > 1", "Unsupported token '//' at position 3"),
        ("name like 'a%'", "Unsupported token 'like'"),
        ("id between 1 and 3", "Unsupported token 'between'"),
        ("id in (select id)", "Unsupported token 'select'"),
        ("id in other", "'in' needs a list of literals"),
        ("id in [a, b]", "lists hold literals only"),
        ("id > ", "Unexpected end of query"),
        ("(id > 1", "Unexpected end of query: expected ')'"),
        ("id > 1)", "Unsupported token ')' at position 6"),
        ("id > 1 2", "Unsupported token '2' at position 7"),
        ("id +", "Unexpected end of query"),
        ("'abc", "Unterminated string at position 0"),
        ("`abc", "Unterminated backtick"),
        ("id > 1 and", "Unexpected end of query"),
        ("id is 3", "expected null or None after 'is'"),
        ("id + 1", "query must be a condition"),
        ("'text'", "query must be a condition"),
        ("id > $x", "Unsupported token '$' at position 5"),
        (
            "id > 99999999999999999999999999999999999999999",
            "the number is too large",
        ),
        ("nope > 1", "Column 'nope' not found in table."),
        ("`no such` == 1", "Column 'no such' not found in table."),
    ] {
        let error = call(&tools, "query_table", json!({"query": query}))
            .await
            .expect_err(query);
        assert!(error.contains(fragment), "{query}: {error}");
    }
    let error = call(&tools, "query_table", json!({"query": 5}))
        .await
        .expect_err("not a string");
    assert!(error.contains("query must be a string"), "{error}");
}

/// Hostile input never panics and never exceeds the bounds.
#[test]
fn the_query_parser_is_bounded_and_never_panics() {
    use super::families::delta_lake::expr::Query;

    assert!(
        Query::parse(&"id > 1 and ".repeat(200)).is_err(),
        "too long"
    );
    assert!(Query::parse(&format!("{}id > 1{}", "(".repeat(100), ")".repeat(100))).is_err());
    assert!(Query::parse(&format!("{}id > 1", "not ".repeat(100))).is_err());
    let many = (0..600)
        .map(|n| format!("id == {n}"))
        .collect::<Vec<_>>()
        .join(" or ");
    assert!(Query::parse(&many).is_err(), "too many terms or characters");
    let list = format!(
        "id in [{}]",
        (0..1_100)
            .map(|n| n.to_string())
            .collect::<Vec<_>>()
            .join(",")
    );
    assert!(Query::parse(&list).is_err(), "list items");
    // Every prefix, every single-token mutation and a spread of junk parses
    // or fails with a sentence; none panics.
    let seed = "(id >= -2.5e3 and name in ['a', \"b\\\"c\"]) or not `my col` is not None | ~score != 1 % 2";
    let chars: Vec<char> = seed.chars().collect();
    for end in 0..=chars.len() {
        let prefix: String = chars[..end].iter().collect();
        let _ = Query::parse(&prefix);
    }
    for noise in [
        "",
        " ",
        "\0",
        "\u{1F600}",
        "((((",
        "))))",
        "[]",
        "()",
        "in",
        "is",
        "not",
        "and or",
        "1 2 3",
        "'\\",
        "`",
        "\"",
        "a b c",
        "== ==",
        "a < < b",
        "a in []",
        "a in ()",
        "a in [1,,2]",
        "a in [1 2]",
        "1e",
        "1e+",
        "1.2.3",
        "ünï > 1",
        "id >\n1",
        "-",
        "--id > 1",
        "id > -",
        "not",
        "~",
        "id not 3",
        "id not in",
        "is null",
        "is not null",
    ] {
        let _ = Query::parse(noise);
    }
    // Deterministic junk from a small alphabet.
    let alphabet: Vec<char> = "ab1 .'\"`()[],=!<>&|~+-*/%@$\\\n".chars().collect();
    let mut state = 0x2545_F491_4F6C_DD1D_u64;
    for _ in 0..3_000 {
        let mut text = String::new();
        for _ in 0..(state % 24) {
            state = state
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            text.push(alphabet[(state >> 33) as usize % alphabet.len()]);
        }
        state = state
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        let _ = Query::parse(&text);
    }
    // A value query is not a condition; a bare boolean literal is.
    assert!(Query::parse("True").is_ok());
    assert!(Query::parse("1").is_err());
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

/// The accept and refuse cases elitea-main's egress derivation is pinned
/// against (`workeregress/delta_lake_hosts_test.go` reads the same file).
#[test]
fn the_endpoint_cases_main_derives_its_egress_host_from_hold() {
    let document: Value =
        serde_json::from_str(include_str!("families/delta_lake/endpoint_cases.json"))
            .expect("endpoint cases");
    let cases = document["cases"].as_array().expect("cases");
    assert!(cases.len() >= 20);
    for case in cases {
        let (region, path) = (
            case["region"].as_str().expect("region"),
            case["path"].as_str().expect("path"),
        );
        let configuration = json!({
            "aws_access_key_id": "AKIAFIXTURE",
            "aws_secret_access_key": "secret-fixture-value",
            "aws_region": region,
            "s3_path": path
        });
        let parsed = DeltaLakeToolkitConfig::parse(&settings(&configuration));
        match case["host"].as_str() {
            Some(host) => {
                let config = parsed.unwrap_or_else(|_| panic!("{region} {path} is refused"));
                assert_eq!(
                    s3_endpoint(config.region()),
                    format!("https://{host}"),
                    "{region} {path}"
                );
            }
            None => assert!(parsed.is_err(), "{region} {path} is accepted"),
        }
    }
}

#[test]
fn a_key_prefix_is_the_literal_key_the_user_wrote() {
    for (path, url) in [
        ("s3://bucket/my%20data/t", "s3://bucket/my%2520data/t/"),
        ("s3://bucket/a b", ""),
        ("s3://bucket/dir/ünï/t", "s3://bucket/dir/%C3%BCn%C3%AF/t/"),
        ("s3://bucket/a+b/c=d", "s3://bucket/a%2Bb/c%3Dd/"),
    ] {
        match TableLocation::parse(path) {
            Ok(location) => assert_eq!(location.url(), url, "{path}"),
            Err(_) => assert!(url.is_empty(), "{path} is refused"),
        }
    }
}

#[tokio::test]
async fn a_long_path_and_name_never_cut_the_usage_text() {
    let long = format!("s3://bucket/{}", "p".repeat(900));
    let mut configuration = good_configuration();
    configuration["s3_path"] = json!(long);
    configuration["table_path"] = json!(long);
    let config = DeltaLakeToolkitConfig::parse(&settings(&configuration)).expect("config");
    let table = DeltaTable::with_store(fixture_store().await, URL.to_owned());
    let name = "n".repeat(255);
    let toolset = build_with_table(
        &name,
        &[],
        &policy(),
        &Arc::new(table),
        &config.description_prefix(),
    )
    .expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    for tool in toolset.tools(readonly).await.expect("tools") {
        let description = tool.description();
        assert!(
            description.len() <= 1_000,
            "{} {}",
            tool.name(),
            description.len()
        );
        let usage = match tool.name() {
            "query_table" => "Returns:\n    List of dicts representing rows",
            "vector_search" => "(cosine similarity, best first).",
            _ => "Get the schema of the Delta Lake table.",
        };
        assert!(
            description.ends_with(usage),
            "{}: {description}",
            tool.name()
        );
    }
}

#[tokio::test]
async fn an_empty_columns_list_selects_every_column_as_in_the_sdk() {
    let tools = fixture_tools().await;
    let rows = call(&tools, "query_table", json!({"columns": []}))
        .await
        .expect("rows");
    assert_eq!(rows[0].as_object().expect("row").len(), 4);
}

#[tokio::test]
async fn a_row_of_another_dimension_is_skipped_unless_no_row_compares() {
    let store: Arc<DynObjectStore> = Arc::new(InMemory::new());
    let rows: Vec<Row> = vec![
        (1, "a", None, Some(vec![1.0, 0.0])),
        (2, "b", None, Some(vec![1.0, 0.0, 0.0])),
        (3, "c", None, Some(vec![0.0, 1.0, 0.0])),
    ];
    write_table(&store, &[rows]).await;
    let tools = tools_for(DeltaTable::with_store(store, URL.to_owned()), &[]).await;
    let found = call(
        &tools,
        "vector_search",
        json!({"embedding": [1.0, 0.0, 0.0]}),
    )
    .await
    .expect("rows");
    assert_eq!(ids(&found), [2, 3], "the two-value row is skipped");
    let error = call(
        &tools,
        "vector_search",
        json!({"embedding": [1.0, 0.0, 0.0, 0.0]}),
    )
    .await
    .expect_err("no row compares");
    assert!(
        error.contains("has 4 values but the rows of 'embedding' have"),
        "{error}"
    );
}

#[tokio::test]
async fn a_cancelled_read_stops_at_the_next_batch() {
    let table = Arc::new(DeltaTable::with_store(
        fixture_store().await,
        URL.to_owned(),
    ));
    let request = QueryRequest {
        columns: None,
        filter: super::families::delta_lake::filter::RowFilter::default(),
        expression: None,
    };
    let result = tokio::task::spawn_blocking(move || table.query(&request, &AtomicBool::new(true)))
        .await
        .expect("join");
    assert!(
        result
            .expect_err("cancelled")
            .message()
            .contains("cancelled")
    );
}
