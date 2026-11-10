//! A running turn survives an app restart (ADR-0045 decision 7, #1438).
//!
//! The host keeps a resume record per running turn in `threads.sqlite`
//! ([`crate::history::ResumeRecord`]): [`record`] makes it when the turn
//! starts, the turn's commit updates it while its chain goes on, and the
//! turn's normal end deletes it. When the app starts again, each record
//! that is left is a turn that never ended:
//! - with an unattended grant, the host continues the chain from the last
//!   committed step, as a continuation of link kind `resume`
//!   ([`continuation`]). Its text is `CONTINUE_PROMPT` with the stored run
//!   brief and last goal check (decision 11). The turn's start takes the
//!   workspace claim and a checkpoint again, as every turn does;
//! - without one, the UI shows **Continue** on the turn
//!   (`agent_turn_resume`, which starts the same continuation without a
//!   grant).
//!
//! A record past its deadline (the chain's, or [`DEFAULT_DEADLINE`]) is
//! dropped. Only the platform's refusal of the continuation ([`refused`])
//! deletes a record and ends its chain; any other failure (no network, a
//! 401 before the sign-in, a 5xx, a busy folder) keeps it, and the host
//! tries again on a back-off ([`backoff`]) and after each sign-in.

use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;

use super::api::LocalTurnStarted;
use super::goal::{Brief, ChainGoal, GoalCheck};
use super::turn::{TurnError, TurnRequest};
use super::unattended::{
    ContinuationKind, GoalHandle, StartedChain, UnattendedGrant, UnattendedTurn,
};
use crate::history::{Owner, ResumeMode, ResumeRecord};

/// The platform's default chain deadline (ADR-0045 decision 4): a record
/// of a turn without a grant, or of a chain that names none, is kept this
/// long.
pub const DEFAULT_DEADLINE: Duration = Duration::from_hours(12);

/// How long after start-up the host looks at the resume records: the
/// window loads and the sign-in refreshes first.
pub const RESUME_AFTER: Duration = Duration::from_secs(5);

/// The first wait before the host tries a record again; it doubles up to
/// [`RETRY_MAX`].
pub const RETRY_FIRST: Duration = Duration::from_secs(30);

/// The longest wait between two tries.
pub const RETRY_MAX: Duration = Duration::from_mins(5);

/// The wait before try `attempt` + 2 (from 0): 30 s, 1 min, 2 min, 4 min,
/// then 5 min.
#[must_use]
pub fn backoff(attempt: u32) -> Duration {
    RETRY_FIRST
        .saturating_mul(2_u32.saturating_pow(attempt.min(16)))
        .min(RETRY_MAX)
}

/// Whether `error` is the platform's refusal of the continuation, which
/// ends the chain for good: a chain limit (409 `chain_limit_reached`), a
/// chain that already ended (409 `chain_ended`), a grant refused (422
/// `unattended_grant_refused`, grants turned off included), a chain the
/// platform does not know (404 `chain_not_found`), or a record that cannot
/// make a turn (`resume_invalid`). Anything else (401, 429, 5xx, no answer,
/// a busy folder) may pass later.
#[must_use]
pub fn refused(error: &TurnError) -> bool {
    use super::unattended::{CHAIN_LIMIT_REACHED, GRANT_REFUSED, GRANTS_OFF};
    let code = error.code.as_str();
    code == "resume_invalid"
        || match error.status {
            Some(409) => code == CHAIN_LIMIT_REACHED || code == "chain_ended",
            Some(422) => code == GRANT_REFUSED || code == GRANTS_OFF,
            Some(404) => code == "chain_not_found",
            _ => false,
        }
}

/// The thread is in the cloud (409 `thread_executor_is_cloud`): a send of
/// it landed, so the turn goes on there and nothing is resumed here.
#[must_use]
pub fn moved_to_cloud(error: &TurnError) -> bool {
    error.code == "thread_executor_is_cloud"
}

/// The record of a turn that just started: `turn_id` of `request`, as the
/// platform started it (`started`), for `owner`, at `now_ms`.
#[must_use]
pub fn record(
    request: &TurnRequest,
    turn_id: &str,
    owner: &Owner,
    started: &LocalTurnStarted,
    conversation_uuid: &str,
    now_ms: i64,
) -> ResumeRecord {
    // A grant the platform did not admit runs as a turn without one.
    let unattended = request
        .unattended
        .as_ref()
        .filter(|_| started.unattended.is_some());
    let goal = unattended.and_then(UnattendedTurn::goal);
    let deadline = deadline(
        started.unattended.as_ref(),
        unattended.map(|turn| &turn.grant),
    );
    ResumeRecord {
        workspace_id: request.workspace_id.clone(),
        turn_id: turn_id.to_owned(),
        owner: owner.clone(),
        conversation_id: request.conversation_id.clone(),
        conversation_uuid: Some(conversation_uuid.to_owned()),
        project_id: request.project_id,
        application_id: request.application_id,
        version_id: request.version_id,
        mode: if request.plan_mode {
            ResumeMode::Plan
        } else if request.bypass {
            ResumeMode::Bypass
        } else {
            ResumeMode::Build
        },
        grant: unattended.and_then(|turn| serde_json::to_value(&turn.grant).ok()),
        chain_root_execution_id: unattended.map(|turn| {
            started
                .unattended
                .as_ref()
                .and_then(|chain| chain.chain_root_execution_id.clone())
                .or_else(|| turn.chain_root_execution_id.clone())
                .unwrap_or_else(|| started.execution_id.clone())
        }),
        continuation_index: unattended.map_or(0, |turn| {
            started
                .unattended
                .as_ref()
                .and_then(|chain| chain.continuation_index)
                .unwrap_or(turn.continuation_index)
        }),
        kind: unattended
            .and_then(|turn| turn.kind)
            .map(|kind| kind.as_str().to_owned()),
        execution_id: started.execution_id.clone(),
        last_committed_execution_id: None,
        request: unattended.map(|_| {
            goal.map(|goal| goal.request())
                .filter(|text| !text.is_empty())
                .unwrap_or_else(|| super::goal::head(&request.prompt, 8 * 1024).to_owned())
        }),
        brief: goal
            .and_then(|goal| goal.ready_brief())
            .and_then(|brief| serde_json::to_value(brief).ok()),
        last_check: goal
            .and_then(|goal| goal.last_check())
            .and_then(|check| serde_json::to_value(check).ok()),
        started_at: now_ms,
        updated_at: now_ms,
        deadline_at: now_ms.saturating_add(i64::try_from(deadline.as_millis()).unwrap_or(i64::MAX)),
        transcript: goal
            .map(|goal| goal.transcript())
            .filter(|text| !text.is_empty()),
    }
}

/// The chain's deadline: the one the platform admitted, else the grant's,
/// else [`DEFAULT_DEADLINE`].
fn deadline(started: Option<&StartedChain>, grant: Option<&UnattendedGrant>) -> Duration {
    started
        .and_then(|chain| chain.chain.as_ref())
        .and_then(|chain| chain.get("deadline_seconds"))
        .and_then(Value::as_u64)
        .or_else(|| {
            grant
                .and_then(|grant| grant.chain.as_ref())
                .and_then(|chain| chain.deadline_seconds)
        })
        .map_or(DEFAULT_DEADLINE, Duration::from_secs)
}

/// The record is past its deadline at `now_ms`: it is dropped.
#[must_use]
pub const fn expired(record: &ResumeRecord, now_ms: i64) -> bool {
    now_ms > record.deadline_at
}

fn invalid(message: &str) -> TurnError {
    TurnError::new("resume_invalid", message)
}

/// The turn that continues `record` after a restart: the same thread,
/// agent and mode, from the runtime's `CONTINUE_PROMPT`. With a grant, the
/// chain's next turn (link kind `resume`), its text restating the stored
/// brief and last check; without one, a turn without a grant.
///
/// # Errors
///
/// `resume_invalid` for a record whose grant or chain does not read.
pub fn continuation(record: &ResumeRecord) -> Result<TurnRequest, TurnError> {
    let unattended = match &record.grant {
        None => None,
        Some(grant) => {
            let grant: UnattendedGrant = serde_json::from_value(grant.clone())
                .map_err(|_| invalid("The stored unattended grant does not read."))?;
            let root = record
                .chain_root_execution_id
                .clone()
                .ok_or_else(|| invalid("The stored chain has no root."))?;
            let goal = grant.goal_check().then(|| {
                GoalHandle(Arc::new(ChainGoal::restored(
                    record
                        .brief
                        .clone()
                        .and_then(|brief| serde_json::from_value::<Brief>(brief).ok()),
                    record
                        .last_check
                        .clone()
                        .and_then(|check| serde_json::from_value::<GoalCheck>(check).ok()),
                    record.request.as_deref().unwrap_or_default(),
                    record.transcript.as_deref().unwrap_or_default(),
                )))
            });
            Some(UnattendedTurn {
                grant,
                chain_root_execution_id: Some(root),
                continuation_index: record.continuation_index.saturating_add(1),
                kind: Some(ContinuationKind::Resume),
                goal,
            })
        }
    };
    let prompt = unattended
        .as_ref()
        .and_then(UnattendedTurn::goal)
        .map_or_else(
            || elitea_agent_runtime::step_limit::CONTINUE_PROMPT.to_owned(),
            |goal| goal.continue_text(),
        );
    Ok(TurnRequest {
        workspace_id: record.workspace_id.clone(),
        project_id: record.project_id,
        conversation_id: record.conversation_id.clone(),
        application_id: record.application_id,
        version_id: record.version_id,
        prompt,
        // An unattended turn never runs in plan mode.
        plan_mode: record.mode == ResumeMode::Plan && unattended.is_none(),
        bypass: record.mode == ResumeMode::Bypass,
        mentions: Vec::new(),
        skills: Vec::new(),
        compact: false,
        model: None,
        project_skills: Vec::new(),
        command: None,
        images: Vec::new(),
        unattended,
        addressed_to: Vec::new(),
        floor_turn: None,
        cloud: false,
    })
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    fn stored(grant: Option<Value>) -> ResumeRecord {
        ResumeRecord {
            workspace_id: "w1".into(),
            turn_id: "t1".into(),
            owner: Owner {
                origin: "https://elitea.example".into(),
                user_id: 1,
            },
            conversation_id: json!(42),
            conversation_uuid: Some("99999999-2222-4333-8444-555555555555".into()),
            project_id: 1,
            application_id: 5,
            version_id: 9,
            mode: ResumeMode::Bypass,
            chain_root_execution_id: grant.as_ref().map(|_| "root".to_owned()),
            grant,
            continuation_index: 2,
            kind: Some("step_limit".into()),
            execution_id: "exec-3".into(),
            last_committed_execution_id: Some("exec-2".into()),
            request: Some("Make the login test pass".into()),
            brief: Some(json!({"goal": "Make the login test pass.",
                "done_when": [{"id": "c1", "text": "The login test passes."}],
                "constraints": [], "assumptions": [], "ambiguity": "low"})),
            last_check: None,
            started_at: 1,
            updated_at: 1,
            deadline_at: 100,
            transcript: Some("\n## Turn 3\nAnswer: The login form renders.\n".into()),
        }
    }

    #[test]
    fn an_unattended_record_continues_its_chain_with_the_brief() {
        let grant = json!({"grant_id": "6f9619ff-8b86-d011-b42d-00c04fc964ff",
            "continue_step_limit": true, "continue_tool_call_cap": false,
            "hook_asks": false, "remote_confirmations": false, "chain": null,
            "goal_check": null});
        let request = continuation(&stored(Some(grant))).expect("request");
        let turn = request.unattended.as_ref().expect("unattended");
        assert_eq!(turn.kind, Some(ContinuationKind::Resume));
        assert_eq!(turn.continuation_index, 3);
        assert_eq!(turn.chain_root_execution_id.as_deref(), Some("root"));
        assert_eq!(turn.wire()["kind"], "resume");
        assert_eq!(
            turn.wire()["grant_id"],
            "6f9619ff-8b86-d011-b42d-00c04fc964ff"
        );
        assert!(
            request
                .prompt
                .starts_with("Continue from where you stopped.")
        );
        assert!(request.prompt.contains("Goal: Make the login test pass."));
        // The goal check after the restart reads the turns from before it.
        let excerpt = turn.goal().expect("goal").excerpt();
        assert!(excerpt.contains("Make the login test pass"));
        assert!(excerpt.contains("Answer: The login form renders."));
        assert!(request.bypass && !request.plan_mode);
        assert_eq!(request.conversation_id, json!(42));
    }

    #[test]
    fn only_the_platforms_refusal_ends_a_resumed_chain() {
        let error = |code: &str, status: Option<u16>| TurnError {
            status,
            ..TurnError::new(code, "x")
        };
        for (code, status) in [
            ("chain_limit_reached", 409),
            ("chain_ended", 409),
            ("unattended_grant_refused", 422),
            ("unattended_grants_off", 422),
            ("chain_not_found", 404),
        ] {
            assert!(refused(&error(code, Some(status))), "{code} {status}");
        }
        assert!(refused(&error("resume_invalid", None)));
        for (code, status) in [
            ("unauthorized", Some(401)),
            ("http_500", Some(500)),
            ("http_503", Some(503)),
            ("rate_limited", Some(429)),
            ("local_turn_conflict", Some(409)),
            ("not_found", Some(404)),
            ("network", None),
            ("workspace_busy", None),
            ("not_signed_in", None),
        ] {
            assert!(!refused(&error(code, status)), "{code} {status:?}");
        }
    }

    #[test]
    fn the_retries_back_off_to_five_minutes() {
        let waits: Vec<u64> = (0..7).map(|n| backoff(n).as_secs()).collect();
        assert_eq!(waits, vec![30, 60, 120, 240, 300, 300, 300]);
        assert_eq!(backoff(u32::MAX), RETRY_MAX);
    }

    #[test]
    fn an_attended_record_continues_without_a_grant() {
        let mut record = stored(None);
        record.mode = ResumeMode::Plan;
        let request = continuation(&record).expect("request");
        assert!(request.unattended.is_none());
        assert!(request.plan_mode);
        assert_eq!(request.prompt, "Continue from where you stopped.");
        assert!(!expired(&record, 100));
        assert!(expired(&record, 101));
    }

    #[test]
    fn a_record_whose_grant_does_not_read_is_refused() {
        let error = continuation(&stored(Some(json!({"other": true})))).expect_err("refused");
        assert_eq!(error.code, "resume_invalid");
    }

    #[test]
    fn the_deadline_is_the_platforms_then_the_grants_then_the_default() {
        let started = StartedChain {
            chain: Some(json!({"deadline_seconds": 60})),
            ..StartedChain::default()
        };
        let grant = UnattendedGrant {
            chain: Some(super::super::unattended::ChainLimits {
                deadline_seconds: Some(30),
                ..Default::default()
            }),
            ..UnattendedGrant::default()
        };
        assert_eq!(
            deadline(Some(&started), Some(&grant)),
            Duration::from_secs(60)
        );
        assert_eq!(deadline(None, Some(&grant)), Duration::from_secs(30));
        assert_eq!(deadline(None, None), DEFAULT_DEADLINE);
    }
}
