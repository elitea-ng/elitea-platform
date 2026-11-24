//! Node journals retain the existing exact execution and current writer fence.

use std::collections::BTreeSet;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;
use sqlx::PgPool;

use super::application_children::{ApplicationCheckpointers, admitted_application_paths};
use super::{
    CheckpointIoCounters, CheckpointLimits, CheckpointWriterAuthority, JournalRootFence,
    MAX_THREAD_BYTES, PostgresCheckpointer,
};
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::{
    NodeAttemptActivation, NodeAttemptJournal, NodeRecoveryFactory, PipelineToolJournalAuthority,
    recovery_error,
};
use crate::state::StateWriterLease;
use crate::state::postgres_session::PostgresSessionService;

/// Maximum distinct effectful direct-tool node journals one non-member
/// pipeline-tool call may open (FU-358). Re-opening an already-opened journal
/// does not consume a new slot.
pub(crate) const MAX_PIPELINE_TOOL_JOURNALS_PER_CALL: usize = 64;

const ROOT_JOURNAL_DOMAIN: &[u8] = b"elitea.pipeline.node-attempt-journal.v1\0";
const PIPELINE_TOOL_JOURNAL_DOMAIN: &[u8] = b"elitea.pipeline.tool-node-attempt-journal.v1\0";

#[async_trait]
impl NodeRecoveryFactory for PostgresCheckpointer {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, adk_rust::graph::GraphError> {
        if activation.root_thread_id != self.scope.authority.thread_id {
            return Err(recovery_error("pipeline.node_recovery.invalid_scope"));
        }
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| recovery_error("pipeline.node_recovery.writer_not_current"))?;
        // FU-248: a saved child's thread and steps are reused by every visit,
        // so its journal is scoped to the visit; there is no unscoped fallback.
        // The run root has no visit and keeps its original journal identity.
        // 5b-5: an authorized direct fan-out child thread (`p1:` / `m1:`, no `/`
        // subgraph suffix) is already unique per occurrence, branch/item and
        // input, so it may open visit-scoped (Code child) or thread-scoped
        // (LLM child) under its own activated child writer while staying fenced
        // on `self.run_root_thread_id`.
        let in_child = activation.root_thread_id != self.run_root_thread_id;
        let direct_fanout_child =
            in_child && is_direct_fanout_child_thread(&activation.root_thread_id);
        if !direct_fanout_child && in_child != activation.visit.is_some() {
            return Err(recovery_error("pipeline.node_recovery.unscoped_child"));
        }
        let authority = &self.scope.authority;
        let activation_id = journal_activation_id(authority, activation, policy)?;
        let thread_id = format!("n1:{}", URL_SAFE_NO_PAD.encode(activation_id));
        // A journal may start an effect, so it opens only while this claim still owns the run.
        let child = Self::activate_under_root(
            self.pool.clone(),
            authority.for_thread(thread_id.clone())?,
            &self.run_root_thread_id,
            self.limits,
            Arc::clone(&self.state_writer_lease),
            &self.io,
        )
        .await?;
        Ok(NodeAttemptJournal::bound(
            Arc::new(child),
            Arc::clone(&self.state_writer_lease),
            thread_id,
            activation_id,
            policy.clone(),
        ))
    }
}

/// Whether `thread_id` is a direct fan-out child thread (`p1:` or `m1:` plus a
/// 32-byte URL-safe base64 digest, with no `/` saved-subgraph suffix).
pub(in crate::state) fn is_direct_fanout_child_thread(thread_id: &str) -> bool {
    let Some(encoded) = thread_id
        .strip_prefix("p1:")
        .or_else(|| thread_id.strip_prefix("m1:"))
    else {
        return false;
    };
    encoded.len() == 43
        && encoded
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}

/// The journal identity of one node activation. A run root's is exactly what
/// it was before saved children got journals (pinned by a test); a saved
/// child's also binds its visit (FU-248).
pub(in crate::state) fn journal_activation_id(
    authority: &CheckpointWriterAuthority,
    activation: &NodeAttemptActivation,
    policy: &NodeRecoveryPolicy,
) -> Result<[u8; 32], adk_rust::graph::GraphError> {
    activation_id_for_domain(ROOT_JOURNAL_DOMAIN, authority, activation, policy)
}

/// Domain-separated journal identity for an effectful direct node inside a
/// saved pipeline called as a tool of a non-member agent (FU-358). Separate
/// from [`journal_activation_id`] so a tool-call thread can never collide with
/// any run-root or subgraph node journal even if a model reuses a node id as a
/// call id.
pub(in crate::state) fn pipeline_tool_journal_activation_id(
    authority: &CheckpointWriterAuthority,
    activation: &NodeAttemptActivation,
    policy: &NodeRecoveryPolicy,
) -> Result<[u8; 32], adk_rust::graph::GraphError> {
    activation_id_for_domain(PIPELINE_TOOL_JOURNAL_DOMAIN, authority, activation, policy)
}

fn activation_id_for_domain(
    domain: &[u8],
    authority: &CheckpointWriterAuthority,
    activation: &NodeAttemptActivation,
    policy: &NodeRecoveryPolicy,
) -> Result<[u8; 32], adk_rust::graph::GraphError> {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(domain);
    let encoded_policy = serde_json::to_vec(policy)
        .map_err(|_| recovery_error("pipeline.node_recovery.invalid_policy"))?;
    for field in [
        authority.tenant_id.as_bytes(),
        &authority.resource_project_id.to_be_bytes(),
        &authority.projection_project_id.to_be_bytes(),
        authority.capability_id.as_bytes(),
        &authority.definition_digest,
        authority.execution_id.as_bytes(),
        &authority.generation.to_be_bytes(),
        activation.root_thread_id.as_bytes(),
        activation.node_id.as_bytes(),
        &activation.step.to_be_bytes(),
        &activation.node_digest,
        &activation.input_digest,
        &encoded_policy,
    ] {
        hash.update(
            &u64::try_from(field.len())
                .map_err(|_| recovery_error("pipeline.node_recovery.invalid_scope"))?
                .to_be_bytes(),
        );
        hash.update(field);
    }
    if let Some(visit) = &activation.visit {
        hash.update(b"\0visit\0");
        hash.update(visit);
    }
    let mut activation_id = [0; 32];
    activation_id.copy_from_slice(hash.finish().as_ref());
    Ok(activation_id)
}

/// Validate and expand the exact thread set admitted for one non-member
/// pipeline-tool call: strictly beneath `owner_thread/`, never `owner_thread`
/// itself, bounded by length and path rules.
fn validate_pipeline_tool_call_threads(
    owner_thread: &str,
    call_thread: String,
    admitted_descendants: BTreeSet<String>,
) -> Result<BTreeSet<String>, adk_rust::graph::GraphError> {
    let invalid = || recovery_error("pipeline.node_recovery.invalid_scope");
    if owner_thread.is_empty()
        || call_thread.is_empty()
        || call_thread.len() > MAX_THREAD_BYTES
        || call_thread.chars().any(char::is_control)
    {
        return Err(invalid());
    }
    let Some(suffix) = call_thread
        .strip_prefix(owner_thread)
        .and_then(|rest| rest.strip_prefix('/'))
    else {
        return Err(invalid());
    };
    if suffix.is_empty()
        || suffix
            .split('/')
            .any(|segment| segment.is_empty() || matches!(segment, "." | ".."))
    {
        return Err(invalid());
    }
    let descendant_paths = admitted_descendants.into_iter().collect::<Vec<_>>();
    let validated_descendants =
        admitted_application_paths(&descendant_paths).map_err(|_| invalid())?;
    let mut threads = BTreeSet::new();
    for path in validated_descendants {
        let descendant = format!("{call_thread}/{path}");
        if descendant.len() > MAX_THREAD_BYTES || descendant.chars().any(char::is_control) {
            return Err(invalid());
        }
        threads.insert(descendant);
    }
    threads.insert(call_thread);
    Ok(threads)
}

/// Claim-fenced [`NodeRecoveryFactory`] scoped to one non-member agent's
/// pipeline-tool call (FU-358).
struct PipelineToolJournalFactory {
    pool: PgPool,
    authority: CheckpointWriterAuthority,
    limits: CheckpointLimits,
    state_writer_lease: Arc<dyn StateWriterLease>,
    root_fence: JournalRootFence,
    io: Arc<CheckpointIoCounters>,
    admitted_threads: BTreeSet<String>,
    opened_journals: Mutex<BTreeSet<String>>,
}

#[async_trait]
impl NodeRecoveryFactory for PipelineToolJournalFactory {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, adk_rust::graph::GraphError> {
        if !self.admitted_threads.contains(&activation.root_thread_id) {
            return Err(recovery_error("pipeline.node_recovery.invalid_scope"));
        }
        if activation.visit.is_none() {
            return Err(recovery_error("pipeline.node_recovery.unscoped_child"));
        }
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| recovery_error("pipeline.node_recovery.writer_not_current"))?;
        let activation_id =
            pipeline_tool_journal_activation_id(&self.authority, activation, policy)?;
        let thread_id = format!("n1:{}", URL_SAFE_NO_PAD.encode(activation_id));
        {
            let opened = self
                .opened_journals
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if !opened.contains(&thread_id) && opened.len() >= MAX_PIPELINE_TOOL_JOURNALS_PER_CALL {
                return Err(recovery_error("pipeline.node_recovery.journal_limit"));
            }
        }
        let child = PostgresCheckpointer::activate_pipeline_tool_journal(
            self.pool.clone(),
            self.authority.for_thread(thread_id.clone())?,
            self.root_fence.clone(),
            self.limits,
            Arc::clone(&self.state_writer_lease),
            &self.io,
        )
        .await?;
        {
            let mut opened = self
                .opened_journals
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if !opened.contains(&thread_id) {
                if opened.len() >= MAX_PIPELINE_TOOL_JOURNALS_PER_CALL {
                    return Err(recovery_error("pipeline.node_recovery.journal_limit"));
                }
                opened.insert(thread_id.clone());
            }
        }
        Ok(NodeAttemptJournal::bound(
            Arc::new(child),
            Arc::clone(&self.state_writer_lease),
            thread_id,
            activation_id,
            policy.clone(),
        ))
    }
}

impl PipelineToolJournalAuthority for ApplicationCheckpointers {
    fn for_pipeline_tool_call(
        &self,
        call_thread: String,
        admitted_descendants: BTreeSet<String>,
    ) -> Result<Arc<dyn NodeRecoveryFactory>, adk_rust::graph::GraphError> {
        let root = self.root_writer()?;
        let owner_thread = &root.scope.authority.thread_id;
        let admitted_threads =
            validate_pipeline_tool_call_threads(owner_thread, call_thread, admitted_descendants)?;
        let authority = root.scope.authority.for_thread(owner_thread.clone())?;
        Ok(Arc::new(PipelineToolJournalFactory {
            pool: root.pool.clone(),
            authority,
            limits: root.limits,
            state_writer_lease: Arc::clone(&root.state_writer_lease),
            root_fence: JournalRootFence::CheckpointRoot(root.run_root_thread_id.clone()),
            io: Arc::clone(&root.io),
            admitted_threads,
            opened_journals: Mutex::default(),
        }))
    }
}

struct SessionPipelineToolJournalAuthority {
    session: Arc<PostgresSessionService>,
    limits: CheckpointLimits,
    io: Arc<CheckpointIoCounters>,
}

impl PipelineToolJournalAuthority for SessionPipelineToolJournalAuthority {
    fn for_pipeline_tool_call(
        &self,
        call_thread: String,
        admitted_descendants: BTreeSet<String>,
    ) -> Result<Arc<dyn NodeRecoveryFactory>, adk_rust::graph::GraphError> {
        let (pool, authority, session_key, state_writer_lease) =
            self.session.checkpoint_journal_binding()?;
        let admitted_threads = validate_pipeline_tool_call_threads(
            &session_key.thread_id,
            call_thread,
            admitted_descendants,
        )?;
        Ok(Arc::new(PipelineToolJournalFactory {
            pool,
            authority,
            limits: self.limits,
            state_writer_lease,
            root_fence: JournalRootFence::SessionRoot(session_key),
            io: Arc::clone(&self.io),
            admitted_threads,
            opened_journals: Mutex::default(),
        }))
    }
}

pub(in crate::state) fn session_pipeline_tool_journals(
    session: Arc<PostgresSessionService>,
    limits: CheckpointLimits,
) -> Arc<dyn PipelineToolJournalAuthority> {
    Arc::new(SessionPipelineToolJournalAuthority {
        session,
        limits,
        io: Arc::new(CheckpointIoCounters::default()),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_pipeline_tool_call_threads_enforces_owner_prefix_traversal_and_bounds() {
        let valid = validate_pipeline_tool_call_threads(
            "thread-1",
            "thread-1/call-1".to_owned(),
            BTreeSet::from(["sub".to_owned(), "sub/inner".to_owned()]),
        )
        .expect("admits call thread and valid subgraph descendants");
        assert_eq!(
            valid,
            BTreeSet::from([
                "thread-1/call-1".to_owned(),
                "thread-1/call-1/sub".to_owned(),
                "thread-1/call-1/sub/inner".to_owned(),
            ])
        );

        // Must never admit the owner root itself, foreign threads, traversal,
        // empty segments, control characters, or over-length threads.
        for bad_thread in [
            "thread-1".to_owned(),
            "thread-1/".to_owned(),
            "thread-2/call-1".to_owned(),
            "thread-10/call-1".to_owned(),
            "thread-1/..".to_owned(),
            "thread-1/a/../b".to_owned(),
            "thread-1/a//b".to_owned(),
            "thread-1/call\n1".to_owned(),
            format!("thread-1/{}", "a".repeat(MAX_THREAD_BYTES)),
        ] {
            assert!(
                validate_pipeline_tool_call_threads(
                    "thread-1",
                    bad_thread.clone(),
                    BTreeSet::new()
                )
                .is_err(),
                "must refuse invalid call thread {bad_thread:?}"
            );
        }

        // Invalid descendants (traversal, missing ancestor, > 128 bound) fail closed.
        for bad_descendants in [
            BTreeSet::from(["../escape".to_owned()]),
            BTreeSet::from(["sub/inner".to_owned()]), // missing ancestor "sub"
            (0..=128)
                .map(|index| format!("node_{index}"))
                .collect::<BTreeSet<_>>(),
        ] {
            assert!(
                validate_pipeline_tool_call_threads(
                    "thread-1",
                    "thread-1/call-1".to_owned(),
                    bad_descendants
                )
                .is_err()
            );
        }
    }

    #[test]
    fn direct_fanout_child_thread_classifier_enforces_exact_prefix_length_and_charset() {
        // Golden derived ids from fanout_children::tests.
        assert!(is_direct_fanout_child_thread(
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
        ));
        assert!(is_direct_fanout_child_thread(
            "m1:KRe5343ZNGVAvfuZ-MJ-RAfTgyFfIxbwR_9OVB4NU24"
        ));
        for refused in [
            "",
            "thread-1",
            "thread-1/delegate",
            "p1:",
            "m1:",
            // Saved child beneath a fan-out branch must stay visit-scoped (FU-248).
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q/fetch_short",
            "m1:KRe5343ZNGVAvfuZ-MJ-RAfTgyFfIxbwR_9OVB4NU24/silver_batch",
            // Wrong prefix.
            "n1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q",
            // Length limit - 1 (42) and limit + 1 (44).
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6",
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Qa",
            // Non-URL-safe base64 / padding / control chars.
            "p1:Z8y5h+dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q",
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6=",
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzx\n6Q",
        ] {
            assert!(
                !is_direct_fanout_child_thread(refused),
                "must refuse {refused:?}"
            );
        }
    }

    #[test]
    fn fanout_sibling_journals_are_isolated_per_child_thread() {
        let auth = CheckpointWriterAuthority::new(
            "tenant-1".to_owned(),
            7,
            7,
            crate::state::postgres_checkpointer::APPLICATION_CAPABILITY_ID,
            [0x11; 32],
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q".to_owned(),
            "execution-1".to_owned(),
            1,
            "claim-1".to_owned(),
            1,
            1,
            1_700_000_000_000_000,
            "workload-1".to_owned(),
            "producer-1".to_owned(),
            [0x22; 32],
        )
        .expect("authority");
        let policy = NodeRecoveryPolicy::default();
        let make = |thread: &str, visit: Option<[u8; 32]>| NodeAttemptActivation {
            root_thread_id: thread.to_owned(),
            node_id: "validate".to_owned(),
            step: 0,
            node_digest: [0x33; 32],
            input_digest: [0x44; 32],
            visit,
        };
        let branch_a = journal_activation_id(
            &auth,
            &make("p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q", None),
            &policy,
        )
        .expect("branch A");
        let branch_b = journal_activation_id(
            &auth,
            &make("p1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", None),
            &policy,
        )
        .expect("branch B");
        let item_0 = journal_activation_id(
            &auth,
            &make(
                "m1:KRe5343ZNGVAvfuZ-MJ-RAfTgyFfIxbwR_9OVB4NU24",
                Some([0x55; 32]),
            ),
            &policy,
        )
        .expect("item 0");
        let item_1 = journal_activation_id(
            &auth,
            &make(
                "m1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
                Some([0x55; 32]),
            ),
            &policy,
        )
        .expect("item 1");
        assert_ne!(branch_a, branch_b);
        assert_ne!(item_0, item_1);
        assert_ne!(branch_a, item_0);
    }
}
