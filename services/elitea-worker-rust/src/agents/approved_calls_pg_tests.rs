//! ADR-0034 decision 9, the run-once ledger of an approved workspace call,
//! against the session store the worker uses in production: the Postgres-backed
//! `PostgresSessionService` with its writer fence, not the in-memory store of
//! `direct_hitl_tests`.
//!
//! Every test needs `ELITEA_TEST_DATABASE_URL` and skips without it. Each one
//! runs in its own isolated database.
//!
//! Each "resume" is its own execution with its own claim on the same session
//! (a retry, or a second delivery of the same approval); a newer claim fences
//! the older one in the store.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::Event;
use adk_rust::session::{CreateRequest, SessionService};
use serde_json::json;

use super::direct_hitl_tests::{
    GateSandbox, ResultLossSessions, approved_push_resume, ledger_workspace_turn, paused_push,
    stored_workspace_session,
};
use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};

type Resume = (
    Arc<dyn adk_rust::Llm>,
    adk_rust::RunConfig,
    adk_rust::Content,
);

/// One execution's session store: the Postgres service activated for `claim`,
/// behind the result-loss wrapper (unarmed unless a test arms it).
async fn claim_sessions(
    pool: &sqlx::PgPool,
    claim: u8,
) -> Result<Arc<ResultLossSessions>, crate::state::PostgresSessionError> {
    let service = PostgresSessionService::activate(
        pool.clone(),
        authority_for(
            &format!("claim-{claim}"),
            u64::from(claim),
            u64::from(claim),
            [claim; 32],
        ),
        SessionLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await?;
    Ok(Arc::new(ResultLossSessions {
        inner: Arc::new(service),
        armed: AtomicBool::new(false),
    }))
}

/// A database with the turn's session created and a `git push` paused for
/// approval under claim 1; nothing has run.
async fn paused_database() -> Option<(IsolatedPostgres, Arc<GateSandbox>)> {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the run-once ledger PostgreSQL proof");
        return None;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    let sessions = claim_sessions(&db.pool, 1).await.expect("claim 1");
    sessions
        .create(CreateRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("session-1".to_owned()),
            state: std::collections::HashMap::new(),
        })
        .await
        .expect("session");
    let sandbox = Arc::new(GateSandbox::default());
    paused_push(&sessions, &sandbox).await;
    Some((db, sandbox))
}

fn runs(sandbox: &GateSandbox) -> usize {
    sandbox.calls.lock().expect("calls").len()
}

async fn resume_turn(
    sessions: &Arc<ResultLossSessions>,
    sandbox: &Arc<GateSandbox>,
    resume: Resume,
) -> (Vec<Event>, Option<adk_rust::AdkError>) {
    let (model, run_config, user_content) = resume;
    ledger_workspace_turn(
        Arc::clone(sessions) as Arc<dyn SessionService>,
        model,
        Arc::clone(sandbox),
        run_config,
        user_content,
    )
    .await
}

fn tool_result(events: &[Event]) -> Option<serde_json::Value> {
    events
        .iter()
        .flat_map(Event::tool_results)
        .find(|result| result.call_id == Some("call-1"))
        .map(|result| result.response.clone())
}

/// An approved `git push`, a resumed turn that fails after the push (the
/// result never reaches the store), and the retry under a new claim: the push
/// runs exactly once and the retry reports the first run's result.
#[tokio::test]
async fn postgres_an_approved_push_runs_once_when_the_resumed_turn_fails_and_is_retried() {
    let Some((db, sandbox)) = paused_database().await else {
        return;
    };
    let first = claim_sessions(&db.pool, 2).await.expect("claim 2");
    let resume = approved_push_resume(first.as_ref()).await;
    first.armed.store(true, Ordering::SeqCst);
    let (_, error) = resume_turn(&first, &sandbox, resume).await;
    assert!(error.is_some(), "the injected failure fails the turn");
    assert_eq!(runs(&sandbox), 1, "pushed once");
    assert!(
        stored_workspace_session(first.as_ref())
            .await
            .events()
            .all()
            .iter()
            .all(|event| event.tool_results().is_empty()),
        "the result never reached the session"
    );

    let second = claim_sessions(&db.pool, 3).await.expect("claim 3");
    let resume = approved_push_resume(second.as_ref()).await;
    let (events, error) = resume_turn(&second, &sandbox, resume).await;
    assert!(error.is_none(), "{error:?}");
    assert_eq!(runs(&sandbox), 1, "the push did not run again");
    assert_eq!(
        tool_result(&events),
        Some(json!({"status": "ok", "run": 1})),
        "the retry answers with the earlier result"
    );
    db.pool.close().await;
}

/// The consumed approval survives a turn that fails after `started` and before
/// any result: a retry does not run the call and the model is told to check.
#[tokio::test]
async fn postgres_an_approval_consumed_without_a_result_is_not_run_again() {
    use elitea_agent_runtime::workspace_tools::{
        ApprovedCall, ApprovedCallClaim, ApprovedCallLedger as _,
    };
    let Some((db, _sandbox)) = paused_database().await else {
        return;
    };
    let args = json!({"command": "git push origin main"});
    let call = || ApprovedCall {
        tool: "run_command",
        call_id: "call-1",
        args: &args,
        invocation_id: "resume-invocation",
    };
    let first = claim_sessions(&db.pool, 2).await.expect("claim 2");
    let ledger = super::approved_calls::SessionApprovedCallLedger::new(
        Arc::clone(&first) as Arc<dyn SessionService>,
        super::direct_hitl_tests::workspace_identity(),
    );
    assert!(matches!(
        ledger.claim(call()).await.expect("claim"),
        ApprovedCallClaim::Run { .. }
    ));
    // The worker dies here. The next claim finds the approval consumed.
    let second = claim_sessions(&db.pool, 3).await.expect("claim 3");
    let ledger = super::approved_calls::SessionApprovedCallLedger::new(
        second as Arc<dyn SessionService>,
        super::direct_hitl_tests::workspace_identity(),
    );
    assert_eq!(
        ledger.claim(call()).await.expect("second claim"),
        ApprovedCallClaim::Started
    );
    // The older claim is fenced: it cannot even read the ledger.
    let stale = super::approved_calls::SessionApprovedCallLedger::new(
        first as Arc<dyn SessionService>,
        super::direct_hitl_tests::workspace_identity(),
    );
    assert!(
        stale.claim(call()).await.is_err(),
        "a fenced claim never runs"
    );
    db.pool.close().await;
}

/// Two resumes of one approval, the newer claim already active when the older
/// one starts: the older is fenced and never runs the call; the newer runs it.
#[tokio::test]
async fn postgres_a_fenced_resume_does_not_run_the_approved_call() {
    let Some((db, sandbox)) = paused_database().await else {
        return;
    };
    let older = claim_sessions(&db.pool, 2).await.expect("claim 2");
    let newer = claim_sessions(&db.pool, 3).await.expect("claim 3");
    let stale_resume = approved_push_resume(newer.as_ref()).await;
    let current_resume = approved_push_resume(newer.as_ref()).await;
    let (_, error) = resume_turn(&older, &sandbox, stale_resume).await;
    assert!(error.is_some(), "the fenced resume fails");
    assert_eq!(runs(&sandbox), 0, "a fenced resume never runs the call");
    let (_, error) = resume_turn(&newer, &sandbox, current_resume).await;
    assert!(error.is_none(), "{error:?}");
    assert_eq!(runs(&sandbox), 1);
    db.pool.close().await;
}

/// Two concurrent resumes of one approval, each activating its own claim and
/// running at once: whichever interleaving the database produces, the call
/// runs exactly once and at least one resume completes. (A resume that starts
/// after the other finished is refused by the replay proof itself, which
/// rejects an approval whose call already has a result.)
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn postgres_two_concurrent_resumes_run_the_approved_call_exactly_once() {
    for round in 0..6 {
        let Some((db, sandbox)) = paused_database().await else {
            return;
        };
        let reader = claim_sessions(&db.pool, 2).await.expect("claim 2");
        // Claim 2 is activated only to read the stored approval twice; both
        // racers then take claims 3 and 4.
        let resumes = [
            approved_push_resume(reader.as_ref()).await,
            approved_push_resume(reader.as_ref()).await,
        ];
        let mut racers = Vec::new();
        for (resume, claim) in resumes.into_iter().zip([3_u8, 4]) {
            let pool = db.pool.clone();
            let sandbox = Arc::clone(&sandbox);
            racers.push(tokio::spawn(async move {
                let sessions = claim_sessions(&pool, claim).await.ok()?;
                let (_, error) = resume_turn(&sessions, &sandbox, resume).await;
                Some(error.is_none())
            }));
        }
        let mut completed = 0;
        for racer in racers {
            if racer.await.expect("racer") == Some(true) {
                completed += 1;
            }
        }
        assert_eq!(runs(&sandbox), 1, "round {round}: exactly one run");
        assert!(completed >= 1, "round {round}: one resume completes");
        db.pool.close().await;
    }
}
