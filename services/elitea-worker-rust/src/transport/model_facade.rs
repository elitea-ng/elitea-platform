//! Claim-scoped model facade over the Elitea gateway transport.
//!
//! The facade owns provider selection and returns one provider-neutral ADK
//! model/completion pair. The provider-specific facade modules share one
//! bounded Elitea `/llm/v1` gateway client; native `OpenAI` can therefore be
//! added without pretending it is the OpenAI-compatible dialect.

#![allow(dead_code)] // Production capability assembly remains gated.

use std::sync::Arc;

use tonic::transport::{Certificate, Identity};

use super::anthropic_facade::BoundAnthropicFacade;
use super::openai_compatible_facade::{BoundOpenAiCompatibleFacade, ModelGatewayClient};
use super::runtime_context::ClaimScopedEliteaContext;
use crate::agents::assembly::{OrdinaryModelProvider, SummaryModelProfile};
use crate::agents::runtime::NativeAgentAssemblyError;
use crate::agents::session::{BoundOrdinaryAgentModel, DurableModelCompletion};

pub(crate) use super::openai_compatible_facade::{
    ModelFacadeError, ModelFacadeInvocation as ModelInvocation,
    ModelGatewayConfig as ModelFacadeConfig, ModelReasoningEffort,
};

/// No tool call leaves the facade when a requested name is not declared.
pub(crate) const TOOL_NOT_ADMITTED_CODE: &str = "model_facade.tool_not_admitted";

/// The longest tool name an unadmitted-call note or log line repeats.
const MAX_NOTED_TOOL_NAME: usize = 64;

/// A model-chosen tool name as a note or a log field may repeat it: only
/// `[A-Za-z0-9_.:-]`, at most [`MAX_NOTED_TOOL_NAME`] characters. The name is
/// model output, so nothing else of it is echoed.
pub(crate) fn noted_tool_name(name: &str) -> String {
    let noted: String = name
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | ':' | '-'))
        .take(MAX_NOTED_TOOL_NAME)
        .collect();
    if noted.is_empty() {
        "unnamed".to_owned()
    } else {
        noted
    }
}

/// Issue 3050: the answer's closing note when the model asked, after its
/// answer had already streamed, only for tools the agent does not have. The
/// call is never emitted (no tool outside the request's declared set leaves
/// the facade); the turn ends on the text the person already read, and the
/// note says why the agent stopped instead of an INTERNAL failure card.
/// `after_text`: answer text already streamed, so the note starts a new
/// paragraph; after reasoning alone it is the whole answer.
pub(crate) fn unadmitted_tool_note(name: &str, after_text: bool) -> String {
    format!(
        "{}_The agent stopped here: it asked for a tool it does not have (`{}`)._",
        if after_text { "\n\n" } else { "" },
        noted_tool_name(name)
    )
}

/// Explicit provider dialect selected from the frozen model configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ModelAdapterKind {
    OpenAiCompatible,
    Anthropic,
}

/// Shared, reusable facade whose bound invocation values remain one-use.
pub(crate) struct ModelFacade {
    gateway: ModelGatewayClient,
}

impl ModelFacade {
    /// Build the shared origin-bound Elitea model transport.
    pub(crate) async fn connect(
        config: ModelFacadeConfig,
        private_ca: Certificate,
        client_identity: Identity,
    ) -> Result<Self, ModelFacadeError> {
        ModelGatewayClient::connect(config, private_ca, client_identity)
            .await
            .map(Self::from_gateway)
    }

    /// Wrap the already constructed Elitea gateway transport.
    pub(crate) const fn from_gateway(gateway: ModelGatewayClient) -> Self {
        Self { gateway }
    }

    /// The channel to elitea-main's platform listener the model calls ride
    /// (`None` over a test transport): the artifact store and the internal
    /// tools are served there too (#1359 U5).
    pub(crate) fn platform_channel(&self) -> Option<tonic::transport::Channel> {
        self.gateway.platform_channel()
    }

    /// The gateway's embeddings route over the same channel (ADR-0030 I1).
    pub(crate) fn embeddings(&self) -> super::embeddings_facade::EmbeddingsClient {
        super::embeddings_facade::EmbeddingsClient::from_gateway(&self.gateway)
    }

    /// The gateway's images routes over the same channel (#864).
    pub(crate) fn images(&self) -> super::images_facade::ImagesClient {
        super::images_facade::ImagesClient::from_gateway(&self.gateway)
    }

    /// The web families' meter of one claim, on the same channel (#1238).
    pub(crate) fn meter(
        &self,
        context: &super::runtime_context::ClaimScopedEliteaContext,
        attribution: Option<&str>,
    ) -> super::metering::GatewayMeter {
        super::metering::GatewayMeter::new(&self.gateway, context, attribution)
    }

    /// Consume the claim credential into the selected native ADK adapter.
    pub(crate) fn bind(
        &self,
        adapter: ModelAdapterKind,
        context: &ClaimScopedEliteaContext,
        model_project_id: u32,
        invocation: ModelInvocation,
    ) -> Result<BoundModelFacade, ModelFacadeError> {
        let primary = match adapter {
            ModelAdapterKind::OpenAiCompatible => self
                .gateway
                .bind_ordinary(context, model_project_id, invocation)
                .map(BoundProviderModel::OpenAiCompatible),
            ModelAdapterKind::Anthropic => self
                .gateway
                .bind_anthropic_ordinary(context, model_project_id, invocation)
                .map(BoundProviderModel::Anthropic),
        }?;
        Ok(BoundModelFacade {
            primary,
            summary: None,
            output_contract: None,
            grounding: None,
        })
    }

    pub(crate) fn bind_with_summary(
        &self,
        adapter: ModelAdapterKind,
        context: &ClaimScopedEliteaContext,
        model_project_id: u32,
        invocation: ModelInvocation,
        summary: Option<&SummaryModelProfile>,
    ) -> Result<BoundModelFacade, ModelFacadeError> {
        let turns = invocation.max_model_turns;
        let mut bound = self.bind(adapter, context, model_project_id, invocation)?;
        if let Some(summary) = summary {
            let adapter = match summary.model_provider {
                OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
                OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
            };
            let selected = self.bind(
                adapter,
                context,
                summary.model_project_id,
                ModelInvocation {
                    response_schema: None,
                    allow_text_continuation: false,
                    context_budget: Some(summary.context_budget),
                    model_name: summary.model_name.clone(),
                    system_instruction: super::summary_model::INSTRUCTION.to_owned(),
                    max_tokens: Some(summary.max_tokens),
                    // A summary call thinks off where the model can be
                    // switched (ADR-0038 decision 4); else nothing is sent.
                    reasoning_effort: summary
                        .model_reasoning
                        .filter(|reasoning| reasoning.control.is_switchable())
                        .map(|_| ModelReasoningEffort::None),
                    reasoning: summary.model_reasoning,
                    temperature: summary.temperature,
                    max_model_turns: turns,
                    run_caps: None,
                    preamble: None,
                },
            )?;
            bound.summary = Some(match selected.primary {
                BoundProviderModel::OpenAiCompatible(model) => {
                    model.summary_model_with_output(Some(summary.max_tokens))
                }
                BoundProviderModel::Anthropic(model) => {
                    model.summary_model_with_output(Some(summary.max_tokens))
                }
            });
        }
        Ok(bound)
    }
}

/// Provider-neutral bound ADK model and exact final-completion owner.
pub(crate) struct BoundModelFacade {
    primary: BoundProviderModel,
    summary: Option<Arc<dyn adk_rust::Llm>>,
    /// The provider model behind the output contract (ADR-0043 decision 2),
    /// when the agent or node declares one. Built once per binding; each
    /// final answer gets its own retry.
    output_contract: Option<Arc<dyn adk_rust::Llm>>,
    /// The model that numbers the request's sources for a root agent that
    /// must cite (ADR-0043 decision 5). It wraps the output contract model
    /// when there is one. The handle is the run's own source count, which
    /// the root event projector reads for the final message.
    grounding: Option<(
        Arc<dyn adk_rust::Llm>,
        crate::agents::grounding_model::GroundingSources,
    )>,
}

enum BoundProviderModel {
    OpenAiCompatible(BoundOpenAiCompatibleFacade),
    Anthropic(BoundAnthropicFacade),
}

impl BoundModelFacade {
    /// Enforce `contract` on every final answer of this binding (ADR-0043
    /// decision 2). `None` leaves the binding as it is.
    #[must_use]
    pub(crate) fn with_output_contract(
        mut self,
        contract: Option<Arc<elitea_agent_runtime::output_contract::OutputContract>>,
        scope: crate::agents::output_contract_model::ContractStepScope,
    ) -> Self {
        self.output_contract = contract.map(|contract| {
            Arc::new(
                crate::agents::output_contract_model::OutputContractModel::new(
                    self.primary_model(),
                    contract,
                    BoundOrdinaryAgentModel::durable_completion(&self),
                    scope,
                ),
            ) as Arc<dyn adk_rust::Llm>
        });
        self
    }

    /// Number the sources of every request of this binding and record the
    /// count for the final message's grounding indicator (ADR-0043 decision
    /// 5) when `cites` is true. Call it after [`Self::with_output_contract`].
    /// `false` leaves the binding as it is.
    #[must_use]
    pub(crate) fn with_grounding(mut self, cites: bool) -> Self {
        self.grounding = cites.then(|| {
            let sources = crate::agents::grounding_model::GroundingSources::default();
            let model = Arc::new(crate::agents::grounding_model::GroundingModel::new(
                self.adk_model(),
                sources.clone(),
            )) as Arc<dyn adk_rust::Llm>;
            (model, sources)
        });
        self
    }

    fn primary_model(&self) -> Arc<dyn adk_rust::Llm> {
        match &self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.adk_model(),
            BoundProviderModel::Anthropic(model) => model.adk_model(),
        }
    }

    /// The model with the generation config its facade admits: what a caller
    /// that builds its own request (`model_text::stream_text`) sends.
    pub(crate) fn admitted_chat(&self) -> super::model_text::AdmittedChat {
        let config = match &self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.admitted_config(),
            BoundProviderModel::Anthropic(model) => model.admitted_config(),
        };
        super::model_text::AdmittedChat {
            model: BoundOrdinaryAgentModel::adk_model(self),
            config,
            budget: None,
        }
    }
}

impl BoundOrdinaryAgentModel for BoundModelFacade {
    fn summarization_model(&self) -> Option<Arc<dyn adk_rust::Llm>> {
        if let Some(summary) = &self.summary {
            return Some(summary.clone());
        }
        match &self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.summarization_model(),
            BoundProviderModel::Anthropic(model) => model.summarization_model(),
        }
    }

    fn request_budget(&self) -> Option<Arc<dyn crate::agents::context_budget::ModelRequestBudget>> {
        match &self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.request_budget(),
            BoundProviderModel::Anthropic(model) => model.request_budget(),
        }
    }

    fn adk_model(&self) -> Arc<dyn adk_rust::Llm> {
        self.grounding
            .as_ref()
            .map(|(model, _)| Arc::clone(model))
            .or_else(|| self.output_contract.clone())
            .unwrap_or_else(|| self.primary_model())
    }

    fn grounding_sources(&self) -> Option<crate::agents::grounding_model::GroundingSources> {
        self.grounding.as_ref().map(|(_, sources)| sources.clone())
    }

    fn idle_adk_model(&self) -> Arc<dyn adk_rust::Llm> {
        self.primary_model()
    }

    fn take_completed_text(self) -> Result<String, NativeAgentAssemblyError> {
        match self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.take_completed_text(),
            BoundProviderModel::Anthropic(model) => model.take_completed_text(),
        }
    }

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
        match &self.primary {
            BoundProviderModel::OpenAiCompatible(model) => model.durable_completion(),
            BoundProviderModel::Anthropic(model) => model.durable_completion(),
        }
    }
}

#[cfg(test)]
mod noted_tool_name_tests {
    use super::{noted_tool_name, unadmitted_tool_note};

    #[test]
    fn a_model_chosen_name_is_cut_to_safe_characters_and_bounded() {
        assert_eq!(noted_tool_name("gh <pr list>"), "ghprlist");
        assert_eq!(noted_tool_name("`;\n"), "unnamed");
        assert_eq!(noted_tool_name(&"a".repeat(200)).len(), 64);
        assert!(unadmitted_tool_note("gh", true).starts_with("\n\n_"));
        assert!(unadmitted_tool_note("gh", false).starts_with('_'));
    }
}
