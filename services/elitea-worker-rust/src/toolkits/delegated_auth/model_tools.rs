//! One model-visible authorization tool per unavailable delegated toolkit.
//!
//! Keep operation placeholders private for exact legacy replay. They cannot
//! dispatch protected work. Direct graph nodes retain their node-start guards.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::LlmResponseStream;
use adk_rust::schema_adapter::SchemaAdapter;
use adk_rust::tool::BasicToolset;
use adk_rust::{Llm, LlmRequest, ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::{
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement,
    delegated_authorization_declined_result, delegated_authorization_error,
};

/// Add local auth tools and suppress protected declarations at the model edge.
/// Caller validates and applies continuation decisions before this function.
pub(crate) type AuthorizationModelBinding = (Arc<dyn Llm>, Vec<Arc<dyn Toolset>>);

pub(crate) fn bind_authorization_model_tools(
    model: Arc<dyn Llm>,
    mut toolsets: Vec<Arc<dyn Toolset>>,
    catalog: &mut DelegatedAuthorizationCatalog,
) -> adk_rust::Result<AuthorizationModelBinding> {
    if catalog.is_empty() {
        return Ok((model, toolsets));
    }
    let mut proxies = BTreeMap::new();
    let hidden = catalog.provider_requirements.keys().cloned().collect();
    for requirement in catalog.provider_requirements.values() {
        let name = requirement.authorization_tool_name();
        if catalog.provider_requirements.contains_key(&name) {
            return Err(adk_rust::AdkError::config(
                "authorization tool name conflicts with an operation",
            ));
        }
        if let Some(existing) = proxies.insert(name, requirement.clone())
            && !existing.same_authority(requirement)
        {
            return Err(adk_rust::AdkError::config(
                "authorization tool identity is ambiguous",
            ));
        }
    }
    let tools = proxies.iter().map(|(name, requirement)| {
        Arc::new(AuthorizationTool {
            name: name.clone(),
            description: format!(
                "Authorize the '{}' toolkit. Call this tool when the user's current task needs this toolkit. Do not ask the user to name this internal tool. A Skip decision from an earlier user turn does not apply to the current turn. Protected operations are unavailable until authorization succeeds.",
                requirement.toolkit_name()
            ),
            requirement: requirement.clone(),
        }) as Arc<dyn Tool>
    }).collect();
    catalog.provider_requirements.extend(proxies);
    toolsets.push(Arc::new(BasicToolset::new(
        "elitea_delegated_authorization",
        tools,
    )));
    let declined: BTreeMap<_, _> = catalog
        .provider_requirements
        .iter()
        .filter(|(name, _)| catalog.is_declined(name))
        .map(|(name, requirement)| (name.clone(), requirement.clone()))
        .collect();
    if !declined.is_empty() {
        let declined = Arc::new(declined);
        toolsets = toolsets
            .into_iter()
            .map(|inner| {
                Arc::new(DeclinedToolset {
                    inner,
                    declined: Arc::clone(&declined),
                }) as Arc<dyn Toolset>
            })
            .collect();
    }
    Ok((
        Arc::new(AuthorizationModel {
            inner: model,
            hidden,
        }),
        toolsets,
    ))
}

struct AuthorizationModel {
    inner: Arc<dyn Llm>,
    hidden: BTreeSet<String>,
}

pub(crate) fn hide_model_tools(model: Arc<dyn Llm>, hidden: BTreeSet<String>) -> Arc<dyn Llm> {
    if hidden.is_empty() {
        model
    } else {
        Arc::new(AuthorizationModel {
            inner: model,
            hidden,
        })
    }
}

#[async_trait]
impl Llm for AuthorizationModel {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.inner.schema_adapter()
    }
    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        mut request: LlmRequest,
        stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        request.tools.retain(|name, _| !self.hidden.contains(name));
        self.inner.generate_content(request, stream).await
    }
}

struct AuthorizationTool {
    name: String,
    description: String,
    requirement: DelegatedAuthorizationRequirement,
}

#[async_trait]
impl Tool for AuthorizationTool {
    fn name(&self) -> &str {
        &self.name
    }
    fn description(&self) -> &str {
        &self.description
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {}, "additionalProperties": false}))
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        // Defense in depth when invoked outside the LLM confirmation boundary.
        Err(delegated_authorization_error(&self.requirement))
    }
}

struct DeclinedToolset {
    inner: Arc<dyn Toolset>,
    declined: Arc<BTreeMap<String, DelegatedAuthorizationRequirement>>,
}

#[async_trait]
impl Toolset for DeclinedToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }
    async fn tools(
        &self,
        context: Arc<dyn ReadonlyContext>,
    ) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        Ok(self
            .inner
            .tools(context)
            .await?
            .into_iter()
            .map(|inner| match self.declined.get(inner.name()) {
                Some(requirement) => Arc::new(DeclinedTool {
                    inner,
                    requirement: requirement.clone(),
                }) as Arc<dyn Tool>,
                None => inner,
            })
            .collect())
    }
}

struct DeclinedTool {
    inner: Arc<dyn Tool>,
    requirement: DelegatedAuthorizationRequirement,
}

#[async_trait]
impl Tool for DeclinedTool {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }
    fn is_read_only(&self) -> bool {
        true
    }
    fn is_concurrency_safe(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        let mut actions = context.actions();
        actions.tool_confirmation_decision = Some(adk_rust::ToolConfirmationDecision::Deny);
        context.set_actions(actions);
        Ok(delegated_authorization_declined_result(
            &self.requirement,
            self.name(),
        ))
    }
}

#[cfg(test)]
mod tests;
