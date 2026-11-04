//! The thinking setting and its provider forms (ADR-0038 decisions 1 to 3).
//!
//! One canonical setting, [`Thinking`] (`auto | off | low | medium | high`),
//! is stored in `llm_settings.reasoning_effort`. The stored field keeps its
//! name. `none` is read as `off`. Each model in the catalogue declares how
//! its thinking can be controlled, [`ReasoningControl`]. This module is the
//! one table that turns a step's resolved thinking ([`StepThinking`]) and
//! the control kind into the request body. The agent worker's facades and
//! the engines' model client both use it, so a desktop turn, a cloud turn
//! and an engine call send the same form. The gateway passes the form
//! through unchanged.
//!
//! `auto` is not a provider value. The agent runtime resolves it per step
//! (`elitea_agent_runtime::thinking`) before a request is built; a caller
//! without steps uses [`Thinking::single_call`].
//!
//! | Control kind | `off` | `low` / `medium` / `high` |
//! | --- | --- | --- |
//! | `effort` | `reasoning_effort: "none"` | `reasoning_effort: <level>` |
//! | `budget`, OpenAI-compatible wire | `reasoning_effort: "none"` | `reasoning_effort: <level>` |
//! | `budget`, native Anthropic | `thinking` omitted | adaptive with the level as effort, else `budget_tokens` per level |
//! | `toggle` | `chat_template_kwargs.enable_thinking: false` | `chat_template_kwargs.enable_thinking: true` |
//! | `fixed_on`, `fixed_off` | nothing | nothing |
//!
//! A `budget` model reached through the OpenAI-compatible wire (Gemini, or
//! a Claude model marked OpenAI-compatible) gets `reasoning_effort`: that
//! wire has no budget field, and the provider adapter behind the gateway
//! maps the effort to its own budget (`thinkingConfig.thinkingBudget` for
//! Gemini).
//!
//! A `toggle` model gets `enable_thinking` and never `reasoning_effort`:
//! vLLM 0.32 maps `reasoning_effort: none` to "off", but older vLLM and
//! `SGLang` servers ignore it, and `low`, `medium` and `high` are not levels
//! on these models (#1452, phase 0 step 1).
//!
//! A fixed kind refuses a conflicting setting at configuration
//! ([`ReasoningControl::conflict`]); a setting that does not conflict sends
//! nothing, because the model has no switch.

use serde_json::{Map, Value};

/// The canonical thinking setting of an agent, a chat or a turn.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Thinking {
    /// The runtime decides per step (ADR-0038 decision 4).
    Auto,
    Off,
    Low,
    Medium,
    High,
}

/// A thinking level: the value of an explicit setting other than `off`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Level {
    Low,
    Medium,
    High,
}

impl Level {
    /// The wire value (`reasoning_effort`, Anthropic `effort`).
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
        }
    }
}

/// The level `auto` thinks at, and the level of a setting with no level of
/// its own (ADR-0038 decision 4: "at the configured level or `medium`").
pub const DEFAULT_LEVEL: Level = Level::Medium;

/// The thinking of one request, after `auto` was resolved.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum StepThinking {
    Off,
    On(Level),
}

impl Thinking {
    /// The setting from its stored value. `none` is `off`. Any other value
    /// is `None`; the caller refuses it.
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "auto" => Some(Self::Auto),
            "off" | "none" => Some(Self::Off),
            "low" => Some(Self::Low),
            "medium" => Some(Self::Medium),
            "high" => Some(Self::High),
            _ => None,
        }
    }

    /// The canonical stored value.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Off => "off",
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
        }
    }

    /// The explicit setting as one request's thinking; `None` for `auto`,
    /// which only a per-step policy can resolve.
    #[must_use]
    pub const fn explicit(self) -> Option<StepThinking> {
        match self {
            Self::Auto => None,
            Self::Off => Some(StepThinking::Off),
            Self::Low => Some(StepThinking::On(Level::Low)),
            Self::Medium => Some(StepThinking::On(Level::Medium)),
            Self::High => Some(StepThinking::On(Level::High)),
        }
    }

    /// The thinking of a caller that makes one request per answer and has
    /// no steps (the engines' model client): `auto` thinks at
    /// [`DEFAULT_LEVEL`], as the first step of an agent turn does.
    #[must_use]
    pub const fn single_call(self) -> StepThinking {
        match self.explicit() {
            Some(step) => step,
            None => StepThinking::On(DEFAULT_LEVEL),
        }
    }
}

// ---------------------------------------------------------------------------
// The `auto` per-step policy (ADR-0038 decision 4). Shared by the agent
// runtime (which classifies an adk request) and the engines' model client
// (which classifies its own chat messages), so both resolve `auto` alike.
// ---------------------------------------------------------------------------

/// The catalogue's `auto` defaults for one model.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct AutoPolicy {
    /// `reasoning_tool_steps`: thinking stays on for a routine follow-up
    /// after successful tool results. Default off.
    pub tool_steps: bool,
}

/// What kind of step one model request is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StepKind {
    /// The first model step of a turn, or any step that does not follow
    /// tool results.
    Planning,
    /// A step after a tool error or a refused tool call.
    Recovery,
    /// A step after successful tool results.
    ToolFollowUp,
    /// A step of an agent with a structured output schema.
    StructuredAnswer,
    /// Summarisation, compaction, chat naming or a cut-off summary.
    Auxiliary,
}

/// The thinking of one request: `None` sends nothing.
#[must_use]
pub const fn resolve(
    setting: Option<Thinking>,
    kind: StepKind,
    policy: AutoPolicy,
) -> Option<StepThinking> {
    let Some(setting) = setting else {
        return None;
    };
    if let Some(explicit) = setting.explicit() {
        return Some(explicit);
    }
    Some(match kind {
        StepKind::Planning | StepKind::Recovery | StepKind::StructuredAnswer => {
            StepThinking::On(DEFAULT_LEVEL)
        }
        StepKind::ToolFollowUp if policy.tool_steps => StepThinking::On(DEFAULT_LEVEL),
        StepKind::ToolFollowUp | StepKind::Auxiliary => StepThinking::Off,
    })
}

/// How a model's thinking can be controlled: the catalogue attribute
/// `reasoning_control` (ADR-0038 decision 3).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum ReasoningControl {
    /// `OpenAI` `reasoning_effort` levels.
    Effort,
    /// A thinking budget: Anthropic `thinking`, Gemini `thinkingBudget`.
    Budget,
    /// On or off only: `chat_template_kwargs.enable_thinking` (Qwen-family
    /// and similar models on vLLM or `SGLang`).
    Toggle,
    /// The model always thinks and cannot be switched.
    FixedOn,
    /// The model never thinks.
    FixedOff,
}

/// Why a setting does not fit a model's control kind.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Conflict {
    /// `off` on a model that always thinks.
    OffOnFixedOn,
    /// A level on a model that never thinks.
    LevelOnFixedOff,
}

impl Conflict {
    /// A readable refusal. It names no model and no value the caller did
    /// not send.
    #[must_use]
    pub const fn message(self) -> &'static str {
        match self {
            Self::OffOnFixedOn => {
                "this model always thinks: set reasoning_effort to auto, low, medium or high, not off"
            }
            Self::LevelOnFixedOff => {
                "this model cannot think: set reasoning_effort to auto or off, not a level"
            }
        }
    }
}

impl ReasoningControl {
    /// Every kind, in the catalogue's order.
    pub const ALL: [Self; 5] = [
        Self::Effort,
        Self::Budget,
        Self::Toggle,
        Self::FixedOn,
        Self::FixedOff,
    ];

    /// The kind from its catalogue value; `None` for anything else.
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|kind| kind.as_str() == value)
    }

    /// The catalogue value.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Effort => "effort",
            Self::Budget => "budget",
            Self::Toggle => "toggle",
            Self::FixedOn => "fixed_on",
            Self::FixedOff => "fixed_off",
        }
    }

    /// The kind a model with no catalogue value had before the catalogue
    /// declared one: the native Anthropic facade sends a budget, every
    /// other caller sends `reasoning_effort`.
    #[must_use]
    pub const fn legacy(native_anthropic: bool) -> Self {
        if native_anthropic {
            Self::Budget
        } else {
            Self::Effort
        }
    }

    /// Whether a request can switch the model's thinking.
    #[must_use]
    pub const fn is_switchable(self) -> bool {
        matches!(self, Self::Effort | Self::Budget | Self::Toggle)
    }

    /// The conflict of `setting` with this kind, if any. `auto` never
    /// conflicts: on a fixed kind it sends nothing.
    #[must_use]
    pub const fn conflict(self, setting: Thinking) -> Option<Conflict> {
        match (self, setting) {
            (Self::FixedOn, Thinking::Off) => Some(Conflict::OffOnFixedOn),
            (Self::FixedOff, Thinking::Low | Thinking::Medium | Thinking::High) => {
                Some(Conflict::LevelOnFixedOff)
            }
            _ => None,
        }
    }

    /// Whether a request with this thinking must not carry a sampling
    /// temperature: a provider that thinks by effort or budget refuses one.
    /// A toggle model (vLLM, `SGLang`) takes a temperature either way.
    #[must_use]
    pub const fn refuses_temperature(self, step: StepThinking) -> bool {
        matches!(
            (self, step),
            (Self::Effort | Self::Budget, StepThinking::On(_))
        )
    }
}

/// The thinking form of one OpenAI-compatible chat-completions request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OpenAiThinking {
    /// Nothing is sent: the model's own default applies.
    Nothing,
    /// `reasoning_effort` with this value.
    ReasoningEffort(&'static str),
    /// `chat_template_kwargs.enable_thinking` with this value.
    EnableThinking(bool),
}

/// The OpenAI-compatible form of `step` for a model of kind `control`.
#[must_use]
pub const fn openai_form(control: ReasoningControl, step: StepThinking) -> OpenAiThinking {
    match (control, step) {
        (ReasoningControl::Effort | ReasoningControl::Budget, StepThinking::Off) => {
            OpenAiThinking::ReasoningEffort("none")
        }
        (ReasoningControl::Effort | ReasoningControl::Budget, StepThinking::On(level)) => {
            OpenAiThinking::ReasoningEffort(level.as_str())
        }
        (ReasoningControl::Toggle, StepThinking::Off) => OpenAiThinking::EnableThinking(false),
        (ReasoningControl::Toggle, StepThinking::On(_)) => OpenAiThinking::EnableThinking(true),
        (ReasoningControl::FixedOn | ReasoningControl::FixedOff, _) => OpenAiThinking::Nothing,
    }
}

/// Writes `form` into a chat-completions body. `chat_template_kwargs` is
/// merged into an object the body already has; a non-object there is
/// replaced, because nothing else in the platform writes that member.
pub fn apply_openai(body: &mut Map<String, Value>, form: OpenAiThinking) {
    match form {
        OpenAiThinking::Nothing => {}
        OpenAiThinking::ReasoningEffort(value) => {
            body.insert(
                "reasoning_effort".to_owned(),
                Value::String(value.to_owned()),
            );
        }
        OpenAiThinking::EnableThinking(enabled) => {
            let kwargs = body
                .entry("chat_template_kwargs")
                .or_insert_with(|| Value::Object(Map::new()));
            if !kwargs.is_object() {
                *kwargs = Value::Object(Map::new());
            }
            if let Value::Object(kwargs) = kwargs {
                kwargs.insert("enable_thinking".to_owned(), Value::Bool(enabled));
            }
        }
    }
}

/// The thinking form of one native Anthropic Messages request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AnthropicThinking {
    /// `thinking` is omitted.
    Omitted,
    /// `thinking: {type: adaptive}` with the level as `output_config.effort`.
    Adaptive(Level),
    /// `thinking: {type: enabled, budget_tokens}`.
    Budget(u32),
}

/// The thinking budget of a level, for a model without adaptive thinking.
/// Low and medium are the values the native facade used before this table.
/// High was 9 092 there, a typo for 8 192 with no recorded reason.
#[must_use]
pub const fn budget_tokens(level: Level) -> u32 {
    match level {
        Level::Low => 2_048,
        Level::Medium => 4_096,
        Level::High => 8_192,
    }
}

/// The native Anthropic form of `step`. `adaptive` says whether the model
/// has adaptive thinking ([`anthropic_adaptive`]). A fixed kind sends
/// nothing, as on the OpenAI-compatible wire.
#[must_use]
pub const fn anthropic_form(
    control: ReasoningControl,
    step: StepThinking,
    adaptive: bool,
) -> AnthropicThinking {
    match (control, step) {
        (ReasoningControl::FixedOn | ReasoningControl::FixedOff, _) | (_, StepThinking::Off) => {
            AnthropicThinking::Omitted
        }
        (_, StepThinking::On(level)) if adaptive => AnthropicThinking::Adaptive(level),
        (_, StepThinking::On(level)) => AnthropicThinking::Budget(budget_tokens(level)),
    }
}

/// Whether an Anthropic model has adaptive thinking (the model families
/// from Claude Opus 4.7 and Sonnet 4.6 on), by its configured name.
#[must_use]
pub fn anthropic_adaptive(model_name: &str) -> bool {
    let name = model_name.to_ascii_lowercase();
    [
        "fable-5",
        "fable_5",
        "opus-4-7",
        "opus_4_7",
        "opus-4.7",
        "opus-4-8",
        "opus_4_8",
        "opus-4.8",
        "sonnet-4-6",
        "sonnet_4_6",
        "sonnet-4.6",
        "sonnet-5",
        "sonnet_5",
        "opus-5",
        "opus_5",
    ]
    .iter()
    .any(|pattern| name.contains(pattern))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn none_is_read_as_off_and_unknown_values_are_refused() {
        assert_eq!(Thinking::parse("none"), Some(Thinking::Off));
        assert_eq!(Thinking::parse("off"), Some(Thinking::Off));
        assert_eq!(Thinking::parse("auto"), Some(Thinking::Auto));
        assert_eq!(Thinking::parse("max"), None);
        assert_eq!(Thinking::parse("Off"), None);
        for setting in [
            Thinking::Auto,
            Thinking::Off,
            Thinking::Low,
            Thinking::Medium,
            Thinking::High,
        ] {
            assert_eq!(Thinking::parse(setting.as_str()), Some(setting));
        }
    }

    #[test]
    fn control_kinds_round_trip() {
        for kind in ReasoningControl::ALL {
            assert_eq!(ReasoningControl::parse(kind.as_str()), Some(kind));
        }
        assert_eq!(ReasoningControl::parse("adaptive"), None);
    }

    #[test]
    fn a_fixed_kind_refuses_only_the_opposite_setting() {
        use ReasoningControl::{FixedOff, FixedOn, Toggle};
        assert_eq!(
            FixedOn.conflict(Thinking::Off),
            Some(Conflict::OffOnFixedOn)
        );
        assert_eq!(FixedOn.conflict(Thinking::High), None);
        assert_eq!(FixedOn.conflict(Thinking::Auto), None);
        assert_eq!(
            FixedOff.conflict(Thinking::Low),
            Some(Conflict::LevelOnFixedOff)
        );
        assert_eq!(FixedOff.conflict(Thinking::Off), None);
        assert_eq!(FixedOff.conflict(Thinking::Auto), None);
        assert_eq!(Toggle.conflict(Thinking::High), None);
    }

    #[test]
    fn the_openai_compatible_table() {
        use ReasoningControl::{Budget, Effort, FixedOff, FixedOn, Toggle};
        let high = StepThinking::On(Level::High);
        assert_eq!(
            openai_form(Effort, StepThinking::Off),
            OpenAiThinking::ReasoningEffort("none")
        );
        assert_eq!(
            openai_form(Effort, high),
            OpenAiThinking::ReasoningEffort("high")
        );
        assert_eq!(
            openai_form(Budget, StepThinking::On(Level::Low)),
            OpenAiThinking::ReasoningEffort("low")
        );
        assert_eq!(
            openai_form(Toggle, StepThinking::Off),
            OpenAiThinking::EnableThinking(false)
        );
        assert_eq!(
            openai_form(Toggle, high),
            OpenAiThinking::EnableThinking(true)
        );
        assert_eq!(openai_form(FixedOn, high), OpenAiThinking::Nothing);
        assert_eq!(
            openai_form(FixedOff, StepThinking::Off),
            OpenAiThinking::Nothing
        );
    }

    #[test]
    fn enable_thinking_merges_into_existing_kwargs() {
        let mut body = Map::new();
        body.insert("chat_template_kwargs".to_owned(), json!({"other": 1}));
        apply_openai(&mut body, OpenAiThinking::EnableThinking(false));
        assert_eq!(
            Value::Object(body.clone()),
            json!({"chat_template_kwargs": {"other": 1, "enable_thinking": false}})
        );
        assert!(body.get("reasoning_effort").is_none());
        let mut empty = Map::new();
        apply_openai(&mut empty, OpenAiThinking::Nothing);
        assert!(empty.is_empty());
    }

    #[test]
    fn the_native_anthropic_table() {
        use ReasoningControl::{Budget, FixedOn};
        let medium = StepThinking::On(Level::Medium);
        assert_eq!(
            anthropic_form(Budget, StepThinking::Off, true),
            AnthropicThinking::Omitted
        );
        assert_eq!(
            anthropic_form(Budget, medium, true),
            AnthropicThinking::Adaptive(Level::Medium)
        );
        assert_eq!(
            anthropic_form(Budget, medium, false),
            AnthropicThinking::Budget(4_096)
        );
        assert_eq!(
            anthropic_form(Budget, StepThinking::On(Level::High), false),
            AnthropicThinking::Budget(8_192)
        );
        assert_eq!(
            anthropic_form(FixedOn, medium, false),
            AnthropicThinking::Omitted
        );
        assert!(anthropic_adaptive("claude-opus-4-7"));
        assert!(!anthropic_adaptive("claude-3-7-sonnet"));
    }

    #[test]
    fn temperature_is_refused_only_while_an_effort_or_budget_model_thinks() {
        use ReasoningControl::{Budget, Effort, Toggle};
        let on = StepThinking::On(Level::Low);
        assert!(Effort.refuses_temperature(on));
        assert!(Budget.refuses_temperature(on));
        assert!(!Effort.refuses_temperature(StepThinking::Off));
        assert!(!Toggle.refuses_temperature(on));
    }

    #[test]
    fn a_single_call_resolves_auto_to_the_default_level() {
        assert_eq!(
            Thinking::Auto.single_call(),
            StepThinking::On(DEFAULT_LEVEL)
        );
        assert_eq!(Thinking::Off.single_call(), StepThinking::Off);
    }
}
