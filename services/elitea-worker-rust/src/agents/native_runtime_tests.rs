use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use async_trait::async_trait;
use serde_json::{Value, json};

use super::assembly_tests::ordinary_request;
use super::events::CompletedAgentBrowserOutput;
use super::native_runtime::{NativeRuntimeAssembler, NativeRuntimeCompletion, NativeRuntimeKind};
use super::request::{AgentExecutionKind, AgentExecutionRequest};
use super::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, NativeAgentCompletionSelector,
};
use super::session::AuthorizedNativeCommandBinding;
use crate::protocol::control::test_runtime_context_authority;

fn pipeline_request() -> AgentExecutionRequest {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert("agent_type".to_owned(), json!("pipeline"));
    request
}

fn authorized(request: &AgentExecutionRequest) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::new(
        request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
}

#[test]
fn frozen_request_selects_one_native_runtime_without_fallback() {
    let application = ordinary_request(AgentExecutionKind::Application);
    assert_eq!(
        NativeRuntimeKind::from_request(&application).expect("direct application"),
        NativeRuntimeKind::Direct
    );

    let adhoc = ordinary_request(AgentExecutionKind::Adhoc);
    assert_eq!(
        NativeRuntimeKind::from_request(&adhoc).expect("direct ad-hoc"),
        NativeRuntimeKind::Direct
    );

    let pipeline = pipeline_request();
    assert_eq!(
        NativeRuntimeKind::from_request(&pipeline).expect("stored pipeline"),
        NativeRuntimeKind::Pipeline
    );

    let mut defaulted = ordinary_request(AgentExecutionKind::Application);
    defaulted
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .remove("agent_type");
    assert_eq!(
        NativeRuntimeKind::from_request(&defaulted).expect("SDK-compatible direct default"),
        NativeRuntimeKind::Direct
    );
}

#[test]
fn malformed_or_unknown_runtime_kind_fails_closed() {
    let mut unknown = ordinary_request(AgentExecutionKind::Application);
    unknown
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert("agent_type".to_owned(), json!("swarm"));
    assert_eq!(
        NativeRuntimeKind::from_request(&unknown)
            .expect_err("unknown kind must fail")
            .code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );

    let mut malformed = ordinary_request(AgentExecutionKind::Application);
    malformed
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert("agent_type".to_owned(), json!(7));
    assert_eq!(
        NativeRuntimeKind::from_request(&malformed)
            .expect_err("non-string kind must fail")
            .code(),
        NativeAgentAssemblyErrorCode::InvalidInput
    );

    let mut missing_version = ordinary_request(AgentExecutionKind::Application);
    missing_version
        .payload
        .application
        .remove("version_details");
    assert_eq!(
        NativeRuntimeKind::from_request(&missing_version)
            .expect_err("missing version must fail")
            .code(),
        NativeAgentAssemblyErrorCode::InvalidInput
    );
}

struct NeverCompletion;

#[async_trait]
impl NativeAgentCompletionSelector for NeverCompletion {
    async fn select_static_pause(
        self,
    ) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        self.select().await
    }

    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        Err(NativeAgentAssemblyError::new(
            NativeAgentAssemblyErrorCode::InvalidResult,
            "test completion is unavailable",
        ))
    }
}

struct RoutedFailureAssembler {
    calls: Arc<AtomicUsize>,
    code: NativeAgentAssemblyErrorCode,
}

#[async_trait]
impl NativeAgentAssembler for RoutedFailureAssembler {
    type Completion = NeverCompletion;

    async fn assemble(
        &self,
        _assembly: AuthorizedNativeAssembly<'_>,
    ) -> Result<AssembledNativeAgentInvocation<Self::Completion>, NativeAgentAssemblyError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Err(NativeAgentAssemblyError::new(
            self.code,
            "test assembler selected",
        ))
    }
}

fn router_fixture() -> (
    NativeRuntimeAssembler<RoutedFailureAssembler, RoutedFailureAssembler>,
    Arc<AtomicUsize>,
    Arc<AtomicUsize>,
) {
    let direct_calls = Arc::new(AtomicUsize::new(0));
    let pipeline_calls = Arc::new(AtomicUsize::new(0));
    (
        NativeRuntimeAssembler::new(
            RoutedFailureAssembler {
                calls: Arc::clone(&direct_calls),
                code: NativeAgentAssemblyErrorCode::AuthorizationFailed,
            },
            RoutedFailureAssembler {
                calls: Arc::clone(&pipeline_calls),
                code: NativeAgentAssemblyErrorCode::DependencyUnavailable,
            },
        ),
        direct_calls,
        pipeline_calls,
    )
}

#[tokio::test]
async fn router_dispatches_exactly_one_concrete_assembler() {
    let direct = ordinary_request(AgentExecutionKind::Adhoc);
    let (router, direct_calls, pipeline_calls) = router_fixture();
    let direct_error = router
        .assemble(authorized(&direct))
        .await
        .err()
        .expect("direct fixture fails after routing");
    assert_eq!(
        direct_error.code(),
        NativeAgentAssemblyErrorCode::AuthorizationFailed
    );
    assert_eq!(direct_calls.load(Ordering::SeqCst), 1);
    assert_eq!(pipeline_calls.load(Ordering::SeqCst), 0);

    let pipeline = pipeline_request();
    let (router, direct_calls, pipeline_calls) = router_fixture();
    let pipeline_error = router
        .assemble(authorized(&pipeline))
        .await
        .err()
        .expect("pipeline fixture fails after routing");
    assert_eq!(
        pipeline_error.code(),
        NativeAgentAssemblyErrorCode::DependencyUnavailable
    );
    assert_eq!(direct_calls.load(Ordering::SeqCst), 0);
    assert_eq!(pipeline_calls.load(Ordering::SeqCst), 1);
}

struct FailedCompletion(NativeAgentAssemblyErrorCode);

#[async_trait]
impl NativeAgentCompletionSelector for FailedCompletion {
    async fn select_static_pause(
        self,
    ) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        self.select().await
    }

    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        Err(NativeAgentAssemblyError::new(
            self.0,
            "test terminal selector selected",
        ))
    }
}

#[tokio::test]
async fn unified_completion_preserves_the_selected_terminal_owner() {
    let direct = NativeRuntimeCompletion::<FailedCompletion, FailedCompletion>::Direct(
        FailedCompletion(NativeAgentAssemblyErrorCode::InvalidResult),
    );
    let Err(direct_error) = direct.select().await else {
        panic!("direct selector fixture unexpectedly succeeded");
    };
    assert_eq!(
        direct_error.code(),
        NativeAgentAssemblyErrorCode::InvalidResult
    );

    let pipeline = NativeRuntimeCompletion::<FailedCompletion, FailedCompletion>::Pipeline(
        FailedCompletion(NativeAgentAssemblyErrorCode::ResourceExhausted),
    );
    let Err(pipeline_error) = pipeline.select().await else {
        panic!("pipeline selector fixture unexpectedly succeeded");
    };
    assert_eq!(
        pipeline_error.code(),
        NativeAgentAssemblyErrorCode::ResourceExhausted
    );
}
