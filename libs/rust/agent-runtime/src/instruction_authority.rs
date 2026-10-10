//! Immutable instruction snapshots live in session state, outside transcript compaction.
#![allow(
    clippy::implicit_hasher,
    clippy::missing_errors_doc,
    clippy::must_use_candidate,
    clippy::return_self_not_must_use,
    reason = "moved verbatim from the worker, where these items were crate-private"
)]
use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::sync::Arc;

use adk_agent::LlmAgentBuilder;
use adk_core::{
    AdkError, Agent, Content, Event, EventStream, InvocationContext, ReadonlyContext, Tool,
    ToolContext, Toolset,
};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use futures::StreamExt as _;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::request::AgentExecutionRequest;
use crate::assembly_error::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

pub const STATE_PREFIX: &str = "elitea.instructions.v1:";
pub const TOOLSET_NAME: &str = "elitea_instructions";
const MAX_SOURCES: usize = 128;
const MAX_CONTENT_BYTES: usize = 256 * 1024;
const MAX_TOTAL_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
struct Snapshot {
    id: String,
    revision: String,
    source_scope: String,
    name: String,
    description: String,
    content: String,
    kind: String,
    skill_id: Value,
    icon_meta: Value,
}

#[derive(Clone, Debug, Default)]
pub struct InstructionPlan {
    catalog: BTreeMap<String, Snapshot>,
    active: BTreeSet<String>,
    run_id: String,
    resume: bool,
    allow_new_scope_on_resume: bool,
    pipeline_node: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
struct InstructionState {
    schema: u32,
    scope: String,
    activated_run_id: String,
    catalog: BTreeMap<String, Snapshot>,
    active: BTreeSet<String>,
}

impl InstructionPlan {
    pub fn admit(request: &AgentExecutionRequest) -> Result<Self, NativeAgentAssemblyError> {
        let mut plan = Self {
            run_id: request.binding.request_immutable_version.clone(),
            resume: request.payload.should_continue || request.payload.hitl_resume,
            ..Self::default()
        };
        if let Some(version) = request
            .payload
            .application
            .get("version_details")
            .and_then(Value::as_object)
        {
            plan.add_skills(
                version
                    .get("skills")
                    .and_then(Value::as_array)
                    .map_or(&[], Vec::as_slice),
            )?;
        }
        plan.add_skills(&request.payload.attached_skills)?;
        plan.add_skills(&request.payload.invoked_skills)?;
        for value in request
            .payload
            .invoked_skills
            .iter()
            .chain(&request.payload.applied_skills)
        {
            let id = plan.resolve_skill(value)?;
            plan.active.insert(id);
        }
        if let Some(context) = &request.payload.project_context {
            plan.add_project_context(context)?;
        }
        Ok(plan)
    }

    pub fn nested(
        version: &serde_json::Map<String, Value>,
        fallback: &Self,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let mut plan = Self {
            run_id: fallback.run_id.clone(),
            resume: fallback.resume,
            allow_new_scope_on_resume: true,
            ..Self::default()
        };
        plan.add_skills(
            version
                .get("skills")
                .and_then(Value::as_array)
                .map_or(&[], Vec::as_slice),
        )?;
        if let Some(context) = version.get("project_context").filter(|v| !v.is_null()) {
            let context = serde_json::from_value(context.clone()).map_err(|_| invalid())?;
            plan.add_project_context(&context)?;
        }
        Ok(plan)
    }

    fn add_project_context(
        &mut self,
        context: &super::request::ProjectContextSnapshot,
    ) -> Result<(), NativeAgentAssemblyError> {
        let snapshot = Snapshot {
            id: context.id.clone(),
            revision: context.revision.clone(),
            source_scope: context.scope.clone(),
            name: "Project Context".to_owned(),
            description: context.activation_description.clone(),
            content: context.content.clone(),
            skill_id: Value::Null,
            icon_meta: Value::Null,
            kind: "project_context".to_owned(),
        };
        snapshot.validate().map_err(|_| invalid())?;
        if context.activation_description.is_empty() {
            self.active.insert(context.id.clone());
        }
        if self.catalog.insert(context.id.clone(), snapshot).is_some() {
            return Err(invalid());
        }
        self.validate_bounds()
    }

    fn add_skills(&mut self, skills: &[Value]) -> Result<(), NativeAgentAssemblyError> {
        for skill in skills {
            let snapshot = skill_snapshot(skill)?;
            if self.catalog.values().any(|existing| {
                existing.name.trim().to_lowercase() == snapshot.name.trim().to_lowercase()
                    && existing.id != snapshot.id
            }) {
                return Err(invalid());
            }
            if let Some(existing) = self.catalog.get(&snapshot.id) {
                // Descriptions may carry Main's instruction-reference hint.
                if existing.revision != snapshot.revision
                    || existing.content != snapshot.content
                    || existing.name != snapshot.name
                    || existing.source_scope != snapshot.source_scope
                {
                    return Err(invalid());
                }
            } else {
                self.catalog.insert(snapshot.id.clone(), snapshot);
            }
        }
        self.validate_bounds()
    }

    fn resolve_skill(&self, value: &Value) -> Result<String, NativeAgentAssemblyError> {
        if let Some(id) = value.get("id").and_then(Value::as_str) {
            return self
                .catalog
                .contains_key(id)
                .then(|| id.to_owned())
                .ok_or_else(invalid);
        }
        let name = value
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(invalid)?;
        self.catalog
            .values()
            .find(|skill| skill.name == name)
            .map(|skill| skill.id.clone())
            .ok_or_else(invalid)
    }

    fn validate_bounds(&self) -> Result<(), NativeAgentAssemblyError> {
        if self.catalog.len() > MAX_SOURCES
            || self
                .catalog
                .values()
                .map(|s| s.content.len())
                .sum::<usize>()
                > MAX_TOTAL_BYTES
        {
            return Err(invalid());
        }
        Ok(())
    }

    pub const fn inherits_pipeline_parent(&self) -> bool {
        !self.allow_new_scope_on_resume
    }

    pub fn for_pipeline_node(&self) -> Self {
        let mut plan = self.clone();
        plan.allow_new_scope_on_resume = true;
        plan.pipeline_node = true;
        plan
    }

    pub fn public_active(&self) -> Vec<Value> {
        self.catalog
            .values()
            .filter(|source| source.kind == "skill" && self.active.contains(&source.id))
            .map(Snapshot::public)
            .collect()
    }

    pub fn is_empty(&self) -> bool {
        self.catalog.is_empty()
    }

    pub fn toolsets(&self) -> Vec<Arc<dyn Toolset>> {
        if self.is_empty() && !self.resume {
            return Vec::new();
        }
        let mut tools: Vec<Arc<dyn Tool>> = Vec::new();
        if self.resume || self.catalog.values().any(|s| s.kind == "skill") {
            tools.push(Arc::new(InstructionTool { kind: "skill" }));
        }
        if self.resume || self.catalog.values().any(|s| s.kind == "project_context") {
            tools.push(Arc::new(InstructionTool {
                kind: "project_context",
            }));
        }
        vec![Arc::new(BasicToolset::new(TOOLSET_NAME, tools))]
    }

    pub fn bind_builder(&self, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        if self.run_id.is_empty() {
            return builder;
        }
        let empty = self.catalog.is_empty();
        builder.before_model_callback(Box::new(move |ctx, mut request| {
            Box::pin(async move {
                if empty
                    && ctx
                        .state()
                        .and_then(|state| state.get(&state_key(&instruction_scope(ctx.as_ref()))))
                        .is_none()
                {
                    return Ok(adk_core::callbacks::BeforeModelResult::Continue(request));
                }
                let state = read_state(ctx.as_ref())?;
                let text = state.render();
                if !text.is_empty() {
                    request
                        .contents
                        .insert(0, Content::new("system").with_text(text));
                }
                Ok(adk_core::callbacks::BeforeModelResult::Continue(request))
            })
        }))
    }

    pub fn wrap(&self, inner: Arc<dyn Agent>) -> Arc<dyn Agent> {
        if self.run_id.is_empty() {
            return inner;
        }
        Arc::new(InstructionAgent {
            inner,
            plan: self.clone(),
        })
    }

    fn start(&self, stored: Option<Value>, scope: String) -> adk_core::Result<InstructionState> {
        if let Some(stored) = stored {
            let state: InstructionState = serde_json::from_value(stored).map_err(|_| corrupt())?;
            state.validate(&scope)?;
            if self.resume || state.activated_run_id == self.run_id {
                return Ok(state);
            }
        } else if self.resume && !self.allow_new_scope_on_resume && !self.catalog.is_empty() {
            return Err(corrupt());
        }
        Ok(InstructionState {
            schema: 1,
            scope,
            activated_run_id: self.run_id.clone(),
            catalog: self.catalog.clone(),
            active: self.active.clone(),
        })
    }
}

/// Fixtures for the worker's composition suites (`test-support`, never in a
/// production build). They run the plan against worker sessions, internal
/// tools and durable compaction, so they build it from frozen sources and
/// read back only what a turn would show; the plan's internals stay private.
#[cfg(any(test, feature = "test-support"))]
impl InstructionPlan {
    /// A plan for `run_id` (resumed or fresh) over these frozen skills and
    /// project contexts.
    pub fn fixture(
        run_id: &str,
        resume: bool,
        skills: &[Value],
        contexts: &[super::request::ProjectContextSnapshot],
    ) -> Result<Self, NativeAgentAssemblyError> {
        let mut plan = Self {
            run_id: run_id.to_owned(),
            resume,
            ..Self::default()
        };
        plan.add_skills(skills)?;
        for context in contexts {
            plan.add_project_context(context)?;
        }
        Ok(plan)
    }

    /// `(id, revision, content)` of every frozen source, in catalog order.
    pub fn sources_for_test(&self) -> Vec<(String, String, String)> {
        self.catalog
            .values()
            .map(|source| {
                (
                    source.id.clone(),
                    source.revision.clone(),
                    source.content.clone(),
                )
            })
            .collect()
    }

    /// The instruction text this plan renders when it starts in `scope`
    /// over `stored` authoritative state.
    pub fn render_for_test(
        &self,
        stored: Option<Value>,
        scope: String,
    ) -> adk_core::Result<String> {
        self.start(stored, scope).map(|state| state.render())
    }
}

impl Snapshot {
    fn public(&self) -> Value {
        json!({"skill_id":self.skill_id,"name":self.name,"icon_meta":self.icon_meta,"id":self.id,"revision":self.revision})
    }
    fn validate(&self) -> adk_core::Result<()> {
        if self.id.is_empty()
            || self.id.len() > 256
            || self.name.is_empty()
            || self.name.len() > 256
            || self.source_scope.is_empty()
            || self.source_scope.len() > 256
            || self.content.is_empty()
            || self.content.len() > MAX_CONTENT_BYTES
            || self.content.contains('\0')
            || self.description.len() > 8192
            || !serde_json::to_vec(&self.icon_meta).is_ok_and(|value| value.len() <= 4096)
            || (!self.skill_id.is_null()
                && !self.skill_id.is_number()
                && !self.skill_id.is_string())
            || self.revision != content_digest(&self.content)
            || !matches!(self.kind.as_str(), "skill" | "project_context")
        {
            return Err(corrupt());
        }
        Ok(())
    }
}

impl InstructionState {
    fn validate(&self, scope: &str) -> adk_core::Result<()> {
        if self.schema != 1
            || self.scope != scope
            || self.activated_run_id.is_empty()
            || self.activated_run_id.len() > 512
            || self.catalog.len() > MAX_SOURCES
            || self
                .catalog
                .values()
                .map(|s| s.content.len())
                .sum::<usize>()
                > MAX_TOTAL_BYTES
            || !self.active.iter().all(|id| self.catalog.contains_key(id))
        {
            return Err(corrupt());
        }
        for (id, source) in &self.catalog {
            source.validate()?;
            if id != &source.id {
                return Err(corrupt());
            }
        }
        Ok(())
    }

    fn render(&self) -> String {
        let mut text = String::new();
        for source in self.catalog.values() {
            if self.active.contains(&source.id) {
                let _ = write!(
                    text,
                    "\n<active_instruction id={:?} revision={:?}>\n{}\n</active_instruction>\n",
                    source.id, source.revision, source.content
                );
            } else {
                let _ = write!(
                    text,
                    "\nAvailable {}: {}. {} Use {} to read its instructions.\n",
                    source.kind,
                    source.name,
                    source.description,
                    if source.kind == "skill" {
                        "load_skill"
                    } else {
                        "read_project_context"
                    }
                );
            }
        }
        text
    }
}

struct InstructionAgent {
    inner: Arc<dyn Agent>,
    plan: InstructionPlan,
}
#[async_trait]
impl Agent for InstructionAgent {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        self.inner.sub_agents()
    }
    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_core::Result<EventStream> {
        let scope = instruction_scope(ctx.as_ref());
        let key = state_key(&scope);
        let stored = ctx.session().state().get(&key);
        if stored.is_none() && self.plan.catalog.is_empty() {
            return self.inner.run(ctx).await;
        }
        let state = self.plan.start(stored, scope)?;
        let owner_state = if self.plan.pipeline_node {
            let mut owner = state.clone();
            owner.scope = pipeline_scope(ctx.session_id());
            Some((
                state_key(&owner.scope),
                serde_json::to_value(owner).map_err(|_| corrupt())?,
            ))
        } else {
            None
        };
        let encoded = serde_json::to_value(state).map_err(|_| corrupt())?;
        let inner = self.inner.clone();
        let mut event = Event::new(ctx.invocation_id());
        self.name().clone_into(&mut event.author);
        ctx.branch().clone_into(&mut event.branch);
        event.actions.state_delta.insert(key, encoded);
        if let Some((key, value)) = owner_state {
            event.actions.state_delta.insert(key, value);
        }
        let authority_keys: Vec<_> = event.actions.state_delta.keys().cloned().collect();
        Ok(Box::pin(async_stream::stream! {
            yield Ok(event);
            match inner.run(ctx.clone()).await {
                Ok(mut events) => while let Some(event) = events.next().await {
                    yield event.and_then(|mut event| {
                        for key in &authority_keys {
                            if let Some(value) = event.actions.state_delta.get_mut(key) {
                                merge_activation_delta(value, ctx.session().state().get(key))?;
                            }
                        }
                        Ok(event)
                    });
                },
                Err(error) => yield Err(error),
            }
        }))
    }
}

// ADK dispatches a model's tool batch before publishing its result events.
// Each instruction tool therefore starts from the same active set. Merge at
// the publication boundary, after Runner persisted the preceding event.
fn merge_activation_delta(value: &mut Value, previous: Option<Value>) -> adk_core::Result<()> {
    let Some(previous) = previous else {
        return Err(corrupt());
    };
    let mut next: InstructionState =
        serde_json::from_value(value.clone()).map_err(|_| corrupt())?;
    let previous: InstructionState = serde_json::from_value(previous).map_err(|_| corrupt())?;
    next.validate(&previous.scope)?;
    previous.validate(&previous.scope)?;
    if next.activated_run_id != previous.activated_run_id || next.catalog != previous.catalog {
        return Err(corrupt());
    }
    if !previous.active.is_subset(&next.active) {
        next.active.extend(previous.active);
        *value = serde_json::to_value(next).map_err(|_| corrupt())?;
    }
    Ok(())
}

struct InstructionTool {
    kind: &'static str,
}
#[async_trait]
impl Tool for InstructionTool {
    fn name(&self) -> &str {
        if self.kind == "skill" {
            "load_skill"
        } else {
            "read_project_context"
        }
    }
    fn description(&self) -> &str {
        if self.kind == "skill" {
            "Load an attached skill by its exact name. Its frozen instructions remain active for this run."
        } else {
            "Read the frozen project instructions. They remain active for this run."
        }
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(if self.kind == "skill" {
            json!({"type":"object","properties":{"skill":{"type":"string"}},"required":["skill"],"additionalProperties":false})
        } else {
            json!({"type":"object","properties":{},"additionalProperties":false})
        })
    }
    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let scope = instruction_scope(ctx.as_ref());
        let value = ctx
            .session()
            .and_then(|session| session.state().get(&state_key(&scope)))
            .ok_or_else(corrupt)?;
        let mut state: InstructionState = serde_json::from_value(value).map_err(|_| corrupt())?;
        state.validate(&scope)?;
        let object = args.as_object().ok_or_else(corrupt)?;
        let source = if self.kind == "skill" {
            if object.len() != 1 {
                return Err(corrupt());
            }
            let name = object
                .get("skill")
                .and_then(Value::as_str)
                .ok_or_else(corrupt)?;
            state.catalog.values().find(|s| {
                s.kind == self.kind && s.name.trim().to_lowercase() == name.trim().to_lowercase()
            })
        } else {
            if !object.is_empty() {
                return Err(corrupt());
            }
            state.catalog.values().find(|s| s.kind == self.kind)
        };
        let Some(source) = source.cloned() else {
            return Ok(json!({"error":"instruction_not_found"}));
        };
        let activated = state.active.insert(source.id.clone());
        let result = json!({"id": source.id, "revision": source.revision, "scope": state.scope, "activated_run_id":state.activated_run_id, "content":source.content, "activated":activated});
        let mut actions = ctx.actions();
        let owner_scope = pipeline_scope(ctx.session_id());
        if ctx
            .session()
            .and_then(|session| session.state().get(&state_key(&owner_scope)))
            .is_some()
        {
            let mut owner = state.clone();
            owner.scope = owner_scope;
            actions.state_delta.insert(
                state_key(&owner.scope),
                serde_json::to_value(owner).map_err(|_| corrupt())?,
            );
        }
        actions.state_delta.insert(
            state_key(&state.scope),
            serde_json::to_value(state).map_err(|_| corrupt())?,
        );
        ctx.set_actions(actions);
        Ok(result)
    }
}

fn instruction_scope(ctx: &dyn ReadonlyContext) -> String {
    format!("{}:{}", ctx.session_id(), ctx.agent_name())
}
fn state_key(scope: &str) -> String {
    format!("{STATE_PREFIX}{}", content_digest(scope))
}
fn read_state(ctx: &dyn ReadonlyContext) -> adk_core::Result<InstructionState> {
    let scope = instruction_scope(ctx);
    let value = ctx
        .state()
        .and_then(|state| state.get(&state_key(&scope)))
        .ok_or_else(corrupt)?;
    let state: InstructionState = serde_json::from_value(value).map_err(|_| corrupt())?;
    state.validate(&scope)?;
    Ok(state)
}
fn pipeline_scope(session_id: &str) -> String {
    format!("{session_id}:__pipeline")
}

pub fn inherited_pipeline_state(
    parent: &dyn InvocationContext,
    session_id: &str,
    agent_name: &str,
    inherit_root: bool,
) -> adk_core::Result<std::collections::HashMap<String, Value>> {
    let owner_scope = pipeline_scope(session_id);
    let own_scope = format!("{session_id}:{agent_name}");
    let parent_scope = instruction_scope(parent);
    let stored = parent.session().state();
    let selected = stored
        .get(&state_key(&owner_scope))
        .map(|value| (owner_scope.clone(), value))
        .or_else(|| {
            if inherit_root {
                stored
                    .get(&state_key(&parent_scope))
                    .map(|value| (parent_scope, value))
            } else {
                None
            }
        });
    let Some((source_scope, value)) = selected else {
        return Ok(std::collections::HashMap::new());
    };
    let mut state: InstructionState = serde_json::from_value(value).map_err(|_| corrupt())?;
    state.validate(&source_scope)?;
    state.scope.clone_from(&owner_scope);
    let mut result = std::collections::HashMap::from([(
        state_key(&owner_scope),
        serde_json::to_value(&state).map_err(|_| corrupt())?,
    )]);
    state.scope = own_scope;
    result.insert(
        state_key(&state.scope),
        serde_json::to_value(state).map_err(|_| corrupt())?,
    );
    Ok(result)
}

pub fn state_for_child(
    parent: &dyn adk_core::State,
    session_id: &str,
    agent_name: &str,
) -> std::collections::HashMap<String, Value> {
    let key = state_key(&format!("{session_id}:{agent_name}"));
    parent
        .get(&key)
        .map_or_else(std::collections::HashMap::new, |value| {
            std::collections::HashMap::from([(key, value)])
        })
}

pub fn public_active_delta(delta: &std::collections::HashMap<String, Value>) -> Option<Vec<Value>> {
    if !valid_state_delta(delta) {
        return None;
    }
    let mut active = BTreeMap::new();
    for value in delta.values() {
        let state: InstructionState = serde_json::from_value(value.clone()).ok()?;
        for source in state
            .catalog
            .values()
            .filter(|source| source.kind == "skill" && state.active.contains(&source.id))
        {
            active.insert(source.id.clone(), source.public());
        }
    }
    Some(active.into_values().collect())
}

pub fn valid_state_delta(delta: &std::collections::HashMap<String, Value>) -> bool {
    !delta.is_empty()
        && delta.iter().all(|(key, value)| {
            let Ok(state) = serde_json::from_value::<InstructionState>(value.clone()) else {
                return false;
            };
            key == &state_key(&state.scope) && state.validate(&state.scope).is_ok()
        })
}
/// One frozen skill snapshot (an entry of a version's `skills`, or an
/// attached or invoked skill), read and checked the way
/// [`InstructionPlan::admit`] reads every skill: `id`, `name`,
/// `instructions`, `revision` and `scope` present, each within its bound,
/// and `revision` the digest ([`content_digest`]) of the instructions.
fn skill_snapshot(skill: &Value) -> Result<Snapshot, NativeAgentAssemblyError> {
    let object = skill.as_object().ok_or_else(invalid)?;
    let text = |key: &str| {
        object
            .get(key)
            .and_then(Value::as_str)
            .ok_or_else(invalid)
            .map(str::to_owned)
    };
    let snapshot = Snapshot {
        id: text("id")?,
        revision: text("revision")?,
        source_scope: text("scope")?,
        name: text("name")?,
        description: object
            .get("description")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        content: text("instructions")?,
        skill_id: object.get("skill_id").cloned().unwrap_or(Value::Null),
        icon_meta: object.get("icon_meta").cloned().unwrap_or(Value::Null),
        kind: "skill".to_owned(),
    };
    snapshot.validate().map_err(|_| invalid())?;
    Ok(snapshot)
}

/// Whether `skill` is a frozen skill snapshot the runtime would admit
/// (see `skill_snapshot`): for a host that applies a skill itself, before
/// the runtime sees it.
///
/// # Errors
///
/// `InvalidInput`: a missing field, a bound exceeded, or a revision that
/// is not the digest of the instructions.
pub fn check_skill(skill: &Value) -> Result<(), NativeAgentAssemblyError> {
    skill_snapshot(skill).map(|_| ())
}

/// Admit a version's `skills` the way [`InstructionPlan::admit`] does —
/// each snapshot ([`check_skill`]), no two skills under one name, the same
/// id never with other content, and the catalogue's bounds — one skill at
/// a time, so a refusal names the skill.
///
/// # Errors
///
/// The index of the first skill refused, and why.
pub fn check_skills(skills: &[Value]) -> Result<(), (usize, NativeAgentAssemblyError)> {
    let mut plan = InstructionPlan::default();
    for (index, skill) in skills.iter().enumerate() {
        plan.add_skills(std::slice::from_ref(skill))
            .map_err(|error| (index, error))?;
    }
    Ok(())
}

pub fn content_digest(content: &str) -> String {
    let mut result = String::with_capacity(64);
    for byte in digest::digest(&digest::SHA256, content.as_bytes()).as_ref() {
        let _ = write!(result, "{byte:02x}");
    }
    result
}

fn corrupt() -> AdkError {
    AdkError::new(
        adk_core::ErrorComponent::Agent,
        adk_core::ErrorCategory::InvalidInput,
        "agent.instructions.invalid_state",
        "the authoritative instruction state is invalid",
    )
}
fn invalid() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "the frozen instruction snapshot is invalid",
    )
}

#[cfg(test)]
#[path = "instruction_authority_tests.rs"]
mod tests;
