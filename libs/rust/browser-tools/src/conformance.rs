//! The conformance kit (ADR-0048 verification 1, ADR-0049 verification 2).
//!
//! Both drivers run the same kit in their own tests, so "the same tool
//! sequence gives the same results" is a test each of them passes rather
//! than a hope:
//!
//! * [`FIXTURES`] — the fixture pages (forms, ARIA widgets, an iframe, shadow
//!   DOM, password and card fields), each with its golden snapshot tree
//!   (`conformance/fixtures/*.html`, `conformance/golden/*.snapshot`). A
//!   driver's test serves the HTML over loopback `http` (the test policy
//!   allows the private network) and says where with [`Config::url_for`].
//! * [`run`] — the parity run: every fixture opened and snapshotted against
//!   its golden, its text and screenshot read, then the scripted steps
//!   (typing, a secure field refused, check, select, submit and wait, tabs,
//!   disclosure, refs going stale after a reload, a refused scheme). Steps
//!   find their targets by role and name in the snapshot they just took,
//!   never by a fixed `eN`. It returns a [`Report`]; a driver's test asserts
//!   [`Report::failures`] is empty.
//! * [`check_exposed`] — a host's registered tools against [`crate::TOOLS`].
//!
//! The drivers compute their trees differently (Chromium's accessibility
//! tree, the webview driver's DOM and ARIA walk), so a golden may not hold
//! for one of them on an edge case. Such a case is a [`Difference`] in that
//! driver's [`Config`], with its own expected tree and a reason; an unlisted
//! difference is a failure.
//!
//! A driver maps its engine's roles onto ARIA role names (`textbox`,
//! `combobox`, `iframe`, `paragraph`, …) and text runs onto `text`, and
//! inlines frame and shadow-root content in place (a slot's assigned nodes
//! where the slot is). [`crate::snapshot::Snapshot::build`] does the rest.

use std::fmt;

use serde_json::{Value, json};

use crate::driver::{BrowserDriver, ToolOutput, dispatch};
use crate::error::ErrorCode;
use crate::frame;
use crate::snapshot::{Node, Snapshot, State};
use crate::spec::{self, TOOLS};

/// One fixture page and its golden snapshot tree.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Fixture {
    /// The file stem: `forms` is `forms.html` and `forms.snapshot`.
    pub name: &'static str,
    pub title: &'static str,
    pub html: &'static str,
    /// The node lines of its first snapshot ([`Snapshot::render_tree`]).
    pub golden: &'static str,
}

macro_rules! fixture {
    ($name:literal, $title:literal) => {
        Fixture {
            name: $name,
            title: $title,
            html: include_str!(concat!("../conformance/fixtures/", $name, ".html")),
            golden: include_str!(concat!("../conformance/golden/", $name, ".snapshot")),
        }
    };
}

/// Every fixture, in the order [`run`] visits them.
pub const FIXTURES: &[Fixture] = &[
    fixture!("forms", "Forms"),
    fixture!("aria-widgets", "ARIA widgets"),
    fixture!("iframes", "Frames"),
    fixture!("shadow-dom", "Shadow DOM"),
    fixture!("secure-fields", "Checkout"),
];

/// Values on the fixtures that no result may ever contain: they sit in
/// password and card fields.
pub const SECRET_VALUES: &[&str] = &["hunter2", "s3cret-new", "4111111111111111", "737"];

/// A driver's listed departure from a golden.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Difference {
    pub fixture: &'static str,
    /// The tree this driver gives instead.
    pub tree: String,
    /// Why, for the reviewer (the engine behaviour behind it).
    pub reason: &'static str,
}

/// How a driver runs the kit.
pub struct Config<'a> {
    /// The `http(s)` URL the fixture is served at.
    pub url_for: &'a dyn Fn(&Fixture) -> String,
    pub differences: Vec<Difference>,
}

/// One check of the run.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Check {
    pub fixture: &'static str,
    pub step: String,
    /// `None` when it passed; else what went wrong.
    pub failure: Option<String>,
}

/// What [`run`] found.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Report {
    pub checks: Vec<Check>,
}

impl Report {
    /// The checks that failed.
    #[must_use]
    pub fn failures(&self) -> Vec<&Check> {
        self.checks
            .iter()
            .filter(|check| check.failure.is_some())
            .collect()
    }

    fn record(&mut self, fixture: &'static str, step: &str, outcome: Result<(), String>) {
        self.checks.push(Check {
            fixture,
            step: step.to_owned(),
            failure: outcome.err(),
        });
    }
}

impl fmt::Display for Report {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        for check in &self.checks {
            match &check.failure {
                None => writeln!(formatter, "ok   {} / {}", check.fixture, check.step)?,
                Some(why) => writeln!(formatter, "FAIL {} / {}: {why}", check.fixture, check.step)?,
            }
        }
        Ok(())
    }
}

/// Compare a host's registered tools with the contract: the same names in
/// the same order, each with the contract's description and schema.
///
/// `exposed` is what the host hands its runtime, as `(name, description,
/// parameters)`.
///
/// # Errors
///
/// Every mismatch, one line each.
pub fn check_exposed(exposed: &[(String, String, Value)]) -> Result<(), Vec<String>> {
    let mut problems = Vec::new();
    let names: Vec<&str> = exposed.iter().map(|(name, ..)| name.as_str()).collect();
    let expected: Vec<&str> = TOOLS.iter().map(|spec| spec.name).collect();
    if names != expected {
        problems.push(format!("tools {names:?}, expected {expected:?}"));
    }
    for (name, description, parameters) in exposed {
        let Some(spec) = spec::find(name) else {
            continue;
        };
        if description != spec.description {
            problems.push(format!("{name}: the description differs"));
        }
        if *parameters != (spec.parameters)() {
            problems.push(format!("{name}: the schema differs"));
        }
    }
    if problems.is_empty() {
        Ok(())
    } else {
        Err(problems)
    }
}

/// The parity run (see the module docs).
pub async fn run(driver: &dyn BrowserDriver, config: &Config<'_>) -> Report {
    let mut report = Report::default();
    for fixture in FIXTURES {
        let url = (config.url_for)(fixture);
        let opened = call(driver, spec::BROWSER_OPEN, &json!({ "url": url })).await;
        let outcome = opened.as_ref().map(|_| ()).map_err(Clone::clone);
        report.record(fixture.name, "open", outcome);
        if opened.is_err() {
            continue;
        }
        let expected = config
            .differences
            .iter()
            .find(|difference| difference.fixture == fixture.name)
            .map_or(fixture.golden, |difference| difference.tree.as_str());
        let first = snapshot(driver).await;
        report.record(
            fixture.name,
            "snapshot matches the golden",
            first
                .as_ref()
                .map_err(Clone::clone)
                .and_then(|(snapshot, _)| same_tree(&snapshot.render_tree(), expected)),
        );
        report.record(
            fixture.name,
            "no secure value in the snapshot",
            first
                .as_ref()
                .map_err(Clone::clone)
                .and_then(|(_, raw)| no_secrets(raw)),
        );
        report.record(fixture.name, "text mode", text_mode(driver, fixture).await);
        report.record(fixture.name, "screenshot", screenshot(driver).await);
        match fixture.name {
            "forms" => forms(driver, &mut report).await,
            "aria-widgets" => widgets(driver, &mut report).await,
            "secure-fields" => secure_fields(driver, &mut report).await,
            _ => {}
        }
    }
    staleness(driver, config, &mut report).await;
    let refused = call(
        driver,
        spec::BROWSER_OPEN,
        &json!({ "url": "file:///etc/passwd" }),
    )
    .await;
    report.record(
        "forms",
        "file: is scheme_refused",
        expect_code(&refused, ErrorCode::SchemeRefused),
    );
    report
}

type CallResult = Result<Value, String>;

/// Run one call; `Err` carries a failed result's JSON, `Ok` a success's.
///
/// A `rate_limited` act is retried once after the rate window: the kit acts
/// as fast as the driver answers, and the limit is not what it measures.
async fn call(driver: &dyn BrowserDriver, name: &str, arguments: &Value) -> CallResult {
    let mut result = dispatch(driver, name, arguments).await.result().clone();
    if result["code"] == ErrorCode::RateLimited.as_str() {
        sleep(std::time::Duration::from_millis(1100)).await;
        result = dispatch(driver, name, arguments).await.result().clone();
    }
    if result["status"] == "ok" {
        Ok(result)
    } else {
        Err(result.to_string())
    }
}

/// Wait `duration` without blocking the executor. The crate links no
/// runtime, so the timer is a thread that wakes the task when it is done.
async fn sleep(duration: std::time::Duration) {
    use std::sync::{Arc, Mutex};
    use std::task::{Poll, Waker};

    #[derive(Default)]
    struct Shared {
        done: bool,
        waker: Option<Waker>,
    }
    let shared = Arc::new(Mutex::new(Shared::default()));
    let timer = Arc::clone(&shared);
    std::thread::spawn(move || {
        std::thread::sleep(duration);
        let waker = timer.lock().ok().and_then(|mut state| {
            state.done = true;
            state.waker.take()
        });
        if let Some(waker) = waker {
            waker.wake();
        }
    });
    std::future::poll_fn(|context| match shared.lock() {
        Ok(mut state) if !state.done => {
            state.waker = Some(context.waker().clone());
            Poll::Pending
        }
        // Done, or the timer thread panicked holding the lock: stop waiting.
        _ => Poll::Ready(()),
    })
    .await;
}

fn expect_code(result: &CallResult, code: ErrorCode) -> Result<(), String> {
    match result {
        Ok(result) => Err(format!("expected {code}, got {result}")),
        Err(failure) => {
            let parsed: Value = serde_json::from_str(failure).unwrap_or(Value::Null);
            if parsed["code"] == code.as_str() {
                Ok(())
            } else {
                Err(format!("expected {code}, got {failure}"))
            }
        }
    }
}

/// A tree snapshot, parsed back from its framed content, and the raw result.
async fn snapshot(driver: &dyn BrowserDriver) -> Result<(Snapshot, String), String> {
    let result = call(driver, spec::BROWSER_SNAPSHOT, &json!({})).await?;
    let raw = result.to_string();
    let content = result["content"].as_str().unwrap_or_default();
    let (inner, nonce) = frame::unframe(content).ok_or("content is not one nonce frame")?;
    // The nonce is random hex and may contain a secret's digits ("737").
    let raw = raw.replace(nonce, "");
    let snapshot = Snapshot::parse(inner).map_err(|error| format!("not the grammar: {error}"))?;
    if result["snapshot"] != snapshot.id.as_str() {
        return Err("the snapshot field is not the rendered id".to_owned());
    }
    if result["refs"] != snapshot.ref_count() {
        return Err("the refs field is not the rendered ref count".to_owned());
    }
    Ok((snapshot, raw))
}

fn same_tree(actual: &str, expected: &str) -> Result<(), String> {
    if actual.trim_end() == expected.trim_end() {
        Ok(())
    } else {
        Err(format!("got:\n{actual}expected:\n{expected}"))
    }
}

fn no_secrets(raw: &str) -> Result<(), String> {
    match SECRET_VALUES.iter().find(|secret| raw.contains(*secret)) {
        Some(secret) => Err(format!("{secret:?} is in the result")),
        None => Ok(()),
    }
}

async fn text_mode(driver: &dyn BrowserDriver, fixture: &Fixture) -> Result<(), String> {
    let result = call(driver, spec::BROWSER_SNAPSHOT, &json!({ "mode": "text" })).await?;
    let content = result["content"].as_str().unwrap_or_default();
    let (inner, nonce) = frame::unframe(content).ok_or("content is not one nonce frame")?;
    // The nonce is random hex and may contain a secret's digits ("737").
    no_secrets(&result.to_string().replace(nonce, ""))?;
    let title = format!("# title {}", Value::String(fixture.title.to_owned()));
    if !inner.lines().any(|line| line == title) {
        return Err(format!("no {title:?} line"));
    }
    Ok(())
}

async fn screenshot(driver: &dyn BrowserDriver) -> Result<(), String> {
    match dispatch(driver, spec::BROWSER_SCREENSHOT, &json!({})).await {
        ToolOutput::Image { data, .. } if !data.is_empty() => Ok(()),
        other => Err(format!("no image: {}", other.result())),
    }
}

/// The ref of the node with this role and name, pinned to its snapshot.
fn ref_of(snapshot: &Snapshot, role: &str, name: &str) -> Result<String, String> {
    snapshot
        .find(role, name)
        .and_then(|node| node.ref_index)
        .map(|index| format!("@{}:e{index}", snapshot.id))
        .ok_or_else(|| format!("no {role} {name:?} with a ref"))
}

fn node<'a>(snapshot: &'a Snapshot, role: &str, name: &str) -> Result<&'a Node, String> {
    snapshot
        .find(role, name)
        .ok_or_else(|| format!("no {role} {name:?}"))
}

/// Act on `role "name"` in a fresh snapshot, then check a fresh snapshot.
async fn act_then(
    driver: &dyn BrowserDriver,
    tool: &str,
    (role, name): (&str, &str),
    mut arguments: Value,
    check: impl FnOnce(&Snapshot) -> Result<(), String>,
) -> Result<(), String> {
    let (before, _) = snapshot(driver).await?;
    arguments["ref"] = Value::String(ref_of(&before, role, name)?);
    call(driver, tool, &arguments).await?;
    let (after, _) = snapshot(driver).await?;
    check(&after)
}

fn has_state(node: &Node, state: State) -> Result<(), String> {
    if node.states.contains(&state) {
        Ok(())
    } else {
        Err(format!("{} {:?} is not [{state}]", node.role, node.name))
    }
}

async fn forms(driver: &dyn BrowserDriver, report: &mut Report) {
    let typed = act_then(
        driver,
        spec::BROWSER_TYPE,
        ("textbox", "Email"),
        json!({ "text": "ada@example.test" }),
        |after| {
            let value = node(after, "textbox", "Email")?.value.clone();
            if value.as_deref() == Some("ada@example.test") {
                Ok(())
            } else {
                Err(format!("Email holds {value:?}"))
            }
        },
    )
    .await;
    report.record("forms", "type into a textbox", typed);

    let refused = match snapshot(driver).await {
        Ok((before, _)) => match ref_of(&before, "textbox", "Password") {
            Ok(target) => expect_code(
                &call(
                    driver,
                    spec::BROWSER_TYPE,
                    &json!({ "ref": target, "text": "hunter2" }),
                )
                .await,
                ErrorCode::SecureFieldRefused,
            ),
            Err(why) => Err(why),
        },
        Err(why) => Err(why),
    };
    report.record("forms", "password is secure_field_refused", refused);

    let checked = act_then(
        driver,
        spec::BROWSER_CLICK,
        ("checkbox", "Remember me"),
        json!({}),
        |after| has_state(node(after, "checkbox", "Remember me")?, State::Checked),
    )
    .await;
    report.record("forms", "click a checkbox", checked);

    let selected = act_then(
        driver,
        spec::BROWSER_SELECT,
        ("combobox", "Country"),
        json!({ "values": ["Canada"] }),
        |after| {
            let value = node(after, "combobox", "Country")?.value.clone();
            if value.as_deref() == Some("Canada") {
                Ok(())
            } else {
                Err(format!("Country holds {value:?}"))
            }
        },
    )
    .await;
    report.record("forms", "select an option", selected);

    let submitted = async {
        let (before, _) = snapshot(driver).await?;
        let target = ref_of(&before, "button", "Sign in")?;
        call(driver, spec::BROWSER_CLICK, &json!({ "ref": target })).await?;
        call(
            driver,
            spec::BROWSER_WAIT,
            &json!({ "text": "Status: submitted", "timeout_ms": 5000 }),
        )
        .await?;
        Ok(())
    }
    .await;
    report.record("forms", "submit and wait for text", submitted);

    let timed_out = call(
        driver,
        spec::BROWSER_WAIT,
        &json!({ "text": "never on this page", "timeout_ms": 200 }),
    )
    .await;
    report.record(
        "forms",
        "wait times out",
        expect_code(&timed_out, ErrorCode::Timeout),
    );
}

async fn widgets(driver: &dyn BrowserDriver, report: &mut Report) {
    let tab = act_then(
        driver,
        spec::BROWSER_CLICK,
        ("tab", "Billing"),
        json!({}),
        |after| {
            has_state(node(after, "tab", "Billing")?, State::Selected)?;
            node(after, "tabpanel", "Billing").map(|_| ())
        },
    )
    .await;
    report.record("aria-widgets", "select a tab", tab);

    let disclosure = act_then(
        driver,
        spec::BROWSER_CLICK,
        ("button", "More options"),
        json!({}),
        |after| {
            has_state(node(after, "button", "More options")?, State::Expanded)?;
            ref_of(after, "link", "Advanced").map(|_| ())
        },
    )
    .await;
    report.record("aria-widgets", "expand a disclosure", disclosure);
}

async fn secure_fields(driver: &dyn BrowserDriver, report: &mut Report) {
    for name in ["Card number", "Security code", "New password"] {
        let refused = match snapshot(driver).await {
            Ok((before, _)) => match ref_of(&before, "textbox", name) {
                Ok(target) => expect_code(
                    &call(
                        driver,
                        spec::BROWSER_TYPE,
                        &json!({ "ref": target, "text": "1" }),
                    )
                    .await,
                    ErrorCode::SecureFieldRefused,
                ),
                Err(why) => Err(why),
            },
            Err(why) => Err(why),
        };
        report.record(
            "secure-fields",
            &format!("{name} is secure_field_refused"),
            refused,
        );
    }
}

async fn staleness(driver: &dyn BrowserDriver, config: &Config<'_>, report: &mut Report) {
    let Some(forms) = FIXTURES.first() else {
        return;
    };
    let outcome = async {
        call(
            driver,
            spec::BROWSER_OPEN,
            &json!({ "url": (config.url_for)(forms) }),
        )
        .await?;
        let (before, _) = snapshot(driver).await?;
        let pinned = ref_of(&before, "button", "Sign in")?;
        let latest = pinned.rsplit(':').next().unwrap_or_default().to_owned();
        let reloaded = call(driver, spec::BROWSER_RELOAD, &json!({})).await?;
        if reloaded["refs_stale"] != true {
            return Err("reload did not report refs_stale".to_owned());
        }
        expect_code(
            &call(driver, spec::BROWSER_CLICK, &json!({ "ref": latest })).await,
            ErrorCode::StaleRef,
        )?;
        snapshot(driver).await?;
        expect_code(
            &call(driver, spec::BROWSER_CLICK, &json!({ "ref": pinned })).await,
            ErrorCode::StaleRef,
        )
    }
    .await;
    report.record("forms", "refs go stale after a reload", outcome);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn sleep_waits_without_blocking() {
        let started = std::time::Instant::now();
        let ticker = async {
            // Runs on the same current-thread executor while the sleep waits.
            tokio::task::yield_now().await;
            std::time::Instant::now()
        };
        let (yielded_at, ()) = tokio::join!(ticker, sleep(std::time::Duration::from_millis(50)));
        assert!(yielded_at.duration_since(started) < std::time::Duration::from_millis(50));
        assert!(started.elapsed() >= std::time::Duration::from_millis(50));
    }

    #[test]
    fn exposed_tools_must_be_the_contract() {
        let exact: Vec<_> = TOOLS
            .iter()
            .map(|spec| {
                (
                    spec.name.to_owned(),
                    spec.description.to_owned(),
                    (spec.parameters)(),
                )
            })
            .collect();
        check_exposed(&exact).unwrap();
        let mut changed = exact.clone();
        changed[0].2 = json!({ "type": "object" });
        changed.pop();
        let problems = check_exposed(&changed).unwrap_err();
        assert_eq!(problems.len(), 2, "{problems:?}");
    }
}
