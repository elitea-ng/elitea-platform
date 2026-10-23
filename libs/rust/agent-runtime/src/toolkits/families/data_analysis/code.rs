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

/// The one result limit: a program's JSON answer above this is refused, by the
/// worker's runner and by the tool alike, and the tool description says so.
pub const MAX_ANALYSIS_RESULT_BYTES: usize = 100 * 1024;

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
    /// The program ran out of time, steps or memory.
    Overrun,
    /// The program's result is over [`MAX_ANALYSIS_RESULT_BYTES`].
    ResultTooLarge,
    /// The run was stopped.
    Cancelled,
    /// Completion could not be confirmed; the program may have run.
    Uncertain,
    /// The sandbox running the program was lost (node loss, eviction, a
    /// platform kill) before it finished. Not an error in the program.
    Interrupted,
}

/// A failed run: the cause, and the program's own diagnostic text when it has
/// one (stderr, already bounded by the runner).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AnalysisCodeError {
    pub failure: AnalysisCodeFailure,
    pub detail: Option<String>,
}

impl From<AnalysisCodeFailure> for AnalysisCodeError {
    fn from(failure: AnalysisCodeFailure) -> Self {
        Self {
            failure,
            detail: None,
        }
    }
}

/// The host side of `run_analysis_code`.
#[async_trait]
pub trait AnalysisCodeRunner: Send + Sync {
    /// Run `request` and return the program's JSON result.
    async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeError>;
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
            "The program failed in the sandbox. Check the code for errors. The sandbox has no pandas, no network and no files."
        }
        AnalysisCodeFailure::Overrun => {
            "The program ran out of time, steps or memory in the sandbox. Send fewer rows (use `columns`, `filters` or `max_rows`) or make the code cheaper."
        }
        AnalysisCodeFailure::ResultTooLarge => {
            "The program's result is over 100 KiB and was refused. Return a summary, not the data."
        }
        AnalysisCodeFailure::Cancelled => "The run was cancelled before it finished.",
        AnalysisCodeFailure::Uncertain => {
            "The sandbox did not confirm whether the program finished. It may have run; do not assume it did not. Tell the user."
        }
        AnalysisCodeFailure::Interrupted => {
            "The sandbox running the program was lost before it finished (an infrastructure fault, not an error in the code). The program has no network, so running the same code again is safe."
        }
    }
}
