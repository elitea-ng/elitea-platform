//! The sentence a model-gateway refusal becomes in an index's failure message
//! (#2042). The code stays in the text, in brackets, for support; the words
//! before it say what happened and what to do, because the message is what the
//! user reads on the index and in its History.

/// What one gateway fault code means for the person who started the run. A
/// code this build does not know gets a neutral sentence rather than none.
pub(super) fn gateway_refusal_hint(code: &str) -> &'static str {
    match code {
        "model_gateway.forbidden" => {
            "this project is not allowed to use that model (access denied by the provider or blocked by the LLM governance policy); choose another model in the toolkit settings or ask an administrator to allow it"
        }
        "model_gateway.unauthorized" => {
            "the model's credential was rejected; check the credential of the model in AI configuration"
        }
        "model_gateway.budget_exhausted"
        | "model_gateway.member_budget_exhausted"
        | "model_gateway.project_budget_exhausted" => {
            "the budget for model calls is used up; ask an administrator to raise it"
        }
        "model_gateway.rejected" => {
            "the model does not exist for this project or did not accept the request; check the model chosen in the toolkit settings"
        }
        _ => "the gateway did not accept the request",
    }
}

/// `"the {subject} refused the run ({code}): {hint}; nothing further was {verb}"`.
pub(super) fn refused_run(subject: &str, code: &str, verb: &str) -> String {
    format!(
        "the {subject} refused the run ({code}): {}; nothing further was {verb}",
        gateway_refusal_hint(code)
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn forbidden_reads_as_a_sentence_and_keeps_the_code() {
        let message = refused_run("embedding gateway", "model_gateway.forbidden", "embedded");
        assert!(
            message
                .starts_with("the embedding gateway refused the run (model_gateway.forbidden): ")
        );
        assert!(message.contains("not allowed to use that model"));
        assert!(message.ends_with("nothing further was embedded"));
    }

    #[test]
    fn every_non_retryable_code_has_its_own_sentence() {
        let known = [
            "model_gateway.forbidden",
            "model_gateway.unauthorized",
            "model_gateway.budget_exhausted",
            "model_gateway.rejected",
        ];
        let hints: std::collections::BTreeSet<_> = known
            .iter()
            .map(|code| gateway_refusal_hint(code))
            .collect();
        assert_eq!(hints.len(), known.len());
        assert_eq!(
            gateway_refusal_hint("model_gateway.member_budget_exhausted"),
            gateway_refusal_hint("model_gateway.budget_exhausted")
        );
        assert_eq!(
            gateway_refusal_hint("model_gateway.something_new"),
            "the gateway did not accept the request"
        );
    }
}
