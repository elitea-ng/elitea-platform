//! The three Carrier archive tools: `get_report_by_id`, `create_excel_report`
//! and `create_ui_excel_report`, over a fixture transport and the in-memory
//! artifact store, plus the statistics they are built on.

#![allow(
    clippy::float_cmp,
    clippy::unnecessary_wraps,
    reason = "exact statistics over small fixtures; helpers match the handler signature"
)]

use std::collections::BTreeMap;
use std::io::{Cursor, Read, Write};
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::attachment_test_support::FakePlatform;
use super::families::carrier::client::{
    CarrierClient, CarrierClientError, CarrierHttpResponse, CarrierTransport,
};
use super::families::carrier::config::CarrierToolkitConfig;
use super::families::carrier::lighthouse;
use super::families::carrier::perf_report::{
    self, LogLines, Parameters, eastern_for_tests, parse_gatling, parse_jmeter,
};
use super::families::carrier::tools::test_build_with_client;
use super::families::office_host::OfficeFailure;
use super::policy::ToolAdmissionPolicy;

const BASE: &str = "https://carrier.example.test/api/v1/";

fn config() -> CarrierToolkitConfig {
    let settings: Map<String, Value> = json!({
        "carrier_configuration":{
            "url":"https://carrier.example.test/",
            "organization":"perf-org",
            "private_token":"carrier-super-secret"
        },
        "project_id":"12",
        "selected_tools":[]
    })
    .as_object()
    .cloned()
    .expect("settings");
    CarrierToolkitConfig::parse(&settings).expect("valid Carrier configuration")
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("carrier-reports").with_function_call_id("call"))
}

#[derive(Clone, Debug)]
struct Seen {
    method: Method,
    url: String,
    authorization: Option<String>,
    content_type: Option<String>,
    bytes: Option<Vec<u8>>,
    effect: bool,
}

type Handler = dyn Fn(&Seen) -> Result<CarrierHttpResponse, CarrierClientError> + Send + Sync;

struct Transport {
    seen: Mutex<Vec<Seen>>,
    handler: Box<Handler>,
}

impl Transport {
    fn new(
        handler: impl Fn(&Seen) -> Result<CarrierHttpResponse, CarrierClientError>
        + Send
        + Sync
        + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            seen: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    fn seen(&self) -> Vec<Seen> {
        self.seen.lock().expect("seen").clone()
    }
}

#[async_trait]
impl CarrierTransport for Transport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        _max_body: usize,
    ) -> Result<CarrierHttpResponse, CarrierClientError> {
        let seen = Seen {
            method: request.method().clone(),
            url: request.url().to_string(),
            authorization: request
                .headers()
                .get(AUTHORIZATION)
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
            content_type: request
                .headers()
                .get(CONTENT_TYPE)
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
            bytes: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .map(<[u8]>::to_vec),
            effect,
        };
        let outcome = (self.handler)(&seen);
        self.seen.lock().expect("seen").push(seen);
        outcome
    }
}

fn ok(body: &Value) -> Result<CarrierHttpResponse, CarrierClientError> {
    Ok(CarrierHttpResponse::fixture(StatusCode::OK, body))
}

fn status(code: StatusCode) -> Result<CarrierHttpResponse, CarrierClientError> {
    Ok(CarrierHttpResponse::fixture_text(code, ""))
}

fn bytes(body: &[u8]) -> Result<CarrierHttpResponse, CarrierClientError> {
    Ok(CarrierHttpResponse::fixture_bytes(StatusCode::OK, body))
}

fn zip_of(members: &[(&str, &[u8])]) -> Vec<u8> {
    let mut writer = zip::ZipWriter::new(Cursor::new(Vec::new()));
    for (name, content) in members {
        writer
            .start_file(*name, zip::write::SimpleFileOptions::default())
            .expect("zip member");
        writer.write_all(content).expect("zip content");
    }
    writer.finish().expect("zip").into_inner()
}

fn unzip(archive: &[u8], member: &str) -> String {
    let mut zip = zip::ZipArchive::new(Cursor::new(archive.to_vec())).expect("zip");
    let mut text = String::new();
    zip.by_name(member)
        .expect("member")
        .read_to_string(&mut text)
        .expect("text");
    text
}

async fn make_tool(
    transport: &Arc<Transport>,
    platform: Option<&Arc<FakePlatform>>,
    name: &str,
) -> Arc<dyn Tool> {
    let client =
        CarrierClient::with_transport(config(), Arc::clone(transport) as Arc<dyn CarrierTransport>);
    let toolset = test_build_with_client(
        "perf",
        &[name.to_owned()],
        &policy(),
        client,
        platform.map(FakePlatform::host),
    )
    .expect("Carrier toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    assert_eq!(tools.len(), 1, "{name}");
    Arc::clone(&tools[0])
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

const JTL: &str = "timeStamp,elapsed,label,responseCode,responseMessage,threadName,dataType,success,failureMessage,bytes,sentBytes,grpThreads,allThreads,URL,Latency,IdleTime,Connect
1700000000000,100,Login,200,OK,Thread Group 1-1,text,true,,100,10,1,1,http://x,50,0,5
1700000001000,200,Login,200,OK,Thread Group 1-2,text,true,,100,10,2,2,http://x,50,0,5
1700000002000,300,Search,500,Err,Thread Group 1-1,text,false,,100,10,2,2,http://x,50,0,5
1700000003000,400,Search,200,OK,Thread Group 1-2,text,true,,100,10,2,2,http://x,50,0,5
1700000004000,500,Tx,200,\"Number of samples in transaction : 4, number of failing samples : 1\",Thread Group 1-1,text,false,,0,0,2,2,,0,0,0
";

fn lines(text: &str) -> LogLines {
    LogLines::new(vec![Box::new(Cursor::new(text.as_bytes().to_vec()))])
}

#[test]
fn jmeter_statistics_follow_the_sdk_parser() {
    let summary = parse_jmeter(&mut lines(JTL), "2,0-5,0").expect("parsed");
    let names: Vec<&str> = summary.requests.iter().map(|r| r.name.as_str()).collect();
    assert_eq!(names, ["Login", "Search", "Tx", "Total Transactions"]);
    let login = &summary.requests[0];
    assert_eq!((login.total, login.ko, login.error_fraction), (2, 0, 0.0));
    assert_eq!((login.min, login.max, login.average), (100.0, 200.0, 150.0));
    assert_eq!(login.median, Some(150.0));
    assert_eq!((login.p90, login.p95), (190.0, 195.0));
    assert_eq!(summary.requests[1].error_fraction, 0.5);
    let total = &summary.requests[3];
    assert_eq!((total.total, total.ko, total.error_fraction), (5, 2, 0.4));
    assert_eq!((total.p90, total.p95), (460.0, 480.0));
    // The transaction-controller row is left out of the summary: 4 samples
    // over 3 s, one failure.
    assert_eq!(summary.throughput, 1.0);
    assert_eq!(summary.error_rate, 25.0);
    assert_eq!(summary.max_user_count, 2);
    assert_eq!(
        summary.duration,
        perf_report::Cell::Text(" 00:00:03".into())
    );
    assert_eq!(
        summary.ramp_up_period,
        perf_report::Cell::Text(" 00:00:01".into())
    );
    assert_eq!(
        summary.date_start,
        perf_report::Cell::Text("2023-11-14 22:13:20".into())
    );
    assert_eq!(
        summary.date_end,
        perf_report::Cell::Text("2023-11-14 22:13:23".into())
    );
}

#[test]
fn jmeter_refuses_what_the_sdk_would_crash_on() {
    for (text, label) in [
        ("timeStamp,elapsed\n1,2\n", "a missing column"),
        ("", "no header"),
        (
            "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n",
            "no rows",
        ),
        (
            "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n1700000000000,x,L,OK,true,1,t\n",
            "a non-numeric elapsed",
        ),
        (
            "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n2024-01-01 10:00:00,5,L,OK,true,1,t\n",
            "a date-string timeStamp",
        ),
        (
            "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n1700000000000,5,L,OK,true,1,t\n1700000000500,5,L,OK,true,1,t\n",
            "a test shorter than a second",
        ),
    ] {
        assert!(
            parse_jmeter(&mut lines(text), "x").is_err(),
            "{label} must be refused"
        );
    }
}

#[test]
fn jmeter_success_column_counts_only_boolean_true_as_success() {
    let header = "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n";
    let body = "1700000000000,10,L,OK,true,1,a\n1700000005000,20,L,OK,garbage,1,a\n";
    let summary = parse_jmeter(&mut lines(&format!("{header}{body}")), "x").expect("parsed");
    // A non-boolean word makes pandas keep strings, and no string equals True.
    assert_eq!(summary.requests[0].ko, 2);
}

#[test]
fn merged_files_drop_the_header_of_every_later_file() {
    let header = "timeStamp,elapsed,label,responseMessage,success,allThreads,threadName\n";
    let first = format!("{header}1700000000000,10,L,OK,true,1,a\n");
    let second = format!("{header}1700000005000,20,L,OK,true,1,a\n");
    let mut merged = LogLines::new(vec![
        Box::new(Cursor::new(first.into_bytes())),
        Box::new(Cursor::new(second.into_bytes())),
    ]);
    let summary = parse_jmeter(&mut merged, "x").expect("parsed");
    assert_eq!(summary.requests[0].total, 2);
}

const SIMULATION: &str = "RUN\tsim\tid\t1700000000000\tdesc\t3.9
USER\tScn\tSTART\t1700000000000\t1700000000000
USER\tScn\tSTART\t1700000060000\t1700000060000
REQUEST\tu\tHome\t1700000000100\t1700000000300\tOK\tmsg
REQUEST\tu\tHome\t1700000030000\t1700000030400\tOK\tmsg
GROUP\tCheckout\t1700000000000\t1700000001000\t700\tOK
REQUEST\tu\tPay\t1700000120000\t1700000121000\tKO\tmsg
";

#[test]
fn gatling_statistics_follow_the_sdk_parser() {
    let summary = parse_gatling(&mut lines(SIMULATION), false, "5,0-10,0").expect("parsed");
    let names: Vec<&str> = summary.requests.iter().map(|r| r.name.as_str()).collect();
    assert_eq!(names, ["Home", "Pay", "Total Requests", "Checkout"]);
    let home = &summary.requests[0];
    assert_eq!((home.total, home.ko), (2, 0));
    assert_eq!((home.min, home.max, home.average), (200.0, 400.0, 300.0));
    let total = &summary.requests[2];
    assert_eq!(
        (total.total, total.ko, total.error_fraction),
        (3, 1, 0.3333)
    );
    // Groups use the fourth number unless pauses are included.
    assert_eq!(summary.requests[3].min, 700.0);
    assert_eq!(summary.max_user_count, 2);
    assert_eq!(summary.duration, perf_report::Cell::Number(2.0));
    assert_eq!(summary.ramp_up_period, perf_report::Cell::Number(1.0));
    assert_eq!(summary.throughput, 3.0 / 120.0);
    assert_eq!(summary.error_rate, 0.3333);
    assert_eq!(
        summary.date_start,
        perf_report::Cell::Text("2023-11-14 17:13:20".into())
    );
    let with_pauses = parse_gatling(&mut lines(SIMULATION), true, "x").expect("parsed");
    assert_eq!(with_pauses.requests[3].min, 1000.0);
}

#[test]
fn gatling_refuses_an_unparsable_first_timestamp_and_a_zero_duration() {
    assert!(parse_gatling(&mut lines("RUN\tsim\tid\tnot-a-number\n"), false, "x").is_err());
    assert!(parse_gatling(&mut lines(""), false, "x").is_err());
    assert!(parse_gatling(&mut lines("RUN\ta\tb\t1700000000000\n"), false, "x").is_err());
}

#[test]
fn eastern_time_follows_the_us_daylight_saving_rules() {
    // 2023-07-22T04:26:40Z is daylight time, 2023-11-14T22:13:20Z standard.
    assert_eq!(eastern_for_tests(1_690_000_000_000), "2023-07-22 00:26:40");
    assert_eq!(eastern_for_tests(1_700_000_000_000), "2023-11-14 17:13:20");
    // The change instants of 2023: 2023-03-12 07:00Z and 2023-11-05 06:00Z.
    assert_eq!(eastern_for_tests(1_678_604_399_000), "2023-03-12 01:59:59");
    assert_eq!(eastern_for_tests(1_678_604_400_000), "2023-03-12 03:00:00");
    assert_eq!(
        eastern_for_tests(1_699_164_000_000 - 1_000),
        "2023-11-05 01:59:59"
    );
    assert_eq!(eastern_for_tests(1_699_164_000_000), "2023-11-05 01:00:00");
}

#[test]
fn thresholds_and_the_build_status_sentence_follow_the_sdk() {
    let summary = parse_jmeter(&mut lines(JTL), "x").expect("parsed");
    let defaults = Parameters::defaults();
    let verdicts = perf_report::thresholds(&summary, &defaults);
    let (status, text) = perf_report::build_status(&verdicts, &defaults.pct);
    assert_eq!(status, "FAILED");
    assert_eq!(
        text,
        "Total error rate exceed the threshold of 5% and Response Time for all transactions doesn't exceed the threshold of 0.5 seconds by 95Pct"
    );
    let strict = Parameters {
        rt_threshold: 300,
        er_threshold: 50,
        ..Parameters::defaults()
    };
    let verdicts = perf_report::thresholds(&summary, &strict);
    let (status, text) = perf_report::build_status(&verdicts, &strict.pct);
    assert_eq!(status, "FAILED");
    // The SDK replaces the error-rate sentence when a response time fails
    // first.
    assert_eq!(
        text,
        "Response Time for some transaction(s) exceed the threshold of 0.3 seconds by 95Pct"
    );
    // An unknown percentile key raises in the SDK; the report then says SUCCESS.
    let unknown = Parameters {
        pct: "99Pct".into(),
        ..Parameters::defaults()
    };
    let verdicts = perf_report::thresholds(&summary, &unknown);
    assert!(verdicts.is_empty());
    assert_eq!(
        perf_report::build_status(&verdicts, "99Pct"),
        ("SUCCESS", String::new())
    );
}

#[test]
fn lighthouse_pivot_sorts_averages_and_drops_empty_rows_and_columns() {
    let step = |name: &str, fcp: &str, cls: &str| {
        json!({"name":name,"lhr":{"audits":{
            "first-contentful-paint":{"displayValue":fcp},
            "cumulative-layout-shift":{"displayValue":cls},
            "bootup-time":{"displayValue":"1,200 ms"},
            "speed-index":{"displayValue":"n/a"}
        }}})
    };
    let data = json!({"steps":[step("b step","1.5 s","0.1"), step("a step","2 s","0"), step("b step","2.5 s","0.3")]});
    let table = lighthouse::table_from(&data).expect("table");
    assert_eq!(
        table.columns,
        [
            "cumulative-layout-shift",
            "first-contentful-paint, sec",
            "javaScript-execution-time, sec"
        ]
    );
    assert_eq!(table.rows[0].0, "a step");
    assert_eq!(table.rows[1].0, "b step");
    assert_eq!(table.rows[1].1[1], Some(2.0));
    assert_eq!(table.rows[1].1[0], Some(0.2));
    assert_eq!(table.rows[0].1[2], Some(1200.0));
    // No steps: the whole document is one `main_report` step.
    let single =
        lighthouse::table_from(&json!({"audits":{"interactive":{"displayValue":"3.2 s"}}}))
            .expect("table");
    assert_eq!(single.rows[0].0, "main_report");
    assert_eq!(single.columns, ["interactive, sec"]);
    // A document the SDK cannot walk adds no sheet.
    assert!(lighthouse::table_from(&json!([1, 2])).is_none());
    assert!(lighthouse::table_from(&json!({"steps":[3]})).is_none());
}

#[test]
fn worksheet_names_are_the_sdk_timestamp_prefix_made_unique() {
    assert_eq!(
        lighthouse::worksheet_name("24Jun2025_02_14_37_user-flow.re.json"),
        "24Jun2025_02_14_37"
    );
    assert_eq!(lighthouse::worksheet_name("a:b/c.json"), "a_b_c");
    let long = format!("{}.json", "x".repeat(40));
    assert_eq!(lighthouse::worksheet_name(&long).chars().count(), 31);
    assert_eq!(lighthouse::unique_title(&[], "run"), "run");
    assert_eq!(lighthouse::unique_title(&["RUN".into()], "run"), "run1");
    assert_eq!(
        lighthouse::unique_title(&["run".into(), "run1".into()], "run"),
        "run2"
    );
}

// ---------------------------------------------------------------------------
// get_report_by_id
// ---------------------------------------------------------------------------

fn report_route(url: &str) -> Option<Result<CarrierHttpResponse, CarrierClientError>> {
    let path = url.strip_prefix(BASE)?;
    if path.starts_with("backend_performance/reports/12?report_id=R1") {
        return Some(ok(&json!({
            "id":1,"name":"My Test_1","build_id":"b7","lg_type":"gatling","status":"ok"
        })));
    }
    if path == "artifacts/artifacts/default/12/mytest1" {
        return Some(ok(&json!({"rows":[
            {"name":"reports_test_results_b7_a.zip"},
            {"name":"reports_test_results_b7_excel_report.xlsx"},
            {"name":"other.zip"}
        ]})));
    }
    None
}

fn errors_text(count: usize) -> String {
    (1..=count)
        .map(|n| format!("error line {n}\n"))
        .collect::<Vec<_>>()
        .concat()
}

#[tokio::test]
async fn report_by_id_returns_the_first_hundred_error_lines_and_uploads_one_zip() {
    let archive = zip_of(&[("simulation-errors.log", errors_text(150).as_bytes())]);
    let transport = Transport::new(move |seen| {
        if let Some(answer) = report_route(&seen.url) {
            return answer;
        }
        if seen.url == format!("{BASE}artifacts/artifact/12/mytest1/reports_test_results_b7_a.zip")
        {
            return bytes(&archive);
        }
        if seen.method == Method::POST {
            return status(StatusCode::OK);
        }
        panic!("unexpected {} {}", seen.method, seen.url);
    });
    let tool = make_tool(&transport, None, "get_report_by_id").await;
    assert!(!tool.is_read_only());
    assert!(!tool.is_concurrency_safe());
    let report = tool
        .execute(context(), json!({"report_id":"R1"}))
        .await
        .expect("report");
    assert_eq!(report["name"], "My Test_1");
    let lines = report["errors_log"].as_array().expect("lines");
    assert_eq!(lines.len(), 100);
    assert_eq!(lines[0], "error line 1\n");
    assert_eq!(
        report["link_to_errors_file"],
        "https://carrier.example.test/api/v1/artifacts/artifact/default/12/mytest1/b7_error_log_archive.zip"
    );
    let upload = transport
        .seen()
        .into_iter()
        .find(|seen| seen.method == Method::POST)
        .expect("one upload");
    assert!(upload.effect);
    assert_eq!(
        upload.url,
        format!("{BASE}artifacts/artifacts/12/mytest1?integration_id=1&is_local=False")
    );
    assert_eq!(
        upload.authorization.as_deref(),
        Some("bearer carrier-super-secret")
    );
    assert!(
        upload
            .content_type
            .expect("type")
            .starts_with("multipart/form-data")
    );
    let body = upload.bytes.expect("body");
    let text = String::from_utf8_lossy(&body);
    assert!(text.contains("filename=\"b7_error_log_archive.zip\""));
    // The uploaded zip holds the whole merged log, not the first 100 lines.
    let start = body
        .windows(4)
        .position(|w| w == b"PK\x03\x04")
        .expect("zip start");
    let end = body
        .windows(4)
        .rposition(|w| w == b"\r\n--")
        .expect("boundary");
    let inner = &body[start..end];
    assert_eq!(
        unzip(inner, "error_mytest1_simulation.log"),
        errors_text(150)
    );
    assert_eq!(
        transport
            .seen()
            .iter()
            .filter(|s| s.method == Method::POST)
            .count(),
        1
    );
}

#[tokio::test]
async fn report_by_id_swallows_error_log_failures_like_the_sdk() {
    // A jmeter report has no simulation-errors.log: the log is empty, the
    // zip of it is still uploaded.
    let archive = zip_of(&[("jmeter.jtl", b"x")]);
    let transport = Transport::new(move |seen| {
        if let Some(answer) = report_route(&seen.url) {
            return match answer {
                Ok(_) if seen.url.contains("report_id") => ok(&json!({
                    "name":"My Test_1","build_id":"b7","lg_type":"jmeter"
                })),
                other => other,
            };
        }
        if seen.method == Method::POST {
            return status(StatusCode::OK);
        }
        bytes(&archive)
    });
    let tool = make_tool(&transport, None, "get_report_by_id").await;
    let report = tool
        .execute(context(), json!({"report_id":"R1"}))
        .await
        .expect("report");
    assert_eq!(report["errors_log"], json!([]));
    assert!(
        report["link_to_errors_file"]
            .as_str()
            .expect("link")
            .ends_with("/mytest1/b7_error_log_archive.zip")
    );

    // A failed upload: no link, no lines, the call itself still answers.
    let archive = zip_of(&[("simulation-errors.log", b"boom\n")]);
    let transport = Transport::new(move |seen| {
        if let Some(answer) = report_route(&seen.url) {
            return answer;
        }
        if seen.method == Method::POST {
            return status(StatusCode::BAD_GATEWAY);
        }
        bytes(&archive)
    });
    let tool = make_tool(&transport, None, "get_report_by_id").await;
    let report = tool
        .execute(context(), json!({"report_id":"R1"}))
        .await
        .expect("report");
    assert_eq!(report["errors_log"], json!([]));
    assert_eq!(report["link_to_errors_file"], "link is not available");
    assert_eq!(
        transport
            .seen()
            .iter()
            .filter(|s| s.method == Method::POST)
            .count(),
        1,
        "the upload is never retried"
    );
}

#[tokio::test]
async fn report_by_id_failures_before_the_upload_are_errors() {
    for (code, category) in [
        (StatusCode::UNAUTHORIZED, ErrorCategory::Unauthorized),
        (StatusCode::NOT_FOUND, ErrorCategory::NotFound),
        (StatusCode::TOO_MANY_REQUESTS, ErrorCategory::RateLimited),
    ] {
        let transport = Transport::new(move |_| status(code));
        let tool = make_tool(&transport, None, "get_report_by_id").await;
        let error = tool
            .execute(context(), json!({"report_id":"R1"}))
            .await
            .expect_err("failure");
        assert_eq!(error.category, category, "{code}");
    }
    // A listing that is not a listing.
    let transport = Transport::new(|seen| {
        if seen.url.contains("report_id") {
            return report_route(&seen.url).expect("report route");
        }
        ok(&json!({"nope":1}))
    });
    let tool = make_tool(&transport, None, "get_report_by_id").await;
    assert!(
        tool.execute(context(), json!({"report_id":"R1"}))
            .await
            .is_err()
    );
    // Bad arguments.
    let transport = Transport::new(|_| panic!("no request expected"));
    let tool = make_tool(&transport, None, "get_report_by_id").await;
    for arguments in [
        json!({}),
        json!({"report_id":1}),
        json!({"report_id":"a","x":1}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
}

// ---------------------------------------------------------------------------
// create_excel_report
// ---------------------------------------------------------------------------

fn gatling_archive() -> Vec<u8> {
    zip_of(&[
        ("run-1/simulation.log", SIMULATION.as_bytes()),
        ("simulation-errors.log", b""),
    ])
}

fn excel_transport() -> Arc<Transport> {
    let archive = gatling_archive();
    Transport::new(move |seen| {
        if let Some(answer) = report_route(&seen.url) {
            return answer;
        }
        if seen.url == format!("{BASE}artifacts/artifact/12/mytest1/reports_test_results_b7_a.zip")
        {
            return bytes(&archive);
        }
        if seen
            .url
            .starts_with(&format!("{BASE}artifacts/artifact/default/12/results/"))
        {
            return bytes(JTL.as_bytes());
        }
        panic!("unexpected {} {}", seen.method, seen.url);
    })
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn excel_report_asks_for_input_then_for_parameter_confirmation() {
    let transport = Transport::new(|_| panic!("no request expected"));
    let platform = FakePlatform::new();
    let tool = make_tool(&transport, Some(&platform), "create_excel_report").await;
    assert!(!tool.is_read_only());
    assert!(!tool.is_concurrency_safe());
    let missing = tool.execute(context(), json!({})).await.expect("message");
    assert_eq!(
        missing,
        json!({
            "message":"Please provide report ID or bucket and .jtl/.log file name.",
            "parameters":{"report_id":null,"bucket":null,"file_name":null}
        })
    );
    // A bucket without a file name is still missing input.
    let half = tool
        .execute(context(), json!({"bucket":"b"}))
        .await
        .expect("message");
    assert_eq!(half["message"], missing["message"]);
    for arguments in [
        json!({"report_id":"R1"}),
        json!({"report_id":"R1","pct":null,"tp_threshold":null}),
    ] {
        let confirm = tool.execute(context(), arguments).await.expect("confirm");
        assert_eq!(
            confirm,
            json!({
                "message":"Please confirm or override the following parameters to proceed with the report generation.",
                "parameters":{
                    "think_time":"2,0-5,0","pct":"95Pct","tp_threshold":10,
                    "rt_threshold":500,"er_threshold":5,"include_group_pauses":false
                }
            })
        );
    }
    assert!(platform.writes.lock().expect("writes").is_empty());
}

#[cfg(feature = "toolkit-office")]
fn cell(workbook: &elitea_office_core::Workbook, row: u32, column: u16) -> Value {
    use elitea_office_core::CellValue;
    match workbook.read_cell(0, row, column).expect("cell") {
        CellValue::Text(text) => Value::String(text),
        CellValue::Number(number) => json!(number),
        _ => Value::Null,
    }
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn excel_report_from_a_report_id_stores_the_sdk_sheet_in_the_artifact_store() {
    let transport = excel_transport();
    let platform = FakePlatform::new();
    // A fresh project: the report's bucket is created, once.
    platform.no_buckets();
    let tool = make_tool(&transport, Some(&platform), "create_excel_report").await;
    let answer = tool
        .execute(
            context(),
            json!({"report_id":"R1","pct":"95Pct","rt_threshold":"300"}),
        )
        .await
        .expect("report");
    assert_eq!(
        answer,
        json!(
            "Excel report generated and stored in the project's artifact store, bucket mytest1, report name: reports_test_results_b7_excel_report.xlsx, artifact path: /mytest1/reports_test_results_b7_excel_report.xlsx"
        )
    );
    assert_eq!(
        *platform.created.lock().expect("created"),
        ["mytest1".to_owned()]
    );
    // The first write found no bucket; the retry after creating it stored.
    let writes = platform.writes.lock().expect("writes").clone();
    assert_eq!(writes.len(), 2);
    assert!(
        writes.iter().all(|write| !write.3),
        "never asks to overwrite"
    );
    let stored = platform
        .get("mytest1", "reports_test_results_b7_excel_report.xlsx")
        .expect("stored");
    let workbook =
        elitea_office_core::Workbook::open(&stored, &elitea_office_core::Limits::default())
            .expect("xlsx");
    assert_eq!(workbook.sheet_names().expect("names"), ["Test results"]);
    assert_eq!(cell(&workbook, 0, 0), json!("Users"));
    assert_eq!(cell(&workbook, 0, 1), json!(2.0));
    assert_eq!(cell(&workbook, 3, 1), json!("2,0-5,0"));
    assert_eq!(cell(&workbook, 4, 1), json!("2023-11-14 17:13:20"));
    assert_eq!(
        cell(&workbook, 8, 1),
        json!("https://carrier.example.test/-/performance/backend/results?result_id=R1")
    );
    assert_eq!(cell(&workbook, 9, 0), json!("Build status"));
    assert_eq!(cell(&workbook, 9, 1), json!("FAILED"));
    assert_eq!(cell(&workbook, 11, 0), json!("Transaction"));
    assert_eq!(cell(&workbook, 11, 8), json!("Max, sec"));
    assert_eq!(cell(&workbook, 12, 0), json!("Home"));
    assert_eq!(cell(&workbook, 12, 1), json!(2.0));
    assert_eq!(cell(&workbook, 12, 4), json!(0.2));
    assert_eq!(cell(&workbook, 12, 8), json!(0.4));
    assert_eq!(cell(&workbook, 14, 0), json!("Total Requests"));

    // A second run never replaces the first: the next free name is used.
    let again = tool
        .execute(context(), json!({"report_id":"R1","pct":"95Pct"}))
        .await
        .expect("second report");
    assert!(
        again
            .as_str()
            .expect("text")
            .contains("/mytest1/reports_test_results_b7_excel_report-2.xlsx")
    );
    assert!(
        platform
            .get("mytest1", "reports_test_results_b7_excel_report.xlsx")
            .is_some()
    );
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn excel_report_from_a_bucket_file_reads_it_with_the_bare_header() {
    let transport = excel_transport();
    let platform = FakePlatform::new();
    let tool = make_tool(&transport, Some(&platform), "create_excel_report").await;
    let answer = tool
        .execute(
            context(),
            json!({"bucket":"results","file_name":"run.jtl","think_time":"1,0-2,0"}),
        )
        .await
        .expect("report");
    let text = answer.as_str().expect("text");
    assert!(
        text.contains("bucket results, report name: run.jtl_"),
        "{text}"
    );
    let seen = transport.seen();
    assert_eq!(seen.len(), 1);
    assert_eq!(
        seen[0].url,
        format!(
            "{BASE}artifacts/artifact/default/12/results/run.jtl?integration_id=1&is_local=False"
        )
    );
    assert_eq!(
        seen[0].authorization.as_deref(),
        Some("bearer carrier-super-secret")
    );
    let writes = platform.writes.lock().expect("writes").clone();
    let stored = platform.get("results", &writes[0].1).expect("stored");
    let workbook =
        elitea_office_core::Workbook::open(&stored, &elitea_office_core::Limits::default())
            .expect("xlsx");
    assert_eq!(cell(&workbook, 3, 1), json!("1,0-2,0"));
    assert_eq!(cell(&workbook, 8, 1), json!("not specified"));
    assert_eq!(cell(&workbook, 12, 0), json!("Login"));
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn excel_report_failures_name_their_cause_and_store_nothing() {
    let platform = FakePlatform::new();
    let text_of = |answer: Value| answer.as_str().expect("a sentence").to_owned();
    // A report type the parser does not know.
    let transport = Transport::new(|seen| {
        if seen.url.contains("report_id") {
            return ok(&json!({"name":"x","build_id":"1","lg_type":"k6"}));
        }
        panic!("unexpected {}", seen.url);
    });
    let tool_unknown = make_tool(&transport, Some(&platform), "create_excel_report").await;
    let answer = tool_unknown
        .execute(context(), json!({"report_id":"R1","pct":"95Pct"}))
        .await
        .expect("sentence");
    assert_eq!(text_of(answer), "Unsupported type of backend report");

    // An archive without its log, and a file that is not a zip.
    for body in [zip_of(&[("readme.txt", b"hi")]), b"not a zip".to_vec()] {
        let transport = Transport::new(move |seen| {
            if let Some(answer) = report_route(&seen.url) {
                return answer;
            }
            bytes(&body)
        });
        let tool_bad = make_tool(&transport, Some(&platform), "create_excel_report").await;
        let answer = tool_bad
            .execute(context(), json!({"report_id":"R1","pct":"95Pct"}))
            .await
            .expect("sentence");
        assert!(text_of(answer).ends_with("nothing was generated."));
    }

    // Provider answers keep their category.
    for (code, category) in [
        (StatusCode::NOT_FOUND, ErrorCategory::NotFound),
        (StatusCode::TOO_MANY_REQUESTS, ErrorCategory::RateLimited),
        (StatusCode::UNAUTHORIZED, ErrorCategory::Unauthorized),
    ] {
        let transport = Transport::new(move |seen| {
            if let Some(routed) = report_route(&seen.url) {
                return routed;
            }
            status(code)
        });
        let tool_failed = make_tool(&transport, Some(&platform), "create_excel_report").await;
        let error = tool_failed
            .execute(context(), json!({"report_id":"R1","pct":"95Pct"}))
            .await
            .expect_err("failure");
        assert_eq!(error.category, category);
    }

    // A test shorter than a second.
    let transport = Transport::new(|_| bytes(b"RUN\ta\tb\t1700000000000\n"));
    let tool_short = make_tool(&transport, Some(&platform), "create_excel_report").await;
    let answer = tool_short
        .execute(
            context(),
            json!({"bucket":"b","file_name":"x.log","pct":"95Pct"}),
        )
        .await
        .expect("sentence");
    assert!(text_of(answer).contains("less than one second"));

    // The store refuses or loses the write.
    let transport = excel_transport();
    for (failure, words) in [
        (OfficeFailure::AccessRefused, "refused access"),
        (OfficeFailure::NotFound, "bucket was not found"),
        (
            OfficeFailure::Unreachable,
            "may or may not have been stored",
        ),
    ] {
        let platform = FakePlatform::new();
        platform.fail_writes(failure);
        let tool_store = make_tool(&transport, Some(&platform), "create_excel_report").await;
        let answer = tool_store
            .execute(context(), json!({"report_id":"R1","pct":"95Pct"}))
            .await
            .expect("sentence");
        let text = text_of(answer);
        assert!(text.contains(words), "{text}");
        assert!(platform.objects.lock().expect("objects").is_empty());
    }
    assert!(platform.writes.lock().expect("writes").is_empty());
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn excel_report_arguments_are_validated_before_any_request() {
    let transport = Transport::new(|_| panic!("no request expected"));
    let platform = FakePlatform::new();
    let tool = make_tool(&transport, Some(&platform), "create_excel_report").await;
    for arguments in [
        json!({"report_id":"R1","tp_threshold":"many"}),
        json!({"report_id":"R1","include_group_pauses":"yes"}),
        json!({"report_id":"R1","pct":5}),
        json!({"report_id":"R1","extra":1}),
        json!({"report_id":7}),
    ] {
        assert!(
            tool.execute(context(), arguments.clone()).await.is_err(),
            "{arguments}"
        );
    }
}

// ---------------------------------------------------------------------------
// create_ui_excel_report
// ---------------------------------------------------------------------------

fn lighthouse_json(fcp: &str) -> Value {
    json!({"steps":[{"name":"home","lhr":{"audits":{
        "first-contentful-paint":{"displayValue":fcp},
        "total-blocking-time":{"displayValue":"120 ms"}
    }}}]})
}

fn ui_transport(fail_second: bool) -> Arc<Transport> {
    Transport::new(move |seen| {
        let path = seen.url.strip_prefix(BASE).unwrap_or_default();
        if path == "ui_performance/reports/12" {
            return ok(&json!({"rows":[
                {"id":5,"uid":"u-5","name":"Checkout_Flow","test_status":"Finished","start_time":"t"},
                {"id":6,"name":"No uid"},
                {"id":8,"uid":"u-8","name":"Empty"}
            ]}));
        }
        if path.starts_with("ui_performance/results/12/u-5") {
            return ok(&json!({"l":[
                {"file_name":"24Jun2025_02_14_37_flow.html#index=1"},
                {"file_name":"25Jun2025_03_15_38_flow.html"}
            ]}));
        }
        if path.starts_with("ui_performance/results/12/u-8") {
            return ok(&json!({"l":[]}));
        }
        if path == "artifacts/artifact/default/12/reports/24Jun2025_02_14_37_flow.json" {
            return ok(&lighthouse_json("1.5 s"));
        }
        if path == "artifacts/artifact/default/12/reports/25Jun2025_03_15_38_flow.json" {
            return if fail_second {
                status(StatusCode::NOT_FOUND)
            } else {
                ok(&lighthouse_json("2.5 s"))
            };
        }
        panic!("unexpected {} {}", seen.method, seen.url);
    })
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn ui_excel_report_answers_without_failing_for_its_input_problems() {
    let transport = ui_transport(false);
    let platform = FakePlatform::new();
    let tool = make_tool(&transport, Some(&platform), "create_ui_excel_report").await;
    assert!(!tool.is_read_only());
    let blank = tool
        .execute(context(), json!({"report_id":"  "}))
        .await
        .expect("text");
    assert_eq!(
        blank,
        json!("Please provide me test id for generating excel report from your UI test")
    );
    assert_eq!(
        tool.execute(context(), json!({})).await.expect("text"),
        blank
    );
    let unknown = tool
        .execute(context(), json!({"report_id":"99"}))
        .await
        .expect("text");
    let text = unknown.as_str().expect("text");
    assert!(text.starts_with("# \u{274c} No report found for the specified report ID\n"));
    assert!(text.contains("- **Report ID: 5** - Checkout_Flow (Finished) - t"));
    assert!(text.contains("report_id: 5"));
    let no_uid = tool
        .execute(context(), json!({"report_id":"6"}))
        .await
        .expect("text");
    assert_eq!(
        no_uid,
        json!("\u{274c} **No UID found for report 6. Cannot process this report.**")
    );
    let no_links = tool
        .execute(context(), json!({"report_id":"8"}))
        .await
        .expect("text");
    assert_eq!(
        no_links,
        json!("\u{274c} **No report links found for report 8.**")
    );
    assert!(platform.writes.lock().expect("writes").is_empty());
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn ui_excel_report_stores_one_sheet_per_lighthouse_json() {
    let transport = ui_transport(false);
    let platform = FakePlatform::new();
    let tool = make_tool(&transport, Some(&platform), "create_ui_excel_report").await;
    let answer = tool
        .execute(context(), json!({"report_id":"5"}))
        .await
        .expect("report");
    let text = answer.as_str().expect("text");
    assert!(
        text.starts_with("# \u{2705} UI Excel Report Generated Successfully!"),
        "{text}"
    );
    assert!(text.contains("- **JSON Files Processed:** `2`"));
    assert!(text.contains("- **Bucket:** `checkoutflow`"));
    assert!(!text.contains("Skipped"));
    let writes = platform.writes.lock().expect("writes").clone();
    assert_eq!(writes.len(), 1);
    assert!(!writes[0].3);
    assert!(writes[0].1.starts_with("ui_report_5_"));
    let stored = platform.get("checkoutflow", &writes[0].1).expect("stored");
    let workbook =
        elitea_office_core::Workbook::open(&stored, &elitea_office_core::Limits::default())
            .expect("xlsx");
    assert_eq!(
        workbook.sheet_names().expect("names"),
        ["24Jun2025_02_14_37", "25Jun2025_03_15_38"]
    );
    assert_eq!(cell(&workbook, 0, 0), json!("Step name"));
    assert_eq!(cell(&workbook, 0, 1), json!("first-contentful-paint, sec"));
    assert_eq!(cell(&workbook, 0, 2), json!("total-blocking-time, ms"));
    assert_eq!(cell(&workbook, 1, 0), json!("home"));
    assert_eq!(cell(&workbook, 1, 1), json!(1.5));
    assert_eq!(cell(&workbook, 1, 2), json!(120.0));
}

#[cfg(feature = "toolkit-office")]
#[tokio::test]
async fn ui_excel_report_reports_files_it_skipped_and_store_failures() {
    let platform = FakePlatform::new();
    let transport = ui_transport(true);
    let tool_skip = make_tool(&transport, Some(&platform), "create_ui_excel_report").await;
    let answer = tool_skip
        .execute(context(), json!({"report_id":"5"}))
        .await
        .expect("report");
    let text = answer.as_str().expect("text");
    assert!(text.contains("- **JSON Files Processed:** `1`"), "{text}");
    assert!(text.contains("- **JSON Files Skipped:** `1`"));

    // Nothing downloadable: the SDK's message, not an error.
    let transport = Transport::new(|seen| {
        let path = seen.url.strip_prefix(BASE).unwrap_or_default();
        if path == "ui_performance/reports/12" {
            return ok(&json!({"rows":[{"id":5,"uid":"u-5","name":"Q"}]}));
        }
        if path.starts_with("ui_performance/results/") {
            return ok(&json!({"l":[{"file_name":"a.html"}]}));
        }
        status(StatusCode::NOT_FOUND)
    });
    let platform = FakePlatform::new();
    let tool_none = make_tool(&transport, Some(&platform), "create_ui_excel_report").await;
    let none = tool_none
        .execute(context(), json!({"report_id":"5"}))
        .await
        .expect("text");
    assert_eq!(
        none,
        json!("\u{274c} **No JSON files could be processed for report 5.**")
    );

    // A write the store refuses.
    let platform = FakePlatform::new();
    platform.fail_writes(OfficeFailure::AccessRefused);
    let transport = ui_transport(false);
    let tool_refused = make_tool(&transport, Some(&platform), "create_ui_excel_report").await;
    let answer = tool_refused
        .execute(context(), json!({"report_id":"5"}))
        .await
        .expect("sentence");
    assert!(answer.as_str().expect("text").contains("refused access"));
}
