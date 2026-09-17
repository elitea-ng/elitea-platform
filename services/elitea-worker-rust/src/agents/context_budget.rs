//! Combined request budget, independent of cumulative usage and output caps.

use adk_rust::{AdkError, Content, ErrorCategory, ErrorComponent, Event, LlmRequest};
use serde_json::{Map, Value};

use super::request::ModelContextLimits;
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

const BALANCED_TOKENS: u32 = 272_000;

/// The compaction boundary consumes measurements of the actual provider body.
/// Implementations must not dispatch, consume a model turn, or change completion state.
pub(crate) trait ModelRequestBudget: Send + Sync {
    fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage>;
}

/// Request occupancy, independent of cumulative usage and without request content.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RequestContextUsage {
    pub(crate) budget: RequestContextBudget,
    pub(crate) estimated_input: u64,
    pub(crate) request_bytes: usize,
    pub(crate) request_byte_limit: usize,
}

impl RequestContextUsage {
    /// Refuse an unusable model checkpoint before it can authorize recovery.
    pub(crate) fn check(self) -> adk_rust::Result<()> {
        tracing::debug!(
            estimated_input = self.estimated_input,
            output_reservation = self.budget.output_reservation,
            margin_tokens = self.budget.margin_tokens,
            total_tokens = self.budget.total_tokens,
            input_limit = self.budget.input_limit,
            compaction_trigger = self.budget.compaction_trigger(),
            compaction_target = self.budget.compaction_target(),
            needs_compaction = self.needs_compaction(),
            request_bytes = self.request_bytes,
            request_byte_limit = self.request_byte_limit,
            "checked model request capacity before checkpoint persistence"
        );
        if self.fits() {
            return Ok(());
        }
        if self.estimated_input > u64::from(self.budget.input_limit) {
            return Err(budget_error());
        }
        Err(AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::InvalidInput,
            "model_request_bytes_exceeded",
            "The model request exceeds the permitted request size.",
        ))
    }

    pub(crate) fn needs_compaction(self) -> bool {
        self.estimated_input >= self.budget.compaction_trigger()
            || self.request_bytes > self.request_byte_limit
    }

    pub(crate) fn fits(self) -> bool {
        self.estimated_input <= u64::from(self.budget.input_limit)
            && self.request_bytes <= self.request_byte_limit
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum BudgetSelection {
    Balanced,
    Full,
    Explicit(u32),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct RequestContextBudget {
    pub(crate) total_tokens: u32,
    pub(crate) output_reservation: u32,
    pub(crate) margin_tokens: u32,
    pub(crate) input_limit: u32,
    pub(crate) limits: ModelContextLimits,
    selection: BudgetSelection,
}

impl RequestContextBudget {
    pub(super) fn mode(self) -> &'static str {
        match self.selection {
            BudgetSelection::Balanced => "balanced",
            BudgetSelection::Full => "full",
            BudgetSelection::Explicit(_) => "legacy",
        }
    }

    /// Trigger before exhausting usable input; output and margin are already reserved.
    pub(crate) fn compaction_trigger(self) -> u64 {
        (u64::from(self.input_limit) * 90).div_ceil(100)
    }

    pub(crate) fn compaction_target(self) -> u64 {
        u64::from(self.input_limit) * 70 / 100
    }

    pub(crate) fn measure_provider_request(
        self,
        encoded: &[u8],
        request_byte_limit: usize,
    ) -> adk_rust::Result<RequestContextUsage> {
        Ok(RequestContextUsage {
            budget: self,
            estimated_input: estimate_provider_request(encoded)?,
            request_bytes: encoded.len(),
            request_byte_limit,
        })
    }

    pub(crate) fn resolve(
        limits: Option<ModelContextLimits>,
        settings: &Map<String, Value>,
        selected_output: Option<u32>,
    ) -> Result<Option<Self>, NativeAgentAssemblyError> {
        let Some(limits) = limits else {
            if settings.contains_key("budget_mode") {
                return Err(invalid_budget());
            }
            return Ok(None); // Previously admitted inputs lack catalogue limits.
        };
        let invalid = invalid_budget;
        let selection = match settings.get("budget_mode") {
            None => BudgetSelection::Balanced,
            Some(Value::String(mode)) if mode == "balanced" => BudgetSelection::Balanced,
            Some(Value::String(mode)) if mode == "full" => BudgetSelection::Full,
            Some(_) => return Err(invalid()),
        };
        // An explicit stored limit is preserved until a preset replaces it.
        let selection = match settings.get("max_context_tokens") {
            None => selection,
            Some(value) => BudgetSelection::Explicit(
                value
                    .as_u64()
                    .and_then(|value| u32::try_from(value).ok())
                    .filter(|value| *value > 0)
                    .ok_or_else(invalid)?,
            ),
        };
        Self::admit(limits, selection, selected_output).map(Some)
    }

    pub(crate) fn for_model(
        self,
        limits: ModelContextLimits,
        selected_output: Option<u32>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::admit(limits, self.selection, selected_output)
    }

    fn admit(
        limits: ModelContextLimits,
        selection: BudgetSelection,
        selected_output: Option<u32>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let invalid = invalid_budget;
        if limits.context_window_tokens == 0
            || limits.max_output_tokens == 0
            || limits.max_output_tokens >= limits.context_window_tokens
            || limits.max_input_tokens == Some(0)
        {
            return Err(invalid());
        }
        let output_reservation = selected_output.unwrap_or(limits.max_output_tokens);
        if output_reservation == 0
            || (output_reservation > limits.max_output_tokens && !limits.max_output_fallback)
        {
            return Err(invalid());
        }
        let selected_total = match selection {
            BudgetSelection::Balanced => BALANCED_TOKENS,
            BudgetSelection::Full if !limits.context_window_fallback => {
                limits.context_window_tokens
            }
            BudgetSelection::Full => return Err(invalid()),
            BudgetSelection::Explicit(tokens) => tokens,
        };
        let total_tokens = selected_total.min(limits.context_window_tokens);
        let margin_tokens = (total_tokens / 100).clamp(1_024, 8_192);
        let input_limit = total_tokens
            .checked_sub(output_reservation)
            .and_then(|value| value.checked_sub(margin_tokens))
            .filter(|value| *value > 0)
            .ok_or_else(invalid)?
            .min(limits.max_input_tokens.unwrap_or(u32::MAX));
        Ok(Self {
            total_tokens,
            output_reservation,
            margin_tokens,
            input_limit,
            limits,
            selection,
        })
    }

    /// Check the completed provider document, including static instructions,
    /// tool declarations, and protocol framing. ADK supplies the byte heuristic.
    /// This is an estimate, not a provider tokenizer or a billing measurement.
    pub(crate) fn check_provider_request(&self, encoded: &[u8]) -> adk_rust::Result<()> {
        let estimated_input = estimate_provider_request(encoded)?;
        tracing::debug!(
            estimated_input,
            output_reservation = self.output_reservation,
            margin_tokens = self.margin_tokens,
            total_tokens = self.total_tokens,
            input_limit = self.input_limit,
            context_window_fallback = self.limits.context_window_fallback,
            max_output_fallback = self.limits.max_output_fallback,
            "model request context estimate"
        );
        if estimated_input > u64::from(self.input_limit) {
            return Err(budget_error());
        }
        Ok(())
    }
}

fn estimate_provider_request(encoded: &[u8]) -> adk_rust::Result<u64> {
    let text = std::str::from_utf8(encoded).map_err(|_| budget_error())?;
    let mut event = Event::new("context-budget-estimate");
    event.set_content(Content::new("user").with_text(text));
    Ok(adk_rust::intra_compaction::estimate_tokens(&[event], 4)
        + u64::from(!encoded.len().is_multiple_of(4)))
}

fn invalid_budget() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "The model context budget is invalid.",
    )
}

fn budget_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        "context_budget_exceeded",
        "The request exceeds the available input context after reserving output capacity.",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    pub(crate) fn limits(window: u32, output: u32) -> ModelContextLimits {
        ModelContextLimits {
            context_window_tokens: window,
            max_output_tokens: output,
            context_window_fallback: false,
            max_output_fallback: false,
            max_input_tokens: None,
        }
    }

    #[test]
    fn presets_reserve_output_inside_the_combined_window() {
        for (window, output, mode, total, before_margin) in [
            (1_000_000, 128_000, "balanced", 272_000, 144_000),
            (1_000_000, 128_000, "full", 1_000_000, 872_000),
            (400_000, 64_000, "full", 400_000, 336_000),
            (128_000, 16_000, "balanced", 128_000, 112_000),
        ] {
            let settings = json!({"budget_mode": mode}).as_object().unwrap().clone();
            let budget =
                RequestContextBudget::resolve(Some(limits(window, output)), &settings, None)
                    .unwrap()
                    .unwrap();
            assert_eq!(budget.total_tokens, total);
            assert_eq!(budget.input_limit + budget.margin_tokens, before_margin);
            assert_eq!(budget.output_reservation, output);
        }
    }

    #[test]
    fn explicit_limit_smaller_output_and_input_only_limit_remain_distinct() {
        let settings = json!({"max_context_tokens": 64_000})
            .as_object()
            .unwrap()
            .clone();
        let budget =
            RequestContextBudget::resolve(Some(limits(1_000_000, 128_000)), &settings, Some(4_000))
                .unwrap()
                .unwrap();
        assert_eq!(budget.total_tokens, 64_000);
        assert_eq!(budget.input_limit + budget.margin_tokens, 60_000);
        let mut constrained = limits(1_000_000, 128_000);
        constrained.max_input_tokens = Some(32_000);
        assert_eq!(
            budget
                .for_model(constrained, Some(4_000))
                .unwrap()
                .input_limit,
            32_000
        );
    }

    #[test]
    fn child_model_recomputes_full_capacity_and_reserves_its_own_output() {
        let settings = json!({"budget_mode": "full"}).as_object().unwrap().clone();
        let parent =
            RequestContextBudget::resolve(Some(limits(1_000_000, 128_000)), &settings, None)
                .unwrap()
                .unwrap();
        let child = parent.for_model(limits(400_000, 64_000), None).unwrap();
        assert_eq!(child.total_tokens, 400_000);
        assert_eq!(child.input_limit + child.margin_tokens, 336_000);
    }

    #[test]
    fn compaction_pressure_uses_each_request_capacity_and_transport_limit() {
        let parent = RequestContextBudget::resolve(
            Some(limits(1_000_000, 128_000)),
            &Map::new(),
            Some(64_000),
        )
        .unwrap()
        .unwrap();
        let child = parent.for_model(limits(128_000, 16_000), None).unwrap();
        assert_eq!(parent.compaction_trigger(), 184_752);
        assert_eq!(parent.compaction_target(), 143_696);
        assert_eq!(child.compaction_trigger(), 99_648);
        assert_eq!(child.compaction_target(), 77_504);

        let request = RequestContextUsage {
            budget: parent,
            estimated_input: 100_000,
            request_bytes: 400_000,
            request_byte_limit: 1_048_576,
        };
        assert!(!request.needs_compaction());
        assert!(
            RequestContextUsage {
                budget: child,
                ..request
            }
            .needs_compaction()
        );
        assert!(request.fits());
        request.check().unwrap();
        let too_many_bytes = RequestContextUsage {
            request_byte_limit: 399_999,
            ..request
        };
        assert!(too_many_bytes.needs_compaction());
        assert!(!too_many_bytes.fits());
        assert_eq!(
            too_many_bytes.check().unwrap_err().code,
            "model_request_bytes_exceeded"
        );
        assert!(
            !RequestContextUsage {
                estimated_input: u64::from(parent.input_limit) + 1,
                ..request
            }
            .fits()
        );

        for capacity in 1..100 {
            let tiny = RequestContextBudget {
                input_limit: capacity,
                ..parent
            };
            assert!(tiny.compaction_trigger() > 0);
            assert!(tiny.compaction_target() < tiny.compaction_trigger());
        }
    }

    #[test]
    fn fallback_limits_never_claim_full_capacity() {
        let mut fallback = limits(128_000, 16_000);
        fallback.context_window_fallback = true;
        fallback.max_output_fallback = true;
        let full = json!({"budget_mode": "full"}).as_object().unwrap().clone();
        assert!(RequestContextBudget::resolve(Some(fallback), &full, None).is_err());
        assert!(RequestContextBudget::resolve(None, &full, None).is_err());
        // A known user cap supersedes an unknown output maximum's fallback.
        let balanced = RequestContextBudget::resolve(Some(fallback), &Map::new(), Some(32_000))
            .unwrap()
            .unwrap();
        assert_eq!(balanced.output_reservation, 32_000);
        assert!(
            RequestContextBudget::resolve(None, &Map::new(), None)
                .unwrap()
                .is_none()
        );
    }

    #[test]
    fn invalid_limits_and_exhausted_input_are_refused_without_underflow() {
        for model in [limits(0, 1), limits(16_000, 16_000), limits(1_000, 500)] {
            assert!(RequestContextBudget::resolve(Some(model), &Map::new(), None).is_err());
        }
        assert!(
            RequestContextBudget::resolve(Some(limits(128_000, 16_000)), &Map::new(), Some(32_000))
                .is_err()
        );
        for settings in [
            json!({"budget_mode":"invented"}),
            json!({"max_context_tokens":-1}),
            json!({"max_context_tokens":1.5}),
        ] {
            assert!(
                RequestContextBudget::resolve(
                    Some(limits(128_000, 16_000)),
                    settings.as_object().unwrap(),
                    None
                )
                .is_err()
            );
        }
    }

    #[test]
    fn input_boundary_includes_margin_and_does_not_accumulate_request_usage() {
        let budget = RequestContextBudget::resolve(Some(limits(8_000, 4_000)), &Map::new(), None)
            .unwrap()
            .unwrap();
        let exact = vec![b'x'; budget.input_limit as usize * 4];
        for _ in 0..10 {
            budget.check_provider_request(&exact).unwrap();
        }
        let mut over = exact;
        over.push(b'x');
        let error = budget.check_provider_request(&over).unwrap_err();
        assert_eq!(error.code, "context_budget_exceeded");
        assert!(!error.is_retryable());
    }
}
