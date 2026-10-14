pub(crate) mod anthropic_facade;
#[cfg(test)]
mod anthropic_facade_tests;
pub mod command_bus;
pub mod control_grpc;
pub(crate) mod embeddings_facade;
pub(crate) mod extractor_client;
pub(crate) mod gateway_call;
pub(crate) mod images_facade;
#[cfg(test)]
mod images_host_tests;
pub mod input_content;
#[cfg(test)]
mod input_content_tests;
pub(crate) mod keepalive;
pub(crate) mod model_facade;
pub(crate) mod multipart;
pub mod nats_jetstream;
pub(crate) mod office_host;
#[cfg(test)]
pub(crate) mod office_host_tests;
pub(crate) mod openai_compatible_facade;
#[cfg(test)]
mod openai_compatible_facade_tests;
pub mod output_grpc;
mod output_session;
pub(crate) mod platform_client;
pub(crate) mod platform_writer;
pub(crate) mod runtime_context;
#[cfg(test)]
mod runtime_context_tests;
mod summary_model;
#[cfg(test)]
mod summary_selection_tests;

pub use control_grpc::{
    ControlGrpcClient, ControlGrpcConfig, ControlGrpcError, ControlRpc, TonicControlRpc,
};
pub use input_content::{
    InputContentClient, InputContentError, InputContentTransportError, MaterializedInput,
};
pub use output_grpc::{
    DurablyAckedProgress, DurablyAckedTerminal, OutputGrpcConfig, OutputGrpcError,
    OutputGrpcSession, PreparedOutputSpool,
};
pub use output_session::OutputSessionError as OutputProtocolError;
