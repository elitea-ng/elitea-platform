//! The `pyodide` internal tool (the agent form's "Code sandbox" toggle):
//! `run_code`.
//!
//! The toggle's job is one thing: let the agent run a short program it wrote
//! and read what came back. It is NOT a second sandbox. The program goes to
//! the platform's one code sandbox through the [`AnalysisCodeRunner`] seam the
//! `data_analysis` tool set's `run_analysis_code` already uses, so the runtime,
//! its admission and grants, its limits (time, steps, memory, bytes), its
//! network policy, its dependency preparation and its receipts are exactly
//! those of `run_analysis_code` and the pipeline `code` node.
//!
//! One tool, one implementation, a `language` argument. The languages offered
//! are the ones the deployment's sandbox serves
//! ([`AnalysisCodeRunner::languages`], read from the same profiles the Code
//! node runs on); none are hard-coded here. For Python the program runs inside
//! a small wrapper that also returns what it printed and its traceback; the
//! other languages follow the sandbox's own module convention and return the
//! program's result.
//!
//! A host without a sandbox (the desktop local agent, a deployment that did
//! not configure one) builds no authority, and the tool is not offered.
//!
//! What the program prints or returns is the program's data. The tool result
//! carries it as plain values inside the ordinary tool-result envelope, which
//! the runtime already treats as quoted untrusted content (ADR-0043 decision
//! 4); nothing here lifts it into an instruction.

use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

use super::code::{
    AnalysisCodeError, AnalysisCodeFailure, AnalysisCodeRequest, AnalysisCodeRunner,
    AnalysisLanguage,
};

/// The internal tool name and the toolkit type the admission policy names this
/// tool set by. It is the agent form's own toggle name, kept so stored agents
/// and the SQL gates keep working.
pub const CODE_SANDBOX_TOOL: &str = "pyodide";
/// The one tool the toggle binds.
pub const RUN_CODE_TOOL_NAME: &str = "run_code";

/// The most bytes of program text one call carries. Below the tool-argument
/// admission's 64 KiB string bound, so an over-long program is a sentence the
/// model reads, not a refused call.
const MAX_CODE_BYTES: usize = 60_000;
/// The most bytes of `input` JSON one call carries (the admission bounds the
/// whole call at 256 KiB and each string in it at 64 KiB).
const MAX_INPUT_BYTES: usize = 128 * 1024;
/// Each of stdout, stderr and the traceback is cut at this many characters,
/// inside the sandbox first and here again, because the answer is untrusted.
const MAX_STREAM_CHARS: usize = 20_000;
/// The program's final expression is shown whole up to this many bytes of JSON.
const MAX_RESULT_BYTES: usize = 40_000;
/// The most import lines hoisted ahead of the Python wrapper so the sandbox's
/// dependency preparation sees them.
const MAX_HOISTED_IMPORTS: usize = 50;
/// The most characters of a program's stderr that ride in a failure sentence.
const MAX_DETAIL_CHARS: usize = 1_500;

/// The Python program that runs in the sandbox. The model's code is NOT
/// spliced into it: the code travels as data in `elitea_state['code']` and is
/// compiled here, so nothing the model writes can break out of the wrapper's
/// quoting. The wrapper captures stdout and stderr, runs the code, takes the
/// value of a final expression the way a REPL does, and ends in one dict (the
/// sandbox's "final expression is the answer" convention).
const PYTHON_WRAPPER: &str = r#"import ast as _ast, io as _io, json as _json, sys as _sys, traceback as _tb
_limit = 20000
_out, _err = _io.StringIO(), _io.StringIO()
_old = (_sys.stdout, _sys.stderr)
_sys.stdout, _sys.stderr = _out, _err
_result, _error = None, None
try:
    _tree = _ast.parse(elitea_state["code"], "<program>", "exec")
    _tail = None
    if _tree.body and isinstance(_tree.body[-1], _ast.Expr):
        _tail = _ast.Expression(body=_tree.body.pop().value)
    _scope = {"__name__": "__main__", "elitea_state": {"data": elitea_state.get("data")}}
    exec(compile(_tree, "<program>", "exec"), _scope)
    if _tail is not None:
        _result = eval(compile(_tail, "<program>", "eval"), _scope)
except BaseException:
    _error = _tb.format_exc()
finally:
    _sys.stdout, _sys.stderr = _old
try:
    _result = _json.loads(_json.dumps(_result, default=repr), parse_constant=lambda _c: None)
except Exception:
    _result = repr(_result)
def _clip(_text):
    return _text if len(_text) <= _limit else _text[:_limit] + "\n[truncated]"
{"stdout": _clip(_out.getvalue()), "stderr": _clip(_err.getvalue()), "error": None if _error is None else _clip(_error), "result": _result}
"#;

/// What the host lends the tool: the code sandbox, through the seam.
#[derive(Clone)]
pub struct CodeSandboxAuthority {
    runner: Arc<dyn AnalysisCodeRunner>,
}

impl CodeSandboxAuthority {
    #[must_use]
    pub fn new(runner: Arc<dyn AnalysisCodeRunner>) -> Self {
        Self { runner }
    }

    /// The languages the sandbox serves, in a stable order. Empty: the tool is
    /// not offered.
    #[must_use]
    pub fn languages(&self) -> Vec<AnalysisLanguage> {
        let served = self.runner.languages();
        [
            AnalysisLanguage::Python,
            AnalysisLanguage::JavaScript,
            AnalysisLanguage::TypeScript,
            AnalysisLanguage::Rust,
        ]
        .into_iter()
        .filter(|language| served.contains(language))
        .collect()
    }
}

/// Stable construction failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CodeSandboxToolsetError;

impl std::fmt::Display for CodeSandboxToolsetError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("the code sandbox tool definition is invalid")
    }
}

impl std::error::Error for CodeSandboxToolsetError {}

impl From<MaterializedToolsetError> for CodeSandboxToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self
    }
}

/// Build the tool set. `None` when the tool policy blocks the `pyodide`
/// toolkit or the sandbox serves no language.
///
/// # Errors
///
/// The tool definition was refused by admission.
pub fn build_code_sandbox_toolset(
    authority: &CodeSandboxAuthority,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Option<BasicToolset>, CodeSandboxToolsetError> {
    let languages = authority.languages();
    if languages.is_empty()
        || policy.toolkit_decision(CODE_SANDBOX_TOOL) != ToolAdmissionDecision::Allowed
    {
        return Ok(None);
    }
    let tool: Arc<dyn Tool> = Arc::new(RunCodeTool {
        runner: Arc::clone(&authority.runner),
        description: description_for(&languages),
        schema: parameters_schema(&languages),
        languages,
    });
    admit_materialized_toolset(CODE_SANDBOX_TOOL, CODE_SANDBOX_TOOL, policy, vec![tool])
        .map(Some)
        .map_err(Into::into)
}

/// The tool description: what it does, then one line per served language.
fn description_for(languages: &[AnalysisLanguage]) -> String {
    let mut text = String::from(
        "Run a short program in an isolated sandbox and get its output. Use it for calculations, parsing and data wrangling you can express in code. The sandbox has no network and no files. Pass data in `input` (a JSON object); the program reads it as elitea_state['data'] (elitea_state.data in JavaScript and TypeScript, state[\"data\"] in Rust). The result must be JSON. Output is cut at 20,000 characters per stream.",
    );
    for language in languages {
        text.push_str(match language {
            AnalysisLanguage::Python => {
                " python: print() output and the last expression are returned (stdout, stderr, result, traceback); the standard library and the packages the sandbox provides can be imported."
            }
            AnalysisLanguage::JavaScript => {
                " javascript: a module whose default export (or the result of a default-exported function of the state) is the result; console output is returned as `output`."
            }
            AnalysisLanguage::TypeScript => " typescript: like javascript, console output included.",
            AnalysisLanguage::Rust => {
                " rust: define `pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>>`; its value is the result; console output is returned as `output`."
            }
        });
    }
    text
}

/// The tool's argument schema: a closed object, plain types, no combinators,
/// so every provider's schema dialect takes it as it is. `language` is an enum
/// of the languages this deployment serves.
fn parameters_schema(languages: &[AnalysisLanguage]) -> Value {
    let names: Vec<&str> = languages.iter().map(|language| language.name()).collect();
    json!({
        "type": "object",
        "properties": {
            "language": {
                "type": "string",
                "enum": names,
                "description": format!("The language of the program. Default: {}.", names.first().copied().unwrap_or("python"))
            },
            "code": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_CODE_BYTES,
                "description": "The program."
            },
            "input": {
                "type": "object",
                "description": "Data for the program, a JSON object, read as the `data` of its state."
            }
        },
        "required": ["code"],
        "additionalProperties": false
    })
}

struct RunCodeTool {
    runner: Arc<dyn AnalysisCodeRunner>,
    languages: Vec<AnalysisLanguage>,
    description: String,
    schema: Value,
}

#[async_trait]
impl Tool for RunCodeTool {
    fn name(&self) -> &str {
        RUN_CODE_TOOL_NAME
    }

    fn description(&self) -> &str {
        &self.description
    }

    // Not read-only: it runs code the model wrote (`code_execution`), so a
    // grant or sub-agent limited to read-only tools must not get it. Runs do
    // not share state, so calls may still run side by side.
    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.schema.clone())
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Identifies this call, so a replay reconciles with the first
        // submission instead of running twice.
        let call_id = format!(
            "code_sandbox:{}:{}",
            context.invocation_id(),
            context.function_call_id()
        );
        Ok(match self.run(&arguments, call_id).await {
            Ok(answer) => answer,
            Err(message) => json!({"status": "error", "message": message}),
        })
    }
}

impl RunCodeTool {
    fn language_of(&self, arguments: &Value) -> Result<AnalysisLanguage, String> {
        let Some(requested) = arguments.get("language").filter(|value| !value.is_null()) else {
            return Ok(self.languages[0]);
        };
        let requested = requested
            .as_str()
            .ok_or_else(|| "`language` must be a string.".to_owned())?;
        self.languages
            .iter()
            .copied()
            .find(|language| language.name() == requested)
            .ok_or_else(|| {
                let served: Vec<&str> = self.languages.iter().map(|l| l.name()).collect();
                format!(
                    "`language` `{requested}` is not available in this deployment's sandbox. Use one of: {}.",
                    served.join(", ")
                )
            })
    }

    async fn run(&self, arguments: &Value, call_id: String) -> Result<Value, String> {
        let language = self.language_of(arguments)?;
        let code = arguments
            .get("code")
            .and_then(Value::as_str)
            .filter(|code| !code.trim().is_empty())
            .ok_or_else(|| "`code` is required.".to_owned())?;
        if code.len() > MAX_CODE_BYTES {
            return Err(format!(
                "`code` is {} bytes, over the {MAX_CODE_BYTES} byte limit. Send a shorter program.",
                code.len()
            ));
        }
        if code.contains('\0') {
            return Err("`code` must not contain NUL characters.".to_owned());
        }
        let data = match arguments.get("input") {
            None | Some(Value::Null) => Value::Null,
            Some(value @ Value::Object(_)) => value.clone(),
            Some(_) => return Err("`input` must be a JSON object.".to_owned()),
        };
        if serde_json::to_string(&data).map_or(usize::MAX, |text| text.len()) > MAX_INPUT_BYTES {
            return Err(format!(
                "`input` is over the {MAX_INPUT_BYTES} byte limit. Send less data."
            ));
        }
        let wrapped = language == AnalysisLanguage::Python;
        let request = AnalysisCodeRequest {
            call_id,
            language,
            source: if wrapped {
                python_source(code)
            } else {
                code.to_owned()
            },
            input: if wrapped {
                json!({"code": code, "data": data})
            } else {
                json!({"data": data})
            },
        };
        // Python returns its own streams from the wrapper. The other languages
        // run the model's module as it is, so what they printed is the
        // sandbox's: the same capture the runner reads from the run's receipt.
        if wrapped {
            return match self.runner.run(request).await {
                Ok(value) => Ok(python_answer(&value)),
                Err(error) => Err(failure_text(&error)),
            };
        }
        match self.runner.run_captured(request).await {
            Ok(run) => Ok(plain_answer(&run.result, run.output.as_deref())),
            Err(error) => Err(failure_text(&error)),
        }
    }
}

/// The Python wrapper, with the program's top-level import lines in front.
///
/// The sandbox prepares packages from the `import` statements it can see in
/// the source it is given, and the model's code is data to the wrapper. Simple
/// one-line top-level imports are therefore repeated ahead of the wrapper, each
/// guarded, so that preparation (the same preparation `run_analysis_code`
/// gets) sees them. A line this does not recognise is only not preloaded: the
/// program's own `import` still runs, and its failure is in the traceback.
fn python_source(code: &str) -> String {
    let mut source = String::new();
    let mut hoisted = 0;
    for line in code.lines() {
        if hoisted == MAX_HOISTED_IMPORTS {
            break;
        }
        // Top level only: an indented import is inside a block.
        if is_simple_import(line) {
            source.push_str("try:\n    ");
            source.push_str(line.trim_end());
            source.push_str("\nexcept Exception:\n    pass\n");
            hoisted += 1;
        }
    }
    source.push_str(PYTHON_WRAPPER);
    source
}

/// The program's result, bounded: whole up to [`MAX_RESULT_BYTES`], else a
/// preview and the size.
fn put_result(answer: &mut Map<String, Value>, result: Value) {
    let encoded = result.to_string();
    if encoded.len() <= MAX_RESULT_BYTES {
        answer.insert("result".into(), result);
        return;
    }
    let mut end = MAX_RESULT_BYTES;
    while !encoded.is_char_boundary(end) {
        end -= 1;
    }
    answer.insert("result_preview".into(), json!(&encoded[..end]));
    answer.insert("result_truncated".into(), json!(true));
    answer.insert("result_bytes".into(), json!(encoded.len()));
}

/// The model-facing answer for the other languages: what the program printed
/// (cut at [`MAX_STREAM_CHARS`], like Python's streams) and its result.
fn plain_answer(value: &Value, output: Option<&str>) -> Value {
    let mut answer = Map::new();
    answer.insert("status".into(), json!("success"));
    if let Some(output) = output.filter(|output| !output.is_empty()) {
        answer.insert("output".into(), json!(clip(output, MAX_STREAM_CHARS)));
    }
    put_result(&mut answer, value.clone());
    Value::Object(answer)
}

/// Whether `line` is, exactly, one top-level `import a.b as c, d` or
/// `from a.b import c as d, e` statement. A line of prose or code that merely
/// starts with those words (inside a string or docstring) is not hoisted: it
/// would be a syntax error at module level, which no `try` catches.
fn is_simple_import(line: &str) -> bool {
    fn ident(text: &str) -> bool {
        let mut chars = text.chars();
        chars
            .next()
            .is_some_and(|first| first.is_ascii_alphabetic() || first == '_')
            && chars.all(|c| c.is_ascii_alphanumeric() || c == '_')
            && !matches!(
                text,
                "import" | "from" | "as" | "in" | "is" | "not" | "and" | "or" | "if" | "else"
            )
    }
    fn dotted(text: &str) -> bool {
        !text.is_empty() && text.split('.').all(ident)
    }
    // `name` or `name as alias`, the name dotted when `dots`.
    fn item(text: &str, dots: bool) -> bool {
        let parts: Vec<&str> = text.split_whitespace().collect();
        let name_ok = |name: &str| if dots { dotted(name) } else { ident(name) };
        match parts.as_slice() {
            [name] => name_ok(name),
            [name, "as", alias] => name_ok(name) && ident(alias),
            _ => false,
        }
    }
    if line.len() > 200 || line.trim_end() != line {
        return false;
    }
    if let Some(rest) = line.strip_prefix("import ") {
        return rest.split(',').all(|part| item(part, true));
    }
    if let Some(rest) = line.strip_prefix("from ") {
        let Some((module, names)) = rest.split_once(" import ") else {
            return false;
        };
        return dotted(module.trim())
            && module.trim() != "__future__"
            && (names.trim() == "*" || names.split(',').all(|part| item(part, false)));
    }
    false
}

/// The model-facing answer from the Python wrapper's dict. Every field is
/// re-checked and re-bounded: the dict is produced by code the model wrote.
fn python_answer(value: &Value) -> Value {
    let mut answer = Map::new();
    let text = |key: &str| {
        value
            .get(key)
            .and_then(Value::as_str)
            .map(|text| clip(text, MAX_STREAM_CHARS))
    };
    let traceback = text("error");
    answer.insert(
        "status".into(),
        json!(if traceback.is_some() {
            "error"
        } else {
            "success"
        }),
    );
    answer.insert("stdout".into(), json!(text("stdout").unwrap_or_default()));
    answer.insert("stderr".into(), json!(text("stderr").unwrap_or_default()));
    if let Some(traceback) = traceback {
        answer.insert(
            "message".into(),
            json!("The program raised an exception; the traceback is in `traceback`."),
        );
        answer.insert("traceback".into(), json!(traceback));
    }
    put_result(
        &mut answer,
        value.get("result").cloned().unwrap_or(Value::Null),
    );
    Value::Object(answer)
}

fn clip(text: &str, max_chars: usize) -> String {
    if text.chars().count() <= max_chars {
        return text.to_owned();
    }
    let mut cut: String = text.chars().take(max_chars).collect();
    cut.push_str("\n[truncated]");
    cut
}

/// The sentence the model reads when the sandbox returned no result.
fn failure_text(error: &AnalysisCodeError) -> String {
    let mut sentence = match error.failure {
        AnalysisCodeFailure::Unavailable => {
            "This deployment has no code sandbox for that language, so run_code cannot run. Tell the user."
        }
        AnalysisCodeFailure::Rejected => {
            "The code sandbox refused the job (it is not authorized, or the code or input is over its size limit). Send shorter code or less input."
        }
        AnalysisCodeFailure::Failed => {
            "The sandbox could not run the program to completion. Check the code (for Rust, that it compiles and defines `run`; for JavaScript, that it has a default export); the sandbox has no network and no files."
        }
        AnalysisCodeFailure::Overrun => {
            "The program ran out of time, steps or memory in the sandbox. Make it cheaper or send less data."
        }
        AnalysisCodeFailure::ResultTooLarge => {
            "The program's output is over the sandbox's size limit and was refused. Print or return a summary, not the data."
        }
        AnalysisCodeFailure::Cancelled => "The run was cancelled before it finished.",
        AnalysisCodeFailure::Uncertain => {
            "The sandbox did not confirm whether the program finished. It may have run; do not assume it did not. Tell the user."
        }
        AnalysisCodeFailure::Interrupted => {
            "The sandbox running the program was lost before it finished (an infrastructure fault, not an error in the code). The program has no network, so running the same code again is safe."
        }
    }
    .to_owned();
    if let Some(detail) = &error.detail {
        sentence.push_str("\nProgram error output:\n");
        sentence.push_str(&clip(detail, MAX_DETAIL_CHARS));
    }
    sentence
}

#[cfg(test)]
mod tests {
    // Test code: formatting and indexing are not worth the ceremony here.
    #![allow(clippy::pedantic)]

    use std::collections::BTreeMap;
    use std::sync::Mutex;

    use adk_core::{ReadonlyContext, Toolset};
    use adk_tool::SimpleToolContext;

    use super::super::code::CapturedRun;
    use super::*;
    use crate::tool_schema::{SchemaProfile, normalize_tool_schema, wire_violations};

    const ALL: [AnalysisLanguage; 4] = [
        AnalysisLanguage::Python,
        AnalysisLanguage::JavaScript,
        AnalysisLanguage::TypeScript,
        AnalysisLanguage::Rust,
    ];

    struct FakeRunner {
        seen: Mutex<Vec<AnalysisCodeRequest>>,
        reply: Mutex<Option<Result<Value, AnalysisCodeError>>>,
        served: Vec<AnalysisLanguage>,
        /// What the runner reports the program printed (`run_captured`).
        output: Mutex<Option<String>>,
    }

    impl FakeRunner {
        fn serving(
            served: &[AnalysisLanguage],
            reply: Result<Value, AnalysisCodeError>,
        ) -> Arc<Self> {
            Arc::new(Self {
                seen: Mutex::new(Vec::new()),
                reply: Mutex::new(Some(reply)),
                served: served.to_vec(),
                output: Mutex::new(None),
            })
        }

        fn printing(self: Arc<Self>, output: &str) -> Arc<Self> {
            *self.output.lock().unwrap() = Some(output.to_owned());
            self
        }

        fn replying(reply: Result<Value, AnalysisCodeError>) -> Arc<Self> {
            Self::serving(&ALL, reply)
        }
    }

    #[async_trait]
    impl AnalysisCodeRunner for FakeRunner {
        async fn run(&self, request: AnalysisCodeRequest) -> Result<Value, AnalysisCodeError> {
            self.seen.lock().unwrap().push(request);
            self.reply.lock().unwrap().take().expect("one reply")
        }

        async fn run_captured(
            &self,
            request: AnalysisCodeRequest,
        ) -> Result<CapturedRun, AnalysisCodeError> {
            let result = self.run(request).await?;
            Ok(CapturedRun {
                result,
                output: self.output.lock().unwrap().take(),
            })
        }

        fn languages(&self) -> Vec<AnalysisLanguage> {
            self.served.clone()
        }
    }

    fn policy(blocked: &[&str]) -> Arc<ToolAdmissionPolicy> {
        let blocked: Vec<String> = blocked.iter().map(|s| (*s).to_owned()).collect();
        Arc::new(ToolAdmissionPolicy::new(&blocked, &BTreeMap::new()).expect("policy"))
    }

    fn context() -> Arc<SimpleToolContext> {
        Arc::new(SimpleToolContext::new("code-test").with_function_call_id("call-1"))
    }

    async fn served_tools(
        runner: Arc<FakeRunner>,
        policy: &Arc<ToolAdmissionPolicy>,
    ) -> Vec<Arc<dyn Tool>> {
        let authority = CodeSandboxAuthority::new(runner);
        match build_code_sandbox_toolset(&authority, policy).expect("builds") {
            Some(set) => set
                .tools(context() as Arc<dyn ReadonlyContext>)
                .await
                .expect("tools"),
            None => Vec::new(),
        }
    }

    async fn call(tool: &Arc<dyn Tool>, args: Value) -> Value {
        tool.execute(context() as Arc<dyn ToolContext>, args)
            .await
            .expect("tool result")
    }

    fn wrapper_reply(stdout: &str, error: Option<&str>, result: Value) -> Value {
        json!({"stdout": stdout, "stderr": "", "error": error, "result": result})
    }

    #[tokio::test]
    async fn builds_one_read_only_run_code_tool_offering_the_served_languages() {
        let runner = FakeRunner::replying(Ok(Value::Null));
        let tools = served_tools(runner, &policy(&[])).await;
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0].name(), RUN_CODE_TOOL_NAME);
        // Code execution is never a read-only tool.
        assert!(!tools[0].is_read_only());
        assert!(tools[0].is_concurrency_safe());
        assert!(tools[0].description().len() <= 1_000);
        let schema = tools[0].parameters_schema().unwrap();
        assert_eq!(schema["required"], json!(["code"]));
        assert_eq!(schema["additionalProperties"], false);
        assert_eq!(
            schema["properties"]["language"]["enum"],
            json!(["python", "javascript", "typescript", "rust"])
        );
    }

    #[tokio::test]
    async fn the_language_list_is_what_the_sandbox_serves() {
        let runner = FakeRunner::serving(&[AnalysisLanguage::Python], Ok(Value::Null));
        let tools = served_tools(runner, &policy(&[])).await;
        let schema = tools[0].parameters_schema().unwrap();
        assert_eq!(schema["properties"]["language"]["enum"], json!(["python"]));
        assert!(!tools[0].description().contains("rust"));
        // A language the sandbox does not serve is refused, never sent.
        let out = call(&tools[0], json!({"code": "1", "language": "rust"})).await;
        assert_eq!(out["status"], "error");
        assert!(out["message"].as_str().unwrap().contains("python"));
    }

    #[tokio::test]
    async fn a_sandbox_serving_nothing_builds_no_tool() {
        let runner = FakeRunner::serving(&[], Ok(Value::Null));
        assert!(served_tools(runner, &policy(&[])).await.is_empty());
    }

    #[tokio::test]
    async fn a_blocked_pyodide_toolkit_builds_nothing() {
        let authority = CodeSandboxAuthority::new(FakeRunner::replying(Ok(Value::Null)));
        let built = build_code_sandbox_toolset(&authority, &policy(&["pyodide"])).unwrap();
        assert!(built.is_none());
    }

    #[test]
    fn the_schema_passes_every_provider_profile() {
        for languages in [&ALL[..], &ALL[..1]] {
            let raw = parameters_schema(languages);
            for profile in [
                SchemaProfile::Anthropic,
                SchemaProfile::OpenAi,
                SchemaProfile::Bedrock,
                SchemaProfile::Gemini,
                SchemaProfile::LlamaCpp,
            ] {
                let out = normalize_tool_schema(&raw, profile);
                assert!(wire_violations(&out, profile).is_empty(), "{profile:?}");
                assert_eq!(out["required"], json!(["code"]), "{profile:?}");
            }
        }
    }

    #[test]
    fn the_description_stays_under_the_sdk_cap_with_every_language() {
        assert!(description_for(&ALL).len() <= 1_000);
    }

    #[tokio::test]
    async fn python_runs_through_the_seam_and_returns_stdout_and_result() {
        let runner = FakeRunner::replying(Ok(wrapper_reply("2\n", None, json!(2))));
        let tools = served_tools(Arc::clone(&runner), &policy(&[])).await;
        let out = call(
            &tools[0],
            json!({"code": "print(1+1)\n1+1", "input": {"k": 1}}),
        )
        .await;
        assert_eq!(out["status"], "success", "{out}");
        assert_eq!(out["stdout"], "2\n");
        assert_eq!(out["result"], 2);
        let seen = runner.seen.lock().unwrap();
        assert_eq!(seen.len(), 1);
        assert_eq!(seen[0].language, AnalysisLanguage::Python);
        assert!(seen[0].call_id.contains("call-1"));
        // The model's code is data, never spliced into the program text.
        assert_eq!(seen[0].input["code"], "print(1+1)\n1+1");
        assert_eq!(seen[0].input["data"], json!({"k": 1}));
        assert!(!seen[0].source.contains("print(1+1)"));
    }

    #[tokio::test]
    async fn every_other_language_sends_its_source_as_is_and_returns_the_result() {
        for (language, code) in [
            (
                AnalysisLanguage::JavaScript,
                "export default (s) => s.data.n + 1;",
            ),
            (
                AnalysisLanguage::TypeScript,
                "export default (s: any): number => s.data.n + 1;",
            ),
            (
                AnalysisLanguage::Rust,
                "pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { Ok(state) }",
            ),
        ] {
            let runner = FakeRunner::replying(Ok(json!(2)));
            let tools = served_tools(Arc::clone(&runner), &policy(&[])).await;
            let out = call(
                &tools[0],
                json!({"language": language.name(), "code": code, "input": {"n": 1}}),
            )
            .await;
            assert_eq!(out["status"], "success", "{language:?}: {out}");
            assert_eq!(out["result"], 2);
            let seen = runner.seen.lock().unwrap();
            assert_eq!(seen[0].language, language);
            assert_eq!(seen[0].source, code);
            assert_eq!(seen[0].input, json!({"data": {"n": 1}}));
        }
    }

    // #2203: what the program printed comes back for every language the
    // sandbox serves besides Python, cut at the same 20,000 characters, and
    // the model's code is still the source as it is (no wrapper).
    #[tokio::test]
    async fn other_languages_return_what_the_program_printed_bounded() {
        for (language, code) in [
            (
                AnalysisLanguage::JavaScript,
                "console.log('hi'); export default 2;",
            ),
            (
                AnalysisLanguage::TypeScript,
                "console.log('hi'); export default 2;",
            ),
            (
                AnalysisLanguage::Rust,
                "pub fn run(s: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { println!(\"hi\"); Ok(s) }",
            ),
        ] {
            let runner = FakeRunner::replying(Ok(json!(2))).printing("hi\n");
            let tools = served_tools(Arc::clone(&runner), &policy(&[])).await;
            let out = call(
                &tools[0],
                json!({"language": language.name(), "code": code}),
            )
            .await;
            assert_eq!(out["status"], "success", "{language:?}: {out}");
            assert_eq!(out["output"], "hi\n");
            assert_eq!(out["result"], 2);
            assert_eq!(runner.seen.lock().unwrap()[0].source, code);
        }
        let runner = FakeRunner::replying(Ok(json!(1))).printing(&"x".repeat(MAX_STREAM_CHARS * 2));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(
            &tools[0],
            json!({"language": "javascript", "code": "export default 1;"}),
        )
        .await;
        let text = out["output"].as_str().unwrap();
        assert!(text.ends_with("\n[truncated]"));
        assert_eq!(
            text.chars().count(),
            MAX_STREAM_CHARS + "\n[truncated]".len()
        );
    }

    #[tokio::test]
    async fn a_runner_without_capture_adds_no_output_field() {
        let runner = FakeRunner::replying(Ok(json!(2)));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(
            &tools[0],
            json!({"language": "javascript", "code": "export default 2;"}),
        )
        .await;
        assert!(out.get("output").is_none(), "{out}");
        // A program that printed nothing adds no empty field either.
        let quiet = FakeRunner::replying(Ok(json!(2))).printing("");
        let tools = served_tools(quiet, &policy(&[])).await;
        let out = call(
            &tools[0],
            json!({"language": "javascript", "code": "export default 2;"}),
        )
        .await;
        assert!(out.get("output").is_none(), "{out}");
    }

    #[tokio::test]
    async fn a_raised_python_exception_is_an_error_answer_with_the_traceback() {
        let runner = FakeRunner::replying(Ok(wrapper_reply(
            "partial",
            Some("Traceback...\nZeroDivisionError"),
            Value::Null,
        )));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(&tools[0], json!({"code": "1/0"})).await;
        assert_eq!(out["status"], "error");
        assert_eq!(out["stdout"], "partial");
        assert!(
            out["traceback"]
                .as_str()
                .unwrap()
                .contains("ZeroDivisionError")
        );
    }

    #[tokio::test]
    async fn output_is_bounded_even_if_the_program_lies() {
        let huge = "x".repeat(MAX_STREAM_CHARS * 3);
        let runner = FakeRunner::replying(Ok(wrapper_reply(
            &huge,
            None,
            json!("y".repeat(MAX_RESULT_BYTES * 2)),
        )));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(&tools[0], json!({"code": "pass"})).await;
        assert!(out["stdout"].as_str().unwrap().chars().count() < MAX_STREAM_CHARS + 20);
        assert!(out["stdout"].as_str().unwrap().ends_with("[truncated]"));
        assert_eq!(out["result_truncated"], true);
        assert!(out.get("result").is_none());
        assert!(out["result_preview"].as_str().unwrap().len() <= MAX_RESULT_BYTES);

        let runner = FakeRunner::replying(Ok(json!("z".repeat(MAX_RESULT_BYTES * 2))));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(&tools[0], json!({"language": "javascript", "code": "x"})).await;
        assert_eq!(out["result_truncated"], true);
    }

    #[tokio::test]
    async fn sandbox_failures_are_sentences_not_turn_errors() {
        for failure in [
            AnalysisCodeFailure::Unavailable,
            AnalysisCodeFailure::Rejected,
            AnalysisCodeFailure::Failed,
            AnalysisCodeFailure::Overrun,
            AnalysisCodeFailure::ResultTooLarge,
            AnalysisCodeFailure::Cancelled,
            AnalysisCodeFailure::Uncertain,
            AnalysisCodeFailure::Interrupted,
        ] {
            let runner = FakeRunner::replying(Err(failure.into()));
            let tools = served_tools(runner, &policy(&[])).await;
            let out = call(&tools[0], json!({"code": "pass"})).await;
            assert_eq!(out["status"], "error", "{failure:?}");
            assert!(!out["message"].as_str().unwrap().is_empty());
        }
        let runner = FakeRunner::replying(Err(AnalysisCodeError {
            failure: AnalysisCodeFailure::Failed,
            detail: Some("boom".into()),
        }));
        let tools = served_tools(runner, &policy(&[])).await;
        let out = call(&tools[0], json!({"code": "pass"})).await;
        assert!(out["message"].as_str().unwrap().contains("boom"));
    }

    #[tokio::test]
    async fn bad_arguments_never_reach_the_sandbox() {
        let runner = FakeRunner::replying(Ok(Value::Null));
        let tools = served_tools(Arc::clone(&runner), &policy(&[])).await;
        for args in [
            json!({}),
            json!({"code": "   "}),
            json!({"code": "a\u{0}b"}),
            json!({"code": "x", "input": [1]}),
            json!({"code": "x", "language": 3}),
            json!({"code": "x".repeat(MAX_CODE_BYTES + 1)}),
            json!({"code": "x", "input": {
                "a": "y".repeat(30_000), "b": "y".repeat(30_000), "c": "y".repeat(30_000),
                "d": "y".repeat(30_000), "e": "y".repeat(30_000),
            }}),
        ] {
            let out = call(&tools[0], args).await;
            assert_eq!(out["status"], "error", "{out}");
        }
        assert!(runner.seen.lock().unwrap().is_empty());
    }

    #[test]
    fn top_level_simple_imports_are_hoisted_ahead_of_the_wrapper() {
        let source = python_source(
            "import numpy as np\nfrom math import sqrt\n    import indented\nfrom x import (a,\n b)\nimport os; import sys\nprint(np)",
        );
        assert!(source.starts_with("try:\n    import numpy as np\n"));
        // Prose that starts with the words is not an import.
        let prose =
            python_source("x = '''\nfrom the data above, take rows\nimport the following\n'''");
        assert!(prose.starts_with("import ast as _ast"), "{prose}");
        for ok in [
            "import os",
            "import a.b as c, d",
            "from a.b import c as d, e",
            "from x import *",
        ] {
            assert!(is_simple_import(ok), "{ok}");
        }
        for bad in [
            "import the following",
            "from the data above, take",
            "import",
            "from x import",
            "import a b",
            "from __future__ import annotations",
            "import os ",
        ] {
            assert!(!is_simple_import(bad), "{bad}");
        }
        assert!(source.contains("    from math import sqrt\n"));
        assert!(!source.contains("indented"));
        assert!(!source.contains("from x import"));
        assert!(!source.contains("import os"));
        assert!(source.ends_with(PYTHON_WRAPPER));
        assert!(!source.contains("print(np)"));
    }
}
