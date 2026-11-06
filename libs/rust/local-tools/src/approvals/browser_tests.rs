//! The browser rows of the approvals table (ADR-0048 decision 8): one test
//! per row, plan mode, the policy ceiling with the domain lists, "allow for
//! this site in this thread", the asks never remembered, and the unattended
//! grant's `browser_submits` (ADR-0045).

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::{Value, json};

use super::{
    APPROVE_ALWAYS, APPROVE_THREAD, BrowserEffect, BrowserTarget, ChoiceStore, ComputerControl,
    MemoryChoices, PAYLOAD_KEY, PermissionProfile, RuleApprovals, RulesEngine, Source, ToolCall,
    ToolKind, Unattended, Verdict, WorkspaceSettings,
};
use crate::policy::LocalWorkPolicy;
use crate::workspace::Workspace;

const SHOP: &str = "shop.example.com";

fn policy() -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        browser: true,
        browser_agent: true,
        ..LocalWorkPolicy::default()
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    choices: Arc<MemoryChoices>,
    engine: Arc<RulesEngine>,
}

fn fixture(policy: LocalWorkPolicy) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
    let choices = Arc::new(MemoryChoices::default());
    let engine = RulesEngine::new(
        policy,
        &workspace,
        WorkspaceSettings::default(),
        choices.clone(),
    )
    .expect("engine");
    engine.set_thread(Some("thread-a".to_owned()));
    Fixture {
        _dir: dir,
        choices,
        engine: Arc::new(engine),
    }
}

fn call(tool: ToolKind, name: &str, host: &str) -> ToolCall {
    ToolCall {
        browser: Some(BrowserTarget {
            tool: name.to_owned(),
            host: Some(host.to_owned()),
            url: Some(format!("https://{host}/cart")),
            ..BrowserTarget::default()
        }),
        ..ToolCall::new(tool)
    }
}

fn read(host: &str) -> ToolCall {
    call(ToolKind::BrowserRead, "browser_snapshot", host)
}

fn navigate(host: &str) -> ToolCall {
    call(ToolKind::BrowserNavigate, "browser_open", host)
}

fn click(host: &str, name: &str, effect: Option<BrowserEffect>) -> ToolCall {
    let mut call = call(ToolKind::BrowserAct, "browser_click", host);
    if let Some(target) = &mut call.browser {
        target.control = Some(ComputerControl {
            role: Some("button".to_owned()),
            name: Some(name.to_owned()),
            ..ComputerControl::default()
        });
        target.effect = effect;
    }
    call
}

fn submit(host: &str) -> ToolCall {
    click(host, "Continue", Some(BrowserEffect::Submit))
}

fn verdict(engine: &RulesEngine, call: &ToolCall) -> (Verdict, Source) {
    let decision = engine.decide(call);
    (decision.verdict, decision.source)
}

/// A prompt that answers each question with the next scripted action
/// (`None`: deferred, the host could not ask) and records what it saw.
struct Script {
    answers: Mutex<Vec<Option<&'static str>>>,
    seen: Mutex<Vec<ApprovalRequest>>,
}

impl Script {
    fn new(answers: &[Option<&'static str>]) -> Arc<Self> {
        Arc::new(Self {
            answers: Mutex::new(answers.iter().rev().copied().collect()),
            seen: Mutex::new(Vec::new()),
        })
    }

    fn seen(&self) -> Vec<ApprovalRequest> {
        self.seen.lock().expect("lock").clone()
    }
}

#[async_trait]
impl ApprovalChannel for Script {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.seen.lock().expect("lock").push(request);
        let next = self.answers.lock().expect("lock").pop().flatten();
        Ok(next.map_or(ApprovalOutcome::Deferred, |action| {
            ApprovalOutcome::Decided {
                action: action.to_owned(),
                value: json!({}),
            }
        }))
    }
}

fn action_of(outcome: &ApprovalOutcome) -> &str {
    match outcome {
        ApprovalOutcome::Decided { action, .. } => action,
        ApprovalOutcome::Deferred => "deferred",
    }
}

async fn ask(channel: &RuleApprovals, call: &ToolCall) -> ApprovalOutcome {
    channel
        .request(ApprovalRequest {
            subject: "call-1".to_owned(),
            message: "browser".to_owned(),
            available_actions: vec!["approve".to_owned(), "reject".to_owned()],
            payload: json!({ PAYLOAD_KEY: call }),
        })
        .await
        .expect("answered")
}

#[test]
fn the_tool_kinds_are_browser_tools_and_only_reading_is_read_only() {
    for kind in [
        ToolKind::BrowserRead,
        ToolKind::BrowserNavigate,
        ToolKind::BrowserAct,
    ] {
        assert!(kind.is_browser(), "{kind:?}");
        assert!(!kind.is_computer() && !kind.is_web(), "{kind:?}");
    }
    assert!(ToolKind::BrowserRead.plan_allows());
    assert!(!ToolKind::BrowserNavigate.plan_allows());
    assert!(!ToolKind::BrowserAct.plan_allows());
    // The target survives the payload round trip the channel relies on.
    let call = submit(SHOP);
    let back: ToolCall =
        serde_json::from_value(serde_json::to_value(&call).expect("ser")).expect("de");
    assert_eq!(back, call);
}

/// The ceiling: `browser` and `browser_agent` both on, and local work;
/// a denied or not-allowed host is refused whatever else allows it.
#[test]
fn the_policy_is_the_ceiling_of_every_browser_call() {
    for off in [
        LocalWorkPolicy {
            browser: false,
            ..policy()
        },
        LocalWorkPolicy {
            browser_agent: false,
            ..policy()
        },
        LocalWorkPolicy {
            allowed: false,
            ..policy()
        },
    ] {
        let fx = fixture(off);
        for call in [read(SHOP), navigate(SHOP), submit(SHOP)] {
            assert_eq!(verdict(&fx.engine, &call), (Verdict::Deny, Source::Policy));
        }
    }
    let fx = fixture(LocalWorkPolicy {
        browser_allow_domains: vec!["example.com".to_owned()],
        browser_deny_domains: vec!["*.ads.example.com".to_owned()],
        ..policy()
    });
    assert_eq!(verdict(&fx.engine, &navigate(SHOP)).0, Verdict::Allow);
    assert_eq!(
        verdict(&fx.engine, &navigate("example.org")),
        (Verdict::Deny, Source::Policy)
    );
    assert_eq!(
        verdict(&fx.engine, &read("x.ads.example.com")),
        (Verdict::Deny, Source::Policy)
    );
    // Bypass mode does not lift the ceiling.
    fx.engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(
        verdict(&fx.engine, &navigate("example.org")),
        (Verdict::Deny, Source::Policy)
    );
}

/// Rows 1-3: reading, navigating and a plain act are allowed.
#[test]
fn reading_navigating_and_plain_acts_are_allowed() {
    let fx = fixture(policy());
    for call in [
        read(SHOP),
        navigate(SHOP),
        click(SHOP, "Next page", None),
        call(ToolKind::BrowserAct, "browser_scroll", SHOP),
    ] {
        assert_eq!(
            verdict(&fx.engine, &call),
            (Verdict::Allow, Source::Default),
            "{call:?}"
        );
    }
    // A history step names no host: still allowed.
    let back = ToolCall {
        browser: Some(BrowserTarget {
            tool: "browser_back".to_owned(),
            ..BrowserTarget::default()
        }),
        ..ToolCall::new(ToolKind::BrowserNavigate)
    };
    assert_eq!(verdict(&fx.engine, &back).0, Verdict::Allow);
}

/// Row 6: plan mode denies acting and navigating, keeps reading.
#[test]
fn plan_mode_keeps_only_reading() {
    let fx = fixture(policy());
    fx.engine.set_profile(PermissionProfile::Plan);
    assert_eq!(verdict(&fx.engine, &read(SHOP)).0, Verdict::Allow);
    for call in [
        navigate(SHOP),
        click(SHOP, "Next page", None),
        submit(SHOP),
        click(SHOP, "Buy now", None),
    ] {
        assert_eq!(
            verdict(&fx.engine, &call),
            (Verdict::Deny, Source::PlanMode),
            "{call:?}"
        );
    }
}

/// Row 4: a submit, an upload or a download is asked, and "allow for this
/// site in this thread" answers the next one on the same host in the same
/// thread only. "Always" is never offered.
#[tokio::test]
async fn submits_uploads_and_downloads_are_asked_and_allowed_per_site_and_thread() {
    let fx = fixture(policy());
    for effect in [
        BrowserEffect::Submit,
        BrowserEffect::Upload,
        BrowserEffect::Download,
    ] {
        let call = click(SHOP, "Continue", Some(effect));
        assert_eq!(
            verdict(&fx.engine, &call),
            (Verdict::Ask, Source::Default),
            "{effect:?}"
        );
        assert!(fx.engine.can_remember(&call));
    }
    let reason = fx.engine.decide(&submit(SHOP)).reason;
    assert!(
        reason.contains("submit a form") && reason.contains(SHOP),
        "{reason}"
    );
    assert!(
        fx.engine.remember(&submit(SHOP), None).is_err(),
        "never always"
    );

    let script = Script::new(&[Some(APPROVE_THREAD)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(action_of(&ask(&channel, &submit(SHOP)).await), "approve");
    assert_eq!(
        script.seen()[0].available_actions,
        ["approve", APPROVE_THREAD, "reject"]
    );
    assert!(fx.choices.list().is_empty(), "nothing in the choice store");

    // The same host, any of the three effects, in this thread.
    assert_eq!(
        verdict(&fx.engine, &submit(SHOP)),
        (Verdict::Allow, Source::Remembered)
    );
    assert_eq!(
        verdict(
            &fx.engine,
            &click("SHOP.example.com.", "Attach", Some(BrowserEffect::Upload))
        )
        .0,
        Verdict::Allow,
        "the host is compared ignoring case and a final dot"
    );
    // Another host, even a subdomain, is asked.
    assert_eq!(
        verdict(&fx.engine, &submit("pay.shop.example.com")).0,
        Verdict::Ask
    );
    // Another thread is asked; forgetting the thread forgets the site.
    fx.engine.set_thread(Some("thread-b".to_owned()));
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);
    fx.engine.set_thread(Some("thread-a".to_owned()));
    fx.engine.forget_thread("thread-a");
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);

    // A stale "always" answer is approved once.
    let script = Script::new(&[Some(APPROVE_ALWAYS)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script);
    assert_eq!(action_of(&ask(&channel, &submit(SHOP)).await), "approve");
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);

    // Bypass mode allows a submit.
    fx.engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(
        verdict(&fx.engine, &submit(SHOP)),
        (Verdict::Allow, Source::Bypass)
    );
}

/// With no thread set, the site is approved once: the scope is not offered.
#[tokio::test]
async fn no_thread_means_no_site_approval() {
    let fx = fixture(policy());
    fx.engine.set_thread(None);
    let script = Script::new(&[Some(APPROVE_THREAD)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(action_of(&ask(&channel, &submit(SHOP)).await), "approve");
    assert_eq!(script.seen()[0].available_actions, ["approve", "reject"]);
    assert!(fx.engine.remember_for_thread(&submit(SHOP)).is_err());
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);
}

/// Row 5: a buy, pay, delete, send or sign control is asked every time,
/// on an approved site and in bypass mode too, and is never remembered.
#[tokio::test]
async fn consequential_controls_are_asked_every_time_and_never_remembered() {
    let fx = fixture(policy());
    fx.engine
        .remember_for_thread(&submit(SHOP))
        .expect("site approved");
    for name in [
        "Buy now",
        "Pay £20",
        "Delete account",
        "Send",
        "Sign document",
    ] {
        for effect in [None, Some(BrowserEffect::Submit)] {
            let call = click(SHOP, name, effect);
            assert_eq!(
                verdict(&fx.engine, &call),
                (Verdict::Ask, Source::Default),
                "{name} {effect:?}"
            );
            assert!(!fx.engine.can_remember(&call), "{name}");
            assert!(fx.engine.remember_for_thread(&call).is_err(), "{name}");
        }
    }
    let buy = click(SHOP, "Buy now", Some(BrowserEffect::Submit));
    let script = Script::new(&[Some(APPROVE_THREAD), Some("approve")]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(action_of(&ask(&channel, &buy).await), "approve");
    assert_eq!(action_of(&ask(&channel, &buy).await), "approve");
    let seen = script.seen();
    assert_eq!(seen.len(), 2, "asked every time");
    assert_eq!(seen[0].available_actions, ["approve", "reject"]);
    let reason = seen[0].payload["reason"].as_str().expect("reason");
    assert!(
        reason.contains("buy, pay, delete, send or sign") && reason.contains("Buy now"),
        "{reason}"
    );

    fx.engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(verdict(&fx.engine, &buy), (Verdict::Ask, Source::Default));
}

/// Unattended (ADR-0045): `browser_submits` answers a submit or an upload
/// on a host it names; a download, a consequential control and every other
/// host wait for the person and decline past the wait.
#[tokio::test]
async fn an_unattended_grant_answers_submits_on_its_hosts_only() {
    let fx = fixture(policy());
    fx.engine.set_unattended(Some(Unattended {
        hook_asks: false,
        browser_submits: vec![SHOP.to_owned()],
    }));
    for effect in [BrowserEffect::Submit, BrowserEffect::Upload] {
        let call = click(SHOP, "Continue", Some(effect));
        let decision = fx.engine.decide(&call);
        assert_eq!(
            (decision.verdict, decision.source),
            (Verdict::Allow, Source::Grant),
            "{effect:?}"
        );
        assert!(decision.reason.contains(SHOP), "{}", decision.reason);
    }
    let script = Script::new(&[]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(action_of(&ask(&channel, &submit(SHOP)).await), "approve");
    assert!(script.seen().is_empty(), "the grant answered");

    for call in [
        click(SHOP, "Save file", Some(BrowserEffect::Download)),
        click(SHOP, "Place order", Some(BrowserEffect::Submit)),
        click(SHOP, "Pay", None),
        submit("pay.shop.example.com"),
        submit("example.org"),
    ] {
        assert_eq!(
            verdict(&fx.engine, &call),
            (Verdict::Ask, Source::Default),
            "{call:?}"
        );
        let script = Script::new(&[None]);
        let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
        let outcome = ask(&channel, &call).await;
        assert_eq!(action_of(&outcome), "reject", "{call:?}");
        assert_eq!(script.seen()[0].payload["attended_only"], Value::Bool(true));
        let ApprovalOutcome::Decided { value, .. } = &outcome else {
            unreachable!()
        };
        assert!(
            value["reason"]
                .as_str()
                .is_some_and(|reason| reason.contains("person at the machine")),
            "{value}"
        );
    }
    // The host's timeout answer is a decline too; a person who is there
    // answers, and the answer stands.
    let script = Script::new(&[Some("timeout"), Some("approve")]);
    let channel = RuleApprovals::new(fx.engine.clone(), script);
    let call = submit("example.org");
    assert_eq!(action_of(&ask(&channel, &call).await), "reject");
    assert_eq!(action_of(&ask(&channel, &call).await), "approve");

    // A grant without the part answers no submit.
    fx.engine.set_unattended(Some(Unattended {
        hook_asks: true,
        ..Unattended::default()
    }));
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);
    // Attended, the grant is gone.
    fx.engine.set_unattended(None);
    assert_eq!(verdict(&fx.engine, &submit(SHOP)).0, Verdict::Ask);
}
