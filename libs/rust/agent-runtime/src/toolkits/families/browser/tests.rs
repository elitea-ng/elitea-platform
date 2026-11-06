//! The `browser` family over a fake session and a fake seconds meter.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{ReadonlyContext, Tool, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use elitea_browser_tools::{frame, spec};
use serde_json::{Map, Value, json};

use super::*;
use crate::approval_policy::{ApprovalPolicy, RiskTier};
use crate::egress::{self, RunEgress};
use crate::run_caps::{RunCap, RunCapsMeter};
use crate::tool_category::ToolCategory;
use crate::toolkits::families::web::meter::{
    Admission, Meter, MeterError, MeterRefusal, MeteredOp, SettleRequest, Settlement,
};

const PAGE: &str = "# snapshot s1\n# url \"https://shop.example/cart\"\n# title \"Cart\"\n\
- textbox \"Search\" [ref=e1]\n\
- button \"Place order\" [ref=e2]\n\
- button \"Delete account\" [ref=e3]\n\
- button \"Submit\" [ref=e4]\n\
- button \"Next page\" [ref=e5]\n";

const NONCE: &str = "0123456789abcdef0123456789abcdef";

/// A session that answers from a queue and records what it was asked.
#[derive(Default)]
struct FakeSession {
    answers: Mutex<VecDeque<Result<SessionAnswer, SessionError>>>,
    calls: Mutex<Vec<(String, String)>>,
    ended: Mutex<Vec<bool>>,
}

impl FakeSession {
    fn answering(
        answers: impl IntoIterator<Item = Result<SessionAnswer, SessionError>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            answers: Mutex::new(answers.into_iter().collect()),
            ..Self::default()
        })
    }

    fn calls(&self) -> Vec<(String, String)> {
        self.calls.lock().expect("calls").clone()
    }
}

#[async_trait]
impl BrowserSession for FakeSession {
    async fn call(
        &self,
        call_id: &str,
        name: &str,
        _arguments: &Value,
    ) -> Result<SessionAnswer, SessionError> {
        self.calls
            .lock()
            .expect("calls")
            .push((call_id.to_owned(), name.to_owned()));
        self.answers
            .lock()
            .expect("answers")
            .pop_front()
            .unwrap_or_else(|| Ok(answer(json!({"status": "ok"}))))
    }

    async fn end(&self, now: bool) {
        self.ended.lock().expect("ended").push(now);
    }
}

/// A meter priced per second that records admits and settles.
#[derive(Default)]
struct SecondsMeter {
    admits: Mutex<Vec<MeteredOp>>,
    settles: Mutex<Vec<SettleRequest>>,
    refusal: Mutex<Option<MeterError>>,
}

impl SecondsMeter {
    fn refusing(error: MeterError) -> Arc<Self> {
        let meter = Self::default();
        *meter.refusal.lock().expect("refusal") = Some(error);
        Arc::new(meter)
    }

    fn admitted(&self) -> Vec<MeteredOp> {
        self.admits.lock().expect("admits").clone()
    }

    async fn settled(&self, count: usize) -> Vec<SettleRequest> {
        for _ in 0..200 {
            let settles = self.settles.lock().expect("settles").clone();
            if settles.len() >= count {
                return settles;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        self.settles.lock().expect("settles").clone()
    }
}

#[async_trait]
impl Meter for SecondsMeter {
    fn attribution_execution_id(&self) -> &'static str {
        "execution-1"
    }

    async fn admit(&self, op: &MeteredOp) -> Result<Admission, MeterError> {
        self.admits.lock().expect("admits").push(*op);
        if let Some(error) = self.refusal.lock().expect("refusal").clone() {
            return Err(error);
        }
        Ok(Admission {
            admission_id: "admission-browser".to_owned(),
            unit: "second".to_owned(),
            nano_usd_per_unit: 1_000,
        })
    }

    async fn settle(&self, request: &SettleRequest) -> Result<Settlement, MeterError> {
        self.settles.lock().expect("settles").push(request.clone());
        Ok(Settlement {
            billed_nano_usd: 1_000,
            duplicate: false,
        })
    }
}

fn answer(result: Value) -> SessionAnswer {
    SessionAnswer {
        result,
        image: None,
    }
}

fn snapshot_answer() -> SessionAnswer {
    answer(json!({
        "status": "ok",
        "snapshot": "s1",
        "refs": 5,
        "truncated": false,
        "omitted": 0,
        "content": frame::frame_with_nonce(PAGE, NONCE),
    }))
}

struct Rig {
    session: Arc<FakeSession>,
    meter: Arc<SecondsMeter>,
    caps: Arc<RunCapsMeter>,
    host: BrowserToolHost,
}

fn rig_with(
    origin: RunOrigin,
    submit_hosts: &[&str],
    approval: Arc<ApprovalPolicy>,
    limits: BrowserLimits,
    session: Arc<FakeSession>,
    meter: Arc<SecondsMeter>,
) -> Rig {
    let caps = RunCapsMeter::unlimited();
    let budget = BrowserBudget::new(limits, Arc::clone(&caps));
    let run = BrowserRun::new(
        origin,
        submit_hosts.iter().map(|host| (*host).to_owned()),
        approval,
        budget,
    );
    let host = BrowserToolHost::new(
        Arc::clone(&session) as Arc<dyn BrowserSession>,
        Arc::clone(&meter) as Arc<dyn Meter>,
        run,
    );
    Rig {
        session,
        meter,
        caps,
        host,
    }
}

fn person_rig(origin: RunOrigin, session: Arc<FakeSession>) -> Rig {
    rig_with(
        origin,
        &[],
        ApprovalPolicy::none(),
        BrowserLimits::default(),
        session,
        Arc::new(SecondsMeter::default()),
    )
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

fn context(call: &str) -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("browser-tool-test")
            .with_session_id("session-1")
            .with_function_call_id(call),
    )
}

async fn tools(
    host: Option<&BrowserToolHost>,
    settings: &Map<String, Value>,
) -> Vec<Arc<dyn Tool>> {
    let toolset = build_browser_toolset("cloud", settings, &policy(), host).expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context("list");
    toolset.tools(readonly).await.expect("tools")
}

async fn call(rig: &Rig, name: &str, arguments: Value) -> Value {
    let spec = spec::find(name).expect("contract tool");
    run_call(&rig.host, spec, "call-1", &arguments).await
}

#[tokio::test]
async fn the_tools_are_the_contract() {
    let tools = tools(None, &Map::new()).await;
    let exposed: Vec<(String, String, Value)> = tools
        .iter()
        .map(|tool| {
            let description = tool
                .description()
                .strip_prefix("Toolkit: cloud\n")
                .expect("toolkit prefix")
                .to_owned();
            (
                tool.name().to_owned(),
                description,
                tool.parameters_schema().expect("schema"),
            )
        })
        .collect();
    elitea_browser_tools::conformance::check_exposed(&exposed).expect("contract");
    let snapshot = tools
        .iter()
        .find(|tool| tool.name() == spec::BROWSER_SNAPSHOT)
        .expect("snapshot");
    assert!(snapshot.is_read_only());
    let click = tools
        .iter()
        .find(|tool| tool.name() == spec::BROWSER_CLICK)
        .expect("click");
    assert!(!click.is_read_only());
}

#[tokio::test]
async fn a_toolset_without_a_host_fails_closed() {
    let tools = tools(None, &Map::new()).await;
    let open = tools
        .iter()
        .find(|tool| tool.name() == spec::BROWSER_OPEN)
        .expect("open");
    let result = open
        .execute(context("call-1"), json!({"url": "https://example.com"}))
        .await
        .expect("result");
    assert_eq!(result["status"], "error");
    assert_eq!(result["code"], "browser_unavailable");
}

#[tokio::test]
async fn settings_take_only_a_known_selection() {
    let mut settings = Map::new();
    settings.insert(
        "selected_tools".to_owned(),
        json!(["browser_open", "browser_snapshot"]),
    );
    let selected = tools(None, &settings).await;
    let names: Vec<&str> = selected.iter().map(|tool| tool.name()).collect();
    assert_eq!(names, ["browser_open", "browser_snapshot"]);

    let mut unknown = Map::new();
    unknown.insert("selected_tools".to_owned(), json!(["browser_teleport"]));
    assert_eq!(
        build_browser_toolset("cloud", &unknown, &policy(), None).err(),
        Some(BrowserToolsetError::UnsupportedSelection)
    );

    let mut extra = Map::new();
    extra.insert("headless".to_owned(), json!(false));
    assert_eq!(
        build_browser_toolset("cloud", &extra, &policy(), None).err(),
        Some(BrowserToolsetError::InvalidConfiguration)
    );
}

#[tokio::test]
async fn an_unattended_run_has_no_person_to_ask() {
    let rig = person_rig(RunOrigin::Unattended, FakeSession::answering([]));
    let result = call(
        &rig,
        spec::BROWSER_REQUEST_USER,
        json!({"reason": "log in"}),
    )
    .await;
    assert_eq!(result["status"], "error");
    assert_eq!(result["code"], "no_person");
    assert!(rig.session.calls().is_empty(), "the session is not called");
    assert!(rig.meter.admitted().is_empty(), "no time is admitted");
}

#[tokio::test]
async fn a_person_run_reaches_the_session_for_a_question() {
    let rig = person_rig(RunOrigin::Person, FakeSession::answering([]));
    let result = call(
        &rig,
        spec::BROWSER_REQUEST_USER,
        json!({"reason": "log in"}),
    )
    .await;
    assert_eq!(result["status"], "ok");
    assert_eq!(rig.session.calls().len(), 1);
}

#[tokio::test]
async fn every_call_is_metered_in_seconds() {
    let rig = person_rig(RunOrigin::Person, FakeSession::answering([]));
    let result = call(
        &rig,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(result["status"], "ok");
    let admitted = rig.meter.admitted();
    assert_eq!(admitted.len(), 1);
    assert_eq!(admitted[0].provider, METERED_PROVIDER);
    assert_eq!(admitted[0].operation, METERED_OPERATION);
    assert_eq!(admitted[0].estimate.seconds, 1);
    let settled = rig.meter.settled(1).await;
    assert_eq!(settled.len(), 1);
    assert_eq!(settled[0].units.seconds, 1);
    assert_eq!(settled[0].admission_id, "admission-browser");
    assert_eq!(rig.host.run().budget().used_seconds(), 1);
}

#[tokio::test]
async fn a_refused_admission_does_not_use_the_browser() {
    let refusal = MeterRefusal {
        code: "budget_exceeded".to_owned(),
        public_code: None,
        retry_after_ms: None,
    };
    let rig = rig_with(
        RunOrigin::Person,
        &[],
        ApprovalPolicy::none(),
        BrowserLimits::default(),
        FakeSession::answering([]),
        SecondsMeter::refusing(MeterError::Refused(refusal)),
    );
    let result = call(
        &rig,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(result["code"], "budget_exceeded");
    assert!(rig.session.calls().is_empty());

    let unpriced = rig_with(
        RunOrigin::Person,
        &[],
        ApprovalPolicy::none(),
        BrowserLimits::default(),
        FakeSession::answering([]),
        SecondsMeter::refusing(MeterError::Unpriced),
    );
    let result = call(
        &unpriced,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(result["code"], "browser_unpriced");
    assert!(unpriced.session.calls().is_empty());
}

#[tokio::test]
async fn the_run_cap_stops_the_next_call_and_latches() {
    let rig = rig_with(
        RunOrigin::Person,
        &[],
        ApprovalPolicy::none(),
        BrowserLimits {
            max_run_seconds: 1,
            ..BrowserLimits::default()
        },
        FakeSession::answering([]),
        Arc::new(SecondsMeter::default()),
    );
    let first = call(
        &rig,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(
        first["status"], "ok",
        "the call that reaches the cap still answers"
    );
    assert_eq!(rig.caps.tripped(), Some(RunCap::MaxBrowserSeconds));
    let second = call(&rig, spec::BROWSER_RELOAD, json!({})).await;
    assert_eq!(second["status"], "error");
    assert_eq!(second["code"], RunCap::MaxBrowserSeconds.error_code());
    assert_eq!(rig.session.calls().len(), 1, "the second call never ran");
}

#[tokio::test]
async fn a_used_up_monthly_quota_refuses_the_first_call() {
    let rig = rig_with(
        RunOrigin::Unattended,
        &[],
        ApprovalPolicy::none(),
        BrowserLimits {
            max_run_seconds: 0,
            monthly_quota_seconds: 60,
            month_used_seconds: 60,
        },
        FakeSession::answering([]),
        Arc::new(SecondsMeter::default()),
    );
    let result = call(
        &rig,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(result["code"], RunCap::BrowserMonthlyQuota.error_code());
    assert_eq!(rig.caps.tripped(), Some(RunCap::BrowserMonthlyQuota));
    assert!(rig.meter.admitted().is_empty());
    assert!(rig.session.calls().is_empty());
}

#[tokio::test]
async fn an_egress_refusal_is_in_the_run_ledger() {
    let refused = json!({
        "status": "error",
        "code": "egress_refused",
        "message": "the platform does not allow this site",
        "host": "Tracker.Example.",
        "port": 443,
    });
    let rig = person_rig(
        RunOrigin::Person,
        FakeSession::answering([Ok(answer(refused))]),
    );
    let run_egress = RunEgress::open();
    let result = egress::scope(
        Arc::clone(&run_egress),
        call(
            &rig,
            spec::BROWSER_OPEN,
            json!({"url": "https://tracker.example"}),
        ),
    )
    .await;
    assert_eq!(
        result["code"], "egress_refused",
        "the contract's result reaches the model"
    );
    let summary = run_egress.summary();
    assert_eq!(summary.refused.len(), 1);
    assert_eq!(summary.refused[0].host, "tracker.example");
    assert_eq!(summary.refused[0].port, 443);
    assert_eq!(summary.refused[0].count, 1);
}

#[tokio::test]
async fn a_session_failure_has_fixed_text() {
    let rig = person_rig(
        RunOrigin::Person,
        FakeSession::answering([Err(SessionError::Capacity)]),
    );
    let result = call(
        &rig,
        spec::BROWSER_OPEN,
        json!({"url": "https://example.com"}),
    )
    .await;
    assert_eq!(result["code"], "browser_capacity");
    assert_eq!(result["message"], SessionError::Capacity.to_string());
    let settled = rig.meter.settled(1).await;
    assert_eq!(settled.len(), 1, "a failed call is settled too");
}

#[tokio::test]
async fn a_screenshot_is_an_image_result() {
    let screenshot = SessionAnswer {
        result: json!({"status": "ok", "width": 2, "height": 2}),
        image: Some(SessionImage {
            media_type: "image/png".to_owned(),
            data: vec![0x89, b'P', b'N', b'G'],
        }),
    };
    let rig = person_rig(RunOrigin::Person, FakeSession::answering([Ok(screenshot)]));
    let result = call(&rig, spec::BROWSER_SCREENSHOT, json!({})).await;
    assert_eq!(result["status"], "ok");
    assert_eq!(result["kind"], crate::image_result::IMAGE_KIND);
    assert_eq!(result["mime"], "image/png");
    assert_eq!(result["bytes"], 4);
    assert_eq!(result[crate::image_result::IMAGE_FIELD], "iVBORw==");

    let wrong = SessionAnswer {
        result: json!({"status": "ok"}),
        image: Some(SessionImage {
            media_type: "image/svg+xml".to_owned(),
            data: b"<svg/>".to_vec(),
        }),
    };
    let wrong_rig = person_rig(RunOrigin::Person, FakeSession::answering([Ok(wrong)]));
    let result = call(&wrong_rig, spec::BROWSER_SCREENSHOT, json!({})).await;
    assert_eq!(result["code"], "browser_invalid_image");
}

#[tokio::test]
async fn the_gate_follows_the_latest_snapshot() {
    let rig = person_rig(
        RunOrigin::Person,
        FakeSession::answering([Ok(snapshot_answer())]),
    );
    let run = Arc::clone(rig.host.run());
    // Bind the names, as building the toolset does.
    drop(tools(Some(&rig.host), &Map::new()).await);

    // Before a snapshot a ref resolves to nothing: a plain write.
    assert!(!run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e2"})));

    let result = call(&rig, spec::BROWSER_SNAPSHOT, json!({})).await;
    assert_eq!(result["status"], "ok");
    assert_eq!(run.page_host().as_deref(), Some("shop.example"));

    // Payment, consequential and submit controls ask.
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e2"})));
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e3"})));
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e4"})));
    // A plain act and a read do not, with no policy.
    assert!(!run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e5"})));
    assert!(!run.requires_confirmation(spec::BROWSER_SNAPSHOT, &json!({})));
    // A routed name is the same tool; another toolkit's tool is not ours.
    assert!(run.requires_confirmation("cloud___browser_click", &json!({"ref": "e4"})));
    assert!(!run.requires_confirmation("github_create_issue", &json!({})));

    // An act makes the page stale: its refs no longer classify.
    let clicked = call(&rig, spec::BROWSER_CLICK, json!({"ref": "e5"})).await;
    assert_eq!(clicked["status"], "ok");
    assert_eq!(run.page_host(), None);
    assert!(!run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e4"})));
}

#[tokio::test]
async fn a_grant_answers_a_submit_on_its_host_only() {
    let rig = rig_with(
        RunOrigin::Unattended,
        &["Shop.Example."],
        ApprovalPolicy::none(),
        BrowserLimits::default(),
        FakeSession::answering([Ok(snapshot_answer())]),
        Arc::new(SecondsMeter::default()),
    );
    let run = Arc::clone(rig.host.run());
    drop(tools(Some(&rig.host), &Map::new()).await);
    call(&rig, spec::BROWSER_SNAPSHOT, json!({})).await;

    // The submit on the granted host does not ask.
    assert!(!run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e4"})));
    // A grant never answers a consequential or a payment control.
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e3"})));
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e2"})));

    let other = rig_with(
        RunOrigin::Unattended,
        &["other.example"],
        ApprovalPolicy::none(),
        BrowserLimits::default(),
        FakeSession::answering([Ok(snapshot_answer())]),
        Arc::new(SecondsMeter::default()),
    );
    let run = Arc::clone(other.host.run());
    drop(tools(Some(&other.host), &Map::new()).await);
    call(&other, spec::BROWSER_SNAPSHOT, json!({})).await;
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e4"})));
}

#[tokio::test]
async fn the_approval_policy_names_plain_categories() {
    let rig = rig_with(
        RunOrigin::Person,
        &[],
        Arc::new(ApprovalPolicy::new(RiskTier::High, [ToolCategory::Write])),
        BrowserLimits::default(),
        FakeSession::answering([]),
        Arc::new(SecondsMeter::default()),
    );
    let run = Arc::clone(rig.host.run());
    drop(tools(Some(&rig.host), &Map::new()).await);
    assert!(run.requires_confirmation(spec::BROWSER_CLICK, &json!({"ref": "e5"})));
    assert!(!run.requires_confirmation(spec::BROWSER_OPEN, &json!({"url": "https://a.example"})));
}

#[test]
fn routed_names_find_their_contract_tool() {
    assert_eq!(contract_name("browser_click"), Some(spec::BROWSER_CLICK));
    assert_eq!(
        contract_name("cloud___browser_open"),
        Some(spec::BROWSER_OPEN)
    );
    assert_eq!(contract_name("cloud___web_search"), None);
}
