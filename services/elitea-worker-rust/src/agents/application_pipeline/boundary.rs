//! Exact original-call joins and minimal untouched-family projection.
use super::{
    PipelineApplicationBoundary, PipelinePendingEnvelope, PipelineToolCallLineage,
    PipelineToolPauseKind, boundary_ledger::outer_boundary_receipt as boundary_receipt,
    family_from_event, invalid_boundary,
};
use crate::agents::events::{
    DESCENDANT_CONTAINER_INVOCATION_KEY, DESCENDANT_PARENT_CALL_KEY,
    PIPELINE_TOOL_BOUNDARY_METADATA_KEY,
};
use crate::agents::pipeline::scope_receipts::{GraphCallOutcome, receipt_for_node};
use crate::agents::pipeline::scoped_applications::route_from_event;
use crate::agents::runtime::NativeAgentAssemblyError;
use adk_rust::Event;
use adk_rust::graph::interrupt::GraphInterruptPayload;
use serde_json::Value;
use std::collections::{BTreeMap, BTreeSet};

pub(super) fn boundary_call(
    events: &[Event],
    container: &str,
    parent_call: &str,
) -> Result<(Event, PipelineToolCallLineage), NativeAgentAssemblyError> {
    let mut selected: Option<(Event, PipelineToolCallLineage)> = None;
    for event in events
        .iter()
        .filter(|event| event.invocation_id == container)
    {
        for (index, _call) in event
            .tool_calls()
            .iter()
            .enumerate()
            .filter(|(_, call)| call.call_id == Some(parent_call))
        {
            let lineage = PipelineToolCallLineage::from_call(event, index)?;
            if let Some((previous, recorded)) = &selected {
                if previous.id != event.id
                    || recorded != &lineage
                    || serde_json::to_value(previous).map_err(|_| invalid_boundary())?
                        != serde_json::to_value(event).map_err(|_| invalid_boundary())?
                {
                    return Err(invalid_boundary());
                }
            } else {
                selected = Some((event.clone(), lineage));
            }
        }
    }
    let (current, lineage) = selected.ok_or_else(invalid_boundary)?;
    let mut originals = events
        .iter()
        .filter(|event| event.id == lineage.original_batch_event_id);
    let original = originals.next().ok_or_else(invalid_boundary)?;
    if originals
        .any(|event| serde_json::to_value(event).ok() != serde_json::to_value(original).ok())
        || original.id != crate::agents::application_tools::original_application_batch_id(original)?
    {
        return Err(invalid_boundary());
    }
    let original_lineage =
        PipelineToolCallLineage::from_call(original, lineage.original_ordinal - 1)?;
    if original_lineage != lineage {
        return Err(invalid_boundary());
    }
    // Current replay can have a subset of the original batch, but cannot edit its arguments.
    if !current.tool_calls().iter().any(|call| {
        call.call_id == Some(parent_call) && lineage.matches(call.name, call.args).unwrap_or(false)
    }) {
        return Err(invalid_boundary());
    }
    Ok((original.clone(), lineage))
}

pub(crate) fn pipeline_boundary_original_call(
    events: &[Event],
    boundary: &PipelineApplicationBoundary,
) -> Result<(Event, String, usize), NativeAgentAssemblyError> {
    let (original, lineage) = boundary_call(
        events,
        &boundary.container_invocation_id,
        &boundary.parent_call_id,
    )?;
    let pending = family_from_event(&boundary.pending_event).map_err(|_| invalid_boundary())?;
    let PipelinePendingEnvelope::Family(family) = pending else {
        return Err(invalid_boundary());
    };
    if family.call_lineage.as_ref() != Some(&lineage) {
        return Err(invalid_boundary());
    }
    Ok((
        original,
        lineage.original_batch_event_id,
        lineage.original_ordinal,
    ))
}

/// Check the exact native frontier and the graph-call receipt that emitted its leaves.
pub(super) fn application_family_interrupt_ids(
    pending: &PipelinePendingEnvelope,
    payload: &GraphInterruptPayload,
) -> Result<BTreeSet<String>, NativeAgentAssemblyError> {
    let PipelinePendingEnvelope::Family(family) = pending else {
        return Err(invalid_boundary());
    };
    if family.pause_kind != Some(PipelineToolPauseKind::Application)
        || family.call_lineage.is_none()
        || payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != family.thread_id
        || payload.checkpoint_id != family.checkpoint_id
    {
        return Err(invalid_boundary());
    }
    let mut checkpoint = pending.root();
    let mut data = payload.data.as_ref().ok_or_else(invalid_boundary)?;
    let mut thread = family.thread_id.clone();
    let mut depth = 0;
    while let Some(node) = data.get("subgraph").and_then(Value::as_str) {
        depth += 1;
        if depth > super::MAX_PIPELINE_COMPOSITION_DEPTH
            || checkpoint.pending_nodes.as_slice() != [node]
        {
            return Err(invalid_boundary());
        }
        thread.push('/');
        thread.push_str(node);
        checkpoint = pending.checkpoint(&thread).ok_or_else(invalid_boundary)?;
        if data.get("thread").and_then(Value::as_str) != Some(thread.as_str())
            || data.get("checkpoint_id").and_then(Value::as_str)
                != Some(checkpoint.checkpoint_id.as_str())
        {
            return Err(invalid_boundary());
        }
        data = data.get("data").ok_or_else(invalid_boundary)?;
    }
    let node = data
        .get("node_name")
        .and_then(Value::as_str)
        .ok_or_else(invalid_boundary)?;
    let call = data
        .get("application_call_id")
        .and_then(Value::as_str)
        .ok_or_else(invalid_boundary)?;
    if data.get("schema_revision").and_then(Value::as_str)
        != Some(crate::agents::graph::PIPELINE_APPLICATION_HITL_SCHEMA)
        || data.get("guardrail_type").and_then(Value::as_str) != Some("application_sensitive_tool")
        || checkpoint.pending_nodes.as_slice() != [node]
    {
        return Err(invalid_boundary());
    }
    let receipt = receipt_for_node(checkpoint, node)
        .map_err(|_| invalid_boundary())?
        .ok_or_else(invalid_boundary)?;
    if receipt.activation().call_id() != call || receipt.activation().step() != checkpoint.step {
        return Err(invalid_boundary());
    }
    let GraphCallOutcome::Paused { result } = receipt.outcome() else {
        return Err(invalid_boundary());
    };
    let recorded = crate::agents::application_tools::nested_application_interrupt_ids(result)
        .ok_or_else(invalid_boundary)?;
    let ids = data
        .get("interrupt_ids")
        .and_then(Value::as_array)
        .ok_or_else(invalid_boundary)?;
    let mut unique = BTreeSet::new();
    if ids.is_empty()
        || ids.len() > 16
        || ids.iter().any(|id| {
            id.as_str()
                .is_none_or(|id| !super::valid_family_identity(id) || !unique.insert(id.to_owned()))
        })
        || unique != recorded
    {
        return Err(invalid_boundary());
    }
    Ok(unique)
}

fn confirmation_id(event: &Event) -> Result<String, NativeAgentAssemblyError> {
    let confirmation = event
        .actions
        .tool_confirmation
        .as_ref()
        .ok_or_else(invalid_boundary)?;
    let call = confirmation
        .function_call_id
        .as_deref()
        .ok_or_else(invalid_boundary)?;
    crate::agents::direct_hitl::sensitive_call_identity(
        &event.invocation_id,
        call,
        &confirmation.tool_name,
        &confirmation.args,
    )
    .map(|(id, _)| id)
    .map_err(|_| invalid_boundary())
}
struct RetainedLeaf {
    id: String,
    invocation: String,
    call: String,
    name: String,
    args: Value,
}
fn retained_leaf(
    event: &Event,
    events: &[Event],
) -> Result<Option<RetainedLeaf>, NativeAgentAssemblyError> {
    if let Some(confirmation) = event.actions.tool_confirmation.as_ref() {
        return Ok(Some(RetainedLeaf {
            id: confirmation_id(event)?,
            invocation: event.invocation_id.clone(),
            call: confirmation
                .function_call_id
                .clone()
                .ok_or_else(invalid_boundary)?,
            name: confirmation.tool_name.clone(),
            args: confirmation.args.clone(),
        }));
    }
    let Some(pause) = super::static_pipeline_tool_pause(event)? else {
        return Ok(None);
    };
    // Static family is independently bound to the exact original inner model call.
    let (original, lineage) = boundary_call(
        events,
        &pause.container_invocation_id,
        &pause.parent_call_id,
    )?;
    pause.matches_original_call(&original)?;
    if pause.lineage != lineage {
        return Err(invalid_boundary());
    }
    let calls = original.tool_calls();
    let call = calls
        .get(lineage.original_ordinal - 1)
        .ok_or_else(invalid_boundary)?;
    Ok(Some(RetainedLeaf {
        id: pause.pause_id,
        invocation: pause.container_invocation_id,
        call: pause.parent_call_id,
        name: call.name.to_owned(),
        args: call.args.clone(),
    }))
}

fn insert_event(
    selected: &mut BTreeMap<String, Event>,
    event: &Event,
) -> Result<(), NativeAgentAssemblyError> {
    if let Some(recorded) = selected.get(&event.id) {
        if serde_json::to_value(recorded).map_err(|_| invalid_boundary())?
            != serde_json::to_value(event).map_err(|_| invalid_boundary())?
        {
            return Err(invalid_boundary());
        }
    } else {
        if selected.len() >= 511 {
            return Err(invalid_boundary());
        }
        selected.insert(event.id.clone(), event.clone());
    }
    Ok(())
}

/// Select only each original unresolved confirmation, exact model-call ancestry,
/// and the family wrapper. Completed output/progress/history is never projected.
#[allow(clippy::too_many_lines)] // Keep the complete retained ancestry proof and minimal projection selection visibly ordered.
pub(crate) fn retained_pipeline_application_events(
    events: &[Event],
    boundary: &PipelineApplicationBoundary,
    ids: &BTreeSet<String>,
) -> Result<Vec<Event>, NativeAgentAssemblyError> {
    let (original, _, _) = pipeline_boundary_original_call(events, boundary)?;
    let pending = family_from_event(&boundary.pending_event).map_err(|_| invalid_boundary())?;
    let payload =
        GraphInterruptPayload::from_event(&boundary.pending_event).ok_or_else(invalid_boundary)?;
    if &application_family_interrupt_ids(&pending, &payload)? != ids {
        return Err(invalid_boundary());
    }
    let mut selected = BTreeMap::new();
    let mut found = BTreeSet::new();
    for leaf in events {
        let Some(identity) = retained_leaf(leaf, events)? else {
            continue;
        };
        let id = identity.id;
        if !ids.contains(&id) {
            continue;
        }
        let scope = route_from_event(&boundary.checkpoint_thread_id, &pending, leaf)?
            .ok_or_else(invalid_boundary)?;
        let Some(receipt) = boundary_receipt(leaf)? else {
            continue;
        };
        if receipt.parent_call_id != boundary.parent_call_id
            || receipt.checkpoint_thread_id != boundary.checkpoint_thread_id
        {
            continue;
        }
        let (_, lineage) = boundary_call(
            events,
            &receipt.container_invocation_id,
            &receipt.parent_call_id,
        )?;
        if pending_family_lineage(&pending) != Some(&lineage) {
            return Err(invalid_boundary());
        }
        found.insert(id);
        insert_event(&mut selected, leaf)?;
        let mut invocation = identity.invocation.as_str();
        let mut call_id = identity.call.as_str();
        let mut expected = Some((identity.name.as_str(), &identity.args));
        let mut seen = BTreeSet::new();
        loop {
            if seen.len() >= 16 || !seen.insert((invocation.to_owned(), call_id.to_owned())) {
                return Err(invalid_boundary());
            }
            let mut matching = events.iter().filter(|event| {
                event.invocation_id == invocation
                    && event.tool_calls().iter().any(|call| {
                        call.call_id == Some(call_id)
                            && expected
                                .is_none_or(|(name, args)| call.name == name && call.args == args)
                    })
            });
            let call_event = matching.next().ok_or_else(invalid_boundary)?;
            if matching.any(|event| event.id != call_event.id) {
                return Err(invalid_boundary());
            }
            insert_event(&mut selected, call_event)?;
            if call_id == scope.application_call_id {
                let checkpoint = pending
                    .checkpoint(&if scope.leaf_graph_path.is_empty() {
                        boundary.checkpoint_thread_id.clone()
                    } else {
                        format!(
                            "{}/{}",
                            boundary.checkpoint_thread_id, scope.leaf_graph_path
                        )
                    })
                    .ok_or_else(invalid_boundary)?;
                let record = receipt_for_node(checkpoint, &scope.leaf_node_name)
                    .map_err(|_| invalid_boundary())?
                    .ok_or_else(invalid_boundary)?;
                if record.original_start().id != call_event.id {
                    return Err(invalid_boundary());
                }
                // The projector still needs saved-pipeline ancestor calls above this graph.
            }
            let container = call_event
                .provider_metadata
                .get(DESCENDANT_CONTAINER_INVOCATION_KEY);
            let parent = call_event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY);
            match (container, parent) {
                (Some(container), Some(parent)) if parent != &boundary.parent_call_id => {
                    invocation = container;
                    call_id = parent;
                    expected = None;
                }
                (Some(_), Some(parent)) if parent == &boundary.parent_call_id => break,
                (None, None) if call_id == scope.application_call_id => break,
                _ => return Err(invalid_boundary()),
            }
        }
    }
    if &found != ids {
        return Err(invalid_boundary());
    }
    insert_event(&mut selected, &boundary.pending_event)?;
    if selected.contains_key(&original.id) {
        return Err(invalid_boundary());
    }
    let mut output = Vec::new();
    let mut emitted = BTreeSet::new();
    for event in events {
        if selected.contains_key(&event.id) && emitted.insert(event.id.clone()) {
            let mut projected = selected.remove(&event.id).ok_or_else(invalid_boundary)?;
            rebind_pipeline_tool_boundary(&mut projected, &boundary.container_invocation_id)?;
            output.push(projected);
        }
    }
    if !selected.is_empty() {
        return Err(invalid_boundary());
    }
    validate_retained_pipeline_application_pause(&original, &boundary.pending_event, &output, ids)?;
    Ok(output)
}
fn pending_family_lineage(pending: &PipelinePendingEnvelope) -> Option<&PipelineToolCallLineage> {
    match pending {
        PipelinePendingEnvelope::Family(family) => family.call_lineage.as_ref(),
        PipelinePendingEnvelope::Legacy(_) => None,
    }
}

#[allow(clippy::too_many_lines)] // Keep the complete retained ancestry proof and minimal projection selection visibly ordered.
pub(crate) fn validate_retained_pipeline_application_pause(
    original: &Event,
    pending_event: &Event,
    events: &[Event],
    ids: &BTreeSet<String>,
) -> Result<(), NativeAgentAssemblyError> {
    if events.is_empty() || events.len() > 511 || ids.is_empty() || ids.len() > 16 {
        return Err(invalid_boundary());
    }
    let pending = family_from_event(pending_event).map_err(|_| invalid_boundary())?;
    let payload = GraphInterruptPayload::from_event(pending_event).ok_or_else(invalid_boundary)?;
    if &application_family_interrupt_ids(&pending, &payload)? != ids {
        return Err(invalid_boundary());
    }
    let lineage = pending_family_lineage(&pending).ok_or_else(invalid_boundary)?;
    if original.id != lineage.original_batch_event_id
        || PipelineToolCallLineage::from_call(original, lineage.original_ordinal - 1)? != *lineage
    {
        return Err(invalid_boundary());
    }
    let receipt = boundary_receipt(pending_event)?.ok_or_else(invalid_boundary)?;
    if receipt.parent_call_id != lineage.parent_call_id
        || receipt.checkpoint_thread_id != pending.root().thread_id
    {
        return Err(invalid_boundary());
    }
    let mut used = BTreeSet::new();
    let mut seen = BTreeSet::new();
    let mut leaves = BTreeSet::new();
    let mut wrappers = 0;
    for event in events {
        if !seen.insert(&event.id)
            || event.id == original.id
            || !event.tool_results().is_empty()
            || event.actions.tool_confirmation_decision.is_some()
        {
            return Err(invalid_boundary());
        }
        if event.id == pending_event.id {
            wrappers += 1;
            if serde_json::to_value(event).map_err(|_| invalid_boundary())?
                != serde_json::to_value(pending_event).map_err(|_| invalid_boundary())?
            {
                return Err(invalid_boundary());
            }
            continue;
        }
        let boundary = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
        super::boundary_ledger::validate_outer_lineage(event, lineage)?;
        if boundary.parent_call_id != receipt.parent_call_id
            || boundary.checkpoint_thread_id != receipt.checkpoint_thread_id
            || boundary.container_invocation_id != receipt.container_invocation_id
        {
            return Err(invalid_boundary());
        }
        if let Some(identity) = retained_leaf(event, events)? {
            let id = identity.id;
            if !ids.contains(&id) || !leaves.insert(id) {
                return Err(invalid_boundary());
            }
            let route = route_from_event(&pending.root().thread_id, &pending, event)?
                .ok_or_else(invalid_boundary)?;
            let graph_thread = if route.leaf_graph_path.is_empty() {
                pending.root().thread_id.clone()
            } else {
                format!("{}/{}", pending.root().thread_id, route.leaf_graph_path)
            };
            let record = receipt_for_node(
                pending
                    .checkpoint(&graph_thread)
                    .ok_or_else(invalid_boundary)?,
                &route.leaf_node_name,
            )
            .map_err(|_| invalid_boundary())?
            .ok_or_else(invalid_boundary)?;
            if !events
                .iter()
                .any(|start| start.id == record.original_start().id)
            {
                return Err(invalid_boundary());
            }
            if !events.iter().any(|call_event| {
                call_event.invocation_id == identity.invocation
                    && call_event.tool_calls().iter().any(|call| {
                        call.call_id == Some(identity.call.as_str())
                            && call.name == identity.name
                            && call.args == &identity.args
                    })
            }) {
                return Err(invalid_boundary());
            }
            used.insert(event.id.clone());
            let mut invocation = identity.invocation.as_str();
            let mut call_id = identity.call.as_str();
            let mut expected = Some((identity.name.as_str(), &identity.args));
            let mut hops = BTreeSet::new();
            let mut graph_start = false;
            let mut graph_ancestors = route.descendants.iter().rev();
            let mut proved_ancestors = 0;
            loop {
                if hops.len() >= 16 || !hops.insert((invocation.to_owned(), call_id.to_owned())) {
                    return Err(invalid_boundary());
                }
                let mut matches = events.iter().filter(|candidate| {
                    candidate.invocation_id == invocation
                        && candidate.tool_calls().iter().any(|call| {
                            call.call_id == Some(call_id)
                                && expected.is_none_or(|(name, args)| {
                                    call.name == name && call.args == args
                                })
                        })
                });
                let call_event = matches.next().ok_or_else(invalid_boundary)?;
                if matches.next().is_some() {
                    return Err(invalid_boundary());
                }
                used.insert(call_event.id.clone());
                if call_id == route.application_call_id {
                    if call_event.id != record.original_start().id
                        || !record.activation().matches_event(call_event)?
                        || call_event.tool_calls().len() != 1
                        || call_event.tool_calls()[0].name
                            != record.original_start().tool_calls()[0].name
                        || call_event.tool_calls()[0].args
                            != record.original_start().tool_calls()[0].args
                    {
                        return Err(invalid_boundary());
                    }
                    graph_start = true;
                }
                if graph_start && call_id != route.application_call_id {
                    let child = graph_ancestors.next().ok_or_else(invalid_boundary)?;
                    let (parent_path, node) = child
                        .graph_path
                        .rsplit_once('/')
                        .unwrap_or(("", child.graph_path.as_str()));
                    let parent_thread = if parent_path.is_empty() {
                        pending.root().thread_id.clone()
                    } else {
                        format!("{}/{}", pending.root().thread_id, parent_path)
                    };
                    let ancestor = receipt_for_node(
                        pending
                            .checkpoint(&parent_thread)
                            .ok_or_else(invalid_boundary)?,
                        node,
                    )
                    .map_err(|_| invalid_boundary())?
                    .ok_or_else(invalid_boundary)?;
                    if ancestor.activation().call_id() != call_id
                        || ancestor.original_start().id != call_event.id
                        || !ancestor.activation().matches_event(call_event)?
                        || call_event.tool_calls().len() != 1
                        || call_event.tool_calls()[0].name
                            != ancestor.original_start().tool_calls()[0].name
                        || call_event.tool_calls()[0].args
                            != ancestor.original_start().tool_calls()[0].args
                    {
                        return Err(invalid_boundary());
                    }
                    proved_ancestors += 1;
                }
                match (
                    call_event
                        .provider_metadata
                        .get(DESCENDANT_CONTAINER_INVOCATION_KEY),
                    call_event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY),
                ) {
                    (Some(_), Some(parent)) if parent == &receipt.parent_call_id => {
                        if !graph_start || proved_ancestors != route.descendants.len() {
                            return Err(invalid_boundary());
                        }
                        break;
                    }
                    (Some(container), Some(parent)) => {
                        invocation = container;
                        call_id = parent;
                        expected = None;
                    }
                    (None, None) if graph_start && proved_ancestors == route.descendants.len() => {
                        break;
                    }
                    _ => return Err(invalid_boundary()),
                }
            }
        } else if event.tool_calls().is_empty() {
            return Err(invalid_boundary());
        }
    }
    if events
        .iter()
        .any(|event| event.id != pending_event.id && !used.contains(&event.id))
    {
        return Err(invalid_boundary());
    }
    if wrappers != 1 || &leaves != ids {
        return Err(invalid_boundary());
    }
    if serde_json::to_vec(&(original, events, ids))
        .map_err(|_| invalid_boundary())?
        .len()
        > super::MAX_PIPELINE_TOOL_PENDING_BYTES
    {
        return Err(invalid_boundary());
    }
    Ok(())
}

pub(crate) fn rebind_pipeline_tool_boundary(
    event: &mut Event,
    new_container: &str,
) -> Result<(), NativeAgentAssemblyError> {
    if !super::valid_family_identity(new_container) {
        return Err(invalid_boundary());
    }
    if super::boundary_ledger::rebind_outer_ledger(event, new_container)? {
        return Ok(());
    }
    let mut receipt = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
    let original_container = receipt.container_invocation_id.clone();
    if original_container == new_container {
        return Ok(());
    }
    new_container.clone_into(&mut receipt.container_invocation_id);
    let encoded = serde_json::to_string(&receipt).map_err(|_| invalid_boundary())?;
    event
        .provider_metadata
        .insert(PIPELINE_TOOL_BOUNDARY_METADATA_KEY.to_owned(), encoded);
    if event
        .provider_metadata
        .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
        == Some(&original_container)
        && event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY) == Some(&receipt.parent_call_id)
    {
        event.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            new_container.to_owned(),
        );
    }
    Ok(())
}

#[cfg(test)]
pub(crate) mod tests {
    use super::super::{
        PIPELINE_TOOL_TYPED_FAMILY_SCHEMA, PipelineToolPendingFamily,
        pipeline_application_boundary, pipeline_application_pending_event,
    };
    use super::*;
    use crate::agents::events::{
        APPLICATION_BRANCH_ROOT, DESCENDANT_CHECKPOINT_THREAD_KEY,
        PIPELINE_TOOL_PENDING_METADATA_KEY,
    };
    use crate::agents::graph::compiler::PipelineDefinition;
    use crate::agents::pipeline::composition::{
        PipelineCheckpointCatalog, PipelineCheckpointRevision,
    };
    use crate::agents::pipeline::scope_receipts::{finish_graph_call, prepare_graph_call};
    use crate::agents::pipeline::scoped_applications::PipelineApplicationScope;
    use adk_rust::graph::{
        Checkpoint, Checkpointer, ExecutionConfig, MemoryCheckpointer, NodeContext, State,
    };
    use adk_rust::{Content, Part, ToolConfirmationRequest};
    use serde_json::json;
    use tokio::sync::Mutex;

    fn definition(node: &str) -> PipelineDefinition {
        PipelineDefinition::from_yaml(&format!("state: {{answer: str}}\nentry_point: {node}\nnodes:\n  - id: {node}\n    type: agent\n    tool: assistant\n    input_mapping: {{task: {{type: fixed, value: work}}}}\n    output: [answer]\n    transition: END\n")).unwrap()
    }
    fn revision(id: u64, definition: &PipelineDefinition) -> PipelineCheckpointRevision {
        PipelineCheckpointRevision {
            application_id: id,
            version_id: 7,
            definition_digest: definition.definition_digest(),
        }
    }
    fn outer(event: &mut Event, container: &str) {
        event.provider_metadata.insert(PIPELINE_TOOL_BOUNDARY_METADATA_KEY.to_owned(),json!({"schema":"elitea.pipeline.tool-boundary.v1","container_invocation_id":container,"parent_call_id":"outer-call","checkpoint_thread_id":"conversation/outer-call"}).to_string());
    }
    fn edge(event: &mut Event, container: &str, parent: &str, thread: Option<&str>) {
        event.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            container.to_owned(),
        );
        event
            .provider_metadata
            .insert(DESCENDANT_PARENT_CALL_KEY.to_owned(), parent.to_owned());
        if let Some(thread) = thread {
            event.provider_metadata.insert(
                DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                thread.to_owned(),
            );
        }
        outer(event, "ordinary-root");
    }
    fn call(id: &str, invocation: &str, name: &str, call_id: &str, args: Value) -> Event {
        let mut event = Event::with_id(id, invocation);
        event.branch = APPLICATION_BRANCH_ROOT.to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: name.to_owned(),
                args,
                id: Some(call_id.to_owned()),
                thought_signature: None,
            }],
        });
        event
    }
    async fn fixture() -> (Vec<Event>, BTreeSet<String>) {
        fixture_with_static(false).await
    }

    #[allow(clippy::too_many_lines)] // Keep three checkpoint levels and their original dynamic/static receipts in one ordered fixture.
    pub(crate) async fn fixture_with_static(static_tools: bool) -> (Vec<Event>, BTreeSet<String>) {
        let original = call(
            "original-parent-model",
            "ordinary-root",
            "saved_pipeline",
            "outer-call",
            json!({"task":"work"}),
        );
        let lineage = PipelineToolCallLineage::from_call(&original, 0).unwrap();
        let checkpointer = MemoryCheckpointer::new();
        let root_definition = definition("delegate");
        let middle_definition = definition("delegate");
        let leaf_definition = definition("agent");
        let root_revision = revision(1, &root_definition);
        let middle_revision = revision(2, &middle_definition);
        let leaf_revision = revision(3, &leaf_definition);
        let specifications = [
            ("", "delegate", &root_definition, root_revision.clone()),
            (
                "delegate",
                "delegate",
                &middle_definition,
                middle_revision.clone(),
            ),
            (
                "delegate/delegate",
                "agent",
                &leaf_definition,
                leaf_revision.clone(),
            ),
        ];
        let mut events = vec![original];
        let mut parent_call = "outer-call".to_owned();
        let mut parent_container = "ordinary-root".to_owned();
        let mut branch = format!("{APPLICATION_BRANCH_ROOT}.application_1");
        let mut leaf_record = None;
        let mut leaf_context = None;
        for (path, node, definition, revision) in specifications {
            let thread = if path.is_empty() {
                "conversation/outer-call".to_owned()
            } else {
                format!("conversation/outer-call/{path}")
            };
            let context = NodeContext::new(State::new(), ExecutionConfig::new(&thread), 2);
            checkpointer
                .save(&Checkpoint::new(
                    &thread,
                    State::new(),
                    2,
                    vec![node.to_owned()],
                ))
                .await
                .unwrap();
            let scope =
                PipelineApplicationScope::new(path.to_owned(), definition, Some(revision)).unwrap();
            let activation = scope.activate(&thread, node, 2).unwrap();
            let invocation = format!("pipeline-child:{parent_call}");
            let (record, _) = prepare_graph_call(
                &checkpointer,
                &Mutex::new(()),
                &context,
                activation,
                "assistant",
                &json!({"task":"work"}),
                &invocation,
                "saved-graph",
                &branch,
            )
            .await
            .unwrap();
            let mut start = record.original_start().clone();
            edge(&mut start, &parent_container, &parent_call, Some(&thread));
            events.push(start);
            parent_call = record.activation().call_id().to_owned();
            parent_container = invocation;
            branch.push_str(".application_2");
            leaf_record = Some(record);
            leaf_context = Some(context);
        }
        let record = leaf_record.unwrap();
        let context = leaf_context.unwrap();
        let mut ids = BTreeSet::new();
        if static_tools {
            let mut model = call(
                "leaf-static-model",
                "ordinary-static-leaf",
                "saved_inner",
                "inner-static-1",
                json!({"task":"work"}),
            );
            model
                .llm_response
                .content
                .as_mut()
                .unwrap()
                .parts
                .push(Part::FunctionCall {
                    name: "saved_inner".to_owned(),
                    args: json!({"task":"work"}),
                    id: Some("inner-static-2".to_owned()),
                    thought_signature: None,
                });
            model.branch = branch.clone();
            edge(&mut model, &parent_container, &parent_call, None);
            record
                .activation()
                .stamp_with_branch(&mut model, record.branch().to_owned())
                .unwrap();
            events.push(model.clone());
            let catalog = PipelineCheckpointCatalog {
                root: Some(root_revision.clone()),
                descendants: BTreeMap::from([
                    ("delegate".to_owned(), middle_revision.clone()),
                    ("delegate/delegate".to_owned(), leaf_revision.clone()),
                    ("completed".to_owned(), revision(4, &leaf_definition)),
                ]),
            };
            for index in 0..2 {
                let (_, _, mut pause, _) =
                    super::super::static_pause_fixture(&model, index, "after");
                pause.branch = format!("{branch}.application_{}", index + 1);
                record
                    .activation()
                    .stamp_with_branch(&mut pause, record.branch().to_owned())
                    .unwrap();
                super::super::append_outer_boundary(
                    &mut pause,
                    "ordinary-root",
                    "outer-call",
                    "conversation/outer-call",
                    lineage.clone(),
                    &catalog,
                )
                .unwrap();
                ids.insert(
                    super::super::static_pipeline_tool_pause(&pause)
                        .unwrap()
                        .unwrap()
                        .pause_id,
                );
                events.push(pause);
            }
        }
        for ordinal in 1..=if static_tools { 0 } else { 2 } {
            let invocation = format!("ordinary-leaf-{ordinal}");
            let call_id = if static_tools {
                format!("inner-static-{ordinal}")
            } else {
                format!("sensitive-{ordinal}")
            };
            let args = if static_tools {
                json!({"task":"work"})
            } else {
                json!({"value":ordinal})
            };
            let name = if static_tools { "saved_inner" } else { "write" };
            let mut model = call(
                &format!("leaf-model-{ordinal}"),
                &invocation,
                name,
                &call_id,
                args.clone(),
            );
            model.branch = branch.clone();
            edge(&mut model, &parent_container, &parent_call, None);
            record
                .activation()
                .stamp_with_branch(&mut model, record.branch().to_owned())
                .unwrap();
            events.push(model);
            let mut confirmation = Event::with_id(format!("confirmation-{ordinal}"), &invocation);
            confirmation.branch = branch.clone();
            confirmation.actions.tool_confirmation = Some(ToolConfirmationRequest {
                tool_name: "write".to_owned(),
                function_call_id: Some(call_id),
                args,
            });
            confirmation.llm_response.interrupted = true;
            edge(&mut confirmation, &parent_container, &parent_call, None);
            record
                .activation()
                .stamp_with_branch(&mut confirmation, record.branch().to_owned())
                .unwrap();
            ids.insert(confirmation_id(&confirmation).unwrap());
            events.push(confirmation);
        }
        finish_graph_call(
            &checkpointer,
            &Mutex::new(()),
            &context,
            &record,
            GraphCallOutcome::Paused {
                result: crate::agents::application_tools::nested_interrupt_result(&ids),
            },
        )
        .await
        .unwrap();
        let root = checkpointer
            .load("conversation/outer-call")
            .await
            .unwrap()
            .unwrap();
        let middle = checkpointer
            .load("conversation/outer-call/delegate")
            .await
            .unwrap()
            .unwrap();
        let leaf = checkpointer
            .load("conversation/outer-call/delegate/delegate")
            .await
            .unwrap()
            .unwrap();
        let completed = Checkpoint::new(
            "conversation/outer-call/completed",
            [("effect_receipt".to_owned(), json!("exactly-once"))]
                .into_iter()
                .collect(),
            8,
            vec![],
        );
        let data = json!({"subgraph":"delegate","thread":middle.thread_id,"checkpoint_id":middle.checkpoint_id,"data":{"subgraph":"delegate","thread":leaf.thread_id,"checkpoint_id":leaf.checkpoint_id,"data":{"schema_revision":crate::agents::graph::PIPELINE_APPLICATION_HITL_SCHEMA,"guardrail_type":"application_sensitive_tool","node_name":"agent","application_call_id":record.activation().call_id(),"interrupt_ids":ids}}});
        let payload = GraphInterruptPayload {
            kind: "dynamic".to_owned(),
            node: None,
            message: Some("Approval".to_owned()),
            data: Some(data),
            thread_id: root.thread_id.clone(),
            checkpoint_id: root.checkpoint_id.clone(),
        };
        let family = PipelinePendingEnvelope::Family(PipelineToolPendingFamily {
            schema_revision: PIPELINE_TOOL_TYPED_FAMILY_SCHEMA.to_owned(),
            pause_kind: Some(PipelineToolPauseKind::Application),
            thread_id: root.thread_id.clone(),
            checkpoint_id: root.checkpoint_id.clone(),
            node_name: "delegate".to_owned(),
            checkpoint: root,
            catalog: PipelineCheckpointCatalog {
                root: Some(root_revision),
                descendants: BTreeMap::from([
                    ("delegate".to_owned(), middle_revision),
                    ("delegate/delegate".to_owned(), leaf_revision),
                    ("completed".to_owned(), revision(4, &leaf_definition)),
                ]),
            },
            descendant_checkpoints: vec![middle, leaf, completed],
            call_lineage: Some(lineage),
        });
        family.validate().unwrap();
        let mut pending = Event::with_id("family-pause", "pipeline-child:outer-call");
        pending.author = "saved_pipeline".to_owned();
        pending.branch = events[1].branch.clone();
        edge(
            &mut pending,
            "ordinary-root",
            "outer-call",
            Some("conversation/outer-call"),
        );
        pending.provider_metadata.insert(
            adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY.to_owned(),
            payload.to_metadata_value(),
        );
        pending.provider_metadata.insert(
            PIPELINE_TOOL_PENDING_METADATA_KEY.to_owned(),
            serde_json::to_string(&family).unwrap(),
        );
        events.push(pending);
        (events, ids)
    }

    #[tokio::test]
    async fn deeper_ordinary_pause_retains_only_exact_call_chain_and_all_pending_leaves() {
        let (events, ids) = fixture().await;
        assert!(pipeline_application_pending_event(events.last().unwrap()).unwrap());
        let boundary = pipeline_application_boundary(&events, &events[5])
            .unwrap()
            .unwrap();
        let retained = retained_pipeline_application_events(&events, &boundary, &ids).unwrap();
        assert_eq!(retained.len(), 8);
        assert!(retained.iter().all(|event|event.id!="original-parent-model" && event.tool_results().is_empty()));
        let (original, _, ordinal) = pipeline_boundary_original_call(&events, &boundary).unwrap();
        assert_eq!(ordinal, 1);
        validate_retained_pipeline_application_pause(
            &original,
            &boundary.pending_event,
            &retained,
            &ids,
        )
        .unwrap();
        let pending = family_from_event(&boundary.pending_event).unwrap();
        assert_eq!(
            pending
                .checkpoint("conversation/outer-call/completed")
                .unwrap()
                .state
                .get("effect_receipt"),
            Some(&json!("exactly-once"))
        );
        let mut missing = retained.clone();
        missing.remove(1);
        assert!(
            validate_retained_pipeline_application_pause(
                &original,
                &boundary.pending_event,
                &missing,
                &ids
            )
            .is_err()
        );
        let mut subset = ids.clone();
        subset.pop_first();
        assert!(retained_pipeline_application_events(&events, &boundary, &subset).is_err());
        let mut changed = retained;
        changed[3].actions.tool_confirmation = Some(ToolConfirmationRequest {
            tool_name: "write".to_owned(),
            function_call_id: Some("sensitive-1".to_owned()),
            args: json!({"value":"forged"}),
        });
        assert!(
            validate_retained_pipeline_application_pause(
                &original,
                &boundary.pending_event,
                &changed,
                &ids
            )
            .is_err()
        );
    }

    #[tokio::test]
    async fn latest_replay_family_joins_original_batch_and_keeps_leaf_identity() {
        let (mut events, ids) = fixture().await;
        let original_leaf = events[5].clone();
        let mut replay = events[0].clone();
        replay.id = "replay-parent-model".to_owned();
        replay.invocation_id = "ordinary-replay".to_owned();
        replay.llm_response.provider_metadata = Some(
            json!({"elitea.application.replay_batch.v1":{"event_id":"original-parent-model","interrupt_ids":[ids.first().unwrap()],"call_ordinals":{"outer-call":1}}}),
        );
        events.push(replay);
        let mut latest = events[8].clone();
        latest.id = "latest-family-pause".to_owned();
        rebind_pipeline_tool_boundary(&mut latest, "ordinary-replay").unwrap();
        events.push(latest);
        let boundary = pipeline_application_boundary(&events, &original_leaf)
            .unwrap()
            .unwrap();
        assert_eq!(boundary.pending_event.id, "latest-family-pause");
        assert_eq!(boundary.container_invocation_id, "ordinary-replay");
        let projected = retained_pipeline_application_events(&events, &boundary, &ids).unwrap();
        let leaf = projected
            .iter()
            .find(|event| event.id == original_leaf.id)
            .unwrap();
        assert_eq!(leaf.invocation_id, original_leaf.invocation_id);
        assert_eq!(
            confirmation_id(leaf).unwrap(),
            confirmation_id(&original_leaf).unwrap()
        );
        assert_eq!(
            boundary_receipt(leaf)
                .unwrap()
                .unwrap()
                .container_invocation_id,
            "ordinary-replay"
        );
        assert_eq!(
            boundary_receipt(&original_leaf)
                .unwrap()
                .unwrap()
                .container_invocation_id,
            "ordinary-root"
        );
        let mut forged = events.clone();
        forged[9].llm_response.content.as_mut().unwrap().parts = vec![Part::FunctionCall {
            name: "saved_pipeline".to_owned(),
            args: json!({"task":"changed"}),
            id: Some("outer-call".to_owned()),
            thought_signature: None,
        }];
        assert!(pipeline_application_boundary(&forged, &original_leaf).is_err());
    }
    #[tokio::test]
    async fn outer_static_family_preserves_nearest_receipt_and_minimal_retained_leaf_chain() {
        let (events, ids) = fixture_with_static(true).await;
        let originals = serde_json::to_vec(&events).unwrap();
        let leaf = &events[5];
        let inner = super::super::static_pipeline_tool_pause(leaf)
            .unwrap()
            .unwrap();
        let nearest = super::super::boundary_receipt(leaf).unwrap().unwrap();
        let boundary = pipeline_application_boundary(&events, leaf)
            .unwrap()
            .unwrap();
        assert_eq!(boundary.parent_call_id, "outer-call");
        assert_eq!(boundary.checkpoint_thread_id, "conversation/outer-call");
        assert_eq!(nearest.parent_call_id, inner.parent_call_id);
        assert_ne!(nearest.parent_call_id, boundary.parent_call_id);
        let retained = retained_pipeline_application_events(&events, &boundary, &ids).unwrap();
        assert_eq!(retained.len(), 7);
        let (original, _, ordinal) = pipeline_boundary_original_call(&events, &boundary).unwrap();
        assert_eq!(ordinal, 1);
        validate_retained_pipeline_application_pause(
            &original,
            &boundary.pending_event,
            &retained,
            &ids,
        )
        .unwrap();
        let mut rebound = retained.clone();
        for event in &mut rebound {
            rebind_pipeline_tool_boundary(event, "replacement-root").unwrap();
        }
        let rebound_leaf = rebound.iter().find(|event| event.id == leaf.id).unwrap();
        let unchanged = super::super::static_pipeline_tool_pause(rebound_leaf)
            .unwrap()
            .unwrap();
        assert_eq!(
            unchanged.container_invocation_id,
            inner.container_invocation_id
        );
        assert_eq!(unchanged.pause_id, inner.pause_id);
        assert_eq!(
            super::super::boundary_ledger::outer_boundary_receipt(rebound_leaf)
                .unwrap()
                .unwrap()
                .container_invocation_id,
            "replacement-root"
        );
        assert_eq!(serde_json::to_vec(&events).unwrap(), originals);
        let mut foreign = events.clone();
        foreign[0].id = "foreign-batch".to_owned();
        assert!(pipeline_application_boundary(&foreign, &foreign[5]).is_err());
    }
}
