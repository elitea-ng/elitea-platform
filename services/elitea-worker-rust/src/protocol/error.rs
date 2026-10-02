use std::fmt;

/// Public input sections. These names never contain caller-supplied keys.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InputLimitField {
    UserMessage,
    ChatHistory,
    AgentSettings,
    ToolConfiguration,
}

impl InputLimitField {
    #[must_use]
    pub const fn safe_message(self) -> &'static str {
        match self {
            Self::UserMessage => {
                "The request cannot start because the user message exceeds a platform size limit. Shorten the message or attach the content as a file. This is not a model token limit."
            }
            Self::ChatHistory => {
                "The request cannot start because the conversation history exceeds a platform input limit before compaction can run. Start a new chat with the required context. Share the support reference if this repeats."
            }
            Self::AgentSettings => {
                "The request cannot start because the agent instructions or settings exceed a platform input limit. Reduce the saved content or ask an administrator to inspect the support reference. This is not a model token limit."
            }
            Self::ToolConfiguration => {
                "The request cannot start because the attached tool configuration exceeds a platform input limit. Reduce the attached tools or ask an administrator to inspect their schemas using the support reference."
            }
        }
    }
}

/// Stable, data-free error classification for language-neutral boundaries.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProtocolError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    InputFieldLimit {
        field: InputLimitField,
        reason: &'static str,
    },
    IncompatibleVersion(&'static str),
    AuthorizationFailed(&'static str),
    UnsupportedCapability(&'static str),
}

impl fmt::Display for ProtocolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InputFieldLimit { field, reason } => write!(formatter, "{field:?}: {reason}"),
            Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::IncompatibleVersion(message)
            | Self::AuthorizationFailed(message)
            | Self::UnsupportedCapability(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for ProtocolError {}

impl ProtocolError {
    #[must_use]
    pub const fn in_input_field(self, field: InputLimitField) -> Self {
        match self {
            Self::ResourceExhausted(reason) => Self::InputFieldLimit { field, reason },
            other => other,
        }
    }
}
