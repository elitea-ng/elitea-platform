//! Free-form analysis code, run in the platform's code sandbox and nowhere
//! else (ADR-0033 decision 5).
//!
//! The SDK's generated pandas ran in the worker process, behind an AST
//! allow-list. Here the worker never evaluates code: `run_analysis_code`
//! hands the sandbox the source and a bounded slice of rows and gets JSON
//! back. The sandbox is the same one `code` pipeline nodes use, behind the
//! same authorization; this module only names the seam the host lends.

use async_trait::async_trait;
use serde_json::Value;

/// The languages the tool offers (the sandbox has more; these two have a
/// result convention a model can use without knowing the adapter).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisLanguage {
    Python,
    JavaScript,
}

/// One program and the data it reads.
pub struct AnalysisCodeRequest {
    /// Identifies the tool call, so a replay of it reconciles with the first
    /// submission instead of running twice.
    pub call_id: String,
    pub language: AnalysisLanguage,
    pub source: String,
    /// The JSON object the program sees as its input state.
    pub input: Value,
}

/// Why the sandbox did not return a result, as a cause the tool can word.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisCodeFailure {
    /// This deployment has no sandbox for the language.
    Unavailable,
    /// The sandbox refused the job (authorization, size, policy).
    Rejected,
    /// The program failed or ran out of time or memory.
    Failed,
    /// The run was stopped.
    Cancelled,
    /// Completion could not be confirmed; the program may have run.
    Uncertain,
}

/// The host side of `run_analysis_code`.
#[async_trait]
pub trait AnalysisCodeRunner: Send + Sync {
    /// Run `request` and return the program's JSON result.
    async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeFailure>;
}

/// The sentence the model reads for a failure.
pub(crate) fn failure_sentence(failure: AnalysisCodeFailure) -> &'static str {
    match failure {
        AnalysisCodeFailure::Unavailable => {
            "This deployment has no code sandbox for that language, so run_analysis_code cannot run. Use the other data tools, or tell the user."
        }
        AnalysisCodeFailure::Rejected => {
            "The code sandbox refused the job (it is not authorized, or the code or data is over its size limit). Send fewer rows or shorter code."
        }
        AnalysisCodeFailure::Failed => {
            "The program failed, or ran out of time or memory, in the sandbox. Check the code for errors, and send fewer rows if it is heavy. The sandbox has no pandas, no network and no files."
        }
        AnalysisCodeFailure::Cancelled => "The run was cancelled before it finished.",
        AnalysisCodeFailure::Uncertain => {
            "The sandbox did not confirm whether the program finished. It may have run; do not assume it did not. Tell the user."
        }
    }
}
