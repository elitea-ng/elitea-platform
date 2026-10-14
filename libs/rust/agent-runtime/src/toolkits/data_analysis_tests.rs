//! The `data_analysis` internal tool (ADR-0033): every tool end to end through
//! an in-memory host on real CSV, JSON and xlsx bytes, the limits against
//! hostile files, the chart PNG, the sandbox hand-off against a scripted
//! runner, and the tool policy.

// Test code: casts, formatting into strings and extension checks are not
// worth the ceremony pedantic asks for here.
#![allow(clippy::pedantic)]

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use elitea_office_core::{CellValue, Workbook};
use serde_json::{Value, json};

use super::families::data_analysis::code::{
    AnalysisCodeFailure, AnalysisCodeRequest, AnalysisCodeRunner, AnalysisLanguage,
};
use super::families::data_analysis::schema::ALL_TOOLS;
use super::families::data_analysis::table::Limits;
use super::families::data_analysis::tools::{DataAnalysisAuthority, build_with};
use super::families::office_host::{
    OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost,
    SharedOfficeToolHost,
};
use super::policy::ToolAdmissionPolicy;

const BUCKET: &str = "chat-attachments";

#[derive(Default)]
struct FakeHost {
    objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    writes: Mutex<Vec<(String, bool)>>,
    link: bool,
}

impl FakeHost {
    fn put(&self, name: &str, bytes: impl Into<Vec<u8>>) {
        self.objects
            .lock()
            .expect("objects")
            .insert((BUCKET.to_owned(), name.to_owned()), bytes.into());
    }

    fn get(&self, name: &str) -> Vec<u8> {
        self.objects
            .lock()
            .expect("objects")
            .get(&(BUCKET.to_owned(), name.to_owned()))
            .cloned()
            .unwrap_or_else(|| panic!("no object {name}"))
    }

    fn has(&self, name: &str) -> bool {
        self.objects
            .lock()
            .expect("objects")
            .contains_key(&(BUCKET.to_owned(), name.to_owned()))
    }

    fn names(&self) -> Vec<String> {
        self.objects
            .lock()
            .expect("objects")
            .keys()
            .map(|(_, n)| n.clone())
            .collect()
    }
}

#[async_trait]
impl OfficeToolHost for FakeHost {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        let objects = self.objects.lock().expect("objects");
        let bytes = objects
            .get(&(bucket.to_owned(), name.to_owned()))
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))?;
        if bytes.len() as u64 > max_bytes {
            return Err(OfficeHostError::new(OfficeFailure::TooLarge, "big"));
        }
        Ok(bytes.clone())
    }

    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        let byte_length = bytes.len() as u64;
        let mut objects = self.objects.lock().expect("objects");
        let slot = (bucket.to_owned(), name.to_owned());
        self.writes
            .lock()
            .expect("writes")
            .push((name.to_owned(), overwrite));
        if !overwrite && objects.contains_key(&slot) {
            return Err(OfficeHostError::new(OfficeFailure::AlreadyExists, "taken"));
        }
        objects.insert(slot, bytes);
        Ok(OfficeObjectWritten {
            bucket: bucket.to_owned(),
            name: name.to_owned(),
            media_type: media_type.to_owned(),
            byte_length,
        })
    }

    async fn complete(&self, _: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NoModel, "none"))
    }

    fn object_link(&self, bucket: &str, name: &str) -> Option<String> {
        self.link.then(|| {
            format!("https://platform.test/api/v2/artifacts/artifact/default/7/{bucket}/{name}")
        })
    }
}

#[derive(Default)]
struct FakeRunner {
    seen: Mutex<Vec<AnalysisCodeRequest>>,
    reply: Mutex<Option<Result<Value, AnalysisCodeFailure>>>,
}

#[async_trait]
impl AnalysisCodeRunner for FakeRunner {
    async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeFailure> {
        self.seen.lock().expect("seen").push(request);
        self.reply
            .lock()
            .expect("reply")
            .take()
            .unwrap_or(Ok(json!({"ok": true})))
    }
}

/// The production limits with room for the test threads: the Office engine's
/// permit gate is process-wide and the tests run in parallel.
fn test_limits() -> Limits {
    Limits {
        office: office_limits(),
        ..Limits::default()
    }
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(SimpleToolContext::new("da-test").with_function_call_id("call-1"))
}

struct Rig {
    host: Arc<FakeHost>,
    tools: Vec<Arc<dyn Tool>>,
}

impl Rig {
    async fn new() -> Self {
        Self::with(FakeHost::default(), test_limits(), None).await
    }

    async fn with(host: FakeHost, limits: Limits, runner: Option<Arc<FakeRunner>>) -> Self {
        let host = Arc::new(host);
        let shared: SharedOfficeToolHost = Arc::clone(&host) as SharedOfficeToolHost;
        let mut authority = DataAnalysisAuthority::new(shared);
        if let Some(runner) = runner {
            authority = authority.with_code_runner(runner);
        }
        let toolset = build_with(&authority, &policy(), limits)
            .expect("toolset")
            .expect("not blocked");
        let tools = toolset
            .tools(context() as Arc<dyn ReadonlyContext>)
            .await
            .expect("tools");
        Self { host, tools }
    }

    async fn call(&self, tool: &str, args: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|t| t.name() == tool)
            .unwrap_or_else(|| panic!("no tool {tool}"));
        tool.execute(context() as Arc<dyn ToolContext>, args)
            .await
            .expect("tool result")
    }

    async fn ok(&self, tool: &str, args: Value) -> Value {
        let out = self.call(tool, args).await;
        assert_eq!(out["status"], "success", "{out}");
        out
    }

    async fn err(&self, tool: &str, args: Value) -> String {
        let out = self.call(tool, args).await;
        assert_eq!(out["status"], "error", "{out}");
        out["message"].as_str().expect("message").to_owned()
    }
}

const SALES: &str = "region,units,price,day,returned\nnorth,10,2.5,2024-01-01,true\nsouth,5,4.0,2024-01-02,false\nnorth,7,2.5,2024-01-03,false\neast,3,10.0,2024-01-04,true\nsouth,,4.0,2024-01-05,false\n,8,1.5,2024-01-06,false\n";

fn office_limits() -> elitea_office_core::Limits {
    elitea_office_core::Limits {
        max_concurrent: 64,
        ..elitea_office_core::Limits::default()
    }
}

fn xlsx(rows: &[Vec<CellValue>]) -> Vec<u8> {
    let limits = office_limits();
    let mut book = Workbook::new(&limits);
    for (r, row) in rows.iter().enumerate() {
        book.write_row(0, r as u32, 0, row).expect("row");
    }
    book.save().expect("save")
}

fn text(s: &str) -> CellValue {
    CellValue::Text(s.to_owned())
}

// ---------------------------------------------------------------------------
// Surface
// ---------------------------------------------------------------------------

#[tokio::test]
async fn the_tool_set_is_the_documented_one() {
    let rig = Rig::new().await;
    let mut names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
    names.sort_unstable();
    // No sandbox lent: run_analysis_code is not offered.
    assert_eq!(
        names,
        [
            "aggregate_data",
            "chart_data",
            "correlate_data",
            "describe_data",
            "filter_data",
            "join_data",
            "pivot_data",
            "save_dataframe",
            "sort_data"
        ]
    );
    // The LLM-written-pandas tool is retired on purpose.
    assert!(!names.contains(&"pandas_analyze_data"));
    for tool in &rig.tools {
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["additionalProperties"], false);
        assert!(
            !tool.description().is_empty() && tool.description().len() <= 1_000,
            "{}",
            tool.name()
        );
        for required in schema["required"].as_array().expect("required") {
            assert!(schema["properties"][required.as_str().unwrap()].is_object());
        }
    }
    assert_eq!(ALL_TOOLS.len(), 10);
    // Every tool, the sandbox one included, keeps the SDK's 1,000 character
    // description cap and a closed object schema.
    for tool in ALL_TOOLS {
        assert!(tool.description().len() <= 1_000, "{}", tool.name());
        let schema = tool.parameters_schema();
        assert_eq!(schema["additionalProperties"], false, "{}", tool.name());
    }
}

#[tokio::test]
async fn save_dataframe_keeps_the_sdk_arguments() {
    let rig = Rig::new().await;
    let tool = rig
        .tools
        .iter()
        .find(|t| t.name() == "save_dataframe")
        .unwrap();
    let schema = tool.parameters_schema().unwrap();
    assert_eq!(schema["required"], json!(["source_df", "target_file"]));
}

#[tokio::test]
async fn read_only_tools_may_run_side_by_side() {
    let rig = Rig::new().await;
    for tool in &rig.tools {
        let writes = !matches!(tool.name(), "describe_data" | "correlate_data");
        assert_eq!(tool.is_read_only(), !writes, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), !writes, "{}", tool.name());
    }
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

#[tokio::test]
async fn describe_profiles_a_csv() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    let out = rig
        .ok("describe_data", json!({"filename": "c1/sales.csv"}))
        .await;
    assert_eq!(out["rows"], 6);
    let columns = out["columns"].as_array().unwrap();
    let by = |n: &str| columns.iter().find(|c| c["name"] == n).unwrap();
    assert_eq!(by("units")["type"], "integer");
    assert_eq!(by("units")["nulls"], 1);
    assert_eq!(by("units")["max"], 10.0);
    assert_eq!(by("price")["type"], "number");
    assert_eq!(by("day")["type"], "date");
    assert_eq!(by("day")["min"], "2024-01-01");
    assert_eq!(by("returned")["type"], "boolean");
    assert_eq!(by("returned")["true"], 2);
    assert_eq!(by("region")["top"], "north");
    assert_eq!(out["head"]["rows"].as_array().unwrap().len(), 5);
    assert_eq!(out["source"]["file"], "c1/sales.csv");
}

#[tokio::test]
async fn the_attachment_path_form_works_as_a_name() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    let out = rig
        .ok(
            "describe_data",
            json!({"filename": "/chat-attachments/c1/sales.csv"}),
        )
        .await;
    assert_eq!(out["source"]["file"], "c1/sales.csv");
}

#[tokio::test]
async fn delimiters_quotes_bom_and_blank_lines_are_handled() {
    let rig = Rig::new().await;
    let body =
        "\u{feff}name;note;n\r\n\"Smith; J\";\"said \"\"hi\"\"\nline two\";1\r\n\r\nLee;ok;2\r\n";
    rig.host.put("a.csv", body);
    let out = rig.ok("filter_data", json!({"filename": "a.csv"})).await;
    assert_eq!(out["columns"], json!(["name", "note", "n"]));
    assert_eq!(out["row_count"], 2);
    assert_eq!(out["rows"][0][0], "Smith; J");
    assert_eq!(out["rows"][0][1], "said \"hi\"\nline two");
}

#[tokio::test]
async fn json_records_and_lines_load() {
    let rig = Rig::new().await;
    rig.host
        .put("a.json", r#"[{"a":1,"b":"x"},{"a":2,"c":true}]"#);
    rig.host.put("b.jsonl", "{\"a\":1}\n\n{\"a\":2.5}\n");
    let out = rig.ok("filter_data", json!({"filename": "a.json"})).await;
    assert_eq!(out["columns"], json!(["a", "b", "c"]));
    assert_eq!(out["rows"][1][1], Value::Null);
    let out = rig
        .ok("describe_data", json!({"filename": "b.jsonl"}))
        .await;
    assert_eq!(out["columns"][0]["type"], "number");
}

#[tokio::test]
async fn xlsx_sheets_dates_and_formulas_are_read() {
    let rig = Rig::new().await;
    let bytes = xlsx(&[
        vec![text("item"), text("qty"), text("note")],
        vec![text("bolt"), CellValue::Number(4.0), text("a")],
        vec![text("nut"), CellValue::Number(6.0), CellValue::Empty],
        vec![text("washer"), CellValue::Number(2.0), text("c")],
    ]);
    rig.host.put("b.xlsx", bytes);
    let out = rig.ok("describe_data", json!({"filename": "b.xlsx"})).await;
    assert_eq!(out["source"]["sheet"], "Sheet1");
    assert_eq!(out["source"]["sheets"], json!(["Sheet1"]));
    assert_eq!(out["columns"][1]["type"], "integer");
    assert_eq!(out["columns"][1]["sum"], 12.0);
    let message = rig
        .err(
            "describe_data",
            json!({"filename": "b.xlsx", "sheet": "Nope"}),
        )
        .await;
    assert!(
        message.contains("There is no sheet named `Nope`. Sheets: `Sheet1`"),
        "{message}"
    );
    let message = rig
        .err("describe_data", json!({"filename": "a.csv", "sheet": "S"}))
        .await;
    assert!(message.contains("Could not read"), "{message}");
    rig.host.put("a.csv", "a\n1\n");
    let message = rig
        .err("describe_data", json!({"filename": "a.csv", "sheet": "S"}))
        .await;
    assert!(
        message.contains("`sheet` applies to .xlsx files only"),
        "{message}"
    );
}

#[tokio::test]
async fn unsupported_and_unknown_files_get_their_own_sentence() {
    let rig = Rig::new().await;
    for (name, expect) in [
        ("a.xls", "`.xls` workbooks are not supported"),
        ("a.parquet", "`.parquet` files are not supported"),
        ("a.pkl", "`.pkl` files are not supported"),
        ("noext", "has no recognized extension"),
    ] {
        rig.host.put(name, "x");
        let message = rig.err("describe_data", json!({"filename": name})).await;
        assert!(message.contains(expect), "{name}: {message}");
    }
    let message = rig
        .err("describe_data", json!({"filename": "missing.csv"}))
        .await;
    assert!(message.contains("no such file in the bucket"), "{message}");
    let message = rig
        .err("describe_data", json!({"filename": "../x.csv"}))
        .await;
    assert!(message.contains("may not contain `..`"), "{message}");
    let message = rig
        .err(
            "describe_data",
            json!({"filename": "a.csv", "bucket_name": "Bad_Bucket"}),
        )
        .await;
    assert!(message.contains("not a valid bucket name"), "{message}");
}

// ---------------------------------------------------------------------------
// Operations through the tools
// ---------------------------------------------------------------------------

#[tokio::test]
async fn filter_selects_sorts_limits_and_says_what_it_cut() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    let out = rig
        .ok(
            "filter_data",
            json!({"filename": "c1/sales.csv",
                "filters": [{"column": "units", "operator": "ge", "value": 5}],
                "columns": ["region", "units"],
                "sort_by": [{"column": "units", "descending": true}],
                "limit": 2}),
        )
        .await;
    assert_eq!(out["rows_matching"], 4);
    assert_eq!(out["columns"], json!(["region", "units"]));
    assert_eq!(out["rows"], json!([["north", 10], [null, 8]]));
    assert_eq!(out["truncated"], true);
    assert!(out["note"].as_str().unwrap().contains("first 2 of 4 rows"));
    assert!(out.get("saved").is_none());
}

#[tokio::test]
async fn a_result_file_goes_next_to_the_source_and_never_overwrites_unasked() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    let args = json!({"filename": "c1/sales.csv", "output_file": "big.csv",
        "filters": [{"column": "units", "operator": "gt", "value": 5}]});
    let out = rig.ok("filter_data", args.clone()).await;
    assert_eq!(out["saved"]["file"], "c1/big.csv");
    assert_eq!(out["saved"]["bucket"], BUCKET);
    let saved = String::from_utf8(rig.host.get("c1/big.csv")).unwrap();
    assert!(saved.starts_with("region,units,price,day,returned\n"));
    assert_eq!(saved.lines().count(), 4);
    // Names round-trip: the reported file is readable by the next call.
    let again = rig
        .ok("describe_data", json!({"filename": out["saved"]["file"]}))
        .await;
    assert_eq!(again["rows"], 3);
    // A second write of the same name is refused, with the source untouched.
    let message = rig.err("filter_data", args.clone()).await;
    assert!(
        message.contains("`c1/big.csv` already exists. Pass overwrite=true"),
        "{message}"
    );
    assert_eq!(rig.host.get("c1/sales.csv"), SALES.as_bytes());
    let mut replace = args;
    replace["overwrite"] = json!(true);
    rig.ok("filter_data", replace).await;
    let writes = rig.host.writes.lock().unwrap().clone();
    assert_eq!(
        writes,
        [
            ("c1/big.csv".into(), false),
            ("c1/big.csv".into(), false),
            ("c1/big.csv".into(), true)
        ]
    );
}

#[tokio::test]
async fn an_output_path_is_taken_as_given_and_formats_come_from_the_extension() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    for name in ["out/all.json", "out/all.tsv", "out/all.xlsx"] {
        let out = rig
            .ok(
                "filter_data",
                json!({"filename": "c1/sales.csv", "output_file": name}),
            )
            .await;
        assert_eq!(out["saved"]["file"], name);
    }
    let json_out: Value = serde_json::from_slice(&rig.host.get("out/all.json")).unwrap();
    assert_eq!(json_out[0]["region"], "north");
    assert_eq!(json_out[0]["day"], "2024-01-01");
    // The xlsx reads back as the same table.
    let back = rig
        .ok("describe_data", json!({"filename": "out/all.xlsx"}))
        .await;
    assert_eq!(back["rows"], 6);
    let message = rig
        .err(
            "filter_data",
            json!({"filename": "c1/sales.csv", "output_file": "x.pdf"}),
        )
        .await;
    assert!(
        message.contains("needs a .csv, .tsv, .json or .xlsx extension"),
        "{message}"
    );
    let message = rig
        .err(
            "filter_data",
            json!({"filename": "c1/sales.csv", "output_file": "../x.csv"}),
        )
        .await;
    assert!(message.contains("may not contain `..`"), "{message}");
}

#[tokio::test]
async fn sort_requires_keys_and_puts_nulls_last() {
    let rig = Rig::new().await;
    rig.host.put("s.csv", SALES);
    let message = rig.err("sort_data", json!({"filename": "s.csv"})).await;
    assert!(message.contains("`sort_by` is required"), "{message}");
    let out = rig
        .ok(
            "sort_data",
            json!({"filename": "s.csv", "sort_by": ["units"], "columns": ["units"]}),
        )
        .await;
    assert_eq!(out["rows"], json!([[3], [5], [7], [8], [10], [null]]));
    let message = rig
        .err(
            "sort_data",
            json!({"filename": "s.csv", "sort_by": ["nope"]}),
        )
        .await;
    assert!(
        message.contains("There is no column named `nope`. Columns: `region`"),
        "{message}"
    );
}

#[tokio::test]
async fn aggregate_groups_and_summarises() {
    let rig = Rig::new().await;
    rig.host.put("s.csv", SALES);
    let out = rig
        .ok(
            "aggregate_data",
            json!({"filename": "s.csv", "group_by": ["region"],
                "aggregations": [
                    {"column": "units", "function": "sum", "name": "total"},
                    {"column": "price", "function": "mean"},
                    {"column": "*", "function": "count", "name": "n"},
                    {"column": "day", "function": "max"}],
                "sort_by": [{"column": "total", "descending": true}]}),
        )
        .await;
    assert_eq!(
        out["columns"],
        json!(["region", "total", "price_mean", "n", "day_max"])
    );
    assert_eq!(
        out["types"],
        json!(["text", "integer", "number", "integer", "date"])
    );
    assert_eq!(out["rows"][0], json!(["north", 17, 2.5, 2, "2024-01-03"]));
    assert_eq!(out["row_count"], 3);
    let whole = rig
        .ok(
            "aggregate_data",
            json!({"filename": "s.csv",
                "filters": [{"column": "returned", "operator": "eq", "value": false}],
                "aggregations": [{"column": "units", "function": "median"}]}),
        )
        .await;
    assert_eq!(whole["rows"], json!([[7.0]]));
    let message = rig
        .err(
            "aggregate_data",
            json!({"filename": "s.csv", "aggregations": [{"column": "region", "function": "sum"}]}),
        )
        .await;
    assert!(
        message.contains("Cannot compute sum of the text column `region`"),
        "{message}"
    );
    let message = rig
        .err(
            "aggregate_data",
            json!({"filename": "s.csv", "aggregations": [{"column": "units", "function": "mode"}]}),
        )
        .await;
    assert!(
        message.contains("Unknown function `mode`. Use one of: count, sum"),
        "{message}"
    );
}

#[tokio::test]
async fn pivot_spreads_and_fills() {
    let rig = Rig::new().await;
    rig.host
        .put("p.csv", "region,tier,units\nn,a,1\nn,b,2\ns,a,3\ns,a,4\n");
    let out = rig
        .ok(
            "pivot_data",
            json!({"filename": "p.csv", "index": ["region"], "columns": "tier",
                "values": "units", "fill_value": 0}),
        )
        .await;
    assert_eq!(out["columns"], json!(["region", "a", "b"]));
    assert_eq!(out["rows"], json!([["n", 1, 2], ["s", 7, 0]]));
}

#[tokio::test]
async fn join_combines_two_files_and_names_overlaps() {
    let rig = Rig::new().await;
    rig.host.put("j/l.csv", "id,name\n1,a\n2,b\n3,c\n");
    rig.host.put(
        "j/r.csv",
        "id,name,score\n2,B,20\n3,C,30\n3,C2,31\n4,D,40\n",
    );
    let out = rig
        .ok(
            "join_data",
            json!({"filename": "j/l.csv", "right_filename": "j/r.csv", "on": ["id"],
                "how": "left", "sort_by": ["id"], "output_file": "joined.csv"}),
        )
        .await;
    assert_eq!(out["columns"], json!(["id", "name_x", "name_y", "score"]));
    assert_eq!(out["row_count"], 4);
    assert_eq!(out["rows"][0], json!([1, "a", null, null]));
    assert_eq!(out["saved"]["file"], "j/joined.csv");
    let message = rig
        .err(
            "join_data",
            json!({"filename": "j/l.csv", "right_filename": "j/r.csv", "left_on": ["name"], "right_on": ["id"]}),
        )
        .await;
    assert!(
        message.contains("must hold the same kind of value"),
        "{message}"
    );
    let message = rig
        .err(
            "join_data",
            json!({"filename": "j/l.csv", "right_filename": "j/r.csv"}),
        )
        .await;
    assert!(
        message.contains("Give `on`, or `left_on` and `right_on`"),
        "{message}"
    );
}

#[tokio::test]
async fn correlate_checks_its_columns() {
    let rig = Rig::new().await;
    rig.host
        .put("c.csv", "a,b,c,t\n1,2,4,x\n2,4,3,y\n3,6,2,z\n4,8,1,w\n");
    let out = rig.ok("correlate_data", json!({"filename": "c.csv"})).await;
    assert_eq!(out["columns"], json!(["column", "a", "b", "c"]));
    assert_eq!(out["rows"][0], json!(["a", 1.0, 1.0, -1.0]));
    let message = rig
        .err(
            "correlate_data",
            json!({"filename": "c.csv", "columns": ["a", "t"]}),
        )
        .await;
    assert!(
        message.contains("The column `t` is text, not numeric"),
        "{message}"
    );
    let message = rig
        .err(
            "correlate_data",
            json!({"filename": "c.csv", "columns": ["a"]}),
        )
        .await;
    assert!(
        message.contains("at least two numeric columns"),
        "{message}"
    );
}

// ---------------------------------------------------------------------------
// Conversion and charts
// ---------------------------------------------------------------------------

#[tokio::test]
async fn save_dataframe_converts_between_formats() {
    let rig = Rig::new().await;
    rig.host.put("c1/sales.csv", SALES);
    let out = rig
        .ok(
            "save_dataframe",
            json!({"source_df": "c1/sales.csv", "target_file": "sales.xlsx"}),
        )
        .await;
    assert_eq!(out["saved"]["file"], "c1/sales.xlsx");
    assert_eq!(
        out["message"],
        "Successfully saved dataframe to c1/sales.xlsx in chat-attachments"
    );
    let back = rig
        .ok(
            "aggregate_data",
            json!({"filename": "c1/sales.xlsx",
            "aggregations": [{"column": "units", "function": "sum"}]}),
        )
        .await;
    assert_eq!(back["rows"], json!([[33]]));
    let message = rig
        .err(
            "save_dataframe",
            json!({"source_df": "c1/sales.csv", "target_file": "sales.xlsx"}),
        )
        .await;
    assert!(message.contains("already exists"), "{message}");
}

#[tokio::test]
async fn csv_cells_that_look_like_formulas_are_neutralized_and_reported() {
    let rig = Rig::new().await;
    rig.host
        .put("f.csv", "who,n\n=SUM(A1),1\n-5x,2\n-7,3\nplain,4\n");
    let out = rig
        .ok(
            "filter_data",
            json!({"filename": "f.csv", "output_file": "g.csv"}),
        )
        .await;
    assert_eq!(out["saved"]["csv_formula_cells_prefixed"], 2);
    let saved = String::from_utf8(rig.host.get("g.csv")).unwrap();
    assert!(saved.contains("'=SUM(A1),1"), "{saved}");
    assert!(saved.contains("'-5x,2"), "{saved}");
    assert!(saved.contains("\n-7,3"), "{saved}");
}

#[tokio::test]
async fn excel_text_that_looks_like_a_formula_stays_text() {
    let rig = Rig::new().await;
    rig.host.put("f.csv", "who,n\n=1+1,1\n@SUM(A1),2\n");
    rig.ok(
        "save_dataframe",
        json!({"source_df": "f.csv", "target_file": "f.xlsx"}),
    )
    .await;
    let book = Workbook::open(&rig.host.get("f.xlsx"), &office_limits()).expect("open");
    assert_eq!(
        book.read_cell(0, 1, 0).unwrap(),
        CellValue::Text("=1+1".into())
    );
    assert_eq!(
        book.read_cell(0, 2, 0).unwrap(),
        CellValue::Text("@SUM(A1)".into())
    );
    assert_eq!(book.read_cell(0, 1, 1).unwrap(), CellValue::Number(1.0));
}

const PNG: &[u8] = &[0x89, b'P', b'N', b'G'];

#[tokio::test]
async fn every_chart_kind_saves_a_png_with_an_embeddable_link() {
    let rig = Rig::with(
        FakeHost {
            link: true,
            ..FakeHost::default()
        },
        test_limits(),
        None,
    )
    .await;
    rig.host.put("c1/sales.csv", SALES);
    for (kind, extra) in [
        ("bar", json!({"x": "region", "y": ["units", "price"]})),
        ("line", json!({"x": "day", "y": ["units"]})),
        ("scatter", json!({"x": "units", "y": ["price"]})),
        ("pie", json!({"x": "region", "y": ["units"], "top_n": 2})),
        ("histogram", json!({"y": ["price"], "bins": 4})),
    ] {
        let mut args = json!({"filename": "c1/sales.csv", "chart_type": kind, "title": "T"});
        args.as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        let out = rig.ok("chart_data", args).await;
        let file = out["saved"]["file"].as_str().unwrap();
        assert!(
            file.starts_with("c1/chart_") && file.ends_with(".png"),
            "{file}"
        );
        let bytes = rig.host.get(file);
        assert!(bytes.starts_with(PNG), "{kind}");
        assert!(bytes.len() > 1_000, "{kind}: {} bytes", bytes.len());
        let markdown = out["markdown"].as_str().unwrap();
        assert!(
            markdown.starts_with("![T](https://platform.test/"),
            "{markdown}"
        );
        assert!(markdown.contains(file));
    }
    let named = rig
        .ok(
            "chart_data",
            json!({"filename": "c1/sales.csv", "chart_type": "bar", "x": "region",
                "output_file": "mine.png"}),
        )
        .await;
    assert_eq!(named["saved"]["file"], "c1/mine.png");
    assert_eq!(named["plotted_rows"][0], json!(["east", 1.0]));
}

#[tokio::test]
async fn chart_mistakes_get_their_own_sentence() {
    let rig = Rig::new().await;
    rig.host.put("s.csv", SALES);
    let call = |extra: Value| {
        let mut args = json!({"filename": "s.csv"});
        args.as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        args
    };
    let m = rig
        .err("chart_data", call(json!({"chart_type": "radar"})))
        .await;
    assert!(m.contains("Unknown chart type `radar`"), "{m}");
    let m = rig
        .err(
            "chart_data",
            call(json!({"chart_type": "bar", "x": "region", "y": ["day"]})),
        )
        .await;
    assert!(m.contains("is date, not numeric"), "{m}");
    let m = rig
        .err("chart_data", call(json!({"chart_type": "bar"})))
        .await;
    assert!(m.contains("`x` is required"), "{m}");
    let m = rig
        .err(
            "chart_data",
            call(json!({"chart_type": "histogram", "y": ["units", "price"]})),
        )
        .await;
    assert!(m.contains("exactly one column"), "{m}");
    let m = rig
        .err(
            "chart_data",
            call(json!({"chart_type": "bar", "x": "region", "output_file": "a.csv"})),
        )
        .await;
    assert!(m.contains("must end in .png"), "{m}");
    let m = rig
        .err(
            "chart_data",
            call(json!({"chart_type": "pie", "x": "region", "y": ["units"],
            "filters": [{"column": "units", "operator": "gt", "value": 100}]})),
        )
        .await;
    assert!(m.contains("No rows are left"), "{m}");
    assert!(rig.host.names().iter().all(|n| !n.ends_with(".png")));
}

#[tokio::test]
async fn too_many_categories_are_refused_not_drawn() {
    let rig = Rig::new().await;
    let mut csv = String::from("k,v\n");
    for i in 0..150 {
        csv.push_str(&format!("k{i},{i}\n"));
    }
    rig.host.put("many.csv", csv);
    let m = rig
        .err(
            "chart_data",
            json!({"filename": "many.csv", "chart_type": "bar", "x": "k", "y": ["v"]}),
        )
        .await;
    assert!(
        m.contains("150 distinct values; a chart shows at most 100"),
        "{m}"
    );
    let ok = rig
        .ok(
            "chart_data",
            json!({"filename": "many.csv", "chart_type": "bar", "x": "k", "y": ["v"], "top_n": 10}),
        )
        .await;
    assert!(ok["notes"][0].as_str().unwrap().contains("10 categories"));
}

// ---------------------------------------------------------------------------
// Hostile input and limits
// ---------------------------------------------------------------------------

fn tight(f: impl FnOnce(&mut Limits)) -> Limits {
    let mut limits = test_limits();
    f(&mut limits);
    limits
}

#[tokio::test]
async fn limits_are_enforced_while_reading() {
    let rig = Rig::with(
        FakeHost::default(),
        tight(|l| {
            l.max_rows = 100;
            l.max_columns = 5;
            l.max_cells = 400;
            l.max_cell_bytes = 64;
        }),
        None,
    )
    .await;
    let rows = |n: usize| {
        let mut s = String::from("a,b\n");
        for i in 0..n {
            s.push_str(&format!("{i},x\n"));
        }
        s
    };
    rig.host.put("ok.csv", rows(100));
    rig.ok("describe_data", json!({"filename": "ok.csv"})).await;
    rig.host.put("rows.csv", rows(101));
    let m = rig
        .err("describe_data", json!({"filename": "rows.csv"}))
        .await;
    assert!(m.contains("more than 100 rows"), "{m}");
    rig.host.put("cols.csv", "a,b,c,d,e,f\n1,2,3,4,5,6\n");
    let m = rig
        .err("describe_data", json!({"filename": "cols.csv"}))
        .await;
    assert!(m.contains("more than 5 columns"), "{m}");
    rig.host.put(
        "cells.csv",
        format!("a,b,c,d,e\n{}", "1,2,3,4,5\n".repeat(90)),
    );
    let m = rig
        .err("describe_data", json!({"filename": "cells.csv"}))
        .await;
    assert!(m.contains("more than 400 cells"), "{m}");
    rig.host
        .put("wide.csv", format!("a\n{}\n", "x".repeat(200)));
    let m = rig
        .err("describe_data", json!({"filename": "wide.csv"}))
        .await;
    assert!(m.contains("longer than 64 bytes"), "{m}");
}

#[tokio::test]
async fn malformed_files_say_why() {
    let rig = Rig::new().await;
    rig.host.put("q.csv", "a,b\n\"never closed,1\n");
    let m = rig.err("describe_data", json!({"filename": "q.csv"})).await;
    assert!(
        m.contains("quoted field that starts on line 2 is never closed"),
        "{m}"
    );
    rig.host.put("r.csv", "a,b\n1,2,3\n");
    let m = rig.err("describe_data", json!({"filename": "r.csv"})).await;
    assert!(
        m.contains("Line 2 has 3 fields but the header has 2"),
        "{m}"
    );
    rig.host.put("u.csv", [0xFF, 0xFE, b'a', 0]);
    let m = rig.err("describe_data", json!({"filename": "u.csv"})).await;
    assert!(m.contains("UTF-16"), "{m}");
    rig.host.put("l.csv", [b'a', b'\n', 0xE9, b'\n']);
    let m = rig.err("describe_data", json!({"filename": "l.csv"})).await;
    assert!(m.contains("not valid UTF-8"), "{m}");
    rig.host.put("e.csv", "");
    let m = rig.err("describe_data", json!({"filename": "e.csv"})).await;
    assert!(m.contains("The file is empty"), "{m}");
    rig.host.put("j.json", "{\"a\":1}\n{bad");
    let m = rig
        .err("describe_data", json!({"filename": "j.json"}))
        .await;
    assert!(m.contains("not valid JSON"), "{m}");
    rig.host.put("j2.json", "[1,2]");
    let m = rig
        .err("describe_data", json!({"filename": "j2.json"}))
        .await;
    assert!(m.contains("array of objects"), "{m}");
    rig.host
        .put("bad.xlsx", b"PK\x03\x04 not really a zip".to_vec());
    let m = rig
        .err("describe_data", json!({"filename": "bad.xlsx"}))
        .await;
    assert!(m.contains("not a readable .xlsx workbook"), "{m}");
    rig.host.put(
        "deep.json",
        format!("{}1{}", "[".repeat(5_000), "]".repeat(5_000)),
    );
    let m = rig
        .err("describe_data", json!({"filename": "deep.json"}))
        .await;
    assert!(m.contains("not valid JSON"), "{m}");
}

#[tokio::test]
async fn a_binary_blob_named_csv_is_not_swallowed() {
    let rig = Rig::new().await;
    let noise: Vec<u8> = (0..4096u32)
        .map(|i| (i.wrapping_mul(2_654_435_761) >> 13) as u8)
        .collect();
    rig.host.put("noise.csv", noise);
    let out = rig
        .call("describe_data", json!({"filename": "noise.csv"}))
        .await;
    // Either a sentence or a bounded profile; never a panic or a hang.
    assert!(out["status"] == "error" || out["rows"].is_number(), "{out}");
}

#[tokio::test]
async fn an_oversized_file_is_refused_with_its_size() {
    let rig = Rig::with(FakeHost::default(), tight(|l| l.max_input_bytes = 50), None).await;
    rig.host
        .put("big.csv", "a\n".to_owned() + &"1\n".repeat(100));
    let m = rig
        .err("describe_data", json!({"filename": "big.csv"}))
        .await;
    assert!(
        m.contains("The file is 202 bytes; the limit is 50 bytes"),
        "{m}"
    );
}

#[tokio::test]
async fn an_expired_clock_stops_the_work_with_a_sentence() {
    let rig = Rig::with(
        FakeHost::default(),
        tight(|l| l.deadline = std::time::Duration::ZERO),
        None,
    )
    .await;
    let mut csv = String::from("a,b\n");
    for i in 0..10_000 {
        csv.push_str(&format!("{i},{}\n", i % 7));
    }
    rig.host.put("t.csv", csv);
    let m = rig.err("describe_data", json!({"filename": "t.csv"})).await;
    assert!(m.contains("longer than the 20 second limit"), "{m}");
}

#[tokio::test]
async fn results_over_the_answer_budget_are_cut_and_say_so() {
    let rig = Rig::with(
        FakeHost::default(),
        tight(|l| l.max_answer_bytes = 200),
        None,
    )
    .await;
    let mut csv = String::from("a,b\n");
    for i in 0..200 {
        csv.push_str(&format!("{i},{}\n", "y".repeat(20)));
    }
    rig.host.put("b.csv", csv);
    let out = rig
        .ok("filter_data", json!({"filename": "b.csv", "limit": 200}))
        .await;
    assert_eq!(out["truncated"], true);
    assert_eq!(out["row_count"], 200);
    assert!(out["rows_returned"].as_u64().unwrap() < 20);
    assert!(out["note"].as_str().unwrap().contains("fit in this answer"));
}

#[tokio::test]
async fn a_failed_write_leaves_nothing_behind_and_a_cell_cap_stops_xlsx() {
    let rig = Rig::new().await;
    rig.host.put("c1/s.csv", SALES);
    let before = rig.host.names();
    let m = rig
        .err(
            "filter_data",
            json!({"filename": "c1/s.csv", "output_file": "o.csv",
            "filters": [{"column": "units", "operator": "gt", "value": "x"}]}),
        )
        .await;
    assert!(m.contains("is not a number"), "{m}");
    assert_eq!(rig.host.names(), before);
    // A result over the xlsx cell limit is refused before any write.
    let wide = Rig::new().await;
    let mut csv = String::from("a,b\n");
    for i in 0..600_000 {
        csv.push_str(&format!("{i},{i}\n"));
    }
    wide.host.put("w.csv", csv);
    let m = wide
        .err(
            "save_dataframe",
            json!({"source_df": "w.csv", "target_file": "w.xlsx"}),
        )
        .await;
    assert!(m.contains("an .xlsx holds at most"), "{m}");
    assert!(!wide.host.has("w.xlsx"));
}

#[tokio::test]
async fn calls_run_side_by_side_without_trouble() {
    let rig = Arc::new(Rig::new().await);
    rig.host.put("s.csv", SALES);
    let mut handles = Vec::new();
    for i in 0..8 {
        let rig = Arc::clone(&rig);
        handles.push(tokio::spawn(async move {
            rig.ok(
                "aggregate_data",
                json!({"filename": "s.csv", "group_by": ["region"],
                    "aggregations": [{"column": "units", "function": "sum"}], "limit": 1 + i}),
            )
            .await
        }));
    }
    for handle in handles {
        assert_eq!(handle.await.unwrap()["row_count"], 3);
    }
}

// ---------------------------------------------------------------------------
// Sandbox hand-off and policy
// ---------------------------------------------------------------------------

#[tokio::test]
async fn code_is_offered_only_with_a_sandbox_and_runs_there() {
    let runner = Arc::new(FakeRunner::default());
    *runner.reply.lock().unwrap() = Some(Ok(json!({"answer": 42})));
    let rig = Rig::with(
        FakeHost::default(),
        test_limits(),
        Some(Arc::clone(&runner)),
    )
    .await;
    assert!(rig.tools.iter().any(|t| t.name() == "run_analysis_code"));
    rig.host.put("s.csv", SALES);
    let out = rig
        .ok(
            "run_analysis_code",
            json!({"filename": "s.csv", "code": "1+1", "columns": ["region", "units"],
                "filters": [{"column": "units", "operator": "gt", "value": 4}], "max_rows": 2}),
        )
        .await;
    assert_eq!(out["result"], json!({"answer": 42}));
    assert_eq!(out["rows_total"], 4);
    assert_eq!(out["rows_sent"], 2);
    assert_eq!(out["truncated"], true);
    assert!(out["note"].as_str().unwrap().contains("first 2 of 4 rows"));
    let seen = runner.seen.lock().unwrap();
    assert_eq!(seen.len(), 1);
    assert_eq!(seen[0].language, AnalysisLanguage::Python);
    assert_eq!(seen[0].source, "1+1");
    assert!(seen[0].call_id.ends_with("call-1"));
    assert_eq!(seen[0].input["data"]["columns"], json!(["region", "units"]));
    assert_eq!(
        seen[0].input["data"]["rows"],
        json!([["north", 10], ["south", 5]])
    );
    assert_eq!(seen[0].input["data"]["total_rows"], 4);
}

#[tokio::test]
async fn code_sends_at_most_the_sandbox_cell_budget() {
    let runner = Arc::new(FakeRunner::default());
    let rig = Rig::with(
        FakeHost::default(),
        tight(|l| l.max_sandbox_cells = 10),
        Some(Arc::clone(&runner)),
    )
    .await;
    rig.host.put("s.csv", SALES);
    let out = rig
        .ok(
            "run_analysis_code",
            json!({"filename": "s.csv", "code": "x", "columns": ["region", "units"]}),
        )
        .await;
    assert_eq!(out["rows_sent"], 5);
    let narrow = Rig::with(
        FakeHost::default(),
        tight(|l| l.max_sandbox_cells = 3),
        Some(runner),
    )
    .await;
    narrow.host.put("s.csv", SALES);
    let m = narrow
        .err(
            "run_analysis_code",
            json!({"filename": "s.csv", "code": "x"}),
        )
        .await;
    assert!(
        m.contains("more than the 3 cells the sandbox accepts"),
        "{m}"
    );
}

#[tokio::test]
async fn every_sandbox_failure_has_its_own_sentence() {
    let runner = Arc::new(FakeRunner::default());
    let rig = Rig::with(
        FakeHost::default(),
        test_limits(),
        Some(Arc::clone(&runner)),
    )
    .await;
    rig.host.put("s.csv", SALES);
    let mut sentences = Vec::new();
    for failure in [
        AnalysisCodeFailure::Unavailable,
        AnalysisCodeFailure::Rejected,
        AnalysisCodeFailure::Failed,
        AnalysisCodeFailure::Cancelled,
        AnalysisCodeFailure::Uncertain,
    ] {
        *runner.reply.lock().unwrap() = Some(Err(failure));
        sentences.push(
            rig.err(
                "run_analysis_code",
                json!({"filename": "s.csv", "code": "x"}),
            )
            .await,
        );
    }
    let mut unique = sentences.clone();
    unique.sort();
    unique.dedup();
    assert_eq!(unique.len(), 5, "{sentences:?}");
    assert!(sentences[4].contains("may have run"));
    // An oversized result is refused, not cut.
    *runner.reply.lock().unwrap() = Some(Ok(json!({"blob": "z".repeat(200_000)})));
    let m = rig
        .err(
            "run_analysis_code",
            json!({"filename": "s.csv", "code": "x"}),
        )
        .await;
    assert!(m.contains("return at most 102400"), "{m}");
    let m = rig
        .err(
            "run_analysis_code",
            json!({"filename": "s.csv", "code": "x", "language": "ruby"}),
        )
        .await;
    assert!(m.contains("`language` must be python or javascript"), "{m}");
}

#[tokio::test]
async fn the_tool_policy_applies() {
    let host: SharedOfficeToolHost = Arc::new(FakeHost::default());
    let authority = DataAnalysisAuthority::new(host);
    let blocked = Arc::new(
        ToolAdmissionPolicy::new(&["Data Analysis".to_owned()], &BTreeMap::new()).expect("policy"),
    );
    assert!(
        build_with(&authority, &blocked, Limits::default())
            .expect("built")
            .is_none()
    );
    let no_chart = Arc::new(
        ToolAdmissionPolicy::new(
            &[],
            &BTreeMap::from([("data_analysis".to_owned(), vec!["chart_data".to_owned()])]),
        )
        .expect("policy"),
    );
    let toolset = build_with(&authority, &no_chart, Limits::default())
        .expect("built")
        .expect("allowed");
    let tools = toolset
        .tools(context() as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    assert!(tools.iter().all(|t| t.name() != "chart_data"));
    assert_eq!(tools.len(), 8);
}
