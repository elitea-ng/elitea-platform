//! The routes of the `/llm` edge, relative to the platform origin.
//!
//! The model client is configured with a base that already ends in
//! [`LLM_BASE_PATH`] (`llm_settings.api_base`) and appends the `*_PATH`
//! suffix; the worker's channel is bound to the bare origin and sends the
//! whole `*_ROUTE`.

/// The OpenAI-compatible surface of the edge.
pub const LLM_BASE_PATH: &str = "/llm/v1";

/// Chat completions, under [`LLM_BASE_PATH`].
pub const CHAT_COMPLETIONS_PATH: &str = "/chat/completions";

/// Embeddings, under [`LLM_BASE_PATH`].
pub const EMBEDDINGS_PATH: &str = "/embeddings";

/// The native Anthropic messages surface, under [`LLM_BASE_PATH`].
pub const MESSAGES_PATH: &str = "/messages";

/// Image generation (JSON), under [`LLM_BASE_PATH`].
pub const IMAGES_GENERATIONS_PATH: &str = "/images/generations";

/// Image edits (multipart), under [`LLM_BASE_PATH`].
pub const IMAGES_EDITS_PATH: &str = "/images/edits";

/// [`LLM_BASE_PATH`] + [`CHAT_COMPLETIONS_PATH`].
pub const CHAT_COMPLETIONS_ROUTE: &str = "/llm/v1/chat/completions";

/// [`LLM_BASE_PATH`] + [`EMBEDDINGS_PATH`].
pub const EMBEDDINGS_ROUTE: &str = "/llm/v1/embeddings";

/// [`LLM_BASE_PATH`] + [`MESSAGES_PATH`].
pub const MESSAGES_ROUTE: &str = "/llm/v1/messages";

/// [`LLM_BASE_PATH`] + [`IMAGES_GENERATIONS_PATH`].
pub const IMAGES_GENERATIONS_ROUTE: &str = "/llm/v1/images/generations";

/// [`LLM_BASE_PATH`] + [`IMAGES_EDITS_PATH`].
pub const IMAGES_EDITS_ROUTE: &str = "/llm/v1/images/edits";

/// A metered tool operation's admission (#1238), under [`LLM_BASE_PATH`].
pub const METERED_ADMIT_PATH: &str = "/metered/admit";

/// A metered tool operation's settlement, under [`LLM_BASE_PATH`].
pub const METERED_SETTLE_PATH: &str = "/metered/settle";

/// [`LLM_BASE_PATH`] + [`METERED_ADMIT_PATH`].
pub const METERED_ADMIT_ROUTE: &str = "/llm/v1/metered/admit";

/// [`LLM_BASE_PATH`] + [`METERED_SETTLE_PATH`].
pub const METERED_SETTLE_ROUTE: &str = "/llm/v1/metered/settle";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn each_route_is_the_base_and_its_path() {
        assert_eq!(
            CHAT_COMPLETIONS_ROUTE,
            format!("{LLM_BASE_PATH}{CHAT_COMPLETIONS_PATH}")
        );
        assert_eq!(
            EMBEDDINGS_ROUTE,
            format!("{LLM_BASE_PATH}{EMBEDDINGS_PATH}")
        );
        assert_eq!(MESSAGES_ROUTE, format!("{LLM_BASE_PATH}{MESSAGES_PATH}"));
        assert_eq!(
            IMAGES_GENERATIONS_ROUTE,
            format!("{LLM_BASE_PATH}{IMAGES_GENERATIONS_PATH}")
        );
        assert_eq!(
            IMAGES_EDITS_ROUTE,
            format!("{LLM_BASE_PATH}{IMAGES_EDITS_PATH}")
        );
        assert_eq!(
            METERED_ADMIT_ROUTE,
            format!("{LLM_BASE_PATH}{METERED_ADMIT_PATH}")
        );
        assert_eq!(
            METERED_SETTLE_ROUTE,
            format!("{LLM_BASE_PATH}{METERED_SETTLE_PATH}")
        );
    }
}
