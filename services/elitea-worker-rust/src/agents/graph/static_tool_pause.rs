//! Exact static leaf selection for ordinary-parent saved pipeline tools.

use std::collections::{BTreeMap, BTreeSet};

use adk_rust::Event;
use adk_rust::graph::{Checkpoint, State};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::compiler::PipelineDefinition;
use super::static_pause::{STATIC_AFTER_CHECKPOINTS_STATE_KEY, STATIC_TEXT_RESUME_STATE_KEY};
use crate::agents::events::{PipelineStaticEventBinding, pipeline_static_event_binding};
use crate::agents::pipeline::composition::PipelineCheckpointCatalog;

pub(crate) const STATIC_TOOL_RESUME_META_KEY: &str = "pipeline_static_tool_resume_v1";
const MAX_STATIC_TOOL_DECISIONS: usize = 16;
const MAX_STATIC_TEXT_BYTES: usize = 8 * 1024;

/// The owner exposes decoded checkpoints without applying dynamic frontier rules.
pub(crate) trait PipelineCheckpointFamilyView {
    fn root(&self) -> &Checkpoint;
    fn checkpoint(&self, thread: &str) -> Option<&Checkpoint>;
    fn catalog(&self) -> Option<&PipelineCheckpointCatalog>;
}

#[derive(Clone, Copy)]
pub(crate) struct StaticToolDefinitionSet<'a> {
    /// Exact absolute checkpoint threads, including the saved tool root.
    pub(crate) definitions_by_exact_path: &'a BTreeMap<String, PipelineDefinition>,
    /// Rebuilt from the original frozen saved application/version tree.
    pub(crate) checkpoint_catalog: &'a PipelineCheckpointCatalog,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct StaticToolDecision {
    pause_id: String,
    child_thread_id: String,
    tool_call_id: String,
    action: String,
    value: String,
}

impl StaticToolDecision {
    pub(crate) fn pause_id(&self) -> &str {
        &self.pause_id
    }
    pub(crate) fn child_thread_id(&self) -> &str {
        &self.child_thread_id
    }
    pub(crate) fn tool_call_id(&self) -> &str {
        &self.tool_call_id
    }
    fn validate(&self) -> bool {
        self.pause_id
            .strip_prefix("pipeline-static:")
            .is_some_and(super::static_pause::valid_digest)
            && valid_identity(&self.child_thread_id, 1024)
            && valid_identity(&self.tool_call_id, 512)
            && self.action == "continue"
            && !self.value.trim().is_empty()
            && self.value.len() <= MAX_STATIC_TEXT_BYTES
            && !self.value.contains('\0')
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StaticToolDecisionEnvelope {
    revision: u8,
    decisions: Vec<StaticToolDecision>,
}

/// Explicit leaf decisions cannot be inferred from ordinary `should_continue`.
pub(crate) fn parse_static_tool_decisions(
    meta: &serde_json::Map<String, Value>,
) -> Result<Option<Vec<StaticToolDecision>>, StaticToolPauseError> {
    let Some(raw) = meta.get(STATIC_TOOL_RESUME_META_KEY) else {
        return Ok(None);
    };
    let envelope: StaticToolDecisionEnvelope =
        serde_json::from_value(raw.clone()).map_err(|_| StaticToolPauseError::Invalid)?;
    if envelope.revision != 1
        || envelope.decisions.is_empty()
        || envelope.decisions.len() > MAX_STATIC_TOOL_DECISIONS
    {
        return Err(StaticToolPauseError::Invalid);
    }
    let total = envelope
        .decisions
        .iter()
        .try_fold(0usize, |sum, decision| {
            sum.checked_add(decision.value.len())
        })
        .ok_or(StaticToolPauseError::Invalid)?;
    if total > 64 * 1024 {
        return Err(StaticToolPauseError::Invalid);
    }
    let mut pauses = BTreeSet::new();
    let mut children = BTreeSet::new();
    for decision in &envelope.decisions {
        if !decision.validate()
            || !pauses.insert(&decision.pause_id)
            || !children.insert((&decision.child_thread_id, &decision.tool_call_id))
        {
            return Err(StaticToolPauseError::Invalid);
        }
    }
    Ok(Some(envelope.decisions))
}

/// Constructed from the original durable event after stripping private wrappers.
/// The coordinator owns the original parent model-call and ordinal lookup.
pub(crate) struct StaticToolPauseBinding {
    graph: PipelineStaticEventBinding,
    pause_id: String,
    root_thread_id: String,
    parent_call_id: String,
    original_batch_event_id: String,
    original_ordinal: usize,
}

impl StaticToolPauseBinding {
    pub(crate) fn from_projected_event(
        event: &Event,
        tool_name: &str,
        root_thread_id: &str,
        parent_call_id: &str,
        original_batch_event_id: &str,
        original_ordinal: usize,
    ) -> Result<Self, StaticToolPauseError> {
        if !valid_identity(parent_call_id, 512)
            || !valid_identity(original_batch_event_id, 512)
            || original_ordinal == 0
            || original_ordinal > 16
        {
            return Err(StaticToolPauseError::Invalid);
        }
        let graph = pipeline_static_event_binding(event, tool_name, root_thread_id)
            .map_err(|_| StaticToolPauseError::Corrupt)?;
        let proof = graph
            .public_proof(event)
            .map_err(|_| StaticToolPauseError::Corrupt)?;
        let pause_id = proof
            .get("pause_id")
            .and_then(Value::as_str)
            .ok_or(StaticToolPauseError::Corrupt)?
            .to_owned();
        Ok(Self {
            graph,
            pause_id,
            root_thread_id: root_thread_id.to_owned(),
            parent_call_id: parent_call_id.to_owned(),
            original_batch_event_id: original_batch_event_id.to_owned(),
            original_ordinal,
        })
    }
    pub(crate) fn pause_id(&self) -> &str {
        &self.pause_id
    }
    pub(crate) fn root_thread_id(&self) -> &str {
        &self.root_thread_id
    }
    pub(crate) fn parent_call_id(&self) -> &str {
        &self.parent_call_id
    }
    pub(crate) fn original_batch_event_id(&self) -> &str {
        &self.original_batch_event_id
    }
    pub(crate) const fn original_ordinal(&self) -> usize {
        self.original_ordinal
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum StaticToolPauseError {
    Invalid,
    Corrupt,
    Stale,
}

/// The only result allowed to become a `PipelineToolResume`. Fields stay private.
pub(crate) struct ValidatedStaticToolContinuation {
    pause_id: String,
    root_checkpoint: Checkpoint,
    resume_state: State,
    proven_checkpoint_ids: BTreeMap<String, String>,
    normalization_ids: BTreeMap<String, String>,
    original_batch_event_id: String,
    original_ordinal: usize,
}
impl ValidatedStaticToolContinuation {
    pub(crate) fn pause_id(&self) -> &str {
        &self.pause_id
    }
    pub(crate) fn root_checkpoint(&self) -> &Checkpoint {
        &self.root_checkpoint
    }
    pub(crate) fn resume_state(&self) -> &State {
        &self.resume_state
    }
    pub(crate) fn proven_checkpoint_ids(&self) -> &BTreeMap<String, String> {
        &self.proven_checkpoint_ids
    }
    pub(crate) fn normalization_ids(&self) -> &BTreeMap<String, String> {
        &self.normalization_ids
    }
    pub(crate) fn original_batch_event_id(&self) -> &str {
        &self.original_batch_event_id
    }
    pub(crate) const fn original_ordinal(&self) -> usize {
        self.original_ordinal
    }
}

/// Validate root, every ancestor, leaf policy, version lineage, and exact selection.
/// This function performs no checkpoint writes or runtime effects.
pub(crate) fn resolve_static_saved_tool_pause(
    view: &dyn PipelineCheckpointFamilyView,
    definitions: StaticToolDefinitionSet<'_>,
    binding: &StaticToolPauseBinding,
    decision: &StaticToolDecision,
) -> Result<ValidatedStaticToolContinuation, StaticToolPauseError> {
    if !decision.validate() {
        return Err(StaticToolPauseError::Invalid);
    }
    if decision.pause_id != binding.pause_id
        || decision.child_thread_id != binding.root_thread_id
        || decision.tool_call_id != binding.parent_call_id
    {
        return Err(StaticToolPauseError::Stale);
    }
    if view.catalog() != Some(definitions.checkpoint_catalog) {
        return Err(StaticToolPauseError::Stale);
    }
    let root = view.root();
    if root.thread_id != binding.root_thread_id || root.checkpoint_id != binding.graph.checkpoint_id
    {
        return Err(StaticToolPauseError::Stale);
    }
    validate_definition_revision(
        root,
        definitions.definitions_by_exact_path,
        definitions.checkpoint_catalog,
        &root.thread_id,
    )?;
    let mut leaf = root;
    let mut proven = BTreeMap::from([(root.thread_id.clone(), root.checkpoint_id.clone())]);
    for child in &binding.graph.nested_checkpoints {
        if leaf.pending_nodes.as_slice() != [child.node_name()] {
            return Err(StaticToolPauseError::Stale);
        }
        leaf = view
            .checkpoint(child.thread_id())
            .ok_or(StaticToolPauseError::Corrupt)?;
        if leaf.thread_id != child.thread_id() || leaf.checkpoint_id != child.checkpoint_id() {
            return Err(StaticToolPauseError::Stale);
        }
        validate_definition_revision(
            leaf,
            definitions.definitions_by_exact_path,
            definitions.checkpoint_catalog,
            &root.thread_id,
        )?;
        if proven
            .insert(leaf.thread_id.clone(), leaf.checkpoint_id.clone())
            .is_some()
        {
            return Err(StaticToolPauseError::Corrupt);
        }
    }
    let definition = definitions
        .definitions_by_exact_path
        .get(&leaf.thread_id)
        .ok_or(StaticToolPauseError::Stale)?;
    if !definition
        .static_pause_catalog()
        .contains_exact(&binding.graph.metadata)
        || !binding.graph.metadata.checkpoint_matches(leaf)
    {
        return Err(StaticToolPauseError::Stale);
    }
    let mut normalization = BTreeMap::new();
    let mut state = State::from([
        ("input".to_owned(), json!(decision.value)),
        ("messages".to_owned(), json!(decision.value)),
    ]);
    if binding.graph.metadata.kind == "after" {
        leaf.step
            .checked_add(1)
            .ok_or(StaticToolPauseError::Corrupt)?;
        normalization.insert(leaf.thread_id.clone(), leaf.checkpoint_id.clone());
        state.insert(
            STATIC_AFTER_CHECKPOINTS_STATE_KEY.to_owned(),
            json!(normalization),
        );
    }
    if leaf.thread_id != root.thread_id {
        state.insert(
            STATIC_TEXT_RESUME_STATE_KEY.to_owned(),
            json!({leaf.thread_id.clone():decision.value}),
        );
    }
    Ok(ValidatedStaticToolContinuation {
        pause_id: binding.pause_id.clone(),
        root_checkpoint: root.clone(),
        resume_state: state,
        proven_checkpoint_ids: proven,
        normalization_ids: normalization,
        original_batch_event_id: binding.original_batch_event_id.clone(),
        original_ordinal: binding.original_ordinal,
    })
}

fn validate_definition_revision(
    checkpoint: &Checkpoint,
    definitions: &BTreeMap<String, PipelineDefinition>,
    catalog: &PipelineCheckpointCatalog,
    root_thread: &str,
) -> Result<(), StaticToolPauseError> {
    let definition = definitions
        .get(&checkpoint.thread_id)
        .ok_or(StaticToolPauseError::Stale)?;
    let revision = if checkpoint.thread_id == root_thread {
        catalog.root.as_ref()
    } else {
        let path = checkpoint
            .thread_id
            .strip_prefix(&format!("{root_thread}/"))
            .ok_or(StaticToolPauseError::Stale)?;
        catalog.descendants.get(path)
    }
    .ok_or(StaticToolPauseError::Stale)?;
    if revision.application_id == 0
        || revision.version_id == 0
        || revision.definition_digest != definition.definition_digest()
    {
        return Err(StaticToolPauseError::Stale);
    }
    Ok(())
}
fn valid_identity(value: &str, limit: usize) -> bool {
    !value.is_empty() && value.len() <= limit && !value.contains('\0')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::graph::static_pause::{STATIC_PAUSE_MESSAGE, STATIC_PAUSE_METADATA_KEY};
    use crate::agents::pipeline::composition::PipelineCheckpointRevision;
    use adk_rust::Content;
    use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};

    struct View {
        root: Checkpoint,
        catalog: Option<PipelineCheckpointCatalog>,
    }
    impl PipelineCheckpointFamilyView for View {
        fn root(&self) -> &Checkpoint {
            &self.root
        }
        fn checkpoint(&self, thread: &str) -> Option<&Checkpoint> {
            (thread == self.root.thread_id).then_some(&self.root)
        }
        fn catalog(&self) -> Option<&PipelineCheckpointCatalog> {
            self.catalog.as_ref()
        }
    }
    fn fixture(
        kind: &str,
    ) -> (
        View,
        BTreeMap<String, PipelineDefinition>,
        StaticToolPauseBinding,
        StaticToolDecision,
        PipelineCheckpointCatalog,
    ) {
        let yaml = format!(
            "interrupt_{kind}: [tick]\nstate:\n  count: {{type: int, value: 0}}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '{{{{ count + 1 }}}}'\n    input: [count]\n    output: [count]\n    transition: END\n"
        );
        let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
        let pending = if kind == "before" {
            vec!["tick".to_owned()]
        } else {
            vec![]
        };
        let checkpoint = Checkpoint::new(
            "child-root",
            State::from([("count".to_owned(), json!(i32::from(kind != "before")))]),
            4,
            pending,
        )
        .with_cleared_interrupt("tick");
        let metadata = definition
            .static_pause_catalog()
            .bind(kind, "tick", &checkpoint)
            .unwrap();
        let payload = GraphInterruptPayload {
            kind: kind.to_owned(),
            node: Some("tick".to_owned()),
            message: None,
            data: None,
            thread_id: checkpoint.thread_id.clone(),
            checkpoint_id: checkpoint.checkpoint_id.clone(),
        };
        let mut event = Event::new("original-child-invocation");
        event.author = "child-tool".to_owned();
        event.set_content(Content::new("assistant").with_text(STATIC_PAUSE_MESSAGE));
        event.provider_metadata.insert(
            INTERRUPT_METADATA_KEY.to_owned(),
            payload.to_metadata_value(),
        );
        event.provider_metadata.insert(
            STATIC_PAUSE_METADATA_KEY.to_owned(),
            serde_json::to_string(&metadata).unwrap(),
        );
        let binding = StaticToolPauseBinding::from_projected_event(
            &event,
            "child-tool",
            "child-root",
            "original-call",
            "original-batch",
            3,
        )
        .unwrap();
        let decision = StaticToolDecision {
            pause_id: binding.pause_id.clone(),
            child_thread_id: "child-root".to_owned(),
            tool_call_id: "original-call".to_owned(),
            action: "continue".to_owned(),
            value: "continue".to_owned(),
        };
        let catalog = PipelineCheckpointCatalog {
            root: Some(PipelineCheckpointRevision {
                application_id: 31,
                version_id: 41,
                definition_digest: definition.definition_digest(),
            }),
            descendants: BTreeMap::new(),
        };
        (
            View {
                root: checkpoint,
                catalog: Some(catalog.clone()),
            },
            BTreeMap::from([("child-root".to_owned(), definition)]),
            binding,
            decision,
            catalog,
        )
    }
    #[test]
    fn explicit_static_tool_selection_preserves_batch_and_checkpoint_bytes() {
        for kind in ["before", "after"] {
            let (view, definitions, binding, decision, catalog) = fixture(kind);
            let before = serde_json::to_vec(&view.root).unwrap();
            let validated = resolve_static_saved_tool_pause(
                &view,
                StaticToolDefinitionSet {
                    definitions_by_exact_path: &definitions,
                    checkpoint_catalog: &catalog,
                },
                &binding,
                &decision,
            )
            .unwrap();
            assert_eq!(validated.original_batch_event_id(), "original-batch");
            assert_eq!(validated.original_ordinal(), 3);
            assert_eq!(
                validated.proven_checkpoint_ids().get("child-root"),
                Some(&view.root.checkpoint_id)
            );
            assert_eq!(validated.resume_state()["input"], json!("continue"));
            assert_eq!(
                validated.normalization_ids().len(),
                usize::from(kind == "after")
            );
            assert_eq!(serde_json::to_vec(&view.root).unwrap(), before);
        }
    }
    #[test]
    fn selection_rejects_other_call_thread_occurrence_and_version_lineage() {
        let (view, definitions, binding, decision, catalog) = fixture("before");
        for field in ["tool_call_id", "child_thread_id", "pause_id"] {
            let mut value = serde_json::to_value(&decision).unwrap();
            value[field] = json!(if field == "pause_id" {
                "pipeline-static:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
            } else {
                "different"
            });
            let selected: StaticToolDecision = serde_json::from_value(value).unwrap();
            assert!(
                resolve_static_saved_tool_pause(
                    &view,
                    StaticToolDefinitionSet {
                        definitions_by_exact_path: &definitions,
                        checkpoint_catalog: &catalog
                    },
                    &binding,
                    &selected
                )
                .is_err()
            );
        }
        let mut changed = catalog.clone();
        changed.root.as_mut().unwrap().version_id += 1;
        assert!(
            resolve_static_saved_tool_pause(
                &view,
                StaticToolDefinitionSet {
                    definitions_by_exact_path: &definitions,
                    checkpoint_catalog: &changed
                },
                &binding,
                &decision
            )
            .is_err()
        );
    }
    #[test]
    fn replacement_frontier_and_legacy_without_catalog_fail_closed() {
        let (mut view, definitions, binding, decision, catalog) = fixture("before");
        view.root.pending_nodes = vec!["different".to_owned()];
        assert!(
            resolve_static_saved_tool_pause(
                &view,
                StaticToolDefinitionSet {
                    definitions_by_exact_path: &definitions,
                    checkpoint_catalog: &catalog
                },
                &binding,
                &decision
            )
            .is_err()
        );
        view.root.pending_nodes = vec!["tick".to_owned()];
        view.catalog = None;
        assert!(
            resolve_static_saved_tool_pause(
                &view,
                StaticToolDefinitionSet {
                    definitions_by_exact_path: &definitions,
                    checkpoint_catalog: &catalog
                },
                &binding,
                &decision
            )
            .is_err()
        );
    }
    #[test]
    fn parser_refuses_implicit_duplicate_and_dynamic_actions() {
        let (_, _, _, decision, _) = fixture("before");
        let mut meta = serde_json::Map::new();
        assert!(parse_static_tool_decisions(&meta).unwrap().is_none());
        for decisions in [
            json!([decision.clone(), decision.clone()]),
            json!([{ "pause_id":decision.pause_id,"child_thread_id":"child-root","tool_call_id":"original-call","action":"approve","value":"continue"}]),
        ] {
            meta.insert(
                STATIC_TOOL_RESUME_META_KEY.to_owned(),
                json!({"revision":1,"decisions":decisions}),
            );
            assert!(parse_static_tool_decisions(&meta).is_err());
        }
    }
}
