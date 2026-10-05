//! Exact graph-scope ownership before an ordinary child continuation is installed.

use std::collections::{BTreeMap, BTreeSet};

use adk_rust::Event;
use adk_rust::graph::Checkpointer;
use ring::digest;
use serde::{Deserialize, Serialize};

use super::composition::{
    MAX_PIPELINE_CHECKPOINT_PATHS, MAX_PIPELINE_COMPOSITION_DEPTH, PipelineCheckpointCatalog,
    PipelineCheckpointRevision,
};
use crate::agents::direct_hitl::ResolvedDirectHitlDecision;
use crate::agents::events::{
    APPLICATION_BRANCH_ROOT, DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY, PIPELINE_TOOL_BOUNDARY_METADATA_KEY,
};
use crate::agents::graph::compiler::PipelineDefinition;
use crate::agents::graph::static_tool_pause::PipelineCheckpointFamilyView;
use crate::agents::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::agents::session::ApplicationRuntimeProjection;

pub(crate) const PIPELINE_APPLICATION_SCOPE_METADATA_KEY: &str =
    "elitea.pipeline.application-scope.v1";
pub(crate) const PIPELINE_ROOT_PROJECTION_METADATA_KEY: &str = "elitea.pipeline.root-projection.v1";
const SCOPE_SCHEMA: &str = "elitea.pipeline.application-scope.v1";
const EVENT_SCOPE_SCHEMA: &str = "elitea.pipeline.application-event-scope.v1";
const ACTIVATION_SCHEMA: &str = "elitea.pipeline.application-activation.v1";
const CALL_DOMAIN: &[u8] = b"elitea.pipeline.application-call.v2\0";
const MAX_SCOPE_METADATA_BYTES: usize = 4 * 1024;
const MAX_CHECKPOINT_THREAD_BYTES: usize = 480;
pub(crate) const MAX_ORDINARY_GRAPH_SCOPES: usize = 2;
pub(crate) const ACTIVATION_CHAIN_KEY: &str = "elitea.pipeline.application-chain.v1";
pub(crate) const LOCAL_PROJECTION_KEY: &str = "elitea.pipeline.application-local-projection.v1";
const MAX_CHAIN_BYTES: usize = 12 * 1024;

/// A frozen graph occurrence, not an alias or a thread prefix grant.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineApplicationScope {
    schema: String,
    graph_path: String,
    definition_digest: [u8; 32],
    saved_revision: Option<PipelineCheckpointRevision>,
}

impl PipelineApplicationScope {
    pub(crate) fn new(
        graph_path: String,
        definition: &PipelineDefinition,
        saved_revision: Option<PipelineCheckpointRevision>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let value = Self {
            schema: SCOPE_SCHEMA.to_owned(),
            graph_path,
            definition_digest: definition.definition_digest(),
            saved_revision,
        };
        value.validate()?;
        Ok(value)
    }

    pub(crate) fn activate(
        &self,
        thread: &str,
        node: &str,
        step: usize,
    ) -> Result<PipelineApplicationActivation, NativeAgentAssemblyError> {
        PipelineApplicationActivation::new(self.clone(), thread.to_owned(), node.to_owned(), step)
    }

    fn validate(&self) -> Result<(), NativeAgentAssemblyError> {
        if self.schema != SCOPE_SCHEMA
            || !valid_path(&self.graph_path, true)
            || self.definition_digest == [0; 32]
            || self.saved_revision.as_ref().is_some_and(|revision| {
                revision.application_id == 0
                    || revision.version_id == 0
                    || revision.definition_digest != self.definition_digest
            })
            || (!self.graph_path.is_empty() && self.saved_revision.is_none())
        {
            return Err(invalid_scope());
        }
        Ok(())
    }

    fn thread(&self, root_thread: &str) -> String {
        if self.graph_path.is_empty() {
            root_thread.to_owned()
        } else {
            format!("{root_thread}/{}", self.graph_path)
        }
    }
}

/// `NodeContext` supplies thread and step; the frozen registry supplies scope.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineApplicationActivation {
    schema: String,
    scope: PipelineApplicationScope,
    checkpoint_thread_id: String,
    node_name: String,
    step: usize,
    call_id: String,
}

impl PipelineApplicationActivation {
    fn new(
        scope: PipelineApplicationScope,
        checkpoint_thread_id: String,
        node_name: String,
        step: usize,
    ) -> Result<Self, NativeAgentAssemblyError> {
        scope.validate()?;
        if !valid_identity(&checkpoint_thread_id) || !valid_node(&node_name) {
            return Err(invalid_scope());
        }
        let call_id = scoped_call_id(&scope, &checkpoint_thread_id, &node_name, step);
        Ok(Self {
            schema: ACTIVATION_SCHEMA.to_owned(),
            scope,
            checkpoint_thread_id,
            node_name,
            step,
            call_id,
        })
    }

    #[must_use]
    pub(crate) fn call_id(&self) -> &str {
        &self.call_id
    }

    pub(crate) fn thread_id(&self) -> &str {
        &self.checkpoint_thread_id
    }
    pub(crate) fn node_name(&self) -> &str {
        &self.node_name
    }
    pub(crate) fn step(&self) -> usize {
        self.step
    }
    pub(crate) fn matches_event(&self, event: &Event) -> Result<bool, NativeAgentAssemblyError> {
        Ok(event_scopes(event)?.iter().any(|receipt| {
            receipt.activation == *self
                && local_application_branch(&event.branch, &receipt.root_branch).is_some()
        }))
    }

    pub(crate) fn matches_original_start(
        &self,
        event: &Event,
    ) -> Result<bool, NativeAgentAssemblyError> {
        Ok(
            PipelineApplicationEventScope::from_event(event)?.is_some_and(|receipt| {
                receipt.activation == *self && receipt.root_branch == event.branch
            }),
        )
    }

    /// The frozen runtime supplies both the activation and its exact local branch boundary.
    pub(crate) fn stamp(&self, event: &mut Event) -> Result<(), NativeAgentAssemblyError> {
        let root_branch = if event.branch.is_empty() {
            APPLICATION_BRANCH_ROOT.to_owned()
        } else {
            event.branch.clone()
        };
        self.stamp_with_branch(event, root_branch)
    }

    pub(crate) fn stamp_with_branch(
        &self,
        event: &mut Event,
        root_branch: String,
    ) -> Result<(), NativeAgentAssemblyError> {
        self.validate()?;
        let receipt = PipelineApplicationEventScope {
            schema: EVENT_SCOPE_SCHEMA.to_owned(),
            activation: self.clone(),
            root_branch,
        };
        receipt.validate()?;
        let raw = serde_json::to_string(&receipt).map_err(|_| invalid_scope())?;
        if raw.len() > MAX_SCOPE_METADATA_BYTES
            || event
                .provider_metadata
                .contains_key(PIPELINE_APPLICATION_SCOPE_METADATA_KEY)
        {
            return Err(invalid_scope());
        }
        event
            .provider_metadata
            .insert(PIPELINE_APPLICATION_SCOPE_METADATA_KEY.to_owned(), raw);
        Ok(())
    }

    fn validate(&self) -> Result<(), NativeAgentAssemblyError> {
        self.scope.validate()?;
        if self.schema != ACTIVATION_SCHEMA
            || !valid_identity(&self.checkpoint_thread_id)
            || !valid_node(&self.node_name)
            || self.call_id
                != scoped_call_id(
                    &self.scope,
                    &self.checkpoint_thread_id,
                    &self.node_name,
                    self.step,
                )
        {
            return Err(invalid_scope());
        }
        Ok(())
    }

    pub(crate) fn from_event(event: &Event) -> Result<Option<Self>, NativeAgentAssemblyError> {
        Ok(PipelineApplicationEventScope::from_event(event)?.map(|value| value.activation))
    }
}

/// Private persisted provenance. It never grants a graph path or credentials by itself.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineApplicationEventScope {
    schema: String,
    activation: PipelineApplicationActivation,
    root_branch: String,
}

impl PipelineApplicationEventScope {
    fn validate(&self) -> Result<(), NativeAgentAssemblyError> {
        self.activation.validate()?;
        if self.schema != EVENT_SCOPE_SCHEMA
            || local_application_branch(&self.root_branch, &self.root_branch).is_none()
        {
            return Err(invalid_scope());
        }
        Ok(())
    }

    fn from_event(event: &Event) -> Result<Option<Self>, NativeAgentAssemblyError> {
        let Some(raw) = event
            .provider_metadata
            .get(PIPELINE_APPLICATION_SCOPE_METADATA_KEY)
        else {
            return Ok(None);
        };
        if raw.len() > MAX_SCOPE_METADATA_BYTES {
            return Err(invalid_scope());
        }
        let value: Self = serde_json::from_str(raw).map_err(|_| invalid_scope())?;
        value.validate()?;
        Ok(Some(value))
    }
}

/// Nearest scope receipts stay immutable. Each outer frame names its own original graph start.
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineApplicationActivationFrame {
    scope: PipelineApplicationEventScope,
    original_start_event_id: String,
    original_start_digest: [u8; 32],
    child_application_call_id: String,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineApplicationActivationChain {
    schema: String,
    outer: Vec<PipelineApplicationActivationFrame>,
}

/// Only a proved parent continuation creates this transient branch view.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineApplicationLocalProjection {
    schema: String,
    source_branch: String,
    source_scope: PipelineApplicationEventScope,
    parent: PipelineApplicationActivationFrame,
}

fn start_digest(event: &Event) -> Result<[u8; 32], NativeAgentAssemblyError> {
    let value = serde_json::to_value(event).map_err(|_| invalid_scope())?;
    let bytes = serde_json::to_vec(&value).map_err(|_| invalid_scope())?;
    let mut result = [0; 32];
    result.copy_from_slice(digest::digest(&digest::SHA256, &bytes).as_ref());
    Ok(result)
}

fn activation_chain(
    event: &Event,
) -> Result<Vec<PipelineApplicationActivationFrame>, NativeAgentAssemblyError> {
    let Some(raw) = event.provider_metadata.get(ACTIVATION_CHAIN_KEY) else {
        return Ok(Vec::new());
    };
    if raw.len() > MAX_CHAIN_BYTES || event.provider_metadata.contains_key(LOCAL_PROJECTION_KEY) {
        return Err(invalid_scope());
    }
    let value: PipelineApplicationActivationChain =
        serde_json::from_str(raw).map_err(|_| invalid_scope())?;
    let mut previous =
        PipelineApplicationEventScope::from_event(event)?.ok_or_else(invalid_scope)?;
    if value.schema != ACTIVATION_CHAIN_KEY
        || value.outer.is_empty()
        || value.outer.len() >= MAX_ORDINARY_GRAPH_SCOPES
    {
        return Err(invalid_scope());
    }
    let mut seen = BTreeSet::from([previous.activation.call_id.clone()]);
    for frame in &value.outer {
        frame.scope.validate()?;
        if !valid_identity(&frame.original_start_event_id)
            || frame.original_start_digest == [0; 32]
            || frame.child_application_call_id != previous.activation.call_id
            || !seen.insert(frame.scope.activation.call_id.clone())
            || frame.scope.root_branch == previous.root_branch
            || local_application_branch(&previous.root_branch, &frame.scope.root_branch).is_none()
            || local_application_branch(&event.branch, &frame.scope.root_branch).is_none()
        {
            return Err(invalid_scope());
        }
        previous = frame.scope.clone();
    }
    Ok(value.outer)
}

fn local_projection(
    event: &Event,
) -> Result<Option<PipelineApplicationLocalProjection>, NativeAgentAssemblyError> {
    let Some(raw) = event.provider_metadata.get(LOCAL_PROJECTION_KEY) else {
        return Ok(None);
    };
    if raw.len() > MAX_CHAIN_BYTES || event.provider_metadata.contains_key(ACTIVATION_CHAIN_KEY) {
        return Err(invalid_scope());
    }
    let value: PipelineApplicationLocalProjection =
        serde_json::from_str(raw).map_err(|_| invalid_scope())?;
    value.source_scope.validate()?;
    value.parent.scope.validate()?;
    let current = PipelineApplicationEventScope::from_event(event)?.ok_or_else(invalid_scope)?;
    if value.schema != LOCAL_PROJECTION_KEY
        || !valid_identity(&value.parent.original_start_event_id)
        || value.parent.original_start_digest == [0; 32]
        || value.parent.child_application_call_id != current.activation.call_id
        || value.source_scope.activation != current.activation
        || value.source_scope.root_branch == value.parent.scope.root_branch
        || local_application_branch(
            &value.source_scope.root_branch,
            &value.parent.scope.root_branch,
        )
        .as_ref()
            != Some(&current.root_branch)
        || local_application_branch(&value.source_branch, &value.parent.scope.root_branch).as_ref()
            != Some(&event.branch)
    {
        return Err(invalid_scope());
    }
    Ok(Some(value))
}

fn event_scopes(
    event: &Event,
) -> Result<Vec<PipelineApplicationEventScope>, NativeAgentAssemblyError> {
    local_projection(event)?;
    let mut scopes = PipelineApplicationEventScope::from_event(event)?
        .into_iter()
        .collect::<Vec<_>>();
    scopes.extend(
        activation_chain(event)?
            .into_iter()
            .map(|frame| frame.scope),
    );
    Ok(scopes)
}

fn event_scope_for_activation(
    event: &Event,
    activation: &PipelineApplicationActivation,
) -> Result<Option<PipelineApplicationEventScope>, NativeAgentAssemblyError> {
    let mut selected = None;
    for scope in event_scopes(event)? {
        if scope.activation == *activation && selected.replace(scope).is_some() {
            return Err(invalid_scope());
        }
    }
    Ok(selected)
}

fn scope_for_catalog(
    root: &str,
    catalog: &PipelineCheckpointCatalog,
    event: &Event,
) -> Result<Option<PipelineApplicationEventScope>, NativeAgentAssemblyError> {
    let scopes = event_scopes(event)?;
    let had_scope = !scopes.is_empty();
    let mut selected = None;
    for scope in scopes {
        let activation = &scope.activation;
        let revision = if activation.scope.graph_path.is_empty() {
            catalog.root.as_ref()
        } else {
            catalog.descendants.get(&activation.scope.graph_path)
        };
        if revision == activation.scope.saved_revision.as_ref()
            && activation.scope.thread(root) == activation.checkpoint_thread_id
            && selected.replace(scope).is_some()
        {
            return Err(invalid_scope());
        }
    }
    if had_scope && selected.is_none() {
        return Err(invalid_scope());
    }
    Ok(selected)
}

/// The current graph receipt is persisted before the receiver can append this frame.
pub(crate) fn stamp_activation_parent(
    event: &mut Event,
    original: &super::scope_receipts::PipelineGraphCallReceipt,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(nearest) = PipelineApplicationEventScope::from_event(event)? else {
        return original
            .activation()
            .stamp_with_branch(event, original.branch().to_owned());
    };
    let mut outer = activation_chain(event)?;
    if outer.len() + 1 >= MAX_ORDINARY_GRAPH_SCOPES || local_projection(event)?.is_some() {
        return Err(invalid_scope());
    }
    let child = outer.last().map_or(&nearest, |frame| &frame.scope);
    let scope = PipelineApplicationEventScope {
        schema: EVENT_SCOPE_SCHEMA.to_owned(),
        activation: original.activation().clone(),
        root_branch: original.branch().to_owned(),
    };
    scope.validate()?;
    if child.activation == scope.activation
        || child.root_branch == scope.root_branch
        || local_application_branch(&child.root_branch, &scope.root_branch).is_none()
    {
        return Err(invalid_scope());
    }
    let child_application_call_id = child.activation.call_id.clone();
    outer.push(PipelineApplicationActivationFrame {
        scope,
        original_start_event_id: original.original_start().id.clone(),
        original_start_digest: start_digest(original.original_start())?,
        child_application_call_id,
    });
    let raw = serde_json::to_string(&PipelineApplicationActivationChain {
        schema: ACTIVATION_CHAIN_KEY.to_owned(),
        outer,
    })
    .map_err(|_| invalid_scope())?;
    if raw.len() > MAX_CHAIN_BYTES {
        return Err(invalid_scope());
    }
    event
        .provider_metadata
        .insert(ACTIVATION_CHAIN_KEY.to_owned(), raw);
    activation_chain(event)?;
    Ok(())
}

fn validate_activation_parent(
    event: &Event,
    original: &super::scope_receipts::PipelineGraphCallReceipt,
) -> Result<(), NativeAgentAssemblyError> {
    for frame in activation_chain(event)? {
        if frame.scope.activation == *original.activation()
            && (frame.original_start_event_id != original.original_start().id
                || frame.original_start_digest != start_digest(original.original_start())?
                || frame.scope.root_branch != original.branch())
        {
            return Err(invalid_scope());
        }
    }
    Ok(())
}

/// Current root routing is presentation context, not execution authority. The
/// original Event, leaf invocation, activation and checkpoint receipt stay intact.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineRootProjection {
    schema: String,
    original_invocation_id: String,
    current_invocation_id: String,
    activation_call_id: String,
}
pub(crate) fn stamp_root_scope_projection(
    event: &mut Event,
    original: &str,
    current: &str,
) -> Result<(), NativeAgentAssemblyError> {
    if original == current {
        return Ok(());
    }
    let receipt = PipelineApplicationEventScope::from_event(event)?.ok_or_else(invalid_scope)?;
    if !receipt.activation.scope.graph_path.is_empty()
        || !valid_identity(original)
        || !valid_identity(current)
    {
        return Err(invalid_scope());
    }
    let value = PipelineRootProjection {
        schema: PIPELINE_ROOT_PROJECTION_METADATA_KEY.to_owned(),
        original_invocation_id: original.to_owned(),
        current_invocation_id: current.to_owned(),
        activation_call_id: receipt.activation.call_id,
    };
    let encoded = serde_json::to_string(&value).map_err(|_| invalid_scope())?;
    if event
        .provider_metadata
        .insert(PIPELINE_ROOT_PROJECTION_METADATA_KEY.to_owned(), encoded)
        .is_some()
    {
        return Err(invalid_scope());
    }
    Ok(())
}
pub(crate) fn project_root_scope_event(
    event: &Event,
    expected: Option<&str>,
) -> Result<Option<Event>, NativeAgentAssemblyError> {
    let Some(raw) = event
        .provider_metadata
        .get(PIPELINE_ROOT_PROJECTION_METADATA_KEY)
    else {
        return Ok(None);
    };
    if raw.len() > 4096 {
        return Err(invalid_scope());
    }
    let value: PipelineRootProjection = serde_json::from_str(raw).map_err(|_| invalid_scope())?;
    let receipt = PipelineApplicationEventScope::from_event(event)?.ok_or_else(invalid_scope)?;
    if value.schema != PIPELINE_ROOT_PROJECTION_METADATA_KEY
        || !receipt.activation.scope.graph_path.is_empty()
        || !valid_identity(&value.original_invocation_id)
        || !valid_identity(&value.current_invocation_id)
        || value.original_invocation_id == value.current_invocation_id
        || value.activation_call_id != receipt.activation.call_id
        || expected.is_some_and(|expected| expected != value.current_invocation_id)
    {
        return Err(invalid_scope());
    }
    let mut projected = event.clone();
    projected
        .provider_metadata
        .remove(PIPELINE_ROOT_PROJECTION_METADATA_KEY);
    if projected.invocation_id == value.original_invocation_id {
        let owns_graph_call = projected
            .tool_calls()
            .iter()
            .any(|call| call.call_id == Some(value.activation_call_id.as_str()))
            || projected
                .tool_results()
                .iter()
                .any(|result| result.call_id == Some(value.activation_call_id.as_str()));
        if !owns_graph_call || projected.branch != receipt.root_branch {
            return Err(invalid_scope());
        }
        projected
            .invocation_id
            .clone_from(&value.current_invocation_id);
    }
    if projected
        .provider_metadata
        .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
        == Some(&value.original_invocation_id)
    {
        if projected.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY)
            != Some(&value.activation_call_id)
        {
            return Err(invalid_scope());
        }
        projected.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            value.current_invocation_id,
        );
    }
    Ok(Some(projected))
}

/// Compare against the checkpoint-owned original. Only transport projection edges
/// may differ; event identity, invocation, branch, content, arguments and timestamp
/// remain exact. This is not a general duplicate-event or provider-replay allowance.
fn canonical_graph_start(
    event: &Event,
    original: &Event,
    root_thread: &str,
    outer_lineage: Option<&crate::agents::application_pipeline::PipelineToolCallLineage>,
) -> Result<Event, NativeAgentAssemblyError> {
    let key = crate::agents::application_pipeline::STATIC_TOOL_THREAD_METADATA_KEY;
    if original.provider_metadata.contains_key(key) {
        return Err(invalid_scope());
    }
    let projection = local_projection(event)?;
    let mut left = event.clone();
    if let Some(projection) = &projection {
        left.branch.clone_from(&projection.source_branch);
        left.provider_metadata.insert(
            PIPELINE_APPLICATION_SCOPE_METADATA_KEY.to_owned(),
            serde_json::to_string(&projection.source_scope).map_err(|_| invalid_scope())?,
        );
        left.provider_metadata.remove(LOCAL_PROJECTION_KEY);
    }
    if event.provider_metadata.contains_key(key) {
        crate::agents::application_pipeline::validate_static_thread_for_saved_start(
            event,
            root_thread,
            outer_lineage.ok_or_else(invalid_scope)?,
        )?;
        // Only a proved transport copy loses this field. Stored originals stay exact.
        left.provider_metadata.remove(key);
    }
    let mut right = original.clone();
    for value in [&mut left, &mut right] {
        for key in [
            PIPELINE_ROOT_PROJECTION_METADATA_KEY,
            PIPELINE_TOOL_BOUNDARY_METADATA_KEY,
            DESCENDANT_CONTAINER_INVOCATION_KEY,
            DESCENDANT_PARENT_CALL_KEY,
            DESCENDANT_CHECKPOINT_THREAD_KEY,
        ] {
            value.provider_metadata.remove(key);
        }
    }
    if serde_json::to_value(left).map_err(|_| invalid_scope())?
        != serde_json::to_value(right).map_err(|_| invalid_scope())?
    {
        return Err(invalid_scope());
    }
    let mut proved = original.clone();
    if projection.is_some() {
        // Keep only the proved local view over the checkpoint-owned original.
        // The next scope owner consumes this view before ordinary replay.
        proved.branch.clone_from(&event.branch);
        for key in [
            PIPELINE_APPLICATION_SCOPE_METADATA_KEY,
            LOCAL_PROJECTION_KEY,
        ] {
            proved.provider_metadata.insert(
                key.to_owned(),
                event
                    .provider_metadata
                    .get(key)
                    .ok_or_else(invalid_scope)?
                    .clone(),
            );
        }
    }
    Ok(proved)
}

/// A scope receipt names one admitted graph occurrence; it never grants a prefix.
pub(crate) fn checkpoint_thread_for_event(
    root: &str,
    catalog: &PipelineCheckpointCatalog,
    event: &Event,
) -> Result<Option<String>, NativeAgentAssemblyError> {
    let Some(receipt) = scope_for_catalog(root, catalog, event)? else {
        return Ok(None);
    };
    let activation = receipt.activation;
    let revision = if activation.scope.graph_path.is_empty() {
        catalog.root.as_ref()
    } else {
        catalog.descendants.get(&activation.scope.graph_path)
    };
    if revision != activation.scope.saved_revision.as_ref()
        || activation.scope.thread(root) != activation.checkpoint_thread_id
    {
        return Err(invalid_scope());
    }
    Ok(Some(activation.checkpoint_thread_id))
}

pub(crate) fn activation_for_catalog(
    root: &str,
    catalog: &PipelineCheckpointCatalog,
    event: &Event,
) -> Result<Option<PipelineApplicationActivation>, NativeAgentAssemblyError> {
    Ok(scope_for_catalog(root, catalog, event)?.map(|scope| scope.activation))
}

pub(crate) fn activations_for_event(
    event: &Event,
) -> Result<Vec<PipelineApplicationActivation>, NativeAgentAssemblyError> {
    Ok(event_scopes(event)?
        .into_iter()
        .map(|scope| scope.activation)
        .collect())
}

/// Transient history projection only. The durable originals are not modified.
#[allow(clippy::too_many_lines)] // Keep parent proof, one-frame projection, exact routing, and deduplication ordered.
fn scope_local_events(
    events: &[Event],
    activation: &PipelineApplicationActivation,
    root_branch: &str,
    root_thread: &str,
) -> Result<Vec<Event>, NativeAgentAssemblyError> {
    let mut projected = Vec::with_capacity(events.len());
    for original in events {
        let receipt =
            event_scope_for_activation(original, activation)?.ok_or_else(invalid_scope)?;
        if receipt.root_branch != root_branch {
            return Err(invalid_scope());
        }
        let mut event = original.clone();
        crate::agents::application_pipeline::project_scope_boundary(
            &mut event,
            root_thread,
            activation,
        )?;
        event.branch =
            local_application_branch(&event.branch, root_branch).ok_or_else(invalid_scope)?;
        let outer = activation_chain(original)?;
        if let Some(frame) = outer.last() {
            if frame.scope.activation != *activation || outer.len() != 1 {
                return Err(invalid_scope());
            }
            let source_scope =
                PipelineApplicationEventScope::from_event(original)?.ok_or_else(invalid_scope)?;
            let mut local_scope = source_scope.clone();
            local_scope.root_branch =
                local_application_branch(&source_scope.root_branch, root_branch)
                    .ok_or_else(invalid_scope)?;
            event.provider_metadata.remove(ACTIVATION_CHAIN_KEY);
            event.provider_metadata.insert(
                PIPELINE_APPLICATION_SCOPE_METADATA_KEY.to_owned(),
                serde_json::to_string(&local_scope).map_err(|_| invalid_scope())?,
            );
            let raw = serde_json::to_string(&PipelineApplicationLocalProjection {
                schema: LOCAL_PROJECTION_KEY.to_owned(),
                source_branch: original.branch.clone(),
                source_scope,
                parent: frame.clone(),
            })
            .map_err(|_| invalid_scope())?;
            if raw.len() > MAX_CHAIN_BYTES {
                return Err(invalid_scope());
            }
            event
                .provider_metadata
                .insert(LOCAL_PROJECTION_KEY.to_owned(), raw);
            local_projection(&event)?;
        } else {
            // This owner has proved the incoming local view. It consumes that view
            // before its own ordinary history boundary is localized again.
            event.provider_metadata.remove(LOCAL_PROJECTION_KEY);
        }
        if event.branch == APPLICATION_BRANCH_ROOT {
            // A graph start is the synthetic model-call boundary for this one node.
            // Removing the outer container stops the ordinary builder at this graph.
            if !event
                .tool_calls()
                .iter()
                .any(|call| call.call_id == Some(activation.call_id()))
            {
                return Err(invalid_scope());
            }
            for key in [
                DESCENDANT_CONTAINER_INVOCATION_KEY,
                DESCENDANT_PARENT_CALL_KEY,
                DESCENDANT_CHECKPOINT_THREAD_KEY,
                PIPELINE_TOOL_BOUNDARY_METADATA_KEY,
            ] {
                event.provider_metadata.remove(key);
            }
        }
        if let Some(previous) = projected
            .iter()
            .find(|previous: &&Event| previous.id == event.id)
        {
            if serde_json::to_value(previous).map_err(|_| invalid_scope())?
                != serde_json::to_value(&event).map_err(|_| invalid_scope())?
            {
                return Err(invalid_scope());
            }
        } else {
            projected.push(event);
        }
    }
    Ok(projected)
}

/// Exact branch suffix, never a textual prefix match on a neighboring branch.
pub(crate) fn local_application_branch(branch: &str, root: &str) -> Option<String> {
    fn valid(value: &str) -> bool {
        if value == APPLICATION_BRANCH_ROOT {
            return true;
        }
        let Some(suffix) = value
            .strip_prefix(APPLICATION_BRANCH_ROOT)
            .and_then(|v| v.strip_prefix('.'))
        else {
            return false;
        };
        let parts = suffix.split('.').collect::<Vec<_>>();
        parts.len() < 16
            && parts.iter().all(|part| {
                part.strip_prefix("application_").is_some_and(|ordinal| {
                    !ordinal.is_empty() && ordinal.bytes().all(|v| v.is_ascii_digit())
                })
            })
    }
    if !valid(root) || !valid(branch) {
        return None;
    }
    if branch == root {
        return Some(APPLICATION_BRANCH_ROOT.to_owned());
    }
    let suffix = branch.strip_prefix(root)?.strip_prefix('.')?;
    Some(format!("{APPLICATION_BRANCH_ROOT}.{suffix}"))
}

/// Build only an exact checkpoint route from one recorded leaf activation.
/// Rebuilt catalog/runtime proof is still required before installing it.
pub(crate) fn route_from_event(
    root_thread: &str,
    family: &dyn PipelineCheckpointFamilyView,
    event: &Event,
) -> Result<Option<PipelineApplicationScopeRoute>, NativeAgentAssemblyError> {
    let catalog = family.catalog().ok_or_else(invalid_scope)?;
    let Some(receipt) = scope_for_catalog(root_thread, catalog, event)? else {
        return Ok(None);
    };
    let activation = receipt.activation;
    if family.root().thread_id != root_thread
        || activation.scope.thread(root_thread) != activation.checkpoint_thread_id
    {
        return Err(invalid_scope());
    }
    let expected_revision = if activation.scope.graph_path.is_empty() {
        catalog.root.as_ref()
    } else {
        catalog.descendants.get(&activation.scope.graph_path)
    };
    if expected_revision != activation.scope.saved_revision.as_ref() {
        return Err(invalid_scope());
    }
    let mut descendants = Vec::new();
    let mut parent = family.root();
    let mut path = String::new();
    if !activation.scope.graph_path.is_empty() {
        for component in activation.scope.graph_path.split('/') {
            if parent.pending_nodes.as_slice() != [component] {
                return Err(invalid_scope());
            }
            if !path.is_empty() {
                path.push('/');
            }
            path.push_str(component);
            if !catalog.descendants.contains_key(&path) {
                return Err(invalid_scope());
            }
            let thread = format!("{root_thread}/{path}");
            parent = family.checkpoint(&thread).ok_or_else(invalid_scope)?;
            if parent.thread_id != thread {
                return Err(invalid_scope());
            }
            descendants.push(PipelineApplicationScopeCheckpoint {
                graph_path: path.clone(),
                checkpoint_id: parent.checkpoint_id.clone(),
            });
        }
    }
    if parent.pending_nodes.as_slice() != [activation.node_name.as_str()]
        || parent.step != activation.step
    {
        return Err(invalid_scope());
    }
    Ok(Some(PipelineApplicationScopeRoute {
        root_checkpoint_id: family.root().checkpoint_id.clone(),
        descendants,
        leaf_graph_path: activation.scope.graph_path,
        leaf_node_name: activation.node_name,
        application_call_id: activation.call_id,
    }))
}

pub(crate) fn events_for_scope(
    events: &[Event],
    leaf: &Event,
    root_thread: &str,
    catalog: &PipelineCheckpointCatalog,
) -> Result<Vec<Event>, NativeAgentAssemblyError> {
    let wanted = scope_for_catalog(root_thread, catalog, leaf)?.ok_or_else(invalid_scope)?;
    let mut selected = Vec::new();
    for event in events {
        let Some(receipt) = event_scope_for_activation(event, &wanted.activation)? else {
            continue;
        };
        if receipt.activation == wanted.activation {
            if receipt.root_branch != wanted.root_branch {
                return Err(invalid_scope());
            }
            selected.push(event.clone());
        }
    }
    if selected.is_empty() {
        return Err(invalid_scope());
    }
    Ok(selected)
}

#[derive(Clone, Eq, PartialEq)]
/// Exact checkpoint IDs read from the persisted wrapper chain.
pub(crate) struct PipelineApplicationScopeRoute {
    pub(crate) root_checkpoint_id: String,
    pub(crate) descendants: Vec<PipelineApplicationScopeCheckpoint>,
    pub(crate) leaf_graph_path: String,
    pub(crate) leaf_node_name: String,
    pub(crate) application_call_id: String,
}

#[derive(Clone, Eq, PartialEq)]
pub(crate) struct PipelineApplicationScopeCheckpoint {
    pub(crate) graph_path: String,
    pub(crate) checkpoint_id: String,
}

#[derive(Clone)]
struct PipelineApplicationScopeRuntime {
    scope: PipelineApplicationScope,
    node_aliases: BTreeMap<String, String>,
    node_digests: BTreeMap<String, [u8; 32]>,
    applications: BTreeMap<String, std::sync::Arc<ApplicationRuntimeProjection>>,
    prepare_gate: std::sync::Arc<tokio::sync::Mutex<()>>,
    metadata_gate: std::sync::Arc<tokio::sync::Mutex<()>>,
}

/// One bounded registry owns catalogs, receivers and coordinators by graph occurrence.
#[derive(Clone, Default)]
pub(crate) struct PipelineApplicationScopeRegistry {
    by_path: BTreeMap<String, PipelineApplicationScopeRuntime>,
}

impl PipelineApplicationScopeRegistry {
    pub(crate) fn insert(
        &mut self,
        graph_path: String,
        definition: &PipelineDefinition,
        saved_revision: Option<PipelineCheckpointRevision>,
        applications: ApplicationRuntimeProjection,
    ) -> Result<(), NativeAgentAssemblyError> {
        if self.by_path.len() > MAX_PIPELINE_CHECKPOINT_PATHS
            || self.by_path.contains_key(&graph_path)
        {
            return Err(invalid_scope());
        }
        let scope = PipelineApplicationScope::new(graph_path.clone(), definition, saved_revision)?;
        let node_aliases: BTreeMap<String, String> = definition
            .application_nodes()
            .map(|(node, selection)| (node.to_owned(), selection.alias().to_owned()))
            .collect();
        if node_aliases.len()
            + self
                .by_path
                .values()
                .map(|runtime| runtime.node_aliases.len())
                .sum::<usize>()
            > MAX_PIPELINE_CHECKPOINT_PATHS
        {
            return Err(invalid_scope());
        }
        let applications = std::sync::Arc::new(applications);
        let by_node = node_aliases
            .keys()
            .map(|node| (node.clone(), applications.clone()))
            .collect();
        self.by_path.insert(
            graph_path,
            PipelineApplicationScopeRuntime {
                scope,
                node_digests: node_aliases
                    .keys()
                    .map(|node| {
                        Ok((
                            node.clone(),
                            definition
                                .application_node_digest(node)
                                .ok_or_else(invalid_scope)?,
                        ))
                    })
                    .collect::<Result<_, NativeAgentAssemblyError>>()?,
                node_aliases,
                applications: by_node,
                prepare_gate: std::sync::Arc::new(tokio::sync::Mutex::new(())),
                metadata_gate: std::sync::Arc::new(tokio::sync::Mutex::new(())),
            },
        );
        Ok(())
    }

    pub(crate) fn register_scope(
        &mut self,
        graph_path: String,
        definition: &PipelineDefinition,
        revision: Option<PipelineCheckpointRevision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let lookup_path = graph_path.clone();
        self.insert(
            graph_path,
            definition,
            revision,
            ApplicationRuntimeProjection::default(),
        )?;
        self.by_path
            .get_mut(&lookup_path)
            .ok_or_else(invalid_scope)?
            .applications
            .clear();
        Ok(())
    }

    pub(crate) fn graph_call_owner(
        &self,
        path: &str,
        node: &str,
    ) -> Result<
        std::sync::Arc<super::scoped_runtime::PipelineGraphCallOwner>,
        NativeAgentAssemblyError,
    > {
        let runtime = self.by_path.get(path).ok_or_else(invalid_scope)?;
        if !runtime.node_aliases.contains_key(node) {
            return Err(invalid_scope());
        }
        Ok(std::sync::Arc::new(
            super::scoped_runtime::PipelineGraphCallOwner {
                scope: runtime.scope.clone(),
                node_name: node.to_owned(),
                prepare_gate: runtime.prepare_gate.clone(),
                parallel_events: None,
            },
        ))
    }

    pub(crate) fn bind_application(
        &mut self,
        path: &str,
        node: &str,
        projection: std::sync::Arc<ApplicationRuntimeProjection>,
        events: crate::agents::graph::PipelineNodeEventSender,
    ) -> Result<
        std::sync::Arc<super::scoped_runtime::PipelineApplicationNodeRuntime>,
        NativeAgentAssemblyError,
    > {
        let runtime = self.by_path.get_mut(path).ok_or_else(invalid_scope)?;
        if !runtime.node_aliases.contains_key(node)
            || runtime
                .applications
                .insert(node.to_owned(), projection.clone())
                .is_some()
        {
            return Err(invalid_scope());
        }
        Ok(std::sync::Arc::new(
            super::scoped_runtime::PipelineApplicationNodeRuntime::new(
                runtime.scope.clone(),
                node.to_owned(),
                projection,
                events,
                runtime.prepare_gate.clone(),
            ),
        ))
    }

    pub(crate) fn merge(&mut self, other: Self) -> Result<(), NativeAgentAssemblyError> {
        let existing = self
            .by_path
            .values()
            .map(|runtime| runtime.node_aliases.len())
            .sum::<usize>();
        let incoming = other
            .by_path
            .values()
            .map(|runtime| runtime.node_aliases.len())
            .sum::<usize>();
        if existing + incoming > MAX_PIPELINE_CHECKPOINT_PATHS {
            return Err(invalid_scope());
        }
        for (path, runtime) in other.by_path {
            if self.by_path.len() > MAX_PIPELINE_CHECKPOINT_PATHS
                || self.by_path.insert(path, runtime).is_some()
            {
                return Err(invalid_scope());
            }
        }
        Ok(())
    }

    pub(crate) fn has_ordinary_applications(&self) -> bool {
        self.by_path
            .values()
            .any(|runtime| !runtime.applications.is_empty())
    }

    pub(crate) fn wrap_checkpointer(
        &self,
        path: &str,
        inner: std::sync::Arc<dyn Checkpointer>,
    ) -> Result<std::sync::Arc<dyn Checkpointer>, NativeAgentAssemblyError> {
        let runtime = self.by_path.get(path).ok_or_else(invalid_scope)?;
        Ok(std::sync::Arc::new(
            super::scope_receipts::PipelineGraphReceiptCheckpointer::new(
                inner,
                runtime.metadata_gate.clone(),
            ),
        ))
    }

    pub(crate) fn wrap_map_authority(
        &self,
        path: &str,
        inner: std::sync::Arc<dyn crate::agents::graph::MapCheckpointAuthority>,
    ) -> Result<
        std::sync::Arc<dyn crate::agents::graph::MapCheckpointAuthority>,
        NativeAgentAssemblyError,
    > {
        let runtime = self.by_path.get(path).ok_or_else(invalid_scope)?;
        Ok(std::sync::Arc::new(
            super::scope_receipts::PipelineMapReceiptAuthority::new(
                inner,
                runtime.metadata_gate.clone(),
            ),
        ))
    }

    /// Preserve all protocols from one already-verified graph checkpoint authority.
    pub(crate) fn wrap_parallel_authority(
        &self,
        path: &str,
        inner: std::sync::Arc<dyn crate::agents::graph::ParallelCheckpointAuthority>,
    ) -> Result<
        std::sync::Arc<dyn crate::agents::graph::ParallelCheckpointAuthority>,
        NativeAgentAssemblyError,
    > {
        let runtime = self.by_path.get(path).ok_or_else(invalid_scope)?;
        Ok(std::sync::Arc::new(
            super::scope_receipts::PipelineGraphReceiptAuthority::new(
                inner,
                runtime.metadata_gate.clone(),
            ),
        ))
    }

    /// Prove that a rebuilt registry is complete before enabling scoped Agent admission.
    pub(crate) fn validate_catalog(
        &self,
        root_definition: &PipelineDefinition,
        catalog: &PipelineCheckpointCatalog,
    ) -> Result<(), NativeAgentAssemblyError> {
        let root = self.by_path.get("").ok_or_else(invalid_scope)?;
        if root.scope.definition_digest != root_definition.definition_digest()
            || root.scope.saved_revision != catalog.root
            || self.by_path.len() != catalog.descendants.len() + 1
        {
            return Err(invalid_scope());
        }
        for (path, revision) in &catalog.descendants {
            let runtime = self.by_path.get(path).ok_or_else(invalid_scope)?;
            if runtime.scope.saved_revision.as_ref() != Some(revision) {
                return Err(invalid_scope());
            }
            let (parent, node) = path.rsplit_once('/').unwrap_or(("", path));
            if !self
                .by_path
                .get(parent)
                .is_some_and(|owner| owner.node_aliases.contains_key(node))
            {
                return Err(invalid_scope());
            }
        }
        Ok(())
    }

    pub(crate) fn activation(
        &self,
        graph_path: &str,
        root_thread: &str,
        node_name: &str,
        step: usize,
    ) -> Result<PipelineApplicationActivation, NativeAgentAssemblyError> {
        if !valid_identity(root_thread) {
            return Err(invalid_scope());
        }
        let runtime = self.by_path.get(graph_path).ok_or_else(invalid_scope)?;
        if !runtime.node_aliases.contains_key(node_name) {
            return Err(invalid_scope());
        }
        PipelineApplicationActivation::new(
            runtime.scope.clone(),
            runtime.scope.thread(root_thread),
            node_name.to_owned(),
            step,
        )
    }

    /// The direct graph uses the same exact catalog, with no saved-tool root revision.
    pub(crate) fn checkpoint_catalog(
        &self,
    ) -> Result<PipelineCheckpointCatalog, NativeAgentAssemblyError> {
        let root = self.by_path.get("").ok_or_else(invalid_scope)?;
        let mut descendants = BTreeMap::new();
        for (path, runtime) in &self.by_path {
            if path.is_empty() {
                continue;
            }
            descendants.insert(
                path.clone(),
                runtime
                    .scope
                    .saved_revision
                    .clone()
                    .ok_or_else(invalid_scope)?,
            );
        }
        Ok(PipelineCheckpointCatalog {
            root: root.scope.saved_revision.clone(),
            descendants,
        })
    }

    /// Install root-graph child decisions from durable events and the fenced exact PG family.
    /// All proofs are resolved before any coordinator is changed; no client path is an authority.
    #[allow(clippy::too_many_lines)] // Prove the complete checkpoint path and all selected scopes before installing coordinators.
    async fn root_application_family(
        &self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        binding: &crate::agents::events::PipelineApplicationHitlEventBinding,
    ) -> Result<RootApplicationCheckpointFamily, NativeAgentAssemblyError> {
        let catalog = self.checkpoint_catalog()?;
        let root = checkpointer
            .load(root_thread)
            .await
            .map_err(|_| unavailable_scope())?
            .ok_or_else(invalid_scope)?;
        if root.thread_id != root_thread
            || root.checkpoint_id != binding.checkpoint_id()
            || root.pending_nodes.as_slice() != [binding.pending_node_name()]
        {
            return Err(invalid_scope());
        }
        let mut checkpoints = BTreeMap::new();
        for nested in binding.nested_checkpoints() {
            let path = nested
                .thread_id()
                .strip_prefix(root_thread)
                .and_then(|path| path.strip_prefix('/'))
                .ok_or_else(invalid_scope)?;
            if !catalog.descendants.contains_key(path) {
                return Err(invalid_scope());
            }
            let checkpoint = checkpointer
                .load(nested.thread_id())
                .await
                .map_err(|_| unavailable_scope())?
                .ok_or_else(invalid_scope)?;
            if checkpoint.thread_id != nested.thread_id()
                || checkpoint.checkpoint_id != nested.checkpoint_id()
            {
                return Err(invalid_scope());
            }
            checkpoints.insert(checkpoint.thread_id.clone(), checkpoint);
        }
        let family = RootApplicationCheckpointFamily {
            root,
            checkpoints,
            catalog,
        };
        Ok(family)
    }

    fn validate_application_binding_route(
        &self,
        root_thread: &str,
        family: &RootApplicationCheckpointFamily,
        binding: &crate::agents::events::PipelineApplicationHitlEventBinding,
        route: &PipelineApplicationScopeRoute,
    ) -> Result<(), NativeAgentAssemblyError> {
        if route.application_call_id != binding.application_call_id()
            || route.leaf_node_name != binding.node_name()
        {
            return Err(invalid_scope());
        }
        let runtime = self
            .by_path
            .get(&route.leaf_graph_path)
            .ok_or_else(invalid_scope)?;
        let digest = hex_digest(
            runtime
                .node_digests
                .get(&route.leaf_node_name)
                .ok_or_else(invalid_scope)?,
        );
        if binding.definition_digest() != format!("sha256:{digest}") {
            return Err(invalid_scope());
        }
        let thread = runtime.scope.thread(root_thread);
        let checkpoint = family.checkpoint(&thread).ok_or_else(invalid_scope)?;
        let receipt = super::scope_receipts::receipt_for_node(checkpoint, &route.leaf_node_name)
            .map_err(|_| invalid_scope())?
            .ok_or_else(invalid_scope)?;
        let super::scope_receipts::GraphCallOutcome::Paused { result } = receipt.outcome() else {
            return Err(invalid_scope());
        };
        let ids = crate::agents::application_tools::nested_application_interrupt_ids(result)
            .ok_or_else(invalid_scope)?;
        if ids
            != binding
                .interrupt_ids()
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>()
        {
            return Err(invalid_scope());
        }
        Ok(())
    }

    pub(crate) async fn install_root_static_continuation(
        &self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        binding: &crate::agents::events::PipelineApplicationHitlEventBinding,
        events: &[Event],
        decisions: Vec<crate::agents::graph::static_tool_pause::StaticToolDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        if decisions.is_empty() || decisions.len() > 16 {
            return Err(invalid_scope());
        }
        let family = self
            .root_application_family(root_thread, checkpointer, binding)
            .await?;
        let mut route = None;
        let mut submitted = BTreeSet::new();
        for decision in &decisions {
            if !submitted.insert(decision.pause_id().to_owned())
                || !binding
                    .interrupt_ids()
                    .iter()
                    .any(|id| id == decision.pause_id())
            {
                return Err(invalid_scope());
            }
            let mut pause = None;
            for event in events {
                if let Some(candidate) =
                    crate::agents::application_pipeline::static_pipeline_tool_pause(event)?
                    && candidate.pause_id == decision.pause_id()
                    && (candidate.thread_id != decision.child_thread_id()
                        || candidate.parent_call_id != decision.tool_call_id()
                        || pause.replace(candidate).is_some())
                {
                    return Err(invalid_scope());
                }
            }
            let pause = pause.ok_or_else(invalid_scope)?;
            let candidate =
                route_from_event(root_thread, &family, &pause.event)?.ok_or_else(invalid_scope)?;
            self.validate_application_binding_route(root_thread, &family, binding, &candidate)?;
            if route.as_ref().is_some_and(|prior| prior != &candidate) {
                return Err(invalid_scope());
            }
            route.get_or_insert(candidate);
        }
        self.validate_continuation(
            root_thread,
            checkpointer,
            &route.ok_or_else(invalid_scope)?,
            events,
        )
        .await?
        .install_static(decisions)
        .await
    }

    pub(crate) async fn install_root_continuation(
        &self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        binding: &crate::agents::events::PipelineApplicationHitlEventBinding,
        events: &[Event],
        decisions: Vec<ResolvedDirectHitlDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let family = self
            .root_application_family(root_thread, checkpointer, binding)
            .await?;
        let mut route = None;
        for decision in &decisions {
            let mut matched = None;
            for event in events {
                let Some(confirmation) = event.actions.tool_confirmation.as_ref() else {
                    continue;
                };
                let Some(call) = confirmation.function_call_id.as_deref() else {
                    continue;
                };
                let (id, _) = crate::agents::direct_hitl::sensitive_call_identity(
                    &event.invocation_id,
                    call,
                    &confirmation.tool_name,
                    &confirmation.args,
                )
                .map_err(|_| invalid_scope())?;
                if id == decision.interrupt_id()
                    && (event.invocation_id != decision.invocation_id()
                        || call != decision.call_id()
                        || &confirmation.args != decision.arguments()
                        || matched.replace(event).is_some())
                {
                    return Err(invalid_scope());
                }
            }
            let event = matched.ok_or_else(invalid_scope)?;
            let candidate =
                route_from_event(root_thread, &family, event)?.ok_or_else(invalid_scope)?;
            if candidate.application_call_id != binding.application_call_id()
                || candidate.leaf_node_name != binding.node_name()
                || route.as_ref().is_some_and(|prior| prior != &candidate)
            {
                return Err(invalid_scope());
            }
            route.get_or_insert(candidate);
        }
        let route = route.ok_or_else(invalid_scope)?;
        let runtime = self
            .by_path
            .get(&route.leaf_graph_path)
            .ok_or_else(invalid_scope)?;
        let digest = hex_digest(
            runtime
                .node_digests
                .get(&route.leaf_node_name)
                .ok_or_else(invalid_scope)?,
        );
        if binding.definition_digest() != format!("sha256:{digest}") {
            return Err(invalid_scope());
        }
        let thread = runtime.scope.thread(root_thread);
        let checkpoint = family.checkpoint(&thread).ok_or_else(invalid_scope)?;
        let receipt = super::scope_receipts::receipt_for_node(checkpoint, &route.leaf_node_name)
            .map_err(|_| invalid_scope())?
            .ok_or_else(invalid_scope)?;
        let super::scope_receipts::GraphCallOutcome::Paused { result } = receipt.outcome() else {
            return Err(invalid_scope());
        };
        let expected = crate::agents::application_tools::nested_application_interrupt_ids(result)
            .ok_or_else(invalid_scope)?;
        if expected
            != binding
                .interrupt_ids()
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>()
        {
            return Err(invalid_scope());
        }
        self.validate_continuation(root_thread, checkpointer, &route, events)
            .await?
            .install(decisions)
            .await
    }

    /// Native graph continuations do not carry an ordinary parent's outer saved-tool receipt.
    pub(crate) async fn validate_continuation<'a>(
        &'a self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        route: &PipelineApplicationScopeRoute,
        events: &[Event],
    ) -> Result<ValidatedPipelineApplicationContinuation<'a>, NativeAgentAssemblyError> {
        self.validate_continuation_with_boundary(root_thread, checkpointer, route, events, None)
            .await
    }

    /// The saved-tool caller supplies its exact captured original call, never a client selector.
    pub(crate) async fn validate_saved_tool_continuation<'a>(
        &'a self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        route: &PipelineApplicationScopeRoute,
        events: &[Event],
        lineage: &crate::agents::application_pipeline::PipelineToolCallLineage,
    ) -> Result<ValidatedPipelineApplicationContinuation<'a>, NativeAgentAssemblyError> {
        self.validate_continuation_with_boundary(
            root_thread,
            checkpointer,
            route,
            events,
            Some(lineage),
        )
        .await
    }

    /// Read the latest exact family and every admitted ancestor before choosing a coordinator.
    #[allow(clippy::too_many_lines)] // Prove the complete checkpoint path and all selected scopes before installing coordinators.
    async fn validate_continuation_with_boundary<'a>(
        &'a self,
        root_thread: &str,
        checkpointer: &dyn Checkpointer,
        route: &PipelineApplicationScopeRoute,
        events: &[Event],
        outer_lineage: Option<&crate::agents::application_pipeline::PipelineToolCallLineage>,
    ) -> Result<ValidatedPipelineApplicationContinuation<'a>, NativeAgentAssemblyError> {
        if !valid_identity(root_thread)
            || !valid_identity(&route.root_checkpoint_id)
            || route.descendants.len() > MAX_PIPELINE_COMPOSITION_DEPTH
            || !valid_path(&route.leaf_graph_path, true)
        {
            return Err(invalid_scope());
        }
        let mut paths = Vec::with_capacity(route.descendants.len() + 1);
        paths.push(("", route.root_checkpoint_id.as_str()));
        let mut previous = "";
        for descendant in &route.descendants {
            if !valid_path(&descendant.graph_path, false)
                || !valid_identity(&descendant.checkpoint_id)
                || descendant
                    .graph_path
                    .rsplit_once('/')
                    .map_or("", |(parent, _)| parent)
                    != previous
            {
                return Err(invalid_scope());
            }
            paths.push((&descendant.graph_path, &descendant.checkpoint_id));
            previous = &descendant.graph_path;
        }
        if previous != route.leaf_graph_path {
            return Err(invalid_scope());
        }
        let mut leaf = None;
        for (index, (path, checkpoint_id)) in paths.iter().enumerate() {
            let runtime = self.by_path.get(*path).ok_or_else(invalid_scope)?;
            let checkpoint = checkpointer
                .load(&runtime.scope.thread(root_thread))
                .await
                .map_err(|_| unavailable_scope())?
                .ok_or_else(invalid_scope)?;
            let pending = paths
                .get(index + 1)
                .map_or(route.leaf_node_name.as_str(), |(next, _)| {
                    next.rsplit('/').next().unwrap_or_default()
                });
            if checkpoint.thread_id != runtime.scope.thread(root_thread)
                || checkpoint.checkpoint_id != *checkpoint_id
                || checkpoint.pending_nodes.as_slice() != [pending]
            {
                return Err(invalid_scope());
            }
            leaf = Some(checkpoint);
        }
        let leaf = leaf.ok_or_else(invalid_scope)?;
        let activation = self.activation(
            &route.leaf_graph_path,
            root_thread,
            &route.leaf_node_name,
            leaf.step,
        )?;
        if activation.call_id != route.application_call_id {
            return Err(invalid_scope());
        }
        let runtime = self
            .by_path
            .get(&route.leaf_graph_path)
            .ok_or_else(invalid_scope)?;
        let applications = runtime
            .applications
            .get(&route.leaf_node_name)
            .ok_or_else(invalid_scope)?;
        let original_receipt =
            super::scope_receipts::receipt_for_node(&leaf, &route.leaf_node_name)
                .map_err(|_| invalid_scope())?
                .ok_or_else(invalid_scope)?;
        if original_receipt.activation() != &activation {
            return Err(invalid_scope());
        }
        let mut selected = Vec::new();
        let mut call_events = BTreeSet::new();
        let mut root_branch = None;
        for event in events {
            let Some(receipt) = event_scope_for_activation(event, &activation)? else {
                continue;
            };
            validate_activation_parent(event, &original_receipt)?;
            let recorded = receipt.activation;
            if recorded.scope == activation.scope && recorded.call_id == activation.call_id {
                crate::agents::application_pipeline::validate_scoped_outer_boundary(
                    event,
                    root_thread,
                    outer_lineage,
                )?;
                if recorded != activation
                    || root_branch
                        .as_ref()
                        .is_some_and(|branch| branch != &receipt.root_branch)
                {
                    return Err(invalid_scope());
                }
                root_branch.get_or_insert(receipt.root_branch);
                if event
                    .tool_calls()
                    .iter()
                    .any(|call| call.call_id == Some(activation.call_id()))
                {
                    let proved_start = canonical_graph_start(
                        event,
                        original_receipt.original_start(),
                        root_thread,
                        outer_lineage,
                    )?;
                    // The checkpoint owns one original call. Repeated projection of that
                    // proved call does not manufacture another ordinary model invocation.
                    if call_events.insert(event.id.clone()) {
                        selected.push(proved_start);
                    }
                    continue;
                }
                selected.push(event.clone());
            }
        }
        if call_events.is_empty() || selected.is_empty() {
            return Err(invalid_scope());
        }
        // The existing builder validates original batch/call/ordinal and immutable arguments.
        // This proof supplies only events belonging to one graph activation.
        let root_branch = root_branch.ok_or_else(invalid_scope)?;
        let events = scope_local_events(&selected, &activation, &root_branch, root_thread)?;
        Ok(ValidatedPipelineApplicationContinuation {
            applications,
            events,
            root_branch,
        })
    }
}

pub(crate) struct ValidatedPipelineApplicationContinuation<'a> {
    applications: &'a ApplicationRuntimeProjection,
    events: Vec<Event>,
    root_branch: String,
}

impl ValidatedPipelineApplicationContinuation<'_> {
    pub(crate) async fn install_static(
        self,
        decisions: Vec<crate::agents::graph::static_tool_pause::StaticToolDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        self.applications
            .install_scope_static_resume(&self.events, decisions)
            .await
    }

    pub(crate) async fn install(
        self,
        decisions: Vec<ResolvedDirectHitlDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let decisions = decisions
            .into_iter()
            .map(|decision| {
                decision
                    .into_pipeline_scope(&self.root_branch)
                    .map_err(|()| invalid_scope())
            })
            .collect::<Result<Vec<_>, _>>()?;
        self.applications
            .install_scope_resume(&self.events, decisions)
            .await
    }
}

fn scoped_call_id(
    scope: &PipelineApplicationScope,
    thread: &str,
    node: &str,
    step: usize,
) -> String {
    let mut value = digest::Context::new(&digest::SHA256);
    value.update(CALL_DOMAIN);
    for field in [
        scope.graph_path.as_bytes(),
        &scope.definition_digest,
        thread.as_bytes(),
        node.as_bytes(),
    ] {
        value.update(&(field.len() as u64).to_be_bytes());
        value.update(field);
    }
    if let Some(revision) = &scope.saved_revision {
        value.update(b"saved");
        value.update(&revision.application_id.to_be_bytes());
        value.update(&revision.version_id.to_be_bytes());
    } else {
        value.update(b"root");
    }
    value.update(&(step as u64).to_be_bytes());
    let hex = hex_digest(value.finish().as_ref());
    format!("pipeline-v2:{node}:{hex}:{step}")
}

pub(crate) fn scoped_graph_call_id(
    thread: &str,
    node: &str,
    step: usize,
    definition_digest: [u8; 32],
) -> String {
    let scope = PipelineApplicationScope {
        schema: SCOPE_SCHEMA.to_owned(),
        graph_path: String::new(),
        definition_digest,
        saved_revision: None,
    };
    scoped_call_id(&scope, thread, node, step)
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_CHECKPOINT_THREAD_BYTES
        && !value.chars().any(char::is_control)
}

fn valid_node(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && !matches!(value, "." | "..")
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':'))
}

fn valid_path(value: &str, root_allowed: bool) -> bool {
    (root_allowed && value.is_empty())
        || (!value.is_empty()
            && value.split('/').count() <= MAX_PIPELINE_COMPOSITION_DEPTH
            && value.split('/').all(valid_node))
}

const fn invalid_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the pipeline application scope does not match its admitted checkpoint family",
    )
}

const fn unavailable_scope() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::DependencyUnavailable,
        "the pipeline application checkpoint family is unavailable",
    )
}

/// A bounded snapshot read only through the existing exact-thread checkpointer owner.
struct RootApplicationCheckpointFamily {
    root: adk_rust::graph::Checkpoint,
    checkpoints: BTreeMap<String, adk_rust::graph::Checkpoint>,
    catalog: PipelineCheckpointCatalog,
}
impl PipelineCheckpointFamilyView for RootApplicationCheckpointFamily {
    fn root(&self) -> &adk_rust::graph::Checkpoint {
        &self.root
    }
    fn checkpoint(&self, thread: &str) -> Option<&adk_rust::graph::Checkpoint> {
        if self.root.thread_id == thread {
            Some(&self.root)
        } else {
            self.checkpoints.get(thread)
        }
    }
    fn catalog(&self) -> Option<&PipelineCheckpointCatalog> {
        Some(&self.catalog)
    }
}

fn hex_digest(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    let mut value = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        let _ = write!(value, "{byte:02x}");
    }
    value
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_rust::graph::{Checkpoint, MemoryCheckpointer, State};
    use adk_rust::{Content, Part};
    use serde_json::json;

    fn definition(node: &str) -> PipelineDefinition {
        PipelineDefinition::from_yaml(&format!("state: {{answer: str}}\nentry_point: {node}\nnodes:\n  - id: {node}\n    type: agent\n    tool: assistant\n    input_mapping: {{task: {{type: fixed, value: work}}}}\n    output: [answer]\n    transition: END\n")).unwrap()
    }

    fn revision(id: u64, definition: &PipelineDefinition) -> PipelineCheckpointRevision {
        PipelineCheckpointRevision {
            application_id: id,
            version_id: 9,
            definition_digest: definition.definition_digest(),
        }
    }

    fn registry() -> (
        PipelineApplicationScopeRegistry,
        PipelineDefinition,
        PipelineCheckpointCatalog,
    ) {
        let root = definition("delegate");
        let middle = definition("delegate");
        let leaf = definition("agent");
        let catalog = PipelineCheckpointCatalog {
            root: None,
            descendants: BTreeMap::from([
                ("delegate".to_owned(), revision(31, &middle)),
                ("delegate/delegate".to_owned(), revision(51, &leaf)),
            ]),
        };
        let mut registry = PipelineApplicationScopeRegistry::default();
        registry
            .insert(
                String::new(),
                &root,
                None,
                ApplicationRuntimeProjection::default(),
            )
            .unwrap();
        registry
            .insert(
                "delegate".to_owned(),
                &middle,
                Some(revision(31, &middle)),
                ApplicationRuntimeProjection::default(),
            )
            .unwrap();
        registry
            .insert(
                "delegate/delegate".to_owned(),
                &leaf,
                Some(revision(51, &leaf)),
                ApplicationRuntimeProjection::default(),
            )
            .unwrap();
        (registry, root, catalog)
    }

    fn call(activation: &PipelineApplicationActivation) -> Event {
        let mut event = Event::new("original-root-batch");
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "assistant".to_owned(),
                args: json!({"task":"work","locale":"one"}),
                id: Some(activation.call_id.clone()),
                thought_signature: None,
            }],
        });
        event.branch = APPLICATION_BRANCH_ROOT.to_owned();
        activation.stamp(&mut event).unwrap();
        event
    }

    #[test]
    fn equal_node_aliases_in_distinct_scopes_have_distinct_stable_activation_ids() {
        let (registry, root, catalog) = registry();
        registry.validate_catalog(&root, &catalog).unwrap();
        let direct = registry.activation("", "thread-1", "delegate", 7).unwrap();
        let deeper = registry
            .activation("delegate", "thread-1", "delegate", 7)
            .unwrap();
        assert_ne!(direct.call_id(), deeper.call_id());
        assert!(
            deeper
                == registry
                    .activation("delegate", "thread-1", "delegate", 7)
                    .unwrap()
        );
        assert_ne!(
            deeper.call_id(),
            registry
                .activation("delegate", "thread-1", "delegate", 8)
                .unwrap()
                .call_id()
        );
        assert!(
            registry
                .activation("unknown", "thread-1", "delegate", 7)
                .is_err()
        );
        let mut changed = catalog;
        changed.descendants.get_mut("delegate").unwrap().version_id += 1;
        assert!(registry.validate_catalog(&root, &changed).is_err());
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)] // Keep the ordered exact-frontier and immutable-history authority fixture readable.
    async fn continuation_requires_exact_ancestor_ids_frontier_and_scope_history() {
        let (registry, _, _) = registry();
        let checkpointer = MemoryCheckpointer::new();
        let root = Checkpoint::new("thread-1", State::new(), 1, vec!["delegate".to_owned()]);
        let middle = Checkpoint::new(
            "thread-1/delegate",
            State::new(),
            2,
            vec!["delegate".to_owned()],
        );
        let leaf = Checkpoint::new(
            "thread-1/delegate/delegate",
            State::new(),
            7,
            vec!["agent".to_owned()],
        );
        for checkpoint in [&root, &middle, &leaf] {
            checkpointer.save(checkpoint).await.unwrap();
        }
        let activation = registry
            .activation("delegate/delegate", "thread-1", "agent", 7)
            .unwrap();
        let other = registry
            .activation("delegate", "thread-1", "delegate", 2)
            .unwrap();
        let context = adk_rust::graph::NodeContext::new(
            State::new(),
            adk_rust::graph::ExecutionConfig::new(&leaf.thread_id),
            leaf.step,
        );
        let (recorded, _) = super::super::scope_receipts::prepare_graph_call(
            &checkpointer,
            &tokio::sync::Mutex::new(()),
            &context,
            activation.clone(),
            "assistant",
            &json!({"task":"work","locale":"one"}),
            "original-root-batch",
            "graph",
            APPLICATION_BRANCH_ROOT,
        )
        .await
        .unwrap();
        let leaf = checkpointer.load(&leaf.thread_id).await.unwrap().unwrap();
        let events = vec![recorded.original_start().clone(), call(&other)];
        let mut route = PipelineApplicationScopeRoute {
            root_checkpoint_id: root.checkpoint_id.clone(),
            descendants: vec![
                PipelineApplicationScopeCheckpoint {
                    graph_path: "delegate".to_owned(),
                    checkpoint_id: middle.checkpoint_id.clone(),
                },
                PipelineApplicationScopeCheckpoint {
                    graph_path: "delegate/delegate".to_owned(),
                    checkpoint_id: leaf.checkpoint_id.clone(),
                },
            ],
            leaf_graph_path: "delegate/delegate".to_owned(),
            leaf_node_name: "agent".to_owned(),
            application_call_id: activation.call_id.clone(),
        };
        let proof = registry
            .validate_continuation("thread-1", &checkpointer, &route, &events)
            .await
            .unwrap();
        assert_eq!(proof.events.len(), 1);
        assert_eq!(proof.events[0].id, events[0].id);
        // Projection may persist the same original graph start again. The
        // checkpoint receipt, not a duplicate-ID convention, proves its bytes.
        let mut repeated = events.clone();
        repeated.push(events[0].clone());
        let repeated_proof = registry
            .validate_continuation("thread-1", &checkpointer, &route, &repeated)
            .await
            .unwrap();
        assert_eq!(repeated_proof.events.len(), 1);
        if let Part::FunctionCall { args, .. } = &mut repeated
            .last_mut()
            .unwrap()
            .llm_response
            .content
            .as_mut()
            .unwrap()
            .parts[0]
        {
            args["locale"] = json!("wrong");
        }
        assert!(
            registry
                .validate_continuation("thread-1", &checkpointer, &route, &repeated)
                .await
                .is_err()
        );
        // Missing ancestor is refused even though the leaf checkpoint exists.
        let parent = route.descendants.remove(0);
        assert!(
            registry
                .validate_continuation("thread-1", &checkpointer, &route, &events)
                .await
                .is_err()
        );
        route.descendants.insert(0, parent);
        route.root_checkpoint_id = "stale-root".to_owned();
        assert!(
            registry
                .validate_continuation("thread-1", &checkpointer, &route, &events)
                .await
                .is_err()
        );
        route.root_checkpoint_id = root.checkpoint_id;
        route.application_call_id = "pipeline:agent:7".to_owned();
        assert!(
            registry
                .validate_continuation("thread-1", &checkpointer, &route, &events)
                .await
                .is_err()
        );
    }

    #[test]
    fn stamping_cannot_rewrite_existing_scope_and_corrupt_receipts_fail_closed() {
        let (registry, _, _) = registry();
        let activation = registry
            .activation("delegate/delegate", "thread-1", "agent", 7)
            .unwrap();
        let mut event = call(&activation);
        let original = event.provider_metadata.clone();
        assert!(activation.stamp(&mut event).is_err());
        assert_eq!(event.provider_metadata, original);
        let mut raw: serde_json::Value = serde_json::from_str(
            event
                .provider_metadata
                .get(PIPELINE_APPLICATION_SCOPE_METADATA_KEY)
                .unwrap(),
        )
        .unwrap();
        raw["activation"]["step"] = json!(8);
        event.provider_metadata.insert(
            PIPELINE_APPLICATION_SCOPE_METADATA_KEY.to_owned(),
            raw.to_string(),
        );
        assert!(PipelineApplicationActivation::from_event(&event).is_err());
    }
    #[test]
    fn local_projection_stops_at_exact_graph_branch_and_preserves_originals() {
        let (registry, _, _) = registry();
        let activation = registry
            .activation("delegate/delegate", "thread-1", "agent", 7)
            .unwrap();
        let branch = format!("{APPLICATION_BRANCH_ROOT}.application_10.application_2");
        let mut root = call(&activation);
        root.provider_metadata
            .remove(PIPELINE_APPLICATION_SCOPE_METADATA_KEY);
        root.branch = branch.clone();
        root.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            "outer".to_owned(),
        );
        root.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            "graph-hop".to_owned(),
        );
        activation
            .stamp_with_branch(&mut root, branch.clone())
            .unwrap();
        let mut leaf = Event::new("original-leaf");
        leaf.branch = format!("{branch}.application_3");
        leaf.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            root.invocation_id.clone(),
        );
        leaf.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            activation.call_id.clone(),
        );
        activation
            .stamp_with_branch(&mut leaf, branch.clone())
            .unwrap();
        let originals = vec![root, leaf];
        let before = serde_json::to_value(&originals).unwrap();
        let projected = scope_local_events(&originals, &activation, &branch, "thread-1").unwrap();
        assert_eq!(projected[0].branch, APPLICATION_BRANCH_ROOT);
        assert!(
            !projected[0]
                .provider_metadata
                .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
        );
        assert_eq!(
            projected[1].branch,
            format!("{APPLICATION_BRANCH_ROOT}.application_3")
        );
        assert_eq!(
            projected[1]
                .provider_metadata
                .get(DESCENDANT_PARENT_CALL_KEY),
            Some(&activation.call_id)
        );
        assert_eq!(projected[0].id, originals[0].id);
        assert_eq!(
            serde_json::to_value(projected[0].content()).unwrap(),
            serde_json::to_value(originals[0].content()).unwrap()
        );
        assert_eq!(before, serde_json::to_value(&originals).unwrap());
        // Segment 20 is not inside segment 2; textual neighboring prefixes refuse.
        assert!(
            local_application_branch(
                &format!("{APPLICATION_BRANCH_ROOT}.application_20"),
                &format!("{APPLICATION_BRANCH_ROOT}.application_2")
            )
            .is_none()
        );
        let mut mismatched = originals.clone();
        mismatched[1].branch = format!("{APPLICATION_BRANCH_ROOT}.application_99");
        assert!(scope_local_events(&mismatched, &activation, &branch, "thread-1").is_err());
    }
    #[test]
    fn root_projection_preserves_original_activation_and_rebinds_only_transient_routes() {
        let (registry, _, _) = registry();
        let activation = registry.activation("", "thread-1", "delegate", 7).unwrap();
        let mut start = call(&activation);
        let original_invocation = start.invocation_id.clone();
        let original_id = start.id.clone();
        let original_call = start.tool_calls()[0].call_id.unwrap().to_owned();
        stamp_root_scope_projection(&mut start, &original_invocation, "resumed-root").unwrap();
        let stored = serde_json::to_vec(&start).unwrap();
        let projected = project_root_scope_event(&start, Some("resumed-root"))
            .unwrap()
            .unwrap();
        assert_eq!(projected.invocation_id, "resumed-root");
        assert_eq!(projected.id, original_id);
        assert_eq!(
            projected.tool_calls()[0].call_id,
            Some(original_call.as_str())
        );
        assert!(activation.matches_original_start(&projected).unwrap());
        assert_eq!(serde_json::to_vec(&start).unwrap(), stored);
        assert!(project_root_scope_event(&start, Some("other-root")).is_err());

        let mut leaf = Event::with_id("original-confirmation", "original-leaf-invocation");
        leaf.branch = format!("{APPLICATION_BRANCH_ROOT}.application_1");
        activation
            .stamp_with_branch(&mut leaf, APPLICATION_BRANCH_ROOT.to_owned())
            .unwrap();
        leaf.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            original_invocation.clone(),
        );
        leaf.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            activation.call_id.clone(),
        );
        stamp_root_scope_projection(&mut leaf, &original_invocation, "resumed-root").unwrap();
        let stored = serde_json::to_vec(&leaf).unwrap();
        let projected = project_root_scope_event(&leaf, Some("resumed-root"))
            .unwrap()
            .unwrap();
        assert_eq!(projected.invocation_id, "original-leaf-invocation");
        assert_eq!(projected.id, "original-confirmation");
        assert_eq!(
            projected
                .provider_metadata
                .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
                .map(String::as_str),
            Some("resumed-root")
        );
        assert_eq!(
            projected.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY),
            Some(&activation.call_id)
        );
        assert_eq!(serde_json::to_vec(&leaf).unwrap(), stored);
        leaf.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            "another-occurrence".to_owned(),
        );
        assert!(project_root_scope_event(&leaf, Some("resumed-root")).is_err());

        let deeper = registry
            .activation("delegate", "thread-1", "delegate", 7)
            .unwrap();
        let mut event = call(&deeper);
        assert!(stamp_root_scope_projection(&mut event, "original-root", "resumed-root").is_err());
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)] // One ordered fixture proves checkpoint, occurrence, static selection, and unchanged original receipts.
    async fn deeper_static_saved_tool_selection_installs_only_its_original_graph_activation() {
        use crate::agents::application_pipeline::{
            static_pause_fixture, static_pipeline_tool_pause,
        };
        use crate::agents::application_tools::{
            ApplicationResumeCoordinator, nested_interrupt_result,
        };
        use crate::agents::events::{
            ApplicationToolGuardCatalogs, ApplicationToolPresentationCatalog,
        };
        use crate::agents::pipeline::scope_receipts::{
            GraphCallOutcome, finish_graph_call, prepare_graph_call,
        };
        use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
        let (mut registry, _, checkpoint_catalog) = registry();
        let coordinator = ApplicationResumeCoordinator::default();
        let mut children = ApplicationToolPresentationCatalog::default();
        children
            .insert_runtime(
                "saved_pipeline".to_owned(),
                "Saved pipeline".to_owned(),
                "pipeline".to_owned(),
                "fixture".to_owned(),
                ApplicationToolPresentationCatalog::default(),
                ApplicationToolGuardCatalogs::default(),
            )
            .unwrap();
        let mut catalog = ApplicationToolPresentationCatalog::default();
        catalog
            .insert_runtime(
                "assistant".to_owned(),
                "Assistant".to_owned(),
                "agent".to_owned(),
                "fixture".to_owned(),
                children,
                ApplicationToolGuardCatalogs::default(),
            )
            .unwrap();
        registry
            .by_path
            .get_mut("delegate/delegate")
            .unwrap()
            .applications
            .insert(
                "agent".to_owned(),
                std::sync::Arc::new(ApplicationRuntimeProjection::coordinated_fixture(
                    catalog,
                    coordinator.clone(),
                )),
            );
        let checkpointer = MemoryCheckpointer::new();
        let root = Checkpoint::new("thread-1", State::new(), 1, vec!["delegate".to_owned()]);
        let middle = Checkpoint::new(
            "thread-1/delegate",
            State::new(),
            2,
            vec!["delegate".to_owned()],
        );
        let leaf = Checkpoint::new(
            "thread-1/delegate/delegate",
            State::new(),
            7,
            vec!["agent".to_owned()],
        );
        for checkpoint in [&root, &middle, &leaf] {
            checkpointer.save(checkpoint).await.unwrap();
        }
        let activation = registry
            .activation("delegate/delegate", "thread-1", "agent", 7)
            .unwrap();
        let context = adk_rust::graph::NodeContext::new(
            State::new(),
            adk_rust::graph::ExecutionConfig::new(&leaf.thread_id),
            7,
        );
        let (original, _) = prepare_graph_call(
            &checkpointer,
            &tokio::sync::Mutex::new(()),
            &context,
            activation.clone(),
            "assistant",
            &json!({"task":"work"}),
            "original-root",
            "graph",
            APPLICATION_BRANCH_ROOT,
        )
        .await
        .unwrap();
        let mut ordinary_call = Event::with_id("original-inner-batch", "original-inner-agent");
        ordinary_call.branch = format!("{APPLICATION_BRANCH_ROOT}.application_1");
        ordinary_call.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "saved_pipeline".to_owned(),
                args: json!({"task":"work"}),
                id: Some("inner-pipeline-call".to_owned()),
                thought_signature: None,
            }],
        });
        ordinary_call.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            original.invocation_id().to_owned(),
        );
        ordinary_call.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            activation.call_id().to_owned(),
        );
        activation
            .stamp_with_branch(&mut ordinary_call, APPLICATION_BRANCH_ROOT.to_owned())
            .unwrap();
        let (_, _, mut pause, decision) = static_pause_fixture(&ordinary_call, 0, "after");
        pause.branch = format!("{}.application_1", ordinary_call.branch);
        activation
            .stamp_with_branch(&mut pause, APPLICATION_BRANCH_ROOT.to_owned())
            .unwrap();
        let stored = serde_json::to_vec(&[
            original.original_start().clone(),
            ordinary_call.clone(),
            pause.clone(),
        ])
        .unwrap();
        let ids = BTreeSet::from([decision.pause_id().to_owned()]);
        finish_graph_call(
            &checkpointer,
            &tokio::sync::Mutex::new(()),
            &context,
            &original,
            GraphCallOutcome::Paused {
                result: nested_interrupt_result(&ids),
            },
        )
        .await
        .unwrap();
        let leaf = checkpointer.load(&leaf.thread_id).await.unwrap().unwrap();
        let message = "The saved agent is waiting for sensitive-tool approval.";
        let data = json!({"schema_revision":crate::agents::graph::PIPELINE_APPLICATION_HITL_SCHEMA,"type":"hitl_checkpoint","guardrail_type":"application_sensitive_tool","node_name":"agent","message":message,"definition_digest":format!("sha256:{}",hex_digest(&registry.by_path["delegate/delegate"].node_digests["agent"])),"application_call_id":activation.call_id(),"application_tool_name":"assistant","interrupt_ids":ids});
        let payload = GraphInterruptPayload {
            kind: "dynamic".to_owned(),
            node: None,
            message: Some(format!("delegate: delegate: {message}")),
            data: Some(
                json!({"subgraph":"delegate","thread":middle.thread_id,"checkpoint_id":middle.checkpoint_id,"data":{"subgraph":"delegate","thread":leaf.thread_id,"checkpoint_id":leaf.checkpoint_id,"data":data}}),
            ),
            thread_id: root.thread_id.clone(),
            checkpoint_id: root.checkpoint_id.clone(),
        };
        let mut wrapper = Event::new("original-root");
        wrapper.author = "graph".to_owned();
        wrapper.set_content(
            Content::new("assistant").with_text(format!("delegate: delegate: {message}")),
        );
        wrapper.provider_metadata.insert(
            INTERRUPT_METADATA_KEY.to_owned(),
            payload.to_metadata_value(),
        );
        let binding = crate::agents::events::pipeline_application_event_binding(
            &wrapper, "graph", "thread-1",
        )
        .unwrap();
        let events = vec![
            original.original_start().clone(),
            ordinary_call.clone(),
            pause.clone(),
        ];
        let mut forged = events.clone();
        if let Part::FunctionCall { args, .. } =
            &mut forged[1].llm_response.content.as_mut().unwrap().parts[0]
        {
            *args = json!({"task":"changed"});
        }
        assert!(
            registry
                .install_root_static_continuation(
                    "thread-1",
                    &checkpointer,
                    &binding,
                    &forged,
                    vec![decision.clone()]
                )
                .await
                .is_err()
        );
        assert!(
            !coordinator
                .has_resume(original.invocation_id(), activation.call_id())
                .await
        );
        registry
            .install_root_static_continuation(
                "thread-1",
                &checkpointer,
                &binding,
                &events,
                vec![decision.clone()],
            )
            .await
            .unwrap();
        assert!(
            coordinator
                .has_resume(original.invocation_id(), activation.call_id())
                .await
        );
        assert!(
            !coordinator
                .has_resume(original.invocation_id(), "inner-pipeline-call")
                .await
        );
        assert_eq!(serde_json::to_vec(&events).unwrap(), stored);
        let family = registry
            .root_application_family("thread-1", &checkpointer, &binding)
            .await
            .unwrap();
        let route = route_from_event("thread-1", &family, &pause)
            .unwrap()
            .unwrap();
        assert_eq!(route.leaf_graph_path, "delegate/delegate");
        let local = registry
            .validate_continuation("thread-1", &checkpointer, &route, &events)
            .await
            .unwrap();
        let local_pause = local
            .events
            .iter()
            .find(|event| event.id == pause.id)
            .unwrap();
        assert!(static_pipeline_tool_pause(local_pause).unwrap().is_some());

        // A saved outer graph must prove its original model-call lineage before local projection.
        let mut outer_call = Event::with_id("outer-original-batch", "outer-original-agent");
        outer_call.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "saved_outer".to_owned(),
                args: json!({"task":"work"}),
                id: Some("outer-original-call".to_owned()),
                thought_signature: None,
            }],
        });
        let outer_lineage =
            crate::agents::application_pipeline::PipelineToolCallLineage::from_call(&outer_call, 0)
                .unwrap();
        let mut outer_events = events.clone();
        crate::agents::application_pipeline::append_outer_boundary(
            &mut outer_events[2],
            "outer-original-agent",
            "outer-original-call",
            "thread-1",
            outer_lineage.clone(),
            &checkpoint_catalog,
        )
        .unwrap();
        let stored_outer = serde_json::to_vec(&outer_events).unwrap();
        assert!(
            registry
                .validate_continuation("thread-1", &checkpointer, &route, &outer_events)
                .await
                .is_err()
        );
        let mut foreign_call = outer_call.clone();
        foreign_call.id = "foreign-outer-batch".to_owned();
        let foreign_lineage =
            crate::agents::application_pipeline::PipelineToolCallLineage::from_call(
                &foreign_call,
                0,
            )
            .unwrap();
        assert!(
            registry
                .validate_saved_tool_continuation(
                    "thread-1",
                    &checkpointer,
                    &route,
                    &outer_events,
                    &foreign_lineage
                )
                .await
                .is_err()
        );
        let proof = registry
            .validate_saved_tool_continuation(
                "thread-1",
                &checkpointer,
                &route,
                &outer_events,
                &outer_lineage,
            )
            .await
            .unwrap();
        let local_pause = proof
            .events
            .iter()
            .find(|event| event.id == pause.id)
            .unwrap();
        assert!(
            !local_pause
                .provider_metadata
                .contains_key(crate::agents::application_pipeline::BOUNDARY_LEDGER_KEY)
        );
        assert_eq!(
            static_pipeline_tool_pause(local_pause)
                .unwrap()
                .unwrap()
                .pause_id,
            static_pipeline_tool_pause(&pause)
                .unwrap()
                .unwrap()
                .pause_id
        );
        assert!(
            coordinator
                .take(original.invocation_id(), activation.call_id())
                .await
                .unwrap()
                .is_some()
        );
        proof.install_static(vec![decision]).await.unwrap();
        assert!(
            coordinator
                .has_resume(original.invocation_id(), activation.call_id())
                .await
        );
        assert_eq!(serde_json::to_vec(&outer_events).unwrap(), stored_outer);
    }
}
