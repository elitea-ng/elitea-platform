//! Route only admitted application-node threads to independently fenced adapters.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;

use super::{CheckpointWriterAuthority, PostgresCheckpointError, PostgresCheckpointer};
use crate::agents::application_pipeline::PipelineToolCallLineage;
use crate::agents::graph::tool_pipeline_threads::{
    AdmittedToolPipeline, MAX_TOOL_PIPELINE_THREADS_PER_MEMBER, TOOL_LEDGER_KEY,
    TOOL_LEDGER_SEGMENT, ToolPipelineAdmission, ToolPipelineThreads, tool_pipeline_thread,
};
use crate::agents::graph::{
    ParallelActivation, ParallelBranchDefinition, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildRequest, ParentHead, ParentSaveProbe, PreparedChildCheckpoint,
};

pub(crate) struct ApplicationCheckpointers {
    /// The family's own root: the run root or one fan-out member's thread.
    root_thread: String,
    threads: BTreeMap<String, PostgresCheckpointer>,
    branch_paths: ApplicationBranchCatalog,
    /// FU-291: the nested pipeline threads this family's Agents admitted. The
    /// one set the member's event projector reads (C1).
    tool_threads: Arc<ToolPipelineThreads>,
    /// The member's ledger writer, activated on the first admission (review B).
    tool_ledger: tokio::sync::OnceCell<PostgresCheckpointer>,
}

/// Exact descendant families derived only from the admitted application paths.
/// A branch lookup does not authorize a prefix or accept a caller-supplied path.
struct ApplicationBranchCatalog {
    by_parent: BTreeMap<String, BTreeMap<String, Vec<String>>>,
}

impl ApplicationBranchCatalog {
    fn from_admitted_paths(root_thread: &str, paths: &BTreeSet<&str>) -> Self {
        let mut by_parent: BTreeMap<String, BTreeMap<String, Vec<String>>> = BTreeMap::new();
        for path in paths {
            let components = path.split('/').collect::<Vec<_>>();
            for owned_position in 0..components.len() {
                let parent_thread = if owned_position == 0 {
                    root_thread.to_owned()
                } else {
                    format!("{root_thread}/{}", components[..owned_position].join("/"))
                };
                by_parent
                    .entry(parent_thread)
                    .or_default()
                    .entry(components[owned_position].to_owned())
                    .or_default()
                    .push(components[owned_position..].join("/"));
            }
        }
        Self { by_parent }
    }

    /// The family of one fan-out child. A child whose owned node runs no
    /// application subgraph (`path_free`: a State Modifier or Code Map worker
    /// or Parallel branch, W3-2) has none and is given no thread (least
    /// authority). A child that owns one (an Agent, an inline LLM branch or
    /// worker) must have been admitted by path at assembly
    /// (`composition_paths`, PM-4); a miss is refused (R3), never widened.
    fn for_child(
        &self,
        activation: &ParallelActivation,
        node: &str,
        path_free: bool,
    ) -> Result<&[String], GraphError> {
        match self.for_owned_node(&activation.root_thread_id, node) {
            Err(_) if path_free => Ok(&[]),
            found => found,
        }
    }

    fn for_owned_node(&self, parent_thread: &str, node: &str) -> Result<&[String], GraphError> {
        self.by_parent
            .get(parent_thread)
            .and_then(|nodes| nodes.get(node))
            .map(Vec::as_slice)
            .ok_or_else(|| {
                PostgresCheckpointError::InvalidScope(
                    "the owned application checkpoint family is not admitted",
                )
                .into()
            })
    }
}

impl PostgresCheckpointer {
    #[cfg(test)]
    pub(crate) async fn with_application_children(
        self,
        node_ids: impl Iterator<Item = &str>,
    ) -> Result<ApplicationCheckpointers, PostgresCheckpointError> {
        let paths = node_ids.map(str::to_owned).collect::<Vec<_>>();
        self.with_application_paths(&paths).await
    }

    /// Activate only complete paths derived from frozen admitted definitions.
    /// All descendant writers are activated in one transaction.
    pub(crate) async fn with_application_paths(
        self,
        paths: &[String],
    ) -> Result<ApplicationCheckpointers, PostgresCheckpointError> {
        // Validate all derived authorities before the first activation write.
        let (catalog, mut authorities) =
            self.application_family(self.scope.authority.thread_id.clone(), paths)?;
        authorities.remove(0);
        let descendants = if authorities.is_empty() {
            Vec::new()
        } else {
            self.activate_children(authorities).await?
        };
        Ok(ApplicationCheckpointers::assemble(
            std::iter::once(self).chain(descendants),
            catalog,
        ))
    }

    /// One branch family: its root thread first, then every admitted path.
    fn application_family(
        &self,
        root_thread: String,
        paths: &[String],
    ) -> Result<(ApplicationBranchCatalog, Vec<CheckpointWriterAuthority>), PostgresCheckpointError>
    {
        let paths = admitted_application_paths(paths)?;
        let catalog = ApplicationBranchCatalog::from_admitted_paths(&root_thread, &paths);
        let mut authorities = Vec::with_capacity(paths.len() + 1);
        for path in &paths {
            authorities.push(
                self.scope
                    .authority
                    .for_thread(format!("{root_thread}/{path}"))?,
            );
        }
        authorities.insert(0, self.scope.authority.for_thread(root_thread)?);
        Ok((catalog, authorities))
    }
}

fn admitted_application_paths(paths: &[String]) -> Result<BTreeSet<&str>, PostgresCheckpointError> {
    let mut admitted = BTreeSet::new();
    if paths.len() > 128 {
        return Err(PostgresCheckpointError::InvalidScope(
            "the application checkpoint family exceeds its bound",
        ));
    }
    for path in paths {
        let components = path.split('/').collect::<Vec<_>>();
        if components.len() > 3
            || components.iter().any(|component| {
                component.is_empty()
                    || matches!(*component, "." | "..")
                    || component.len() > 128
                    || !component.bytes().all(|byte| {
                        byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':')
                    })
            })
            || !admitted.insert(path.as_str())
        {
            return Err(PostgresCheckpointError::InvalidScope(
                "the application checkpoint path is invalid",
            ));
        }
    }
    for path in &admitted {
        let mut ancestor = *path;
        while let Some((parent, _)) = ancestor.rsplit_once('/') {
            if !admitted.contains(parent) {
                return Err(PostgresCheckpointError::InvalidScope(
                    "the application checkpoint ancestor is not admitted",
                ));
            }
            ancestor = parent;
        }
    }
    Ok(admitted)
}

impl ApplicationCheckpointers {
    /// `writers` holds the family's root first (`application_family`).
    fn assemble(
        writers: impl IntoIterator<Item = PostgresCheckpointer>,
        branch_paths: ApplicationBranchCatalog,
    ) -> Self {
        let mut root_thread = None;
        let threads = writers
            .into_iter()
            .map(|writer| {
                let thread = writer.scope.authority.thread_id.clone();
                root_thread.get_or_insert_with(|| thread.clone());
                (thread, writer)
            })
            .collect::<BTreeMap<_, _>>();
        Self {
            root_thread: root_thread.unwrap_or_default(),
            tool_threads: Arc::new(ToolPipelineThreads::new(threads.len())),
            tool_ledger: tokio::sync::OnceCell::new(),
            threads,
            branch_paths,
        }
    }

    pub(in crate::state) fn for_thread(
        &self,
        thread_id: &str,
    ) -> Result<&PostgresCheckpointer, GraphError> {
        self.threads.get(thread_id).ok_or_else(|| {
            PostgresCheckpointError::InvalidScope(
                "the requested thread is not an admitted application checkpoint",
            )
            .into()
        })
    }
}

#[async_trait]
impl Checkpointer for ApplicationCheckpointers {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.for_thread(&checkpoint.thread_id)?
            .save(checkpoint)
            .await
    }

    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.for_thread(thread_id)?.load(thread_id).await
    }

    async fn load_by_id(&self, checkpoint_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        // Every lookup remains restricted to one admitted, fenced lineage.
        for checkpointer in self.threads.values() {
            if let Some(checkpoint) = checkpointer.load_by_id(checkpoint_id).await? {
                return Ok(Some(checkpoint));
            }
        }
        Ok(None)
    }

    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.for_thread(thread_id)?.list(thread_id).await
    }

    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.for_thread(thread_id)?.delete(thread_id).await
    }

    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.for_thread(thread_id)?.prune(thread_id, policy).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for ApplicationCheckpointers {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if expected_latest.is_some_and(|parent| parent.thread_id != candidate.thread_id) {
            return Err(PostgresCheckpointError::InvalidScope(
                "the checkpoint append crosses application threads",
            )
            .into());
        }
        self.for_thread(&candidate.thread_id)?
            .append_after(expected_latest, candidate)
            .await
    }

    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        self.for_thread(thread_id)?
            .probe_parent(thread_id, candidate_id)
            .await
    }

    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if expected.is_some_and(|parent| parent.thread_id != candidate.thread_id) {
            return Err(PostgresCheckpointError::InvalidScope(
                "the checkpoint append crosses application threads",
            )
            .into());
        }
        self.for_thread(&candidate.thread_id)?
            .append_after_head(expected, candidate)
            .await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for ApplicationCheckpointers {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<crate::agents::graph::ParallelChildOrigin, GraphError> {
        self.for_thread(&activation.root_thread_id)?
            .child_origin(activation)
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.for_thread(&activation.root_thread_id)?
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let parent = self.for_thread(&activation.root_thread_id)?;
        // A single-branch activation knows nothing of the owned node's kind:
        // fail closed (R3).
        let paths = self
            .branch_paths
            .for_child(activation, branch.node(), false)?;
        // The compiler admits the branch identity and frozen definition first.
        // The parent adapter adds its opaque claim scope to the hashed lineage.
        let child = parent
            .activate_parallel_branch(activation, branch, ordinal, input_digest, origin)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        let child = Arc::new(child.with_application_paths(paths).await?);
        let admitted_threads = child.threads.keys().cloned().collect();
        Ok(ParallelChildCheckpoint {
            thread_id,
            checkpointer: child.clone(),
            admitted_threads,
            node_recovery: Some(child.clone()),
            tool_pipelines: Some(child),
        })
    }

    /// Two transactions for every branch family together: one activation of
    /// each branch root and its admitted application threads, one read of the
    /// branch roots' latest receipts.
    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        let parent = self.for_thread(&activation.root_thread_id)?;
        let mut catalogs = Vec::with_capacity(children.len());
        let mut sizes = Vec::with_capacity(children.len());
        let mut authorities = Vec::new();
        for request in children {
            let paths = self.branch_paths.for_child(
                activation,
                request.branch.node(),
                request.path_free,
            )?;
            let thread_id = parent.branch_thread_id(
                activation,
                request.branch,
                request.ordinal,
                request.input_digest,
                origin,
            )?;
            let (catalog, family) = parent.application_family(thread_id, paths)?;
            catalogs.push(catalog);
            sizes.push(family.len());
            authorities.extend(family);
        }
        let mut writers = parent.activate_children(authorities).await?.into_iter();
        let families = sizes
            .into_iter()
            .map(|size| writers.by_ref().take(size).collect::<Vec<_>>())
            .collect::<Vec<_>>();
        let roots = families
            .iter()
            .map(|family| {
                family
                    .first()
                    .ok_or(PostgresCheckpointError::CorruptStoredState)
            })
            .collect::<Result<Vec<_>, _>>()?;
        let latest = parent.load_children_latest(&roots).await?;
        Ok(families
            .into_iter()
            .zip(catalogs)
            .zip(latest)
            .map(|((family, catalog), latest)| {
                let thread_id = family
                    .first()
                    .map(|root| root.scope.authority.thread_id.clone())
                    .unwrap_or_default();
                let child = Arc::new(ApplicationCheckpointers::assemble(family, catalog));
                let admitted_threads = child.threads.keys().cloned().collect();
                PreparedChildCheckpoint {
                    child: ParallelChildCheckpoint {
                        thread_id,
                        checkpointer: child.clone(),
                        admitted_threads,
                        node_recovery: Some(child.clone()),
                        tool_pipelines: Some(child),
                    },
                    latest,
                }
            })
            .collect())
    }
}

impl ParallelCheckpointAuthority for ApplicationCheckpointers {}

#[async_trait]
impl crate::agents::graph::node_recovery_runtime::NodeRecoveryFactory for ApplicationCheckpointers {
    async fn open(
        &self,
        activation: &crate::agents::graph::node_recovery_runtime::NodeAttemptActivation,
        policy: &crate::agents::graph::node_recovery::NodeRecoveryPolicy,
    ) -> Result<crate::agents::graph::node_recovery_runtime::NodeAttemptJournal, GraphError> {
        crate::agents::graph::node_recovery_runtime::NodeRecoveryFactory::open(
            self.for_thread(&activation.root_thread_id)?,
            activation,
            policy,
        )
        .await
    }
}

/// The admitted nested threads a member ledger row holds: each one of this
/// member's, at most the per-member bound. Anything else is corrupt.
fn ledger_threads(
    member: &str,
    latest: Option<&Checkpoint>,
) -> Result<Vec<String>, PostgresCheckpointError> {
    let Some(latest) = latest else {
        return Ok(Vec::new());
    };
    let threads: Vec<String> = latest
        .state
        .get(TOOL_LEDGER_KEY)
        .cloned()
        .map(serde_json::from_value)
        .transpose()
        .map_err(|_| PostgresCheckpointError::CorruptStoredState)?
        .unwrap_or_default();
    let prefix = format!("{member}/");
    if threads.len() > MAX_TOOL_PIPELINE_THREADS_PER_MEMBER
        || threads
            .iter()
            .any(|thread| !thread.starts_with(&prefix) || !thread.contains("/pt1:"))
    {
        return Err(PostgresCheckpointError::CorruptStoredState);
    }
    Ok(threads)
}

impl ApplicationCheckpointers {
    /// FU-291 review B: the member's admitted nested threads live in one
    /// ledger thread (`<member>/pt1=ledger`), read by its exact thread (the
    /// `thread_order` index) and appended only after its latest row, under
    /// the ledger writer's row lock (`append_after`). Two admissions of one
    /// member, from any process or family object, serialize there: the bound
    /// holds across takeovers and races. `thread` already in the ledger
    /// re-admits; past the bound the admission is refused typed.
    async fn reserve_in_ledger(&self, thread: &str) -> Result<(), GraphError> {
        let ledger_thread = format!("{}{TOOL_LEDGER_SEGMENT}", self.root_thread);
        let ledger = self
            .tool_ledger
            .get_or_try_init(|| async {
                let root = self.for_thread(&self.root_thread)?;
                root.activate_fanout_child(ledger_thread.clone()).await
            })
            .await?;
        // Each conflict means another admission appended; at most the bound
        // can, so this ends.
        for _ in 0..=MAX_TOOL_PIPELINE_THREADS_PER_MEMBER + 1 {
            let latest = ledger.load(&ledger_thread).await?;
            let mut threads = ledger_threads(&self.root_thread, latest.as_ref())?;
            if threads.iter().any(|admitted| admitted == thread) {
                return Ok(());
            }
            self.tool_threads.check_room_for(threads.len())?;
            threads.push(thread.to_owned());
            let candidate = Checkpoint::new(
                &ledger_thread,
                std::collections::HashMap::from([(
                    TOOL_LEDGER_KEY.to_owned(),
                    serde_json::json!(threads),
                )]),
                latest.as_ref().map_or(0, |row| row.step + 1),
                Vec::new(),
            );
            match ParallelCheckpointAppender::append_after(ledger, latest.as_ref(), &candidate)
                .await
            {
                Ok(_) => return Ok(()),
                Err(error)
                    if crate::state::checkpoint_error_code(&error)
                        .is_some_and(|(code, _)| code == "checkpoint.conflict") => {}
                Err(error) => return Err(error),
            }
        }
        Err(PostgresCheckpointError::CheckpointConflict.into())
    }
}

#[async_trait]
impl ToolPipelineAdmission for ApplicationCheckpointers {
    fn tool_threads(&self) -> Arc<ToolPipelineThreads> {
        Arc::clone(&self.tool_threads)
    }

    /// D-3: the Agent thread is one of this family's own threads (never its
    /// root), the nested thread is derived from it and the captured lineage,
    /// and its writer is activated under the Agent writer's exact claim and
    /// fence before it is registered. Re-admission in this process returns the
    /// registered writer; after a takeover the same derivation re-activates it.
    async fn admit_tool_pipeline(
        &self,
        agent_thread: &str,
        lineage: &PipelineToolCallLineage,
    ) -> Result<AdmittedToolPipeline, GraphError> {
        let agent = self
            .threads
            .get(agent_thread)
            .filter(|_| agent_thread != self.root_thread)
            .ok_or(PostgresCheckpointError::InvalidScope(
                "the nested pipeline's Agent thread is not admitted in this family",
            ))?;
        let thread_id = tool_pipeline_thread(agent_thread, lineage)?;
        if self.threads.contains_key(&thread_id) {
            return Err(PostgresCheckpointError::InvalidScope(
                "the nested pipeline thread collides with an admitted thread",
            )
            .into());
        }
        let _admission = self.tool_threads.admission().await;
        if let Some(admitted) = self.tool_threads.admitted(&thread_id, agent_thread) {
            return Ok(admitted);
        }
        self.reserve_in_ledger(&thread_id).await?;
        // The slot is durable before the writer: a crash in between leaves a
        // reserved thread the same call re-admits, never an unledgered row.
        let writer = agent.activate_fanout_child(thread_id.clone()).await?;
        let admitted = AdmittedToolPipeline {
            thread_id,
            agent_thread: agent_thread.to_owned(),
            writer: Arc::new(writer),
        };
        self.tool_threads.register(admitted.clone());
        Ok(admitted)
    }
}

#[cfg(test)]
mod tests {
    use super::{ApplicationBranchCatalog, admitted_application_paths};

    #[test]
    fn branch_catalog_selects_only_exact_admitted_parent_and_owned_node() {
        let paths = [
            "branch",
            "branch/leaf",
            "branch_suffix",
            "nested",
            "nested/branch",
            "nested/branch/leaf",
        ]
        .map(str::to_owned);
        let admitted = admitted_application_paths(&paths).unwrap();
        let catalog = ApplicationBranchCatalog::from_admitted_paths("root", &admitted);
        assert_eq!(
            catalog.for_owned_node("root", "branch").unwrap(),
            ["branch", "branch/leaf"]
        );
        assert_eq!(
            catalog.for_owned_node("root/nested", "branch").unwrap(),
            ["branch", "branch/leaf"]
        );
        assert_eq!(
            catalog.for_owned_node("root", "branch_suffix").unwrap(),
            ["branch_suffix"]
        );
        for (parent, node) in [
            ("other", "branch"),
            ("root/unknown", "branch"),
            ("root/branch", "branch"),
            ("root", "bran"),
            ("root", "branch/leaf"),
            ("root", "branch_suffix/leaf"),
            ("root", "nested/branch"),
        ] {
            assert!(catalog.for_owned_node(parent, node).is_err());
        }
    }

    /// B1 (W3-2): a fan-out child whose owned node has no application path
    /// (a Code or State Modifier node), in a Parallel as in a Map, gets an
    /// empty family; an admitted node keeps exactly its own paths.
    #[test]
    fn a_path_free_child_gets_no_family_in_a_parallel_and_a_map() {
        let paths = ["agent_branch", "agent_branch/leaf"].map(str::to_owned);
        let admitted = admitted_application_paths(&paths).unwrap();
        let catalog = ApplicationBranchCatalog::from_admitted_paths("root", &admitted);
        let parallel = crate::agents::graph::ParallelActivation {
            root_thread_id: "root".to_owned(),
            node_id: "gather".to_owned(),
            step: 1,
            config_digest: [1; 32],
            map: None,
        };
        let mut map_value = serde_json::to_value(&parallel).unwrap();
        map_value["map"] =
            serde_json::json!({"worker_digest": vec![2_u8; 32], "source_digest": vec![3_u8; 32]});
        let map: crate::agents::graph::ParallelActivation =
            serde_json::from_value(map_value).unwrap();
        assert!(map.map.is_some());
        for activation in [&parallel, &map] {
            assert_eq!(
                catalog.for_child(activation, "code_branch", true).unwrap(),
                [] as [&str; 0]
            );
            assert_eq!(
                catalog
                    .for_child(activation, "agent_branch", false)
                    .unwrap(),
                ["agent_branch", "agent_branch/leaf"]
            );
            // R3: a child that owns an application subgraph (an Agent or an
            // inline LLM branch) with no admitted family is refused, never
            // given an empty one.
            assert!(
                catalog
                    .for_child(activation, "unadmitted_agent", false)
                    .is_err()
            );
        }
    }

    #[test]
    fn exact_bounded_paths_require_each_admitted_ancestor() {
        let paths = [
            "child",
            "child/grandchild",
            "child/grandchild/leaf",
            "sibling",
        ]
        .map(str::to_owned);
        assert_eq!(admitted_application_paths(&paths).unwrap().len(), 4);
        for invalid in [
            vec!["child/grandchild"],
            vec!["child", "child"],
            vec!["child", "child//grandchild"],
            vec!["../other"],
            vec!["..", "../other"],
            vec!["."],
            vec![
                "child",
                "child/grandchild",
                "child/grandchild/leaf",
                "child/grandchild/leaf/deeper",
            ],
        ] {
            assert!(
                admitted_application_paths(
                    &invalid.into_iter().map(str::to_owned).collect::<Vec<_>>()
                )
                .is_err()
            );
        }
        let oversized = (0..129).map(|n| format!("node_{n}")).collect::<Vec<_>>();
        assert!(admitted_application_paths(&oversized).is_err());
    }
}
