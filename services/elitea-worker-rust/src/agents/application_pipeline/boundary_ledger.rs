//! Ordered outer saved-tool boundaries; the nearest inner receipt remains unchanged.
use super::{
    PipelineToolBoundaryReceipt, PipelineToolCallLineage, boundary_receipt, invalid_boundary,
};
use crate::agents::pipeline::composition::{
    MAX_PIPELINE_COMPOSITION_DEPTH, PipelineCheckpointCatalog,
};
use crate::agents::pipeline::scoped_applications::{
    PipelineApplicationActivation, activation_for_catalog, activations_for_event,
    checkpoint_thread_for_event,
};
use crate::agents::runtime::NativeAgentAssemblyError;
use adk_rust::Event;
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

pub(crate) const BOUNDARY_LEDGER_KEY: &str = "elitea.pipeline.tool-boundary-ledger.v1";
const MAX_LEDGER_BYTES: usize = 16 * 1024;
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct OuterBoundaryFrame {
    boundary: PipelineToolBoundaryReceipt,
    lineage: PipelineToolCallLineage,
    activation: PipelineApplicationActivation,
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct BoundaryLedger {
    schema: String,
    outer: Vec<OuterBoundaryFrame>,
}

fn decode(event: &Event) -> Result<Option<BoundaryLedger>, NativeAgentAssemblyError> {
    let Some(raw) = event.provider_metadata.get(BOUNDARY_LEDGER_KEY) else {
        return Ok(None);
    };
    if raw.len() > MAX_LEDGER_BYTES {
        return Err(invalid_boundary());
    }
    let value: BoundaryLedger = serde_json::from_str(raw).map_err(|_| invalid_boundary())?;
    let nearest = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
    if value.schema != BOUNDARY_LEDGER_KEY
        || value.outer.is_empty()
        || value.outer.len() >= MAX_PIPELINE_COMPOSITION_DEPTH
    {
        return Err(invalid_boundary());
    }
    let mut seen = BTreeSet::from([(nearest.parent_call_id, nearest.checkpoint_thread_id)]);
    let activations = activations_for_event(event)?;
    let mut previous_scope = None;
    for frame in &value.outer {
        frame.lineage.validate()?;
        let position = activations
            .iter()
            .position(|activation| activation == &frame.activation)
            .ok_or_else(invalid_boundary)?;
        if previous_scope.is_some_and(|previous| position <= previous)
            || !frame.activation.matches_event(event)?
        {
            return Err(invalid_boundary());
        }
        previous_scope = Some(position);
        if frame.boundary.schema != "elitea.pipeline.tool-boundary.v1"
            || frame.boundary.parent_call_id != frame.lineage.parent_call_id
            || [
                &frame.boundary.container_invocation_id,
                &frame.boundary.parent_call_id,
                &frame.boundary.checkpoint_thread_id,
            ]
            .iter()
            .any(|id| !super::valid_family_identity(id))
            || !seen.insert((
                frame.boundary.parent_call_id.clone(),
                frame.boundary.checkpoint_thread_id.clone(),
            ))
        {
            return Err(invalid_boundary());
        }
    }
    Ok(Some(value))
}

pub(super) fn outer_boundary_receipt(
    event: &Event,
) -> Result<Option<PipelineToolBoundaryReceipt>, NativeAgentAssemblyError> {
    match decode(event)? {
        Some(value) => Ok(value.outer.last().map(|frame| frame.boundary.clone())),
        None => boundary_receipt(event),
    }
}
pub(super) fn validate_outer_lineage(
    event: &Event,
    lineage: &PipelineToolCallLineage,
) -> Result<(), NativeAgentAssemblyError> {
    if let Some(value) = decode(event)?
        && value
            .outer
            .last()
            .is_none_or(|frame| &frame.lineage != lineage)
    {
        return Err(invalid_boundary());
    }
    Ok(())
}

pub(crate) struct OuterBoundaryProjectionBinding {
    pub(crate) container: String,
    pub(crate) call: String,
    pub(crate) thread: String,
    pub(crate) lineage: PipelineToolCallLineage,
}
pub(crate) fn projected_outer_boundaries(
    event: &Event,
) -> Result<Vec<OuterBoundaryProjectionBinding>, NativeAgentAssemblyError> {
    Ok(decode(event)?.map_or_else(Vec::new, |value| {
        value
            .outer
            .into_iter()
            .map(|frame| OuterBoundaryProjectionBinding {
                container: frame.boundary.container_invocation_id,
                call: frame.boundary.parent_call_id,
                thread: frame.boundary.checkpoint_thread_id,
                lineage: frame.lineage,
            })
            .collect()
    }))
}

/// A rebuilt saved-tool coordinator proves its exact outer call before local projection.
pub(crate) fn validate_scoped_outer_boundary(
    event: &Event,
    root_thread: &str,
    expected: Option<&PipelineToolCallLineage>,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(value) = decode(event)? else {
        return Ok(());
    };
    let expected = expected.ok_or_else(invalid_boundary)?;
    let frame = value.outer.last().ok_or_else(invalid_boundary)?;
    if frame.boundary.checkpoint_thread_id != root_thread || frame.lineage != *expected {
        return Err(invalid_boundary());
    }
    Ok(())
}

/// Remove only the parent boundary that the current graph owner has proved.
pub(crate) fn project_scope_boundary(
    event: &mut Event,
    root_thread: &str,
    activation: &PipelineApplicationActivation,
) -> Result<(), NativeAgentAssemblyError> {
    if let Some(mut ledger) = decode(event)? {
        let frame = ledger.outer.pop().ok_or_else(invalid_boundary)?;
        if frame.boundary.checkpoint_thread_id != root_thread || frame.activation != *activation {
            return Err(invalid_boundary());
        }
        if ledger.outer.is_empty() {
            event.provider_metadata.remove(BOUNDARY_LEDGER_KEY);
        } else {
            event.provider_metadata.insert(
                BOUNDARY_LEDGER_KEY.to_owned(),
                serde_json::to_string(&ledger).map_err(|_| invalid_boundary())?,
            );
        }
    }
    if boundary_receipt(event)?.is_some_and(|receipt| receipt.checkpoint_thread_id == root_thread) {
        event
            .provider_metadata
            .remove(crate::agents::events::PIPELINE_TOOL_BOUNDARY_METADATA_KEY);
    }
    Ok(())
}

/// Called only by the outer saved-tool sender with its captured original call lineage.
pub(crate) fn append_outer_boundary(
    event: &mut Event,
    container: &str,
    call: &str,
    thread: &str,
    lineage: PipelineToolCallLineage,
    catalog: &PipelineCheckpointCatalog,
) -> Result<(), NativeAgentAssemblyError> {
    lineage.validate()?;
    if lineage.parent_call_id != call
        || [container, call, thread]
            .iter()
            .any(|id| !super::valid_family_identity(id))
    {
        return Err(invalid_boundary());
    }
    let proved =
        checkpoint_thread_for_event(thread, catalog, event)?.ok_or_else(invalid_boundary)?;
    let activation =
        activation_for_catalog(thread, catalog, event)?.ok_or_else(invalid_boundary)?;
    if activation.thread_id() != proved {
        return Err(invalid_boundary());
    }
    let mut value = decode(event)?.unwrap_or(BoundaryLedger {
        schema: BOUNDARY_LEDGER_KEY.to_owned(),
        outer: Vec::new(),
    });
    let nearest = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
    if nearest.parent_call_id == call && nearest.checkpoint_thread_id == thread
        || value.outer.len() + 1 >= MAX_PIPELINE_COMPOSITION_DEPTH
        || value.outer.iter().any(|frame| {
            (frame.boundary.parent_call_id == call && frame.boundary.checkpoint_thread_id == thread)
                || frame.lineage == lineage
        })
    {
        return Err(invalid_boundary());
    }
    value.outer.push(OuterBoundaryFrame {
        boundary: PipelineToolBoundaryReceipt {
            schema: "elitea.pipeline.tool-boundary.v1".to_owned(),
            container_invocation_id: container.to_owned(),
            parent_call_id: call.to_owned(),
            checkpoint_thread_id: thread.to_owned(),
        },
        lineage,
        activation,
    });
    let encoded = serde_json::to_string(&value).map_err(|_| invalid_boundary())?;
    if encoded.len() > MAX_LEDGER_BYTES {
        return Err(invalid_boundary());
    }
    event
        .provider_metadata
        .insert(BOUNDARY_LEDGER_KEY.to_owned(), encoded);
    decode(event)?;
    Ok(())
}

/// Only a validated retained projection may change the current outer container.
pub(super) fn rebind_outer_ledger(
    event: &mut Event,
    new_container: &str,
) -> Result<bool, NativeAgentAssemblyError> {
    let Some(mut value) = decode(event)? else {
        return Ok(false);
    };
    if !super::valid_family_identity(new_container) {
        return Err(invalid_boundary());
    }
    let frame = value.outer.last_mut().ok_or_else(invalid_boundary)?;
    if frame.boundary.container_invocation_id == new_container {
        return Ok(true);
    }
    new_container.clone_into(&mut frame.boundary.container_invocation_id);
    event.provider_metadata.insert(
        BOUNDARY_LEDGER_KEY.to_owned(),
        serde_json::to_string(&value).map_err(|_| invalid_boundary())?,
    );
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::application_pipeline::{
        PIPELINE_TOOL_BOUNDARY_METADATA_KEY, PipelinePendingEnvelope, family_from_event,
    };
    use crate::agents::application_tools::{ApplicationEventSignal, application_signal_event};
    use std::sync::Arc;

    #[tokio::test]
    async fn outer_static_sender_proves_exact_activation_before_appending() {
        let (events, _) =
            crate::agents::application_pipeline::boundary::tests::fixture_with_static(true).await;
        let PipelinePendingEnvelope::Family(family) =
            family_from_event(events.last().unwrap()).unwrap()
        else {
            panic!("typed family")
        };
        let lineage = PipelineToolCallLineage::from_call(&events[0], 0).unwrap();
        let mut leaf = events[5].clone();
        leaf.provider_metadata.remove(BOUNDARY_LEDGER_KEY);
        let nearest = leaf.provider_metadata[PIPELINE_TOOL_BOUNDARY_METADATA_KEY].clone();
        let before = serde_json::to_vec(&leaf).unwrap();
        let signal = |event: Event,
                      catalog: PipelineCheckpointCatalog,
                      lineage: Option<PipelineToolCallLineage>| {
            ApplicationEventSignal::GraphDescendant {
                root_container_invocation_id: "ordinary-root".to_owned(),
                root_parent_call_id: "outer-call".to_owned(),
                root_checkpoint_thread_id: "conversation/outer-call".to_owned(),
                catalog: Arc::new(catalog),
                lineage,
                event: Box::new(event),
            }
        };
        let emitted = application_signal_event(signal(
            leaf.clone(),
            family.catalog.clone(),
            Some(lineage.clone()),
        ))
        .unwrap();
        assert_eq!(
            emitted.provider_metadata[PIPELINE_TOOL_BOUNDARY_METADATA_KEY],
            nearest
        );
        assert_eq!(emitted.id, leaf.id);
        let frames = projected_outer_boundaries(&emitted).unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].thread, "conversation/outer-call");
        assert!(frames[0].lineage == lineage);
        assert_eq!(serde_json::to_vec(&leaf).unwrap(), before);
        assert!(
            application_signal_event(signal(leaf.clone(), family.catalog.clone(), None)).is_err()
        );
        let mut wrong = family.catalog.clone();
        wrong
            .descendants
            .get_mut("delegate/delegate")
            .unwrap()
            .version_id += 1;
        assert!(
            application_signal_event(signal(leaf.clone(), wrong, Some(lineage.clone()))).is_err()
        );
        assert!(application_signal_event(signal(emitted, family.catalog, Some(lineage))).is_err());
    }

    #[tokio::test]
    async fn outer_static_ledger_rejects_duplicate_drifted_and_oversized_frames() {
        let (events, _) =
            crate::agents::application_pipeline::boundary::tests::fixture_with_static(true).await;
        let original = serde_json::to_vec(&events[5]).unwrap();
        let mut duplicate = events[5].clone();
        let mut value: serde_json::Value =
            serde_json::from_str(&duplicate.provider_metadata[BOUNDARY_LEDGER_KEY]).unwrap();
        let frame = value["outer"][0].clone();
        value["outer"].as_array_mut().unwrap().push(frame);
        duplicate
            .provider_metadata
            .insert(BOUNDARY_LEDGER_KEY.to_owned(), value.to_string());
        assert!(projected_outer_boundaries(&duplicate).is_err());
        let mut drift = events.clone();
        let mut value: serde_json::Value =
            serde_json::from_str(&drift[5].provider_metadata[BOUNDARY_LEDGER_KEY]).unwrap();
        value["outer"][0]["lineage"]["original_batch_event_id"] = "foreign-batch".into();
        drift[5]
            .provider_metadata
            .insert(BOUNDARY_LEDGER_KEY.to_owned(), value.to_string());
        assert!(
            crate::agents::application_pipeline::pipeline_application_boundary(&drift, &drift[5])
                .is_err()
        );
        let mut oversized = events[5].clone();
        oversized.provider_metadata.insert(
            BOUNDARY_LEDGER_KEY.to_owned(),
            "x".repeat(MAX_LEDGER_BYTES + 1),
        );
        assert!(projected_outer_boundaries(&oversized).is_err());
        let mut missing = events[5].clone();
        missing
            .provider_metadata
            .remove(PIPELINE_TOOL_BOUNDARY_METADATA_KEY);
        assert!(projected_outer_boundaries(&missing).is_err());
        assert_eq!(serde_json::to_vec(&events[5]).unwrap(), original);
    }
}
