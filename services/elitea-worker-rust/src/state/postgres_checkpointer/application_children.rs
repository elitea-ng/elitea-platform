//! Route only admitted application-node threads to independently fenced adapters.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;

use super::{CheckpointWriterAuthority, PostgresCheckpointError, PostgresCheckpointer};
use crate::agents::graph::{
    ParallelActivation, ParallelBranchDefinition, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildRequest, ParentHead, ParentSaveProbe, PreparedChildCheckpoint,
};

pub(crate) struct ApplicationCheckpointers {
    threads: BTreeMap<String, PostgresCheckpointer>,
    branch_paths: ApplicationBranchCatalog,
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
    fn assemble(
        writers: impl IntoIterator<Item = PostgresCheckpointer>,
        branch_paths: ApplicationBranchCatalog,
    ) -> Self {
        Self {
            threads: writers
                .into_iter()
                .map(|writer| (writer.scope.authority.thread_id.clone(), writer))
                .collect(),
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
        let paths = self
            .branch_paths
            .for_owned_node(&activation.root_thread_id, branch.node())?;
        // The compiler admits the branch identity and frozen definition first.
        // The parent adapter adds its opaque claim scope to the hashed lineage.
        let child = parent
            .activate_parallel_branch(activation, branch, ordinal, input_digest, origin)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        let child = child.with_application_paths(paths).await?;
        let admitted_threads = child.threads.keys().cloned().collect();
        Ok(ParallelChildCheckpoint {
            thread_id,
            checkpointer: Arc::new(child),
            admitted_threads,
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
            let paths = self
                .branch_paths
                .for_owned_node(&activation.root_thread_id, request.branch.node())?;
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
                let child = ApplicationCheckpointers::assemble(family, catalog);
                let admitted_threads = child.threads.keys().cloned().collect();
                PreparedChildCheckpoint {
                    child: ParallelChildCheckpoint {
                        thread_id,
                        checkpointer: Arc::new(child),
                        admitted_threads,
                    },
                    latest,
                }
            })
            .collect())
    }
}

impl ParallelCheckpointAuthority for ApplicationCheckpointers {}

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

#[async_trait]
impl crate::agents::graph::MapChildCheckpointerFactory for ApplicationCheckpointers {
    fn execution_identity(
        &self,
        root_thread: &str,
    ) -> Result<crate::agents::graph::MapExecutionIdentity, GraphError> {
        crate::agents::graph::MapChildCheckpointerFactory::execution_identity(
            self.for_thread(root_thread)?,
            root_thread,
        )
    }
    fn item_thread_id(
        &self,
        activation: &crate::agents::graph::MapActivation,
        item: &crate::agents::graph::FrozenMapItem,
        worker: &str,
        origin: &crate::agents::graph::MapExecutionIdentity,
    ) -> Result<String, GraphError> {
        crate::agents::graph::MapChildCheckpointerFactory::item_thread_id(
            self.for_thread(&activation.root_thread_id)?,
            activation,
            item,
            worker,
            origin,
        )
    }
    async fn for_item(
        &self,
        activation: &crate::agents::graph::MapActivation,
        item: &crate::agents::graph::FrozenMapItem,
        worker: &str,
        kind: crate::agents::graph::MapWorkerKind,
        origin: &crate::agents::graph::MapExecutionIdentity,
    ) -> Result<crate::agents::graph::MapChildCheckpoint, GraphError> {
        let parent = self.for_thread(&activation.root_thread_id)?;
        let paths = self
            .branch_paths
            .for_map_worker(&activation.root_thread_id, worker, kind)?;
        let child = parent
            .activate_map_item(activation, item, worker, origin)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        let child = child.with_application_paths(paths).await?;
        let admitted_threads = child.threads.keys().cloned().collect();
        Ok(crate::agents::graph::MapChildCheckpoint {
            thread_id,
            checkpointer: Arc::new(child),
            admitted_threads,
        })
    }
}
impl crate::agents::graph::map_authority::sealed::Sealed for ApplicationCheckpointers {}
impl crate::agents::graph::MapCheckpointAuthority for ApplicationCheckpointers {}

impl ApplicationBranchCatalog {
    fn for_map_worker(
        &self,
        parent: &str,
        worker: &str,
        kind: crate::agents::graph::MapWorkerKind,
    ) -> Result<&[String], GraphError> {
        match kind {
            crate::agents::graph::MapWorkerKind::StateModifier => Ok(&[]),
            crate::agents::graph::MapWorkerKind::Application => self.for_owned_node(parent, worker),
        }
    }
}

#[cfg(test)]
mod map_catalog_tests {
    use super::*;
    use crate::agents::graph::MapWorkerKind;

    #[test]
    fn map_state_modifier_has_no_synthetic_application_authority() {
        let paths = BTreeSet::from(["ordinary_agent"]);
        let catalog = ApplicationBranchCatalog::from_admitted_paths("root", &paths);
        assert!(
            catalog
                .for_map_worker("root", "render", MapWorkerKind::StateModifier)
                .unwrap()
                .is_empty()
        );
        assert!(
            catalog
                .for_map_worker("root", "render", MapWorkerKind::Application)
                .is_err()
        );
    }

    #[test]
    fn map_application_requires_exact_original_owned_family() {
        let paths = BTreeSet::from(["render", "render/inner", "other"]);
        let catalog = ApplicationBranchCatalog::from_admitted_paths("root", &paths);
        assert_eq!(
            catalog
                .for_map_worker("root", "render", MapWorkerKind::Application)
                .unwrap(),
            ["render", "render/inner"]
        );
        for (parent, worker) in [
            ("foreign", "render"),
            ("root", "inner"),
            ("root", "render/inner"),
        ] {
            assert!(
                catalog
                    .for_map_worker(parent, worker, MapWorkerKind::Application)
                    .is_err()
            );
        }
    }
}

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
