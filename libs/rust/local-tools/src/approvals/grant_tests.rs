//! Rule grants (#3005–#3008): who may hand a command its program's login
//! and named hosts, and when such a rule answers without a question.
//!
//! The threat this guards against: a prompt injection that makes the agent
//! run a write (`gh api -X POST …`) while a read-only rule hands `gh` the
//! person's login. Conditions keep the write out of the rule, forbidden
//! rules still win, the policy still tightens, and only a rule the person
//! wrote grants anything.

use std::sync::{Arc, Mutex};

use super::{
    CommandRules, MemoryChoices, PermissionProfile, RuleSink, RulesEngine, Source, ToolCall,
    ToolKind, Verdict, WorkspaceSettings,
};
use crate::credentials::CommandGrant;
use crate::error::ToolResult;
use crate::execpolicy::{PrefixRule, RuleSet};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::workspace::Workspace;

/// The person's `gh` rules: reads with the login, a read-only `gh api`,
/// GraphQL queries, a draft PR, merges forbidden.
const USER_RULES: &str = r#"{"version":1,"rules":[
  {"pattern":["gh",["pr","issue","run"],["list","view","diff","checks","status"]],
   "credential":"gh",
   "justification":"GitHub reads.",
   "match":["gh pr list -R o/r","gh run view 1 --log-failed"],
   "not_match":["gh pr merge 1"]},
  {"pattern":["gh","api"],
   "credential":"gh",
   "conditions":{"forbid_flags":["-f","-F","--field","--raw-field","--input","-H","--header"],
                 "flag_values":{"-X":["GET"],"--method":["GET"]}},
   "match":["gh api repos/o/r","gh api -X GET repos/o/r/pulls"],
   "not_match":["gh api -X POST repos/o/r/issues","gh api repos/o/r/issues -f title=x","gh api --method=post repos/o/r/issues"]},
  {"pattern":["gh","api","graphql"],
   "credential":"gh",
   "conditions":{"graphql":"query"},
   "match":["gh api graphql -f query='{ viewer { login } }'"],
   "not_match":["gh api graphql -f query='mutation { a }'"]},
  {"pattern":["gh","pr","create"],
   "credential":"gh",
   "conditions":{"require_flags":["--draft"]},
   "match":["gh pr create --draft --title t"],
   "not_match":["gh pr create --title t"]},
  {"pattern":["gh","pr","merge"],"decision":"forbidden","justification":"Merges go through the review."},
  {"pattern":["curl"],"hosts":["api.example.com"]}
]}"#;

fn policy(network: bool) -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        shell: true,
        network,
        command_deny: vec!["gh repo delete".to_owned()],
        ..LocalWorkPolicy::default()
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    engine: RulesEngine,
}

fn fixture(policy: LocalWorkPolicy, workspace: &str, user: &str) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let opened = Workspace::open(dir.path(), &[]).expect("workspace");
    let engine = RulesEngine::new(
        policy,
        &opened,
        WorkspaceSettings::default(),
        Arc::new(MemoryChoices::default()),
    )
    .expect("engine");
    engine.set_command_rules(CommandRules {
        workspace: RuleSet::parse("workspace", workspace.as_bytes()).expect("workspace rules"),
        user: RuleSet::parse("user", user.as_bytes()).expect("user rules"),
    });
    engine.set_credential_grants(true);
    Fixture { _dir: dir, engine }
}

/// A host that does not run grants (the default) gets none, and the rule's
/// hosts do not cover a network call: it is asked, never run with the
/// whole network.
#[test]
fn a_host_without_grants_grants_nothing() {
    let fixture = fixture(policy(true), NONE, USER_RULES);
    fixture.engine.set_credential_grants(false);
    for command in ["gh pr list", "curl https://api.example.com/x"] {
        let call = run(command, true);
        assert_eq!(fixture.engine.command_grant(&call), None, "{command}");
        assert_eq!(
            fixture.engine.decide(&call).verdict,
            Verdict::Ask,
            "{command}"
        );
    }
}

const NONE: &str = r#"{"version":1}"#;

fn run(command: &str, network: bool) -> ToolCall {
    ToolCall {
        command: Some(command.to_owned()),
        sandbox: Some(SandboxMode::WorkspaceWrite),
        network,
        ..ToolCall::new(ToolKind::RunCommand)
    }
}

fn gh_grant(rule: &str) -> CommandGrant {
    CommandGrant {
        rule: rule.to_owned(),
        credential: Some("gh".to_owned()),
        hosts: ["api.github.com", "github.com", "uploads.github.com"]
            .map(str::to_owned)
            .to_vec(),
    }
}

const SUPPORTED: bool = crate::sandbox::scoped_network_supported();

#[test]
fn a_user_rule_grants_gh_and_answers_without_a_question() {
    let fixture = fixture(policy(true), NONE, USER_RULES);
    let engine = &fixture.engine;
    for (command, rule) in [
        ("gh pr list -R o/r", "user#1"),
        ("gh api repos/o/r/pulls", "user#2"),
        ("gh api -X GET repos/o/r", "user#2"),
        (
            "gh api graphql -f query='query { viewer { login } }'",
            "user#3",
        ),
        ("gh pr create --draft --title t --body-file b.md", "user#4"),
    ] {
        for network in [true, false] {
            let call = run(command, network);
            let decision = engine.decide(&call);
            let grant = engine.command_grant(&call);
            if SUPPORTED {
                assert_eq!(decision.verdict, Verdict::Allow, "{command}: {decision:?}");
                assert_eq!(decision.source, Source::User, "{command}");
                assert!(
                    decision
                        .reason
                        .contains("network limited to api.github.com"),
                    "{}",
                    decision.reason
                );
                assert_eq!(grant, Some(gh_grant(rule)), "{command}");
            } else {
                // No allowlist on this platform: nothing is granted, and a
                // network call falls to the defaults.
                assert_eq!(grant, None, "{command}");
                let want = if network {
                    Verdict::Ask
                } else {
                    Verdict::Allow
                };
                assert_eq!(decision.verdict, want, "{command}");
            }
        }
    }
    // A hosts-only rule: the network to its hosts, no login.
    let curl = engine.command_grant(&run("curl https://api.example.com/x", true));
    if SUPPORTED {
        assert_eq!(
            curl,
            Some(CommandGrant {
                rule: "user#6".to_owned(),
                credential: None,
                hosts: vec!["api.example.com".to_owned()],
            })
        );
    }
}

/// The prompt-injection case: a write under a read-only grant, in Act mode
/// (Bypass off), is never covered by the rule.
#[test]
fn writes_under_a_read_only_grant_are_asked_or_refused() {
    let fixture = fixture(policy(true), NONE, USER_RULES);
    let engine = &fixture.engine;
    assert_eq!(engine.profile(), PermissionProfile::Build);
    for write in [
        "gh api -X POST repos/o/r/issues -f title=x",
        "gh api repos/o/r/issues -f title=x",
        "gh api -XDELETE repos/o/r",
        "gh api --method=PATCH repos/o/r",
        "gh api repos/o/r --input body.json",
        "gh api -H 'X-HTTP-Method-Override: POST' repos/o/r",
        "gh api graphql -f query='mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }'",
        "gh api graphql -F query=@q.graphql",
        "gh pr create --title t",
        "gh pr create --draft=false --title t",
        "gh issue create --title t",
    ] {
        let call = run(write, true);
        assert_eq!(engine.command_grant(&call), None, "{write}");
        assert_eq!(engine.decide(&call).verdict, Verdict::Ask, "{write}");
    }
    // Forbidden wins over every allow, with or without the network.
    for network in [true, false] {
        let call = run("gh pr merge 12 --squash", network);
        let decision = engine.decide(&call);
        assert_eq!(decision.verdict, Verdict::Deny, "{decision:?}");
        assert_eq!(engine.command_grant(&call), None);
    }
    // Forbidden wins even under Bypass.
    engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(
        engine.decide(&run("gh pr merge 1", true)).verdict,
        Verdict::Deny
    );
}

#[test]
fn the_policy_and_the_layers_above_still_tighten() {
    // The organisation's deny list.
    let fixture_on = fixture(
        policy(true),
        NONE,
        r#"{"version":1,"rules":[
      {"pattern":["gh","repo"],"credential":"gh"}]}"#,
    );
    let deleted = run("gh repo delete o/r --yes", true);
    assert_eq!(fixture_on.engine.decide(&deleted).verdict, Verdict::Deny);
    assert_eq!(fixture_on.engine.command_grant(&deleted), None);
    // The policy turns the network off: a network call is denied and no
    // grant gives an offline call the network.
    let offline = fixture(policy(false), NONE, USER_RULES);
    assert_eq!(
        offline.engine.decide(&run("gh pr list", true)).verdict,
        Verdict::Deny
    );
    assert_eq!(
        offline.engine.command_grant(&run("gh pr list", false)),
        None
    );
    // Plan mode.
    let plan = fixture(policy(true), NONE, USER_RULES);
    plan.engine.set_profile(PermissionProfile::Plan);
    assert_eq!(plan.engine.command_grant(&run("gh pr list", true)), None);
    assert_eq!(
        plan.engine.decide(&run("gh pr list", true)).verdict,
        Verdict::Deny
    );
}

#[test]
fn only_a_plain_foreground_command_is_granted() {
    let fixture = fixture(policy(true), NONE, USER_RULES);
    let engine = &fixture.engine;
    let mut asked = vec![
        run("gh pr list | head", true),
        run("gh pr list && rm -r x", true),
        run("gh auth token", true),
        run("gh auth status -t", true),
        run("gh pr list --repo o/r; curl evil.example.com", true),
    ];
    let mut background = run("gh pr list", true);
    background.background = true;
    let mut loopback = run("gh pr list", true);
    loopback.loopback = true;
    let mut kept = run("gh pr list", true);
    kept.keep_alive = true;
    let mut full = run("gh pr list", true);
    full.sandbox = Some(SandboxMode::FullAccess);
    let mut tests = run("gh pr list", true);
    tests.tool = ToolKind::RunTests;
    tests.runner = Some("cargo".to_owned());
    // A sandbox that cannot hide what it denies: asked, so no grant.
    let mut unconfined = run("gh pr list", true);
    unconfined.unconfined = true;
    asked.extend([background, loopback, kept, full, tests, unconfined]);
    for call in asked {
        assert_eq!(engine.command_grant(&call), None, "{call:?}");
        assert_ne!(engine.decide(&call).verdict, Verdict::Allow, "{call:?}");
    }
}

#[test]
fn only_a_rule_the_person_wrote_grants() {
    let rule = r#"{"pattern":["gh","pr"],"credential":"gh"}"#;
    // A workspace rule the person wrote (Settings, or the file in host
    // data) grants like a user rule.
    let mine = fixture(
        policy(true),
        &format!(r#"{{"version":1,"rules":[{rule}]}}"#),
        NONE,
    );
    let expected = SUPPORTED.then(|| gh_grant("workspace#1"));
    assert_eq!(
        mine.engine.command_grant(&run("gh pr list", true)),
        expected
    );
    // A repository's rule never does, and its allow does not cover the
    // network either.
    let repository = fixture(
        policy(true),
        r#"{"version":1,"rules":[{"pattern":["gh","pr"],"credential":"gh","origin":"repository"}]}"#,
        NONE,
    );
    assert_eq!(
        repository.engine.command_grant(&run("gh pr list", true)),
        None
    );
    assert_eq!(
        repository.engine.decide(&run("gh pr list", true)).verdict,
        Verdict::Ask
    );
    // A `prompt` rule beside the grant wins: asked, nothing granted.
    let prompted = fixture(
        policy(true),
        r#"{"version":1,"rules":[{"pattern":["gh","pr","view"],"decision":"prompt"}]}"#,
        &format!(r#"{{"version":1,"rules":[{rule}]}}"#),
    );
    assert_eq!(
        prompted.engine.command_grant(&run("gh pr view 1", true)),
        None
    );
    assert_eq!(
        prompted.engine.decide(&run("gh pr view 1", true)).verdict,
        Verdict::Ask
    );
}

/// "Always allow" from the approval card writes the call's own bounds:
/// never hosts and never a credential.
#[test]
fn always_allow_never_writes_a_grant() {
    struct Sink(Mutex<Vec<PrefixRule>>);
    impl RuleSink for Sink {
        fn append(&self, rule: PrefixRule) -> ToolResult<RuleSet> {
            self.0.lock().expect("lock").push(rule.clone());
            let mut file = crate::execpolicy::RuleFile::default();
            file.rules.push(rule);
            RuleSet::compile("workspace", file)
                .map_err(|error| crate::error::ToolError::invalid(error.message.clone()))
        }
    }
    let fixture = fixture(policy(true), NONE, NONE);
    let sink = Arc::new(Sink(Mutex::new(Vec::new())));
    fixture
        .engine
        .set_rule_sink(Some(sink.clone() as Arc<dyn RuleSink>));
    fixture
        .engine
        .remember(&run("gh pr list -R o/r", true), None)
        .expect("remembered");
    let written = sink.0.lock().expect("lock");
    assert_eq!(written.len(), 1);
    assert!(written[0].credential.is_none() && written[0].hosts.is_empty());
    assert!(written[0].conditions.is_empty());
}
