//! The `computer` rows of the approvals table (ADR-0047 decisions 3, 4
//! and 8): one test per row, plan mode, the asks that are never
//! remembered, the policy ceiling with the platform deny list, and the
//! unattended rule.

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::{Value, json};

use super::{
    APPROVE_ALWAYS, APPROVE_THREAD, ChoiceStore, ComputerControl, ComputerTarget, MemoryChoices,
    PAYLOAD_KEY, PermissionProfile, RuleApprovals, RulesEngine, Source, ToolCall, ToolKind,
    Unattended, Verdict, WorkspaceSettings, is_consequential,
};
use crate::policy::LocalWorkPolicy;
use crate::workspace::Workspace;

const MAIL: &str = "com.apple.mail";

fn policy() -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        computer_use: true,
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

fn target(bundle_id: &str) -> ComputerTarget {
    ComputerTarget {
        bundle_id: Some(bundle_id.to_owned()),
        app_name: Some("Mail".to_owned()),
        window_title: Some("Inbox".to_owned()),
        ..ComputerTarget::default()
    }
}

fn see(bundle_id: &str) -> ToolCall {
    ToolCall {
        computer: Some(ComputerTarget {
            action: Some("snapshot".to_owned()),
            ..target(bundle_id)
        }),
        ..ToolCall::new(ToolKind::ComputerRead)
    }
}

fn act(bundle_id: &str, control: &str) -> ToolCall {
    ToolCall {
        computer: Some(ComputerTarget {
            action: Some("press".to_owned()),
            control: Some(ComputerControl {
                role: Some("AXButton".to_owned()),
                name: Some(control.to_owned()),
                ..ComputerControl::default()
            }),
            ..target(bundle_id)
        }),
        ..ToolCall::new(ToolKind::ComputerAct)
    }
}

fn open(bundle_id: &str) -> ToolCall {
    ToolCall {
        computer: Some(ComputerTarget {
            action: Some("open".to_owned()),
            open_app: true,
            ..target(bundle_id)
        }),
        ..ToolCall::new(ToolKind::ComputerAct)
    }
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

fn request(call: &ToolCall) -> ApprovalRequest {
    ApprovalRequest {
        subject: "call-1".to_owned(),
        message: "computer".to_owned(),
        available_actions: vec!["approve".to_owned(), "reject".to_owned()],
        payload: json!({ PAYLOAD_KEY: call }),
    }
}

fn action_of(outcome: &ApprovalOutcome) -> &str {
    match outcome {
        ApprovalOutcome::Decided { action, .. } => action,
        ApprovalOutcome::Deferred => "deferred",
    }
}

async fn ask(channel: &RuleApprovals, call: &ToolCall) -> ApprovalOutcome {
    channel.request(request(call)).await.expect("answered")
}

#[test]
fn the_tool_kinds_and_plan_mode_keep_only_seeing() {
    assert!(ToolKind::ComputerRead.is_read_only());
    assert!(ToolKind::ComputerRead.plan_allows());
    assert!(!ToolKind::ComputerAct.is_read_only());
    assert!(!ToolKind::ComputerAct.plan_allows());
    assert!(ToolKind::ComputerRead.is_computer() && ToolKind::ComputerAct.is_computer());
    let wire: ToolKind = serde_json::from_value(json!("computer_act")).expect("wire name");
    assert_eq!(wire, ToolKind::ComputerAct);

    let fx = fixture(policy());
    fx.engine.set_profile(PermissionProfile::Plan);
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Ask);
    assert!(!fx.engine.refused_before_hooks(&see(MAIL)));
    assert_eq!(
        verdict(&fx.engine, &act(MAIL, "Reply")),
        (Verdict::Deny, Source::PlanMode)
    );
    assert_eq!(
        verdict(&fx.engine, &open(MAIL)),
        (Verdict::Deny, Source::PlanMode)
    );
    assert!(fx.engine.refused_before_hooks(&act(MAIL, "Reply")));
    // An app the person approved for seeing stays visible in plan mode.
    fx.engine.remember_for_thread(&see(MAIL)).expect("thread");
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Allow);
}

/// Policy ceiling: off is off, the platform deny list is merged and beats
/// the allow list, an action names its app.
#[test]
fn the_policy_is_the_ceiling_of_every_computer_call() {
    let off = fixture(LocalWorkPolicy {
        computer_use: false,
        ..policy()
    });
    assert_eq!(
        verdict(&off.engine, &see(MAIL)),
        (Verdict::Deny, Source::Policy)
    );
    assert!(off.engine.refused_before_hooks(&see(MAIL)));

    let fx = fixture(LocalWorkPolicy {
        computer_use_apps_allow: vec![MAIL.to_owned(), "com.apple.Terminal".to_owned()],
        computer_use_apps_deny: vec!["com.apple.Notes".to_owned()],
        ..policy()
    });
    for denied in [
        "com.apple.Terminal",
        "ai.elitea.desktop",
        "com.1password.1password",
        "com.apple.notes",
        "com.apple.Safari",
    ] {
        assert_eq!(
            verdict(&fx.engine, &see(denied)),
            (Verdict::Deny, Source::Policy),
            "{denied}"
        );
        assert_eq!(
            verdict(&fx.engine, &act(denied, "OK")),
            (Verdict::Deny, Source::Policy),
            "{denied}"
        );
    }
    // A remembered "always" does not lift a deny either.
    fx.choices
        .remember(super::RememberedChoice {
            tool: ToolKind::ComputerRead,
            command: None,
            paths: None,
            glob: None,
            sandbox: None,
            network: false,
            runner: None,
            loopback: false,
            bundle_id: Some("com.apple.terminal".to_owned()),
        })
        .expect("remember");
    assert_eq!(
        verdict(&fx.engine, &see("com.apple.Terminal")).0,
        Verdict::Deny
    );
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Ask);
    // A security pane of System Settings is denied; the app is not.
    let open = fixture(policy());
    let settings = |pane: Option<&str>| ToolCall {
        computer: Some(ComputerTarget {
            pane: pane.map(str::to_owned),
            ..target("com.apple.systempreferences")
        }),
        ..ToolCall::new(ToolKind::ComputerRead)
    };
    // With no pane it fails closed: a security pane could be on screen.
    assert_eq!(
        verdict(&open.engine, &settings(None)),
        (Verdict::Deny, Source::Policy)
    );
    assert_eq!(
        verdict(
            &open.engine,
            &settings(Some("com.apple.Displays-Settings.extension"))
        )
        .0,
        Verdict::Ask
    );
    assert_eq!(
        verdict(
            &open.engine,
            &settings(Some("com.apple.settings.PrivacySecurity.extension"))
        ),
        (Verdict::Deny, Source::Policy)
    );
}

/// `computer_apps` names no app and is allowed; every other call must name
/// one: a display or region screenshot, a wait, an action.
#[test]
fn only_listing_the_apps_names_no_application() {
    let open = fixture(policy());
    let list_apps = ToolCall {
        computer: Some(ComputerTarget {
            list_apps: true,
            ..ComputerTarget::default()
        }),
        ..ToolCall::new(ToolKind::ComputerRead)
    };
    assert_eq!(
        verdict(&open.engine, &list_apps),
        (Verdict::Allow, Source::Default)
    );
    let no_app = |tool: ToolKind, action: Option<&str>, list_apps: bool| ToolCall {
        computer: Some(ComputerTarget {
            action: action.map(str::to_owned),
            list_apps,
            ..ComputerTarget::default()
        }),
        ..ToolCall::new(tool)
    };
    for call in [
        ToolCall::new(ToolKind::ComputerRead),
        ToolCall::new(ToolKind::ComputerAct),
        no_app(ToolKind::ComputerRead, Some("screenshot display"), false),
        no_app(ToolKind::ComputerRead, Some("screenshot region"), false),
        no_app(ToolKind::ComputerRead, Some("wait"), false),
        no_app(ToolKind::ComputerAct, Some("click_at"), false),
        // `list_apps` on an action is no listing.
        no_app(ToolKind::ComputerAct, None, true),
    ] {
        let decision = open.engine.decide(&call);
        assert_eq!(
            (decision.verdict, decision.source),
            (Verdict::Deny, Source::Policy),
            "{call:?}"
        );
        assert!(
            decision.reason.contains("names its application"),
            "{}",
            decision.reason
        );
        assert!(open.engine.refused_before_hooks(&call));
    }
}

/// With no thread set, nothing is approved "for this thread": it is not
/// offered, cannot be remembered, and an opened app is asked every time.
#[tokio::test]
async fn no_thread_means_no_thread_approvals() {
    let fx = fixture(policy());
    fx.engine.set_thread(None);
    assert!(fx.engine.remember_for_thread(&see(MAIL)).is_err());
    assert!(fx.engine.remember_for_thread(&open(MAIL)).is_err());

    let script = Script::new(&[Some(APPROVE_THREAD), Some("approve"), Some("approve")]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    // A forged "allow for this thread" is approved once, never kept.
    assert_eq!(action_of(&ask(&channel, &see(MAIL)).await), "approve");
    assert_eq!(
        script.seen()[0].available_actions,
        ["approve", APPROVE_ALWAYS, "reject"]
    );
    assert_eq!(
        verdict(&fx.engine, &see(MAIL)),
        (Verdict::Ask, Source::Default)
    );
    // An opened app is asked again.
    assert_eq!(action_of(&ask(&channel, &open(MAIL)).await), "approve");
    assert_eq!(verdict(&fx.engine, &open(MAIL)).0, Verdict::Ask);
    assert_eq!(action_of(&ask(&channel, &open(MAIL)).await), "approve");
    assert_eq!(script.seen().len(), 3);

    // Setting a thread afterwards does not see a session-wide approval.
    fx.engine.set_thread(Some("thread-a".to_owned()));
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Ask);
    assert_eq!(verdict(&fx.engine, &open(MAIL)).0, Verdict::Ask);
}

/// Row 1 and 2: seeing an app not yet approved is asked, with both
/// remember scopes; "allow for this thread" answers this thread only, and
/// "always for this app" every thread, by bundle id.
#[tokio::test]
async fn seeing_an_app_is_asked_once_per_thread_or_always() {
    let fx = fixture(policy());
    let script = Script::new(&[Some(APPROVE_THREAD), Some(APPROVE_ALWAYS)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());

    let first = ask(&channel, &see(MAIL)).await;
    assert_eq!(action_of(&first), "approve");
    let seen = script.seen();
    assert_eq!(seen.len(), 1);
    assert_eq!(
        seen[0].available_actions,
        ["approve", APPROVE_THREAD, APPROVE_ALWAYS, "reject"]
    );
    let reason = seen[0].payload["reason"].as_str().expect("reason");
    assert!(reason.contains("see Mail (com.apple.mail)"), "{reason}");
    assert!(
        reason.contains("Inbox"),
        "the ask names the window: {reason}"
    );

    // The same thread: allowed, from the thread's answer.
    assert_eq!(
        verdict(&fx.engine, &see(MAIL)),
        (Verdict::Allow, Source::Remembered)
    );
    // Seeing is not acting.
    assert_eq!(verdict(&fx.engine, &act(MAIL, "Reply")).0, Verdict::Ask);
    // Another thread asks again; its answer is "always for this app".
    fx.engine.set_thread(Some("thread-b".to_owned()));
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Ask);
    ask(&channel, &see(MAIL)).await;
    assert_eq!(script.seen().len(), 2);
    let stored = fx.choices.list();
    assert_eq!(stored.len(), 1);
    assert_eq!(stored[0].tool, ToolKind::ComputerRead);
    assert_eq!(stored[0].bundle_id.as_deref(), Some(MAIL));
    fx.engine.set_thread(Some("thread-c".to_owned()));
    assert_eq!(
        verdict(&fx.engine, &see("COM.APPLE.MAIL")),
        (Verdict::Allow, Source::Remembered),
        "approved by bundle id, ignoring case"
    );
    // A window title is no key: another app titled "Inbox" is asked.
    assert_eq!(
        verdict(&fx.engine, &see("com.example.mail")).0,
        Verdict::Ask
    );
    // Forgetting the thread drops its answers, not "always".
    fx.engine.set_thread(Some("thread-a".to_owned()));
    fx.engine.forget_thread("thread-a");
    assert_eq!(
        verdict(&fx.engine, &see(MAIL)).1,
        Source::Remembered,
        "still always"
    );
}

/// Row 3 and 4: acting is asked separately from seeing, with the same two
/// scopes; once approved it is allowed.
#[tokio::test]
async fn acting_is_asked_apart_from_seeing_with_the_same_scopes() {
    let fx = fixture(policy());
    let script = Script::new(&[Some(APPROVE_ALWAYS), Some(APPROVE_THREAD)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    fx.engine.remember_for_thread(&see(MAIL)).expect("seeing");

    assert_eq!(verdict(&fx.engine, &act(MAIL, "Reply")).0, Verdict::Ask);
    let answered = ask(&channel, &act(MAIL, "Reply")).await;
    assert_eq!(action_of(&answered), "approve");
    let seen = script.seen();
    assert_eq!(
        seen[0].available_actions,
        ["approve", APPROVE_THREAD, APPROVE_ALWAYS, "reject"]
    );
    let reason = seen[0].payload["reason"].as_str().expect("reason");
    assert!(reason.contains("use Mail (com.apple.mail)"), "{reason}");
    assert!(
        reason.contains("`press`") && reason.contains("Reply"),
        "{reason}"
    );
    assert_eq!(
        verdict(&fx.engine, &act(MAIL, "Archive")),
        (Verdict::Allow, Source::Remembered)
    );
    let stored = fx.choices.list();
    assert_eq!(stored.len(), 1);
    assert_eq!(stored[0].tool, ToolKind::ComputerAct);
    // "Always use" is not "always see".
    fx.engine.set_thread(Some("thread-b".to_owned()));
    assert_eq!(verdict(&fx.engine, &see(MAIL)).0, Verdict::Ask);
    assert_eq!(
        verdict(&fx.engine, &act(MAIL, "Archive")).1,
        Source::Remembered
    );

    // The thread scope for acting, on another app.
    let notes = "com.apple.Notes";
    ask(&channel, &act(notes, "Bold")).await;
    assert_eq!(
        verdict(&fx.engine, &act(notes, "Italic")),
        (Verdict::Allow, Source::Remembered)
    );
    fx.engine.set_thread(Some("thread-c".to_owned()));
    assert_eq!(verdict(&fx.engine, &act(notes, "Italic")).0, Verdict::Ask);
}

/// Row 5: `computer_open_app` is asked once per bundle id per thread; a
/// plain approval is kept for the thread and nothing is written.
#[tokio::test]
async fn opening_an_app_is_asked_once_per_bundle_id_per_thread() {
    let fx = fixture(policy());
    let script = Script::new(&[Some("approve"), Some("approve")]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());

    assert_eq!(verdict(&fx.engine, &open(MAIL)).0, Verdict::Ask);
    assert!(!fx.engine.can_remember(&open(MAIL)));
    let answered = ask(&channel, &open(MAIL)).await;
    assert_eq!(action_of(&answered), "approve");
    assert_eq!(
        script.seen()[0].available_actions,
        ["approve", "reject"],
        "no remember scope: the approval is the thread's"
    );
    assert_eq!(
        verdict(&fx.engine, &open(MAIL)),
        (Verdict::Allow, Source::Remembered)
    );
    assert!(fx.choices.list().is_empty(), "nothing is written");
    // Another app, and another thread, ask again.
    assert_eq!(
        verdict(&fx.engine, &open("com.apple.Notes")).0,
        Verdict::Ask
    );
    fx.engine.set_thread(Some("thread-b".to_owned()));
    assert_eq!(verdict(&fx.engine, &open(MAIL)).0, Verdict::Ask);
    // Opening is not using: acting is still asked.
    fx.engine.set_thread(Some("thread-a".to_owned()));
    assert_eq!(verdict(&fx.engine, &act(MAIL, "Reply")).0, Verdict::Ask);
    // A rejected open is not kept.
    let rejecting = Script::new(&[Some("reject")]);
    let channel = RuleApprovals::new(fx.engine.clone(), rejecting);
    ask(&channel, &open("com.apple.Notes")).await;
    assert_eq!(
        verdict(&fx.engine, &open("com.apple.Notes")).0,
        Verdict::Ask
    );
}

/// Row 6: a consequential action is asked every time, on an approved app,
/// in bypass mode too, and is never remembered.
#[tokio::test]
async fn consequential_actions_are_asked_every_time_and_never_remembered() {
    let fx = fixture(policy());
    fx.engine
        .remember_for_thread(&act(MAIL, "Reply"))
        .expect("thread");
    fx.choices
        .remember(super::RememberedChoice {
            tool: ToolKind::ComputerAct,
            command: None,
            paths: None,
            glob: None,
            sandbox: None,
            network: false,
            runner: None,
            loopback: false,
            bundle_id: Some(MAIL.to_owned()),
        })
        .expect("always");
    assert_eq!(verdict(&fx.engine, &act(MAIL, "Reply")).0, Verdict::Allow);

    let send = act(MAIL, "Send");
    assert_eq!(verdict(&fx.engine, &send), (Verdict::Ask, Source::Default));
    assert!(!fx.engine.can_remember(&send));
    assert!(fx.engine.remember(&send, None).is_err());
    assert!(fx.engine.remember_for_thread(&send).is_err());

    // The person picks a scope anyway (a stale card): approved once, and
    // asked again next time.
    let script = Script::new(&[Some(APPROVE_ALWAYS), Some(APPROVE_THREAD)]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    ask(&channel, &send).await;
    ask(&channel, &send).await;
    let seen = script.seen();
    assert_eq!(seen.len(), 2, "asked every time");
    assert_eq!(seen[0].available_actions, ["approve", "reject"]);
    let reason = seen[0].payload["reason"].as_str().expect("reason");
    assert!(reason.contains("send, delete"), "{reason}");
    assert_eq!(fx.choices.list().len(), 1, "nothing more remembered");
    assert_eq!(verdict(&fx.engine, &send).0, Verdict::Ask);

    // A destructive role is consequential whatever its name.
    let destructive = ToolCall {
        computer: Some(ComputerTarget {
            control: Some(ComputerControl {
                role: Some("AXMenuItem".to_owned()),
                name: Some("Clear History…".to_owned()),
                destructive: true,
                ..ComputerControl::default()
            }),
            ..target(MAIL)
        }),
        ..ToolCall::new(ToolKind::ComputerAct)
    };
    assert_eq!(verdict(&fx.engine, &destructive).0, Verdict::Ask);

    // Bypass mode does not answer a computer ask.
    fx.engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(verdict(&fx.engine, &send), (Verdict::Ask, Source::Default));
    assert_eq!(verdict(&fx.engine, &see("com.apple.Notes")).0, Verdict::Ask);
}

#[test]
fn the_consequential_matcher_reads_names_and_roles() {
    let named = |name: &str| ComputerControl {
        role: Some("AXButton".to_owned()),
        name: Some(name.to_owned()),
        ..ComputerControl::default()
    };
    for name in [
        "Send",
        "send",
        "SEND NOW",
        "Delete",
        "Delete…",
        "Move to Trash",
        "Buy",
        "Buy with Apple Pay",
        "Pay $10.00",
        "Sign",
        "Sign In",
        "Submit",
        "Submit order",
        "Place Order",
        "Purchase",
        "Proceed to checkout",
        "Transfer funds",
        "Erase Disk",
        "Remove account",
        "Publish",
        "Send\u{a0}reply",
        "SendNow",
        "sendNow",
        "QuickSend",
        "URLSend",
        "Confirm payment",
        "Confirm",
        "Approve request",
        "Authorize",
        "Authorise app",
        "Accept invitation",
        "Post",
        "Share",
        "Book now",
        "Subscribe",
        "Withdraw",
        "Wipe Device",
        "Reset",
        "Format",
        "PlaceOrder",
        "Place order",
    ] {
        assert!(is_consequential(&named(name)), "{name}");
    }
    for name in [
        "Reply",
        "Sender",
        "Signature",
        "Payment methods",
        "Deleted Items",
        "Buyer guide",
        "Submitted",
        "Archive",
        "Cancel",
        "Sendmail",
        "Posted",
        "Booking history",
        "Formatting",
        "Resetting…",
        "",
    ] {
        assert!(!is_consequential(&named(name)), "{name}");
    }
    assert!(is_consequential(&ComputerControl {
        subrole: Some("AXDestructiveButton".to_owned()),
        ..ComputerControl::default()
    }));
    assert!(is_consequential(&ComputerControl {
        destructive: true,
        ..ComputerControl::default()
    }));
    assert!(!is_consequential(&ComputerControl::default()));
}

/// Unattended (ADR-0047 decision 8, ADR-0045): no grant answers a
/// `ComputerAct` ask, a hook's included. It waits for the person, and
/// anything but an approval declines it. Seeing an approved app, and an
/// act on an app approved before the run, still apply.
#[tokio::test]
async fn unattended_turns_never_answer_acting_from_a_grant() {
    let fx = fixture(policy());
    fx.engine
        .set_unattended(Some(Unattended { hook_asks: true }));

    // Nobody at the machine: the host defers (its wait ran out), and the
    // act is declined.
    let script = Script::new(&[None]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    let declined = ask(&channel, &act(MAIL, "Reply")).await;
    assert_eq!(action_of(&declined), "reject");
    let ApprovalOutcome::Decided { value, .. } = &declined else {
        unreachable!()
    };
    assert!(
        value["reason"]
            .as_str()
            .is_some_and(|reason| reason.contains("person at the machine")),
        "{value}"
    );
    assert_eq!(
        script.seen()[0].payload["attended_only"],
        Value::Bool(true),
        "the host sees it waits for a person"
    );
    // The host's timeout answer is a decline too.
    let timed_out = Script::new(&[Some("timeout")]);
    let channel = RuleApprovals::new(fx.engine.clone(), timed_out);
    assert_eq!(
        action_of(&ask(&channel, &act(MAIL, "Reply")).await),
        "reject"
    );

    // A hook's ask of an allowed act is not the grant's to answer.
    fx.engine
        .remember_for_thread(&act(MAIL, "Reply"))
        .expect("approved");
    let script = Script::new(&[None]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    let mut hooked = request(&act(MAIL, "Reply"));
    hooked.payload[super::HOOK_ASK_PAYLOAD_KEY] = json!({ "hook": "guard" });
    let outcome = channel.request(hooked).await.expect("answered");
    let seen = script.seen();
    assert_eq!(seen.len(), 1, "the person was asked, not the grant");
    // Unattended, the act is asked by the default row already (the
    // thread's approval does not answer it); the hook's ask changes nothing.
    assert_eq!(seen[0].payload["source"], "default");
    assert_eq!(seen[0].payload["attended_only"], Value::Bool(true));
    assert_eq!(action_of(&outcome), "reject", "unanswered, it declines");

    // No approval made before the run answers an act: not the thread's
    // (above), not "always for this app", not an opened app. Each waits
    // for the person, and declines unanswered.
    fx.choices
        .remember(super::RememberedChoice {
            tool: ToolKind::ComputerAct,
            command: None,
            paths: None,
            glob: None,
            sandbox: None,
            network: false,
            runner: None,
            loopback: false,
            bundle_id: Some(MAIL.to_owned()),
        })
        .expect("remember");
    fx.engine.remember_for_thread(&open(MAIL)).expect("opened");
    for call in [act(MAIL, "Reply"), open(MAIL)] {
        assert_eq!(verdict(&fx.engine, &call), (Verdict::Ask, Source::Default));
        let script = Script::new(&[None]);
        let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
        assert_eq!(action_of(&ask(&channel, &call).await), "reject");
        assert_eq!(script.seen().len(), 1, "the person was asked");
        assert_eq!(script.seen()[0].payload["attended_only"], Value::Bool(true));
    }
    // Attended again, the same approvals answer it.
    fx.engine.set_unattended(None);
    assert_eq!(
        verdict(&fx.engine, &act(MAIL, "Reply")),
        (Verdict::Allow, Source::Remembered)
    );
    fx.engine
        .set_unattended(Some(Unattended { hook_asks: true }));

    // Seeing an app approved before the run still applies (decision 8
    // limits acting only).
    fx.engine.remember_for_thread(&see(MAIL)).expect("approved");
    assert_eq!(
        verdict(&fx.engine, &see(MAIL)),
        (Verdict::Allow, Source::Remembered)
    );

    // A person who is there answers, and the answer stands.
    let script = Script::new(&[Some("approve")]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(
        action_of(&ask(&channel, &act("com.apple.Notes", "Bold")).await),
        "approve"
    );

    // A read ask is not attended-only: a deferral stays the host's.
    let script = Script::new(&[None]);
    let channel = RuleApprovals::new(fx.engine.clone(), script.clone());
    assert_eq!(
        action_of(&ask(&channel, &see("com.apple.Notes")).await),
        "deferred"
    );
    assert!(script.seen()[0].payload.get("attended_only").is_none());
}

/// A choice file written by an older host has no `bundle_id`; one written
/// now round-trips it.
#[test]
fn remembered_app_choices_round_trip() {
    let dir = tempfile::tempdir().expect("dir");
    let path = dir.path().join("choices.json");
    let store = super::JsonFileChoices::open(path.clone()).expect("open");
    store
        .remember(super::RememberedChoice {
            tool: ToolKind::ComputerAct,
            command: None,
            paths: None,
            glob: None,
            sandbox: None,
            network: false,
            runner: None,
            loopback: false,
            bundle_id: Some(MAIL.to_owned()),
        })
        .expect("remember");
    let reopened = super::JsonFileChoices::open(path).expect("reopen");
    assert_eq!(reopened.list()[0].bundle_id.as_deref(), Some(MAIL));
    let old: super::RememberedChoice =
        serde_json::from_value(json!({"tool": "write_file", "paths": ["a"]})).expect("old");
    assert_eq!(old.bundle_id, None);
}
