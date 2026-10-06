//! Saved-tool static selection crosses the graph boundary without ordinary model replay.

use super::*;
use crate::agents::graph::static_tool_pause::{
    StaticToolDecision, StaticToolDefinitionSet, StaticToolPauseBinding,
    resolve_static_saved_tool_pause,
};
use std::collections::BTreeMap;

/// Read only from one immutable durable family event. No browser proof is accepted.
pub(crate) struct PipelineStaticToolPause {
    pub(crate) event: Event,
    pub(crate) container_invocation_id: String,
    pub(crate) parent_call_id: String,
    pub(crate) thread_id: String,
    pub(crate) lineage: PipelineToolCallLineage,
    pub(crate) pause_id: String,
    pub(crate) public_proof: Value,
}
impl PipelineStaticToolPause {
    pub(crate) fn matches_projected_call(
        &self,
        batch: &str,
        ordinal: usize,
        name: &str,
        args: &Value,
    ) -> Result<bool, NativeAgentAssemblyError> {
        Ok(self.lineage.original_batch_event_id == batch
            && self.lineage.original_ordinal == ordinal
            && self.lineage.matches(name, args)?)
    }
    pub(crate) fn inventory(&self) -> Value {
        json!({"tool_call_id":self.parent_call_id,"child_thread_id":self.thread_id,
            "original_batch_event_id":self.lineage.original_batch_event_id,
            "original_ordinal":self.lineage.original_ordinal,"proof":self.public_proof})
    }
    pub(crate) fn matches_original_call(
        &self,
        original: &Event,
    ) -> Result<(), NativeAgentAssemblyError> {
        let calls = original.tool_calls();
        let mut matched = calls
            .iter()
            .enumerate()
            .filter(|(_, call)| call.call_id == Some(self.parent_call_id.as_str()));
        let (index, call) = matched.next().ok_or_else(invalid_boundary)?;
        if matched.next().is_some()
            || PipelineToolCallLineage::from_call(original, index)? != self.lineage
            || !self.lineage.matches(call.name, call.args)?
        {
            return Err(invalid_boundary());
        }
        Ok(())
    }
}

pub(crate) fn static_pipeline_tool_pause(
    event: &Event,
) -> Result<Option<PipelineStaticToolPause>, NativeAgentAssemblyError> {
    if !event
        .provider_metadata
        .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY)
    {
        return Ok(None);
    }
    let pending = family_from_event(event).map_err(|_| invalid_boundary())?;
    let PipelinePendingEnvelope::Family(family) = pending else {
        return Ok(None);
    };
    if family.pause_kind != Some(PipelineToolPauseKind::Static) {
        return Ok(None);
    }
    if family.schema_revision != PIPELINE_TOOL_TYPED_FAMILY_SCHEMA {
        return Err(invalid_boundary());
    }
    let boundary = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
    let lineage = family.call_lineage.ok_or_else(invalid_boundary)?;
    if boundary.checkpoint_thread_id != family.thread_id
        || boundary.parent_call_id != lineage.parent_call_id
        || event.author != lineage.tool_name
        || event
            .provider_metadata
            .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
            != Some(&boundary.container_invocation_id)
        || event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY) != Some(&boundary.parent_call_id)
        || event
            .provider_metadata
            .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
            != Some(&family.thread_id)
    {
        return Err(invalid_boundary());
    }
    let mut projected = event.clone();
    strip_descendant_private_metadata(&mut projected);
    projected.branch.clear();
    let binding = super::super::events::pipeline_static_event_binding(
        &projected,
        &lineage.tool_name,
        &family.thread_id,
    )
    .map_err(|_| invalid_boundary())?;
    let public_proof = binding
        .public_proof(&projected)
        .map_err(|_| invalid_boundary())?;
    let pause_id = public_proof
        .get("pause_id")
        .and_then(Value::as_str)
        .ok_or_else(invalid_boundary)?
        .to_owned();
    // Prove every wrapper against exactly this stored family before showing any public inventory.
    let mut checkpoint = &family.checkpoint;
    for (index, nested) in binding.nested_checkpoints.iter().enumerate() {
        if checkpoint.pending_nodes.as_slice() != [nested.node_name()] {
            return Err(invalid_boundary());
        }
        checkpoint = family
            .descendant_checkpoints
            .iter()
            .find(|checkpoint| checkpoint.thread_id == nested.thread_id())
            .ok_or_else(invalid_boundary)?;
        if checkpoint.checkpoint_id != nested.checkpoint_id() {
            return Err(invalid_boundary());
        }
        if let Some(next) = binding.nested_checkpoints.get(index + 1)
            && checkpoint.pending_nodes.as_slice() != [next.node_name()]
        {
            return Err(invalid_boundary());
        }
    }
    if !binding.metadata.checkpoint_matches(checkpoint) {
        return Err(invalid_boundary());
    }
    Ok(Some(PipelineStaticToolPause {
        event: event.clone(),
        container_invocation_id: boundary.container_invocation_id,
        parent_call_id: boundary.parent_call_id,
        thread_id: family.thread_id,
        lineage,
        pause_id,
        public_proof,
    }))
}

/// This is a typed request, not a validated resume. The actual rebuilt saved-tool
/// runtime proves definition/catalog/frontier again before any checkpoint hydration.
pub(crate) struct PipelineStaticToolResume {
    pause: PipelineStaticToolPause,
    decision: StaticToolDecision,
}
impl PipelineStaticToolResume {
    pub(crate) fn new(
        pause: PipelineStaticToolPause,
        decision: StaticToolDecision,
    ) -> Result<Self, NativeAgentAssemblyError> {
        if pause.pause_id != decision.pause_id()
            || pause.thread_id != decision.child_thread_id()
            || pause.parent_call_id != decision.tool_call_id()
        {
            return Err(invalid_boundary());
        }
        Ok(Self { pause, decision })
    }
    pub(crate) fn pause_id(&self) -> &str {
        &self.pause.pause_id
    }
    pub(crate) fn resolve(
        self,
        definition: &PipelineDefinition,
        runtimes: &PipelineNodeRuntimes,
        thread: &str,
    ) -> Result<PipelineToolResume, NativeAgentAssemblyError> {
        if self.pause.thread_id != thread {
            return Err(invalid_boundary());
        }
        let pending = family_from_event(&self.pause.event).map_err(|_| invalid_boundary())?;
        let catalog = runtimes.checkpoint_catalog().ok_or_else(invalid_boundary)?;
        if pending.catalog() != Some(catalog) {
            return Err(invalid_boundary());
        }
        let mut definitions = BTreeMap::from([(thread.to_owned(), definition.clone())]);
        if let Some(descendants) = runtimes.composition_definitions() {
            for (path, definition) in descendants {
                definitions.insert(format!("{thread}/{path}"), definition.clone());
            }
        }
        let mut projected = self.pause.event.clone();
        strip_descendant_private_metadata(&mut projected);
        projected.branch.clear();
        let binding = StaticToolPauseBinding::from_projected_event(
            &projected,
            &self.pause.lineage.tool_name,
            thread,
            &self.pause.parent_call_id,
            &self.pause.lineage.original_batch_event_id,
            self.pause.lineage.original_ordinal,
        )
        .map_err(|_| invalid_boundary())?;
        if binding.pause_id() != self.pause.pause_id
            || binding.root_thread_id() != thread
            || binding.parent_call_id() != self.pause.parent_call_id
            || binding.original_batch_event_id() != self.pause.lineage.original_batch_event_id
            || binding.original_ordinal() != self.pause.lineage.original_ordinal
        {
            return Err(invalid_boundary());
        }
        let proof = resolve_static_saved_tool_pause(
            &pending,
            StaticToolDefinitionSet {
                definitions_by_exact_path: &definitions,
                checkpoint_catalog: catalog,
            },
            &binding,
            &self.decision,
        )
        .map_err(|_| invalid_boundary())?;
        if proof.original_batch_event_id() != self.pause.lineage.original_batch_event_id
            || proof.original_ordinal() != self.pause.lineage.original_ordinal
        {
            return Err(invalid_boundary());
        }
        static_pipeline_tool_resume(&self.pause.event, &proof).map_err(|_| invalid_boundary())
    }
}

#[cfg(test)]
#[allow(clippy::too_many_lines)] // Keep the original call, checkpoint, pause, and decision in one fixture.
pub(crate) fn static_pause_fixture(
    original: &Event,
    index: usize,
    kind: &str,
) -> (
    PipelineDefinition,
    PipelineNodeRuntimes,
    Event,
    StaticToolDecision,
) {
    use crate::agents::events::APPLICATION_BRANCH_ROOT;
    use crate::agents::graph::static_pause::{STATIC_PAUSE_MESSAGE, STATIC_PAUSE_METADATA_KEY};
    use crate::agents::graph::static_tool_pause::parse_static_tool_decisions;
    use crate::agents::pipeline::composition::PipelineCheckpointRevision;
    let calls = original.tool_calls();
    let call = &calls[index];
    let call_id = call.call_id.unwrap();
    let definition = PipelineDefinition::from_yaml(&format!(
        "interrupt_{kind}: [tick]\nstate:\n  count: {{type: int, value: 0}}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '{{{{ count + 1 }}}}'\n    input: [count]\n    output: [count]\n    transition: END\n"
    )).unwrap();
    let thread = format!("conversation/{call_id}");
    let checkpoint = Checkpoint::new(
        &thread,
        State::from([("count".to_owned(), json!(i32::from(kind != "before")))]),
        4,
        if kind == "before" {
            vec!["tick".to_owned()]
        } else {
            vec![]
        },
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
        thread_id: thread.clone(),
        checkpoint_id: checkpoint.checkpoint_id.clone(),
    };
    let catalog = PipelineCheckpointCatalog {
        root: Some(PipelineCheckpointRevision {
            application_id: 31,
            version_id: 41,
            definition_digest: definition.definition_digest(),
        }),
        descendants: BTreeMap::new(),
    };
    let family = PipelineToolPendingFamily {
        schema_revision: PIPELINE_TOOL_TYPED_FAMILY_SCHEMA.to_owned(),
        pause_kind: Some(PipelineToolPauseKind::Static),
        thread_id: thread.clone(),
        checkpoint_id: checkpoint.checkpoint_id.clone(),
        node_name: "tick".to_owned(),
        checkpoint,
        catalog: catalog.clone(),
        descendant_checkpoints: vec![],
        call_lineage: Some(PipelineToolCallLineage::from_call(original, index).unwrap()),
    };
    let mut event = Event::with_id(
        format!("static-pause-{call_id}"),
        format!("pipeline-child:{call_id}"),
    );
    event.author = call.name.to_owned();
    event.branch = format!("{APPLICATION_BRANCH_ROOT}.application_{}", index + 1);
    event.set_content(Content::new("assistant").with_text(STATIC_PAUSE_MESSAGE));
    event.provider_metadata.insert(
        INTERRUPT_METADATA_KEY.to_owned(),
        payload.to_metadata_value(),
    );
    event.provider_metadata.insert(
        STATIC_PAUSE_METADATA_KEY.to_owned(),
        serde_json::to_string(&metadata).unwrap(),
    );
    event.provider_metadata.insert(
        PIPELINE_TOOL_PENDING_METADATA_KEY.to_owned(),
        serde_json::to_string(&family).unwrap(),
    );
    event.provider_metadata.insert(
        DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
        original.invocation_id.clone(),
    );
    event
        .provider_metadata
        .insert(DESCENDANT_PARENT_CALL_KEY.to_owned(), call_id.to_owned());
    event
        .provider_metadata
        .insert(DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(), thread.clone());
    event.provider_metadata.insert(PIPELINE_TOOL_BOUNDARY_METADATA_KEY.to_owned(),json!({"schema":"elitea.pipeline.tool-boundary.v1",
        "container_invocation_id":original.invocation_id,"parent_call_id":call_id,"checkpoint_thread_id":thread}).to_string());
    let pause = static_pipeline_tool_pause(&event).unwrap().unwrap();
    let meta = serde_json::Map::from_iter([(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
        "pause_id":pause.pause_id,"child_thread_id":thread,"tool_call_id":call_id,"action":"continue","value":"continue once"}]}),
    )]);
    let decision = parse_static_tool_decisions(&meta)
        .unwrap()
        .unwrap()
        .remove(0);
    (
        definition,
        PipelineNodeRuntimes::default().with_checkpoint_catalog(catalog),
        event,
        decision,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::events::APPLICATION_BRANCH_ROOT;
    use crate::agents::graph::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY;

    fn original() -> Event {
        let mut event = Event::with_id("original-model-batch", "original-root-invocation");
        event.branch = APPLICATION_BRANCH_ROOT.to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "saved_pipeline".to_owned(),
                args: json!({"task":"work","locale":"one"}),
                id: Some("call-one".to_owned()),
                thought_signature: None,
            }],
        });
        event
    }

    #[test]
    fn static_family_selection_preserves_original_batch_and_before_after_frontier() {
        for kind in ["before", "after"] {
            let original = original();
            let (definition, runtimes, event, decision) = static_pause_fixture(&original, 0, kind);
            let original_bytes = serde_json::to_vec(&event).unwrap();
            let pause = static_pipeline_tool_pause(&event).unwrap().unwrap();
            pause.matches_original_call(&original).unwrap();
            assert_eq!(
                pause.inventory()["original_batch_event_id"],
                json!(original.id)
            );
            assert_eq!(pause.inventory()["original_ordinal"], json!(1));
            let thread = pause.thread_id.clone();
            let resume = PipelineStaticToolResume::new(pause, decision)
                .unwrap()
                .resolve(&definition, &runtimes, &thread)
                .unwrap();
            assert_eq!(resume.thread_id, thread);
            assert_eq!(
                resume.checkpoint.state["count"],
                json!(i32::from(kind != "before"))
            );
            assert_eq!(resume.checkpoint.pending_nodes.is_empty(), kind == "after");
            assert_eq!(
                resume
                    .resume_state
                    .contains_key(STATIC_AFTER_CHECKPOINTS_STATE_KEY),
                kind == "after"
            );
            assert_eq!(serde_json::to_vec(&event).unwrap(), original_bytes);
        }
    }

    #[test]
    fn changed_args_saved_revision_or_static_policy_are_refused_before_hydration() {
        let original = original();
        let (definition, runtimes, event, decision) = static_pause_fixture(&original, 0, "before");
        let mut altered = original.clone();
        if let Part::FunctionCall { args, .. } =
            &mut altered.llm_response.content.as_mut().unwrap().parts[0]
        {
            args["locale"] = json!("other");
        }
        assert!(
            static_pipeline_tool_pause(&event)
                .unwrap()
                .unwrap()
                .matches_original_call(&altered)
                .is_err()
        );
        let mut changed = runtimes.checkpoint_catalog().unwrap().clone();
        changed.root.as_mut().unwrap().version_id += 1;
        let changed_runtime = PipelineNodeRuntimes::default().with_checkpoint_catalog(changed);
        let pause = static_pipeline_tool_pause(&event).unwrap().unwrap();
        let thread = pause.thread_id.clone();
        assert!(
            PipelineStaticToolResume::new(pause, decision.clone())
                .unwrap()
                .resolve(&definition, &changed_runtime, &thread)
                .is_err()
        );
        let changed_definition=PipelineDefinition::from_yaml("state: {count: int}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '1'\n    output: [count]\n    transition: END\n").unwrap();
        assert!(
            PipelineStaticToolResume::new(
                static_pipeline_tool_pause(&event).unwrap().unwrap(),
                decision
            )
            .unwrap()
            .resolve(&changed_definition, &runtimes, &thread)
            .is_err()
        );
    }
}
