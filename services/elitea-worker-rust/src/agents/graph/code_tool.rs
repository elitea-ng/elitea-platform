//! The sandbox as a tool's code runner (ADR-0033 decision 5).
//!
//! `data_analysis`'s `run_analysis_code` hands the platform's code sandbox a
//! program and a bounded slice of rows, and gets the program's JSON result
//! back. It is the same runtime, authority, journal and receipt as a `code`
//! pipeline node: this adapter only names the activation and reads the
//! receipt. Nothing is evaluated in this process.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    AnalysisCodeError, AnalysisCodeFailure, AnalysisCodeRequest, AnalysisCodeRunner,
    AnalysisLanguage, MAX_ANALYSIS_RESULT_BYTES,
};
use ring::digest;
use serde_json::Value;

use super::code::{CodeLanguage, CodeProvenance};
use super::code_result::{ReceiptError, parse_receipt_detailed};
use super::code_runtime::{CodeInvocation, CodeSandboxRuntime, ToolCodeError};

/// The most bytes of input state one program is sent.
const MAX_INPUT_BYTES: usize = 1024 * 1024;

/// Runs analysis programs on the invocation's sandbox runtime.
pub(crate) struct SandboxAnalysisRunner {
    runtime: Arc<dyn CodeSandboxRuntime>,
}

impl SandboxAnalysisRunner {
    pub(crate) fn new(runtime: Arc<dyn CodeSandboxRuntime>) -> Self {
        Self { runtime }
    }
}

/// The most characters of the program's stderr that ride in an answer.
const MAX_DETAIL_CHARS: usize = 1_500;

/// The program's result from a receipt, by the same reader the Code node
/// uses. The run's status is judged before any size: a program that failed
/// and wrote a great deal to stderr failed; it did not return too large a
/// result. This is the one place the result limit is checked.
fn result_of(bytes: &[u8]) -> Result<Value, AnalysisCodeError> {
    match parse_receipt_detailed(bytes) {
        Ok(result) => {
            let size = serde_json::to_vec(&result).map_or(usize::MAX, |encoded| encoded.len());
            if size > MAX_ANALYSIS_RESULT_BYTES {
                return Err(AnalysisCodeFailure::ResultTooLarge.into());
            }
            Ok(result)
        }
        Err(ReceiptError::TooLarge) => Err(AnalysisCodeFailure::ResultTooLarge.into()),
        Err(ReceiptError::Malformed) => Err(AnalysisCodeFailure::Failed.into()),
        Err(ReceiptError::ProgramFailed { stderr }) => {
            let mut detail: String = stderr.chars().take(MAX_DETAIL_CHARS).collect();
            if stderr.chars().count() > MAX_DETAIL_CHARS {
                detail.push_str(" [stderr truncated]");
            }
            Err(AnalysisCodeError {
                failure: AnalysisCodeFailure::Failed,
                detail: (!detail.trim().is_empty()).then_some(detail),
            })
        }
    }
}

/// The cause of a run that produced no receipt, from what the runtime and the
/// supervisor said as values: the supervisor's failure code, or which call
/// refused. No message text is read.
fn classify(error: &ToolCodeError) -> AnalysisCodeFailure {
    match error {
        ToolCodeError::Unavailable => AnalysisCodeFailure::Unavailable,
        ToolCodeError::Cancelled => AnalysisCodeFailure::Cancelled,
        ToolCodeError::Uncertain => AnalysisCodeFailure::Uncertain,
        ToolCodeError::Refused => AnalysisCodeFailure::Rejected,
        ToolCodeError::Sandbox { code } => match code.as_str() {
            "sandbox.output_limit" | "sandbox.receipt_limit" => AnalysisCodeFailure::ResultTooLarge,
            "sandbox.invalid_request" => AnalysisCodeFailure::Rejected,
            "sandbox.deadline_exceeded"
            | "sandbox.hydration_deadline_exceeded"
            | "sandbox.memory_limit"
            | "sandbox.step_limit" => AnalysisCodeFailure::Overrun,
            "sandbox.cancelled" => AnalysisCodeFailure::Cancelled,
            _ => AnalysisCodeFailure::Failed,
        },
        ToolCodeError::Submission | ToolCodeError::Untyped(_) => AnalysisCodeFailure::Failed,
    }
}

/// Stable per tool call: a replay of the same call with the same program and
/// data names the same activation, so the supervisor reconciles it instead of
/// running it twice.
fn activation(call_id: &str, source: &str, input: &[u8]) -> [u8; 32] {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.data_analysis.code.activation.v1\0");
    for field in [call_id.as_bytes(), source.as_bytes(), input] {
        hash.update(&(field.len() as u64).to_be_bytes());
        hash.update(field);
    }
    let mut out = [0; 32];
    out.copy_from_slice(hash.finish().as_ref());
    out
}

#[async_trait]
impl AnalysisCodeRunner for SandboxAnalysisRunner {
    async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeError> {
        let input_json = serde_json::to_vec(&request.input)
            .map_err(|_| AnalysisCodeError::from(AnalysisCodeFailure::Rejected))?;
        if input_json.len() > MAX_INPUT_BYTES || request.source.contains('\0') {
            return Err(AnalysisCodeFailure::Rejected.into());
        }
        let language = match request.language {
            AnalysisLanguage::Python => CodeLanguage::Python,
            AnalysisLanguage::JavaScript => CodeLanguage::JavaScript,
        };
        let activation = activation(&request.call_id, &request.source, &input_json);
        let invocation = CodeInvocation {
            activation,
            language,
            platform_client: false,
            source: &request.source,
            dependencies_toml: None,
            // Model-authored source, supplied by the call itself.
            provenance: CodeProvenance::StateVariable,
            input_json,
            trace: None,
            original: None,
            debug: None,
            workspace: None,
        };
        match self.runtime.execute_tool(invocation).await {
            Ok(receipt) => result_of(&receipt),
            Err(error) => {
                let failure = classify(&error);
                tracing::warn!(
                    event = "data_analysis_code_failed",
                    failure = ?failure,
                    "the sandbox did not return a result for run_analysis_code"
                );
                Err(failure.into())
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use adk_rust::graph::GraphError;

    use super::*;

    /// What the runtime was asked: activation, language, source, input.
    type Seen = ([u8; 32], &'static str, String, Vec<u8>);

    struct Fake {
        seen: Mutex<Vec<Seen>>,
        reply: Mutex<Option<Result<Vec<u8>, ToolCodeError>>>,
    }

    #[async_trait]
    impl CodeSandboxRuntime for Fake {
        async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
            unreachable!("the tool path asks for typed failures")
        }

        async fn execute_tool(
            &self,
            invocation: CodeInvocation<'_>,
        ) -> Result<Vec<u8>, ToolCodeError> {
            self.seen.lock().unwrap().push((
                invocation.activation,
                match invocation.language {
                    CodeLanguage::Python => "python",
                    CodeLanguage::JavaScript => "javascript",
                    _ => "other",
                },
                invocation.source.to_owned(),
                invocation.input_json.clone(),
            ));
            self.reply.lock().unwrap().take().expect("reply")
        }
    }

    fn runner(reply: Result<Vec<u8>, ToolCodeError>) -> (SandboxAnalysisRunner, Arc<Fake>) {
        let fake = Arc::new(Fake {
            seen: Mutex::new(Vec::new()),
            reply: Mutex::new(Some(reply)),
        });
        (
            SandboxAnalysisRunner::new(Arc::clone(&fake) as Arc<dyn CodeSandboxRuntime>),
            fake,
        )
    }

    fn request(call_id: &str) -> AnalysisCodeRequest {
        AnalysisCodeRequest {
            call_id: call_id.into(),
            language: AnalysisLanguage::Python,
            source: "elitea_state['data']['rows'][0]".into(),
            input: serde_json::json!({"data": {"rows": [[1]]}}),
        }
    }

    fn receipt(stdout: &str, exit: i32) -> Vec<u8> {
        receipt_with(stdout, exit, "diagnostic", "completed")
    }

    fn receipt_with(stdout: &str, exit: i32, stderr: &str, status: &str) -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "revision": 1, "status": status, "exit_code": exit,
            "stdout": stdout, "stderr": stderr
        }))
        .unwrap()
    }

    async fn failure_of(reply: Result<Vec<u8>, ToolCodeError>) -> AnalysisCodeError {
        let (runner, _) = runner(reply);
        runner.run(request("c")).await.unwrap_err()
    }

    #[tokio::test]
    async fn a_completed_receipt_yields_the_program_result() {
        let (runner, fake) = runner(Ok(receipt(r#"{"revision":1,"result":{"n":3}}"#, 0)));
        let result = runner.run(request("c1")).await.unwrap();
        assert_eq!(result, serde_json::json!({"n": 3}));
        let seen = fake.seen.lock().unwrap();
        assert_eq!(seen[0].1, "python");
        assert_eq!(seen[0].2, "elitea_state['data']['rows'][0]");
        assert_eq!(
            serde_json::from_slice::<Value>(&seen[0].3).unwrap(),
            serde_json::json!({"data": {"rows": [[1]]}})
        );
    }

    #[tokio::test]
    async fn the_activation_is_stable_per_call_and_differs_across_calls() {
        let ok = || Ok(receipt(r#"{"revision":1,"result":1}"#, 0));
        let (a, fa) = runner(ok());
        let (b, fb) = runner(ok());
        let (c, fc) = runner(ok());
        a.run(request("same")).await.unwrap();
        b.run(request("same")).await.unwrap();
        c.run(request("other")).await.unwrap();
        let get = |f: &Arc<Fake>| f.seen.lock().unwrap()[0].0;
        assert_eq!(get(&fa), get(&fb));
        assert_ne!(get(&fa), get(&fc));
    }

    #[tokio::test]
    async fn receipts_that_are_not_clean_completions_fail() {
        for bad in [
            receipt(r#"{"revision":1,"result":1}"#, 1),
            receipt("not json", 0),
            receipt(r#"{"revision":2,"result":1}"#, 0),
            b"{}".to_vec(),
        ] {
            assert_eq!(
                failure_of(Ok(bad)).await.failure,
                AnalysisCodeFailure::Failed
            );
        }
    }

    #[tokio::test]
    async fn a_failed_run_with_huge_stderr_is_a_failure_with_bounded_stderr() {
        // Status is read before size: 600 KB of stderr from a failed run is
        // "the program failed", never "result too large".
        let big = receipt_with("", 1, &"E".repeat(600 * 1024), "completed");
        let error = failure_of(Ok(big)).await;
        assert_eq!(error.failure, AnalysisCodeFailure::Failed);
        let detail = error.detail.expect("stderr rides along");
        assert!(detail.ends_with("[stderr truncated]"));
        assert!(detail.len() < 2_000);
        let killed = receipt_with("", 137, "boom", "failed");
        let error = failure_of(Ok(killed)).await;
        assert_eq!(error.failure, AnalysisCodeFailure::Failed);
        assert_eq!(error.detail.as_deref(), Some("boom"));
    }

    #[tokio::test]
    async fn an_oversized_receipt_is_a_size_refusal_not_a_failure() {
        let huge = format!(
            "{{\"revision\":1,\"result\":\"{}\"}}",
            "x".repeat(300 * 1024)
        );
        let big = receipt_with(&huge, 0, "", "completed");
        assert_eq!(
            failure_of(Ok(big)).await.failure,
            AnalysisCodeFailure::ResultTooLarge
        );
        assert_eq!(
            failure_of(Ok(vec![b'x'; 512 * 1024 + 1])).await.failure,
            AnalysisCodeFailure::ResultTooLarge
        );
    }

    #[tokio::test]
    async fn a_result_over_the_tool_cap_is_refused_here() {
        let big =
            serde_json::json!({"revision": 1, "result": "z".repeat(MAX_ANALYSIS_RESULT_BYTES + 1)});
        assert_eq!(
            failure_of(Ok(receipt(&big.to_string(), 0))).await.failure,
            AnalysisCodeFailure::ResultTooLarge
        );
    }

    #[tokio::test]
    async fn typed_runtime_failures_map_by_code_without_reading_text() {
        let sandbox = |code: &str| ToolCodeError::Sandbox { code: code.into() };
        for (error, expected) in [
            (ToolCodeError::Unavailable, AnalysisCodeFailure::Unavailable),
            (ToolCodeError::Cancelled, AnalysisCodeFailure::Cancelled),
            (ToolCodeError::Uncertain, AnalysisCodeFailure::Uncertain),
            (ToolCodeError::Refused, AnalysisCodeFailure::Rejected),
            (ToolCodeError::Submission, AnalysisCodeFailure::Failed),
            (
                ToolCodeError::Untyped("memory time exceeds cancel authorized".into()),
                AnalysisCodeFailure::Failed,
            ),
            (
                sandbox("sandbox.output_limit"),
                AnalysisCodeFailure::ResultTooLarge,
            ),
            (
                sandbox("sandbox.receipt_limit"),
                AnalysisCodeFailure::ResultTooLarge,
            ),
            (
                sandbox("sandbox.invalid_request"),
                AnalysisCodeFailure::Rejected,
            ),
            (
                sandbox("sandbox.deadline_exceeded"),
                AnalysisCodeFailure::Overrun,
            ),
            (
                sandbox("sandbox.hydration_deadline_exceeded"),
                AnalysisCodeFailure::Overrun,
            ),
            (
                sandbox("sandbox.memory_limit"),
                AnalysisCodeFailure::Overrun,
            ),
            (sandbox("sandbox.cancelled"), AnalysisCodeFailure::Cancelled),
            (sandbox("sandbox.code_failed"), AnalysisCodeFailure::Failed),
            (
                sandbox("sandbox.launch_failed"),
                AnalysisCodeFailure::Failed,
            ),
        ] {
            assert_eq!(
                failure_of(Err(error.clone())).await.failure,
                expected,
                "{error:?}"
            );
        }
    }

    #[tokio::test]
    async fn the_default_tool_path_reports_untyped_text_as_a_plain_failure() {
        struct Plain;
        #[async_trait]
        impl CodeSandboxRuntime for Plain {
            async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
                Err(GraphError::Other(
                    "graph.code.execution_failed: not authorized, memory, cancel".into(),
                ))
            }
        }
        let runner = SandboxAnalysisRunner::new(Arc::new(Plain));
        // The words in the message decide nothing.
        assert_eq!(
            runner.run(request("c")).await.unwrap_err().failure,
            AnalysisCodeFailure::Failed
        );
    }

    #[tokio::test]
    async fn oversized_input_never_reaches_the_runtime() {
        let (runner, fake) = runner(Ok(Vec::new()));
        let mut big = request("c");
        big.input = serde_json::json!({"data": "x".repeat(MAX_INPUT_BYTES)});
        assert_eq!(
            runner.run(big).await.unwrap_err().failure,
            AnalysisCodeFailure::Rejected
        );
        assert!(fake.seen.lock().unwrap().is_empty());
    }
}
