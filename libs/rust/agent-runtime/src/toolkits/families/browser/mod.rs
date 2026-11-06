//! The `browser` toolkit family (ADR-0049 decision 3, #1596): the browser
//! tool contract of `libs/rust/browser-tools` on a cloud browser session.
//!
//! The tools are [`elitea_browser_tools::TOOLS`]: the same names,
//! descriptions and schemas as the desktop's browser pane, never retyped
//! here. Each call goes to the run's session through the host's
//! [`BrowserSession`] (the worker: `BrowserSessionService` over gRPC and
//! mTLS to the supervisor). The family adds the cloud's own rules around it:
//!
//! * **Category.** [`category::classify`] gives every call its category
//!   from the call and the latest snapshot the run read: `read`, `write`,
//!   `external_send` (a submit, an upload, a consequential control) or
//!   `payment`. [`BrowserRun::requires_confirmation`] is the argument-aware
//!   gate the host binds: it asks for every escalated call, and for a
//!   `write` or `read` call only when the run's approval policy names that
//!   category.
//! * **Unattended.** In a run with no person (a schedule or a webhook),
//!   `browser_request_user` fails at once with `no_person`. Asks follow the
//!   run's `on_approval_required` in Main, as every other ask does. A grant's
//!   `browser_submits` answers a submit ask for its hosts and nothing else.
//! * **Egress.** The session's only route out is the egress proxy with the
//!   run's token. A host the proxy refuses is the contract's `egress_refused`
//!   result, and the family records it in the run's egress ledger, so the
//!   refusal is in the run's egress summary and its step record.
//! * **Time.** Every call is admitted and settled through the gateway's
//!   metered API (provider `browser`, operation `engine_seconds`, unit
//!   `second`), and counted against the run's cap and the project's monthly
//!   quota ([`budget`]).
//! * **Images.** A screenshot reaches the model as an image result
//!   ([`crate::image_result`]).
//!
//! A toolset built without a host refuses every call
//! (`browser_unavailable`), so a host that forgets to lend a session fails
//! closed. How to change the family: `README.md` beside this file.

pub mod budget;
pub mod category;
// Only the Office catalogue test (`toolkit-office`) reads it.
#[cfg(all(test, feature = "toolkit-office"))]
pub(crate) mod catalogue;
pub mod session;
#[cfg(test)]
mod tests;

use std::collections::BTreeSet;
use std::fmt;
use std::sync::{Arc, Mutex};

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use base64::Engine as _;
use elitea_browser_tools::{ErrorCode, Snapshot, TOOLS, ToolKind, ToolSpec, frame, spec};
use serde_json::{Map, Value, json};

use crate::approval_policy::ApprovalPolicy;
use crate::image_result::{IMAGE_FIELD, IMAGE_KIND};
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::web::meter::{
    AuditTarget, Meter, MeterError, MeteredOp, Outcome, SettleRequest, Units, event_id,
    spawn_settle,
};
pub use budget::{BrowserBudget, BrowserLimits};
pub use category::{Classified, Escalation, classify};
pub use session::{BrowserSession, SessionAnswer, SessionError, SessionImage};

/// The toolkit type.
pub const TOOLKIT_TYPE: &str = "browser";
/// The metered provider and operation of a call's engine seconds.
pub const METERED_PROVIDER: &str = "browser";
pub const METERED_OPERATION: &str = "engine_seconds";
/// The engine a cloud session runs (ADR-0049 decision 2): only `full` is
/// admitted until the bake-off admits `lite`.
pub const ENGINE_FULL: &str = "full";
/// The longest screenshot the family passes on (the supervisor's own bound
/// is the same order).
const MAX_IMAGE_BYTES: usize = 8 * 1024 * 1024;
/// The media types a screenshot may have.
const IMAGE_MEDIA_TYPES: [&str; 3] = ["image/png", "image/jpeg", "image/webp"];
/// The most hosts a grant's `browser_submits` names.
pub const MAX_SUBMIT_HOSTS: usize = 32;

/// Who started the run, as far as the browser rules need to know.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RunOrigin {
    /// A person started it and may answer a question or an ask.
    Person,
    /// A schedule or a webhook started it: nobody can answer
    /// (ADR-0045 decision 2).
    Unattended,
}

/// What the host lends the family for one run.
#[derive(Clone)]
pub struct BrowserToolHost {
    session: Arc<dyn BrowserSession>,
    meter: Arc<dyn Meter>,
    run: Arc<BrowserRun>,
}

impl BrowserToolHost {
    #[must_use]
    pub fn new(
        session: Arc<dyn BrowserSession>,
        meter: Arc<dyn Meter>,
        run: Arc<BrowserRun>,
    ) -> Self {
        Self {
            session,
            meter,
            run,
        }
    }

    /// The run's shared browser state: its gate and budget.
    #[must_use]
    pub const fn run(&self) -> &Arc<BrowserRun> {
        &self.run
    }

    /// The run's session.
    #[must_use]
    pub const fn session(&self) -> &Arc<dyn BrowserSession> {
        &self.session
    }
}

impl fmt::Debug for BrowserToolHost {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BrowserToolHost")
            .finish_non_exhaustive()
    }
}

/// The run's browser state every call shares: the latest snapshot (for
/// the classifier), the origin, the grant's submit hosts, the approval
/// policy and the budget.
pub struct BrowserRun {
    origin: RunOrigin,
    submit_hosts: BTreeSet<String>,
    approval: Arc<ApprovalPolicy>,
    budget: Arc<BrowserBudget>,
    page: Mutex<Option<Snapshot>>,
    /// The model-visible names of this run's browser tools, for the gate.
    names: Mutex<BTreeSet<String>>,
}

impl BrowserRun {
    #[must_use]
    pub fn new(
        origin: RunOrigin,
        submit_hosts: impl IntoIterator<Item = String>,
        approval: Arc<ApprovalPolicy>,
        budget: Arc<BrowserBudget>,
    ) -> Arc<Self> {
        Arc::new(Self {
            origin,
            submit_hosts: submit_hosts
                .into_iter()
                .map(|host| host.trim().trim_end_matches('.').to_ascii_lowercase())
                .filter(|host| !host.is_empty())
                .take(MAX_SUBMIT_HOSTS)
                .collect(),
            approval,
            budget,
            page: Mutex::new(None),
            names: Mutex::new(BTreeSet::new()),
        })
    }

    #[must_use]
    pub const fn origin(&self) -> RunOrigin {
        self.origin
    }

    #[must_use]
    pub const fn budget(&self) -> &Arc<BrowserBudget> {
        &self.budget
    }

    /// Bind the model-visible name of one browser tool (after the run's
    /// tool binding may have renamed it) to its contract name.
    pub fn bind_name(&self, exposed: &str) {
        if let Ok(mut names) = self.names.lock() {
            names.insert(exposed.to_owned());
        }
    }

    /// Whether `exposed` is one of this run's browser tools: its bound name,
    /// or that name behind a routing prefix (`toolkit___browser_click`).
    fn is_browser_tool(&self, exposed: &str) -> bool {
        let bare = exposed.rsplit("___").next().unwrap_or(exposed);
        self.names
            .lock()
            .is_ok_and(|names| names.contains(exposed) || names.contains(bare))
    }

    /// The category of a call against the latest snapshot.
    #[must_use]
    pub fn classify(&self, name: &str, arguments: &Value) -> Classified {
        let page = self.page.lock().ok();
        classify(
            name,
            arguments,
            page.as_ref().and_then(|page| page.as_ref()),
        )
    }

    /// The host of the latest snapshot's page, lower case.
    #[must_use]
    pub fn page_host(&self) -> Option<String> {
        let page = self.page.lock().ok()?;
        let url = page.as_ref()?.url.clone();
        reqwest::Url::parse(&url)
            .ok()?
            .host_str()
            .map(|host| host.trim_end_matches('.').to_ascii_lowercase())
    }

    /// Whether the run's grant answers this call's ask: a submit on a host
    /// the grant's `browser_submits` names.
    #[must_use]
    pub fn granted(&self, classified: &Classified) -> bool {
        classified.grantable()
            && self
                .page_host()
                .is_some_and(|host| self.submit_hosts.contains(&host))
    }

    /// The argument-aware gate (ADR-0048 decision 8, ADR-0049 decision 3):
    /// an escalated call asks unless the grant answers it; a plain call asks
    /// only when the approval policy names its category. `tool_name` is the
    /// model-visible name; a name that is not a browser tool of this run
    /// answers `false` (another gate decides).
    #[must_use]
    pub fn requires_confirmation(&self, tool_name: &str, arguments: &Value) -> bool {
        if !self.is_browser_tool(tool_name) {
            return false;
        }
        let Some(contract) = contract_name(tool_name) else {
            return false;
        };
        let classified = self.classify(contract, arguments);
        if classified.escalation.is_some() {
            return !self.granted(&classified);
        }
        self.approval.rule_for(classified.category).is_some()
    }

    fn install(&self, snapshot: Option<Snapshot>) {
        if let Ok(mut page) = self.page.lock() {
            *page = snapshot;
        }
    }
}

impl fmt::Debug for BrowserRun {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BrowserRun")
            .field("origin", &self.origin)
            .finish_non_exhaustive()
    }
}

/// The contract name inside a model-visible name: the name itself, or the
/// part after a routing prefix (`toolkit___browser_click`).
#[must_use]
pub fn contract_name(exposed: &str) -> Option<&'static str> {
    let bare = exposed.rsplit("___").next().unwrap_or(exposed);
    spec::find(bare).map(|spec| spec.name)
}

/// A browser toolset could not be built from its settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BrowserToolsetError {
    InvalidConfiguration,
    UnsupportedSelection,
    InvalidDefinition,
}

struct BrowserTool {
    spec: &'static ToolSpec,
    description: String,
    host: Option<BrowserToolHost>,
}

#[async_trait]
impl Tool for BrowserTool {
    fn name(&self) -> &str {
        self.spec.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.spec.kind == ToolKind::BrowserRead
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some((self.spec.parameters)())
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let Some(host) = &self.host else {
            return Ok(error_result(
                "browser_unavailable",
                "the cloud browser is not available on this host",
            ));
        };
        Ok(run_call(host, self.spec, context.function_call_id(), &arguments).await)
    }
}

/// Run one call: the unattended rule, the budget, the meter, the session,
/// and the result's shaping.
pub(crate) async fn run_call(
    host: &BrowserToolHost,
    spec: &'static ToolSpec,
    call_id: &str,
    arguments: &Value,
) -> Value {
    let run = &host.run;
    if spec.kind == ToolKind::Question && run.origin == RunOrigin::Unattended {
        return elitea_browser_tools::BrowserError::new(
            ErrorCode::NoPerson,
            "this run has no person to ask (a schedule or a webhook started it); continue without asking, or stop",
        )
        .to_result();
    }
    if let Err(cap) = run.budget.admit() {
        // The run ends with the cap's sentence; the model reads why.
        return error_result(
            cap.error_code(),
            "the run reached its cloud browser time limit; the call does not run",
        );
    }
    let meter = &host.meter;
    let op = MeteredOp {
        provider: METERED_PROVIDER,
        operation: METERED_OPERATION,
        estimate: Units::seconds(1),
    };
    let admitted_at = tokio::time::Instant::now();
    let admission = match meter.admit(&op).await {
        Ok(admission) => admission,
        Err(MeterError::Refused(refusal)) => {
            return json!({
                "status": "error",
                "code": refusal.public_code.clone().unwrap_or_else(|| refusal.code.clone()),
                "message": "the platform refused the cloud browser time for this call (a budget or a rate limit); the browser was not used",
            });
        }
        Err(MeterError::Unpriced) => {
            return error_result(
                "browser_unpriced",
                "cloud browser time has no price yet; ask an administrator to set one in the Tools pricing tab. The browser was not used.",
            );
        }
        Err(MeterError::Unavailable) => {
            return error_result(
                "metering_unavailable",
                "the platform could not admit this call right now; the browser was not used",
            );
        }
    };
    let started = std::time::Instant::now();
    let answer = host.session.call(call_id, spec.name, arguments).await;
    let seconds = run.budget.charge(started.elapsed());
    let (outcome, failure) = match &answer {
        Ok(_) => (Outcome::Ok, None),
        Err(_) => (
            Outcome::ProviderError,
            Some(super::web::meter::FailureClass::Failed),
        ),
    };
    let invocation = super::web::capability::nonce();
    drop(spawn_settle(
        Arc::clone(meter),
        SettleRequest {
            admission_id: admission.admission_id,
            event_id: event_id(meter.attribution_execution_id(), call_id, &invocation),
            op,
            units: Units::seconds(seconds),
            outcome,
            failure,
            audit: AuditTarget {
                tool: spec.name,
                target: run.page_host().unwrap_or_default(),
            },
        },
        admitted_at,
    ));
    match answer {
        Ok(answer) => shape(run, spec, answer),
        Err(error) => error_result(session_code(error), &error.to_string()),
    }
}

const fn session_code(error: SessionError) -> &'static str {
    match error {
        SessionError::Capacity => "browser_capacity",
        SessionError::Ended => "browser_session_ended",
        SessionError::Refused => "browser_refused",
        SessionError::Unavailable => "browser_unavailable",
    }
}

/// Keep the snapshot for the classifier, record an egress refusal, and turn
/// a screenshot into an image result.
fn shape(run: &BrowserRun, spec: &ToolSpec, answer: SessionAnswer) -> Value {
    let SessionAnswer { mut result, image } = answer;
    if !result.is_object() {
        return error_result(
            "browser_invalid_result",
            "the cloud browser answered no result",
        );
    }
    let ok = result.get("status").and_then(Value::as_str) == Some("ok");
    if ok {
        if spec.name == spec::BROWSER_SNAPSHOT && result.get("snapshot").is_some() {
            let snapshot = result
                .get("content")
                .and_then(Value::as_str)
                .and_then(frame::unframe)
                .and_then(|(inner, _)| Snapshot::parse(inner).ok());
            run.install(snapshot);
        } else if spec.kind != ToolKind::BrowserRead && spec.kind != ToolKind::Question {
            // A navigation or an act may change the page: its refs are
            // stale, and so is the classifier's view of it.
            run.install(None);
        }
    } else if result.get("code").and_then(Value::as_str) == Some(ErrorCode::EgressRefused.as_str())
    {
        record_egress_refusal(&result);
    }
    match image {
        Some(image) if ok => image_result(result, &image),
        _ => {
            if let Some(object) = result.as_object_mut() {
                object.remove(IMAGE_FIELD);
            }
            result
        }
    }
}

/// An `egress_refused` result names the refused host in `host` (and `port`)
/// when the session agent knows it: the run's egress ledger records it, as
/// a refused toolkit request is recorded.
fn record_egress_refusal(result: &Value) {
    let host = result
        .get("host")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let port = result
        .get("port")
        .and_then(Value::as_u64)
        .and_then(|port| u16::try_from(port).ok())
        .unwrap_or(443);
    crate::egress::current().record_refused(host, port);
}

fn image_result(mut result: Value, image: &SessionImage) -> Value {
    if !IMAGE_MEDIA_TYPES.contains(&image.media_type.as_str())
        || image.data.is_empty()
        || image.data.len() > MAX_IMAGE_BYTES
    {
        return error_result(
            "browser_invalid_image",
            "the cloud browser answered an unusable image",
        );
    }
    if let Some(object) = result.as_object_mut() {
        object.insert("kind".to_owned(), json!(IMAGE_KIND));
        object.insert("mime".to_owned(), json!(image.media_type));
        object.insert("bytes".to_owned(), json!(image.data.len()));
        object.insert(
            IMAGE_FIELD.to_owned(),
            json!(base64::engine::general_purpose::STANDARD.encode(&image.data)),
        );
    }
    result
}

fn error_result(code: &str, message: &str) -> Value {
    json!({"status": "error", "code": code, "message": message})
}

/// The toolkit's settings: `selected_tools` only. Every other key is
/// refused, so a later Main cannot set a switch this worker would ignore.
fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<String>, BrowserToolsetError> {
    if settings.keys().any(|key| key != "selected_tools") {
        return Err(BrowserToolsetError::InvalidConfiguration);
    }
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value
        .as_array()
        .ok_or(BrowserToolsetError::InvalidConfiguration)?;
    if values.len() > TOOLS.len() {
        return Err(BrowserToolsetError::InvalidConfiguration);
    }
    let mut out = Vec::with_capacity(values.len());
    for value in values {
        let name = value
            .as_str()
            .ok_or(BrowserToolsetError::InvalidConfiguration)?;
        if spec::find(name).is_none() {
            return Err(BrowserToolsetError::UnsupportedSelection);
        }
        if !out.iter().any(|existing: &String| existing == name) {
            out.push(name.to_owned());
        }
    }
    Ok(out)
}

/// Build the `browser` toolset. Without a host every call answers
/// `browser_unavailable`.
pub(crate) fn build_browser_toolset(
    toolkit_name: &str,
    settings: &Map<String, Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    host: Option<&BrowserToolHost>,
) -> Result<BasicToolset, BrowserToolsetError> {
    let selected = selected_tools(settings)?;
    let tools: Vec<Arc<dyn Tool>> = TOOLS
        .iter()
        .filter(|spec| selected.is_empty() || selected.iter().any(|name| name == spec.name))
        .map(|spec| {
            if let Some(host) = host {
                host.run.bind_name(spec.name);
            }
            Arc::new(BrowserTool {
                spec,
                description: format!("Toolkit: {toolkit_name}\n{}", spec.description),
                host: host.cloned(),
            }) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, TOOLKIT_TYPE, policy, tools)
        .map_err(|_| BrowserToolsetError::InvalidDefinition)
}

/// Every tool the family serves (`materialize::omitted_selection`).
pub(crate) fn browser_tool_catalog() -> Vec<(&'static str, bool)> {
    TOOLS.iter().map(|spec| (spec.name, false)).collect()
}
