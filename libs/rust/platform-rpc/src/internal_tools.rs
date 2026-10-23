//! The client of `elitea.runtime.v1.InternalToolService`: the MCP
//! `tools/list` and `tools/call` of one internal tool category
//! (`/app/{project}/mcp/{scope}`), with the MCP shapes kept as JSON bytes.
//!
//! A JSON-RPC error the endpoint answered is in band ([`RpcError`]), as it
//! was over HTTP; an HTTP refusal of the endpoint (a bad bearer, no project
//! access, MCP disabled) is [`ToolCallError::Refused`].

use bytes::Bytes;
use tonic::transport::Channel;

use crate::pb::internal_tool_service_client::InternalToolServiceClient;
use crate::{Bearer, MAX_MESSAGE_BYTES, pb, trailer};

/// The trailer that carries a refusal (`InternalToolRefusalV1`).
pub const REFUSAL_TRAILER: &str = "elitea-internal-tool-refusal-bin";

/// The endpoint a call names: `/app/{project_id}/mcp/{scope}`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Scope {
    pub project_id: i64,
    /// The path below `/mcp/` (`applications`); empty is the bare endpoint.
    pub scope: String,
}

impl Scope {
    fn message(&self) -> pb::InternalToolScopeV1 {
        pb::InternalToolScopeV1 {
            project_id: self.project_id,
            scope: self.scope.clone(),
        }
    }
}

/// A JSON-RPC error object the endpoint answered.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RpcError {
    pub code: i32,
    pub message: String,
}

/// The endpoint's answer: the JSON-RPC `result` (as JSON bytes) or its
/// JSON-RPC error.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Outcome {
    Result(Bytes),
    Error(RpcError),
}

/// A refusal of the endpoint: its HTTP status and body.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Refusal {
    pub http_status: u16,
    pub body: Bytes,
}

/// Why a call produced no [`Outcome`].
#[derive(Debug)]
pub enum ToolCallError {
    Refused(Refusal),
    /// The hop failed (an unadmitted peer, the listener unreachable, a
    /// cancelled or broken call). Never a refusal.
    Hop(tonic::Status),
    /// The service broke its own protocol.
    Protocol(&'static str),
}

impl ToolCallError {
    fn from_status(status: tonic::Status) -> Self {
        match refusal(&status) {
            Some(refusal) => Self::Refused(refusal),
            None => Self::Hop(status),
        }
    }
}

impl std::fmt::Display for ToolCallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Refused(refusal) => write!(
                f,
                "the internal tool call was refused (HTTP {})",
                refusal.http_status
            ),
            Self::Hop(status) => write!(
                f,
                "the internal tool service call failed: {:?}",
                status.code()
            ),
            Self::Protocol(reason) => f.write_str(reason),
        }
    }
}

impl std::error::Error for ToolCallError {}

/// The refusal a status carries.
#[must_use]
pub fn refusal(status: &tonic::Status) -> Option<Refusal> {
    let refusal: pb::InternalToolRefusalV1 = trailer(status, REFUSAL_TRAILER)?;
    let http_status = u16::try_from(refusal.http_status)
        .ok()
        .filter(|code| !(200..=299).contains(code) && *code >= 100)?;
    Some(Refusal {
        http_status,
        body: refusal.body,
    })
}

fn rpc_error(error: pb::InternalToolRpcErrorV1) -> RpcError {
    RpcError {
        code: error.code,
        message: error.message,
    }
}

/// One running `tools/call`: progress frames, then its outcome.
pub struct CallStream {
    stream: tonic::Streaming<pb::InternalToolCallFrameV1>,
}

/// One frame of a running call.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CallFrame {
    /// The call is running (`admitted`, then `running` keepalives).
    Progress(String),
    /// The call's outcome; nothing follows it.
    Done(Outcome),
}

impl CallStream {
    /// The next frame, `None` when the stream ended (which, before an
    /// outcome, is a protocol break the caller reports).
    ///
    /// # Errors
    ///
    /// A refusal or the stream breaking.
    pub async fn next(&mut self) -> Result<Option<CallFrame>, ToolCallError> {
        use pb::internal_tool_call_frame_v1::Frame;
        loop {
            let Some(message) = self
                .stream
                .message()
                .await
                .map_err(ToolCallError::from_status)?
            else {
                return Ok(None);
            };
            return Ok(Some(match message.frame {
                Some(Frame::Progress(progress)) => CallFrame::Progress(progress.stage),
                Some(Frame::ResultJson(result)) => CallFrame::Done(Outcome::Result(result)),
                Some(Frame::Error(error)) => CallFrame::Done(Outcome::Error(rpc_error(error))),
                None => continue,
            }));
        }
    }

    /// Skip the progress and return the outcome.
    ///
    /// # Errors
    ///
    /// A refusal, the stream breaking, or a stream that ends without one.
    pub async fn outcome(mut self) -> Result<Outcome, ToolCallError> {
        while let Some(frame) = self.next().await? {
            if let CallFrame::Done(outcome) = frame {
                return Ok(outcome);
            }
        }
        Err(ToolCallError::Protocol(
            "the internal tool call ended without a result",
        ))
    }
}

/// The service client over a caller-built channel. Cheap to clone.
#[derive(Clone, Debug)]
pub struct InternalToolClient {
    inner: InternalToolServiceClient<Channel>,
}

impl InternalToolClient {
    /// The client over `channel` (the platform listener's).
    #[must_use]
    pub fn new(channel: Channel) -> Self {
        Self {
            inner: InternalToolServiceClient::new(channel)
                .max_decoding_message_size(MAX_MESSAGE_BYTES)
                .max_encoding_message_size(MAX_MESSAGE_BYTES),
        }
    }

    /// `tools/list`: the result is `{"tools": [...]}`.
    ///
    /// # Errors
    ///
    /// A refusal, the hop failing, or an answer with neither outcome.
    pub async fn list_tools(
        &self,
        bearer: &Bearer,
        scope: &Scope,
    ) -> Result<Outcome, ToolCallError> {
        use pb::internal_tool_list_response_v1::Outcome as O;
        let request = bearer.request(pb::InternalToolListRequestV1 {
            scope: Some(scope.message()),
        });
        let answer = self
            .inner
            .clone()
            .list_tools(request)
            .await
            .map_err(ToolCallError::from_status)?
            .into_inner();
        match answer.outcome {
            Some(O::ResultJson(result)) => Ok(Outcome::Result(result)),
            Some(O::Error(error)) => Ok(Outcome::Error(rpc_error(error))),
            None => Err(ToolCallError::Protocol("the tool listing has no outcome")),
        }
    }

    /// `tools/call` of `name` with `arguments` (a JSON object; empty is
    /// `{}`). Dropping the stream cancels the call.
    ///
    /// # Errors
    ///
    /// A refusal or the hop failing before the stream opened.
    pub async fn call_tool(
        &self,
        bearer: &Bearer,
        scope: &Scope,
        name: &str,
        arguments: Bytes,
    ) -> Result<CallStream, ToolCallError> {
        let request = bearer.request(pb::InternalToolCallRequestV1 {
            scope: Some(scope.message()),
            name: name.to_owned(),
            arguments_json: arguments,
        });
        let stream = self
            .inner
            .clone()
            .call_tool(request)
            .await
            .map_err(ToolCallError::from_status)?
            .into_inner();
        Ok(CallStream { stream })
    }

    /// Run one tool of a saved toolkit (the Inventory engine's source
    /// calls): progress, then the route's JSON answer. A non-2xx answer is
    /// [`ToolCallError::Refused`], its body the route's `{"ok": false,
    /// "error"}`. Dropping the call cancels it.
    ///
    /// # Errors
    ///
    /// A refusal, the hop failing, or a stream that ends without a result.
    pub async fn invoke_toolkit(
        &self,
        bearer: &Bearer,
        invoke: InvokeToolkit,
    ) -> Result<Bytes, ToolCallError> {
        use pb::internal_tool_invoke_frame_v1::Frame;
        let request = bearer.request(pb::InternalToolInvokeRequestV1 {
            project_id: invoke.project_id,
            toolkit_id: invoke.toolkit_id,
            tool_name: invoke.tool_name,
            tool_params_json: invoke.tool_params,
            request_id: invoke.request_id,
        });
        let mut stream = self
            .inner
            .clone()
            .invoke_toolkit(request)
            .await
            .map_err(ToolCallError::from_status)?
            .into_inner();
        while let Some(frame) = stream.message().await.map_err(ToolCallError::from_status)? {
            if let Some(Frame::ResultJson(result)) = frame.frame {
                return Ok(result);
            }
        }
        Err(ToolCallError::Protocol(
            "the toolkit invocation ended without a result",
        ))
    }
}

/// One saved-toolkit tool run ([`InternalToolClient::invoke_toolkit`]).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct InvokeToolkit {
    pub project_id: i64,
    pub toolkit_id: String,
    pub tool_name: String,
    /// The JSON object of the tool's arguments; empty is `{}`.
    pub tool_params: Bytes,
    pub request_id: String,
}
