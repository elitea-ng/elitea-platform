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
    AnalysisCodeFailure, AnalysisCodeRequest, AnalysisCodeRunner, AnalysisLanguage,
};
use ring::digest;
use serde::Deserialize;
use serde_json::Value;

use super::code::{CodeLanguage, CodeProvenance};
use super::code_runtime::{CodeInvocation, CodeSandboxRuntime};

/// The most bytes of input state one program is sent.
const MAX_INPUT_BYTES: usize = 1024 * 1024;
const MAX_RECEIPT_BYTES: usize = 512 * 1024;
const MAX_RESULT_BYTES: usize = 256 * 1024 + 64;

/// Runs analysis programs on the invocation's sandbox runtime.
pub(crate) struct SandboxAnalysisRunner {
    runtime: Arc<dyn CodeSandboxRuntime>,
}

impl SandboxAnalysisRunner {
    pub(crate) fn new(runtime: Arc<dyn CodeSandboxRuntime>) -> Self {
        Self { runtime }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Receipt {
    revision: u8,
    status: String,
    exit_code: Option<i32>,
    stdout: String,
    #[allow(dead_code)]
    stderr: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AdapterResult {
    revision: u8,
    result: Value,
}

/// The program's result, from a completed receipt. A receipt that is not
/// exactly the supervisor's, or a program that did not exit cleanly, is a
/// failure; stderr is never read into a result.
fn result_of(bytes: &[u8]) -> Result<Value, AnalysisCodeFailure> {
    if bytes.len() > MAX_RECEIPT_BYTES {
        return Err(AnalysisCodeFailure::Failed);
    }
    let receipt: Receipt =
        serde_json::from_slice(bytes).map_err(|_| AnalysisCodeFailure::Failed)?;
    if receipt.revision != 1
        || receipt.status != "completed"
        || receipt.exit_code != Some(0)
        || receipt.stdout.len() > MAX_RESULT_BYTES
    {
        return Err(AnalysisCodeFailure::Failed);
    }
    let adapter: AdapterResult =
        serde_json::from_str(&receipt.stdout).map_err(|_| AnalysisCodeFailure::Failed)?;
    if adapter.revision != 1 {
        return Err(AnalysisCodeFailure::Failed);
    }
    Ok(adapter.result)
}

/// The cause of a refused run, from the runtime's error text. The runtime
/// reports through `GraphError` strings (`graph.code.execution_failed: ...`),
/// so this is a match on its wording; an unrecognised message is `Failed`.
fn classify(message: &str) -> AnalysisCodeFailure {
    let lower = message.to_ascii_lowercase();
    if lower.contains("no sandbox runtime is configured")
        || lower.contains("multiple sandbox runtimes")
    {
        AnalysisCodeFailure::Unavailable
    } else if lower.contains("could not be confirmed") || lower.contains("reconcile") {
        AnalysisCodeFailure::Uncertain
    } else if lower.contains("cancel") {
        AnalysisCodeFailure::Cancelled
    } else if lower.contains("authoriz") || lower.contains("exceeds") || lower.contains("too large")
    {
        AnalysisCodeFailure::Rejected
    } else {
        AnalysisCodeFailure::Failed
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
    async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeFailure> {
        let input_json =
            serde_json::to_vec(&request.input).map_err(|_| AnalysisCodeFailure::Rejected)?;
        if input_json.len() > MAX_INPUT_BYTES || request.source.contains('\0') {
            return Err(AnalysisCodeFailure::Rejected);
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
        match self.runtime.execute(invocation).await {
            Ok(receipt) => result_of(&receipt),
            Err(error) => {
                let message = error.to_string();
                let failure = classify(&message);
                tracing::warn!(
                    event = "data_analysis_code_failed",
                    failure = ?failure,
                    "the sandbox did not return a result for run_analysis_code"
                );
                Err(failure)
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
        reply: Mutex<Option<Result<Vec<u8>, GraphError>>>,
    }

    #[async_trait]
    impl CodeSandboxRuntime for Fake {
        async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
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

    fn runner(reply: Result<Vec<u8>, GraphError>) -> (SandboxAnalysisRunner, Arc<Fake>) {
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
        serde_json::to_vec(&serde_json::json!({
            "revision": 1, "status": "completed", "exit_code": exit,
            "stdout": stdout, "stderr": "diagnostic"
        }))
        .unwrap()
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
        let (a, fa) = runner(Ok(receipt(r#"{"revision":1,"result":1}"#, 0)));
        let (b, fb) = runner(Ok(receipt(r#"{"revision":1,"result":1}"#, 0)));
        let (c, fc) = runner(Ok(receipt(r#"{"revision":1,"result":1}"#, 0)));
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
            vec![b'x'; MAX_RECEIPT_BYTES + 1],
        ] {
            let (runner, _) = runner(Ok(bad));
            assert_eq!(
                runner.run(request("c")).await.unwrap_err(),
                AnalysisCodeFailure::Failed
            );
        }
    }

    #[tokio::test]
    async fn runtime_errors_are_classified_by_cause() {
        for (message, expected) in [
            (
                "graph.code.execution_failed: No sandbox runtime is configured for this Code language.",
                AnalysisCodeFailure::Unavailable,
            ),
            (
                "graph.code.execution_failed: Sandbox completion could not be confirmed. The job was not restarted; reconcile its durable status before retrying the pipeline.",
                AnalysisCodeFailure::Uncertain,
            ),
            (
                "graph.code.execution_failed: The job was cancelled.",
                AnalysisCodeFailure::Cancelled,
            ),
            (
                "graph.code.execution_failed: not authorized",
                AnalysisCodeFailure::Rejected,
            ),
            (
                "graph.code.execution_failed: exit status 1",
                AnalysisCodeFailure::Failed,
            ),
        ] {
            let (runner, _) = runner(Err(GraphError::Other(message.into())));
            assert_eq!(
                runner.run(request("c")).await.unwrap_err(),
                expected,
                "{message}"
            );
        }
    }

    #[tokio::test]
    async fn oversized_input_never_reaches_the_runtime() {
        let (runner, fake) = runner(Ok(Vec::new()));
        let mut big = request("c");
        big.input = serde_json::json!({"data": "x".repeat(MAX_INPUT_BYTES)});
        assert_eq!(
            runner.run(big).await.unwrap_err(),
            AnalysisCodeFailure::Rejected
        );
        assert!(fake.seen.lock().unwrap().is_empty());
    }
}
