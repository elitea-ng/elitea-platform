//! The trust store (ADR-0061 decision 5i): grant, revoke, a policy change
//! suspends, tampering suspends everything, worktrees inherit, refusals.

use std::sync::{Arc, Mutex};

use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};

use super::{CONFIRMATION_VERSION, SuspendedBy, TrustChange, TrustState, TrustStore};
use crate::workspaces::WorkspaceStore;

fn trusting() -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        shell: true,
        network: true,
        max_sandbox_mode: SandboxMode::FullAccess,
        folder_trust: true,
        ..LocalWorkPolicy::default()
    }
}

struct Fixture {
    dir: tempfile::TempDir,
    workspaces: Arc<WorkspaceStore>,
    store: Arc<TrustStore>,
    changes: Arc<Mutex<Vec<TrustChange>>>,
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let workspaces = Arc::new(WorkspaceStore::new(dir.path().join("data")));
    let store = TrustStore::new(
        workspaces.clone(),
        Some(dir.path().join("config").join("rules.json")),
        vec![dir.path().join("data")],
    );
    let changes = Arc::new(Mutex::new(Vec::new()));
    let seen = changes.clone();
    store.on_change(move |change| seen.lock().expect("lock").push(change));
    Fixture {
        dir,
        workspaces,
        store,
        changes,
    }
}

impl Fixture {
    fn folder(&self, name: &str) -> String {
        let path = self.dir.path().join(name);
        std::fs::create_dir_all(&path).expect("folder");
        self.workspaces.add(&path).expect("add").id
    }
}

#[test]
fn grant_and_revoke_are_recorded() {
    let fixture = fixture();
    let id = fixture.folder("repo");
    let policy = trusting();
    let status = fixture.store.status(&id, &policy).expect("status");
    assert_eq!(status.state, TrustState::Untrusted);
    assert!(status.grantable);
    let granted = fixture
        .store
        .grant(&id, &policy, CONFIRMATION_VERSION, true)
        .expect("granted");
    assert_eq!(granted.state, TrustState::Trusted);
    assert!(granted.ask_every_command);
    assert!(granted.folder_trust().is_some());
    assert_eq!(fixture.store.list(&policy).expect("list").len(), 1);
    let ledger = std::fs::read_to_string(fixture.store.ledger_path()).expect("ledger");
    assert!(ledger.contains("\"granted\""));
    assert!(!ledger.contains("repo"), "paths appear as digests only");
    let revoked = fixture.store.revoke(&id, &policy).expect("revoked");
    assert_eq!(revoked.state, TrustState::Untrusted);
    assert!(fixture.store.list(&policy).expect("list").is_empty());
    assert!(
        fixture
            .changes
            .lock()
            .expect("lock")
            .contains(&TrustChange::Changed(Some(id)))
    );
}

#[test]
fn a_grant_is_refused_when_the_policy_or_the_confirmation_says_no() {
    let fixture = fixture();
    let id = fixture.folder("repo");
    let blocked = LocalWorkPolicy {
        folder_trust: false,
        ..trusting()
    };
    let refused = fixture
        .store
        .grant(&id, &blocked, CONFIRMATION_VERSION, false);
    assert_eq!(refused.err().map(|r| r.code), Some("trust_not_allowed"));
    let ceiling = LocalWorkPolicy {
        max_sandbox_mode: SandboxMode::WorkspaceWrite,
        ..trusting()
    };
    assert!(
        !fixture
            .store
            .status(&id, &ceiling)
            .expect("status")
            .grantable
    );
    let outdated = fixture
        .store
        .grant(&id, &trusting(), CONFIRMATION_VERSION + 1, false);
    assert_eq!(
        outdated.err().map(|r| r.code),
        Some("confirmation_outdated")
    );
}

/// A policy change suspends trust; it stays suspended until granted again.
#[test]
fn a_policy_change_suspends_trust() {
    let fixture = fixture();
    let id = fixture.folder("repo");
    fixture
        .store
        .grant(&id, &trusting(), CONFIRMATION_VERSION, false)
        .expect("granted");
    let ledger = || {
        std::fs::read_to_string(fixture.store.ledger_path())
            .expect("ledger")
            .lines()
            .filter(|line| line.contains("\"suspended\""))
            .count()
    };
    let lowered = LocalWorkPolicy {
        max_sandbox_mode: SandboxMode::WorkspaceWrite,
        ..trusting()
    };
    fixture.store.policy_changed(&lowered);
    fixture.store.policy_changed(&lowered);
    assert_eq!(ledger(), 1, "one ledger entry");
    // Raising it back keeps it suspended until a new grant.
    let status = fixture.store.status(&id, &trusting()).expect("status");
    assert_eq!(status.state, TrustState::Suspended);
    assert_eq!(status.suspended_by, Some(SuspendedBy::Policy));
    assert!(status.folder_trust().is_none());
    // Granted again under the policy of the day.
    let again = fixture
        .store
        .grant(&id, &trusting(), CONFIRMATION_VERSION, false)
        .expect("granted");
    assert_eq!(again.state, TrustState::Trusted);
}

/// Editing a record (or the ledger) outside the app suspends every
/// trusted folder.
#[test]
fn tampering_suspends_every_folder() {
    let fixture = fixture();
    let one = fixture.folder("one");
    let two = fixture.folder("two");
    let policy = trusting();
    for id in [&one, &two] {
        fixture
            .store
            .grant(id, &policy, CONFIRMATION_VERSION, false)
            .expect("granted");
    }
    // Someone turns "ask for every command" off by hand.
    let path = fixture.store.record_path(&one);
    let text = std::fs::read_to_string(&path).expect("record");
    let edited = text.replace(
        "\"ask_every_command\": false",
        "\"ask_every_command\": true",
    );
    assert_ne!(edited, text);
    crate::rules::write_private(&path, edited.as_bytes()).expect("write");
    for id in [&one, &two] {
        let status = fixture.store.status(id, &policy).expect("status");
        assert_eq!(status.state, TrustState::Suspended, "{id}");
        assert_eq!(status.suspended_by, Some(SuspendedBy::Tamper));
        assert!(status.tampered_file.is_some());
    }
    assert!(
        fixture
            .changes
            .lock()
            .expect("lock")
            .iter()
            .any(|change| matches!(change, TrustChange::Tamper(_)))
    );
    // A record with no ledger line is tampering too.
    let ledger = fixture.store.ledger_path();
    std::fs::remove_file(&ledger).expect("ledger removed");
    let status = fixture.store.status(&one, &policy).expect("status");
    assert_eq!(status.suspended_by, Some(SuspendedBy::Tamper));
}

/// An outside write to a trusted folder's `rules.json` (or the person's)
/// suspends every trusted folder; the app's own write does not.
#[test]
fn an_outside_write_to_the_rules_suspends_trust() {
    let fixture = fixture();
    let id = fixture.folder("repo");
    let policy = trusting();
    fixture
        .store
        .grant(&id, &policy, CONFIRMATION_VERSION, false)
        .expect("granted");
    let rules = fixture.workspaces.data_dir(&id).join("rules.json");
    // The app writes it.
    fixture.store.rules_writing(&rules);
    crate::rules::write_private(&rules, br#"{"version":1}"#).expect("write");
    fixture.store.rules_written(&rules);
    assert_eq!(
        fixture.store.status(&id, &policy).expect("status").state,
        TrustState::Trusted
    );
    // Someone else adds an allow rule.
    crate::rules::write_private(&rules, br#"{"version":1,"rules":[{"pattern":["curl"]}]}"#)
        .expect("write");
    let status = fixture.store.status(&id, &policy).expect("status");
    assert_eq!(status.suspended_by, Some(SuspendedBy::Tamper));
    assert!(
        status
            .tampered_file
            .is_some_and(|file| file.ends_with("rules.json"))
    );
    // Suspended, it stays suspended (no loop), and a new grant trusts it.
    assert_eq!(
        fixture.store.status(&id, &policy).expect("status").state,
        TrustState::Suspended
    );
    let again = fixture
        .store
        .grant(&id, &policy, CONFIRMATION_VERSION, false)
        .expect("granted");
    assert_eq!(again.state, TrustState::Trusted);
}

/// A task worktree inherits its folder's trust and cannot be trusted on
/// its own (#3015).
#[test]
fn a_worktree_inherits_its_folders_trust() {
    let fixture = fixture();
    let parent = fixture.folder("repo");
    let tree = fixture.dir.path().join("trees").join("t1");
    std::fs::create_dir_all(&tree).expect("tree");
    let child = fixture
        .workspaces
        .add_worktree(&parent, &tree, "thread-1", "task/t1")
        .expect("worktree")
        .id;
    let policy = trusting();
    let status = fixture.store.status(&child, &policy).expect("status");
    assert_eq!(status.inherited_from.as_deref(), Some(parent.as_str()));
    assert!(!status.grantable);
    let refused = fixture
        .store
        .grant(&child, &policy, CONFIRMATION_VERSION, false);
    assert_eq!(refused.err().map(|r| r.code), Some("trust_refused"));
    fixture
        .store
        .grant(&parent, &policy, CONFIRMATION_VERSION, false)
        .expect("granted");
    assert_eq!(
        fixture.store.status(&child, &policy).expect("status").state,
        TrustState::Trusted
    );
}

#[test]
fn the_apps_own_data_is_never_trusted() {
    let fixture = fixture();
    let inside = fixture.dir.path().join("data").join("inside");
    std::fs::create_dir_all(&inside).expect("inside");
    let id = fixture.workspaces.add(&inside).expect("add").id;
    let refused = fixture
        .store
        .grant(&id, &trusting(), CONFIRMATION_VERSION, false);
    assert_eq!(refused.err().map(|r| r.code), Some("trust_refused"));
}
