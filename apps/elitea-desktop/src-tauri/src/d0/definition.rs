//! What D0 accepts from a resolved definition (decision 5a), and the clear
//! refusals for what it does not run locally yet.
//!
//! D0 runs one ordinary agent: its instructions, its model, its frozen
//! skills and project context, and its credentialed toolkits and platform
//! MCP servers as remote tools. A pipeline, a nested agent and a platform MCP
//! server without a `toolkit_ref` (an older platform) are refused before the
//! turn starts, so no execution is opened for them.

use serde_json::{Map, Value};

use super::api::ResolvedVersion;
use elitea_agent_runtime::host::ReasoningEffort;

/// Model steps of a workspace turn's run when neither the workspace's
/// settings nor the agent (`meta.step_limit`) set them. At the limit
/// the person is asked whether to continue (`step_limit` in the runtime).
pub const DEFAULT_STEP_LIMIT: u32 = 100;
/// The most steps one run may take, whoever sets them.
pub const MAX_STEP_LIMIT: u32 = 200;
/// The OpenAI tool-name rule (`^[A-Za-z0-9_-]{1,64}$`).
const MAX_TOOL_NAME: usize = 64;

/// Why a definition is not run locally.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Refusal {
    pub code: &'static str,
    pub message: String,
}

impl Refusal {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

/// The frozen model settings (`version_details.llm_settings`).
#[derive(Clone, Debug, PartialEq)]
pub struct ModelSettings {
    pub model_name: String,
    pub max_tokens: Option<u32>,
    pub temperature: Option<f32>,
    pub reasoning_effort: Option<ReasoningEffort>,
}

/// One remote tool the model may call.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RemoteToolSpec {
    /// The name the model sees (unique in the turn).
    pub exposed_name: String,
    pub toolkit_id: i64,
    pub toolkit_ref: String,
    pub toolkit_name: String,
    /// The toolkit's `type` (`mcp_*` names an MCP server toolkit), for the
    /// web app page that signs in to it.
    pub toolkit_type: String,
    pub description: String,
    /// `None`: the toolkit's every tool (`all_tools`); the model names the
    /// tool in a `tool_name` argument.
    pub tool_name: Option<String>,
}

/// A definition D0 can run.
#[derive(Clone, Debug)]
pub struct Admitted {
    pub instructions: String,
    pub model: ModelSettings,
    pub remote_tools: Vec<RemoteToolSpec>,
    /// Model steps per run: the workspace's own setting (see
    /// [`Admitted::with_workspace_step_limit`]), else the agent's
    /// `meta.step_limit`, else [`DEFAULT_STEP_LIMIT`]; at most
    /// [`MAX_STEP_LIMIT`].
    pub step_limit: u32,
    /// The version document, for the runtime's instruction authority.
    pub version_details: Map<String, Value>,
}

/// Check a resolved definition against what D0 runs.
///
/// # Errors
///
/// A [`Refusal`] the UI shows as it is.
pub fn admit(resolved: &ResolvedVersion, reserved: &[&str]) -> Result<Admitted, Refusal> {
    if !resolved.withheld_secrets.is_empty() {
        return Err(Refusal::new(
            "secrets_withheld",
            "This agent needs secrets that are never sent to the desktop; run it in the cloud.",
        ));
    }
    let details = &resolved.version_details;
    if details.get("agent_type").and_then(Value::as_str) == Some("pipeline") {
        return Err(Refusal::new(
            "pipeline_unsupported",
            "Pipelines do not run locally yet; run this pipeline in the cloud.",
        ));
    }
    let tools = details
        .get("tools")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice);
    let mut remote_tools = Vec::new();
    for tool in tools {
        match tool.get("kind").and_then(Value::as_str) {
            Some("remote_toolkit") => remote_tools.extend(remote_specs(tool)?),
            Some("application") => {
                return Err(Refusal::new(
                    "nested_agents_unsupported",
                    "This agent uses other agents as tools, which do not run locally yet; run it in the cloud.",
                ));
            }
            // A platform MCP server runs on the platform like a credentialed
            // toolkit (ADR-0029 decision 3): a server that hands it a
            // `toolkit_ref` (client contract 1.16) is called through
            // `executeRemoteToolkitTool`; an older one cannot be reached.
            Some("platform_mcp") if tool.get("toolkit_ref").is_some_and(Value::is_object) => {
                remote_tools.extend(remote_specs(tool)?);
            }
            Some("platform_mcp") => {
                return Err(Refusal::new(
                    "platform_mcp_unsupported",
                    "This agent uses a project MCP server that this platform version cannot reach from a local turn; update the platform or run it in the cloud.",
                ));
            }
            _ => {
                return Err(Refusal::new(
                    "unknown_tool_kind",
                    "This agent has a tool this version of the app does not know; update the app or run it in the cloud.",
                ));
            }
        }
    }
    name_tools(&mut remote_tools, reserved);
    Ok(Admitted {
        instructions: details
            .get("instructions")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        model: model_settings(details)?,
        remote_tools,
        step_limit: agent_step_limit(details).unwrap_or(DEFAULT_STEP_LIMIT),
        version_details: details.clone(),
    })
}

impl Admitted {
    /// The workspace's own step limit (its settings), which wins over the
    /// agent's `meta.step_limit`: it is the person's choice on their own
    /// machine, while an agent's value is often the platform's cloud
    /// default (every agent created without one is stamped 25).
    #[must_use]
    pub fn with_workspace_step_limit(mut self, workspace: Option<u32>) -> Self {
        if let Some(limit) = workspace.filter(|limit| *limit > 0) {
            self.step_limit = limit.min(MAX_STEP_LIMIT);
        }
        self
    }
}

fn invalid_model() -> Refusal {
    Refusal::new(
        "model_unresolved",
        "The agent's model settings could not be read; check the agent's model in the web app.",
    )
}

fn model_settings(details: &Map<String, Value>) -> Result<ModelSettings, Refusal> {
    let settings = details
        .get("llm_settings")
        .and_then(Value::as_object)
        .ok_or_else(invalid_model)?;
    let model_name = settings
        .get("model_name")
        .and_then(Value::as_str)
        .filter(|name| elitea_llm_wire::headers::bounded_header_text(name, 256))
        .ok_or_else(invalid_model)?
        .to_owned();
    let auto = settings.get("max_tokens_auto") == Some(&Value::Bool(true));
    let max_tokens = settings
        .get("max_tokens")
        .and_then(Value::as_u64)
        .filter(|tokens| *tokens > 0 && !auto)
        .and_then(|tokens| u32::try_from(tokens).ok());
    #[allow(clippy::cast_possible_truncation)] // a temperature is 0..=2
    let temperature = settings
        .get("temperature")
        .and_then(Value::as_f64)
        .map(|value| value as f32);
    let reasoning_effort = match settings.get("reasoning_effort").and_then(Value::as_str) {
        Some("low") => Some(ReasoningEffort::Low),
        Some("medium") => Some(ReasoningEffort::Medium),
        Some("high") => Some(ReasoningEffort::High),
        Some("none") => Some(ReasoningEffort::None),
        _ => None,
    };
    Ok(ModelSettings {
        model_name,
        max_tokens,
        // The gateway refuses a temperature next to a reasoning effort.
        temperature: temperature
            .filter(|_| reasoning_effort.is_none_or(|e| e == ReasoningEffort::None)),
        reasoning_effort,
    })
}

/// The agent's own `meta.step_limit`, capped at [`MAX_STEP_LIMIT`].
fn agent_step_limit(details: &Map<String, Value>) -> Option<u32> {
    details
        .get("meta")
        .and_then(|meta| meta.get("step_limit"))
        .and_then(Value::as_u64)
        .and_then(|limit| u32::try_from(limit).ok())
        .filter(|limit| *limit > 0)
        .map(|limit| limit.min(MAX_STEP_LIMIT))
}

/// One spec per selected tool, or one generic spec for `all_tools`; none
/// when the guardrails blocked every selected tool.
fn remote_specs(tool: &Value) -> Result<Vec<RemoteToolSpec>, Refusal> {
    let reference = tool.get("toolkit_ref");
    let toolkit_id = reference
        .and_then(|r| r.get("toolkit_id"))
        .and_then(Value::as_i64);
    let toolkit_ref = reference
        .and_then(|r| r.get("ref"))
        .and_then(Value::as_str)
        .filter(|r| valid_toolkit_ref(r));
    let (Some(toolkit_id), Some(toolkit_ref)) = (toolkit_id, toolkit_ref) else {
        return Err(Refusal::new(
            "toolkit_ref_missing",
            "A toolkit of this agent has no remote reference; update the platform or run it in the cloud.",
        ));
    };
    let text = |key: &str| {
        tool.get(key)
            .and_then(Value::as_str)
            .filter(|v| !v.is_empty())
    };
    let toolkit_name = text("toolkit_name")
        .or_else(|| text("name"))
        .or_else(|| text("type"))
        .unwrap_or("toolkit")
        .to_owned();
    let description = text("description").unwrap_or_default().to_owned();
    let toolkit_type = text("type").unwrap_or_default().to_owned();
    let all_tools = tool.get("all_tools") == Some(&Value::Bool(true));
    let selected: Vec<String> = tool
        .get("selected_tools")
        .and_then(Value::as_array)
        .map(|names| {
            names
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default();
    let spec = |tool_name: Option<String>| RemoteToolSpec {
        exposed_name: String::new(),
        toolkit_id,
        toolkit_ref: toolkit_ref.to_owned(),
        toolkit_name: toolkit_name.clone(),
        toolkit_type: toolkit_type.clone(),
        description: description.clone(),
        tool_name,
    };
    if all_tools {
        return Ok(vec![spec(None)]);
    }
    Ok(selected.into_iter().map(|name| spec(Some(name))).collect())
}

fn valid_toolkit_ref(value: &str) -> bool {
    value.len() == 37
        && value.starts_with("tkr1_")
        && value[5..]
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

fn sanitize(value: &str) -> String {
    let mut name: String = value
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '_' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect();
    name.truncate(MAX_TOOL_NAME);
    name
}

/// `<toolkit>_<tool>` (or `<toolkit>_call` for `all_tools`), sanitised,
/// unique, and never one of the `reserved` local tool names.
fn name_tools(specs: &mut [RemoteToolSpec], reserved: &[&str]) {
    let mut taken: Vec<String> = reserved.iter().map(|name| (*name).to_owned()).collect();
    for spec in specs {
        let base = sanitize(&format!(
            "{}_{}",
            spec.toolkit_name,
            spec.tool_name.as_deref().unwrap_or("call")
        ));
        let mut name = base.clone();
        let mut n = 2;
        while taken.contains(&name) {
            let suffix = format!("_{n}");
            let mut trimmed = base.clone();
            trimmed.truncate(MAX_TOOL_NAME - suffix.len());
            name = format!("{trimmed}{suffix}");
            n += 1;
        }
        taken.push(name.clone());
        spec.exposed_name = name;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn resolved(details: Value) -> ResolvedVersion {
        ResolvedVersion {
            withheld_secrets: Vec::new(),
            project_context_withheld: false,
            version_details: details.as_object().cloned().unwrap_or_default(),
        }
    }

    fn toolkit(all: bool, selected: &[&str]) -> Value {
        json!({
            "kind": "remote_toolkit", "type": "jira", "toolkit_name": "Jira",
            "all_tools": all, "selected_tools": selected,
            "toolkit_ref": {"toolkit_id": 3, "project_id": 1, "ref": "tkr1_0123456789abcdef0123456789abcdef"}
        })
    }

    #[test]
    fn workspace_default_step_limit_is_100() {
        let admit_meta = |meta: Value| {
            admit(
                &resolved(json!({
                    "instructions": "Be brief.",
                    "llm_settings": {"model_name": "gpt-x"},
                    "meta": meta,
                })),
                &[],
            )
            .unwrap()
        };
        // No `meta.step_limit`: a workspace turn runs 100 steps, or the
        // workspace's own limit when its settings set one.
        let default = admit_meta(json!({}));
        assert_eq!(default.step_limit, 100);
        assert_eq!(
            default.clone().with_workspace_step_limit(None).step_limit,
            100
        );
        assert_eq!(
            default
                .clone()
                .with_workspace_step_limit(Some(150))
                .step_limit,
            150
        );
        assert_eq!(default.with_workspace_step_limit(Some(900)).step_limit, 200);
        // The agent's explicit value beats the default; the workspace's own
        // setting beats the agent's.
        let agent = admit_meta(json!({"step_limit": 30}));
        assert_eq!(agent.step_limit, 30);
        assert_eq!(agent.clone().with_workspace_step_limit(None).step_limit, 30);
        assert_eq!(agent.with_workspace_step_limit(Some(150)).step_limit, 150);
        // Capped at 200; zero or nonsense is no value.
        assert_eq!(admit_meta(json!({"step_limit": 5000})).step_limit, 200);
        assert_eq!(admit_meta(json!({"step_limit": 0})).step_limit, 100);
        assert_eq!(admit_meta(json!({"step_limit": "many"})).step_limit, 100);
    }

    #[test]
    fn an_ordinary_agent_is_admitted_with_one_tool_per_selected_tool() {
        let admitted = admit(
            &resolved(json!({
                "instructions": "Be brief.",
                "llm_settings": {"model_name": "gpt-x", "max_tokens": 512, "temperature": 0.2},
                "meta": {"step_limit": 7},
                "tools": [toolkit(false, &["search issue", "create_issue"])]
            })),
            &["read_file"],
        )
        .unwrap();
        assert_eq!(admitted.instructions, "Be brief.");
        assert_eq!(admitted.model.model_name, "gpt-x");
        assert_eq!(admitted.model.max_tokens, Some(512));
        assert_eq!(admitted.step_limit, 7);
        let names: Vec<_> = admitted
            .remote_tools
            .iter()
            .map(|t| t.exposed_name.as_str())
            .collect();
        assert_eq!(names, ["Jira_search_issue", "Jira_create_issue"]);
        assert_eq!(
            admitted.remote_tools[0].tool_name.as_deref(),
            Some("search issue")
        );
    }

    #[test]
    fn all_tools_is_one_generic_tool_and_an_empty_selection_is_none() {
        let all = admit(
            &resolved(json!({"llm_settings": {"model_name": "m"}, "tools": [toolkit(true, &[])]})),
            &[],
        )
        .unwrap();
        assert_eq!(all.remote_tools.len(), 1);
        assert_eq!(all.remote_tools[0].tool_name, None);
        assert_eq!(all.remote_tools[0].exposed_name, "Jira_call");
        let blocked = admit(
            &resolved(json!({"llm_settings": {"model_name": "m"}, "tools": [toolkit(false, &[])]})),
            &[],
        )
        .unwrap();
        assert!(blocked.remote_tools.is_empty());
    }

    #[test]
    fn refusals_name_what_cannot_run_locally() {
        let mut secret = resolved(json!({"llm_settings": {"model_name": "m"}, "tools": []}));
        secret.withheld_secrets = vec!["/instructions".into()];
        assert_eq!(admit(&secret, &[]).unwrap_err().code, "secrets_withheld");
        assert!(
            admit(&secret, &[])
                .unwrap_err()
                .message
                .contains("run it in the cloud")
        );
        let pipeline = resolved(json!({"agent_type": "pipeline", "tools": []}));
        assert_eq!(
            admit(&pipeline, &[]).unwrap_err().code,
            "pipeline_unsupported"
        );
        let nested = resolved(json!({
            "llm_settings": {"model_name": "m"},
            "tools": [{"kind": "application", "type": "application", "application_id": 2, "application_version_id": 3}]
        }));
        assert_eq!(
            admit(&nested, &[]).unwrap_err().code,
            "nested_agents_unsupported"
        );
        let old_platform = resolved(json!({
            "llm_settings": {"model_name": "m"},
            "tools": [{"kind": "platform_mcp", "type": "mcp_elitea_internal_applications",
                       "server_name": "mcp_elitea_internal_applications"}]
        }));
        assert_eq!(
            admit(&old_platform, &[]).unwrap_err().code,
            "platform_mcp_unsupported"
        );
        let no_model = resolved(json!({"tools": []}));
        assert_eq!(admit(&no_model, &[]).unwrap_err().code, "model_unresolved");
    }

    #[test]
    fn a_platform_mcp_server_with_a_ref_is_one_generic_remote_tool() {
        let admitted = admit(
            &resolved(json!({
                "llm_settings": {"model_name": "m"},
                "tools": [{
                    "kind": "platform_mcp", "type": "mcp_elitea_internal_applications",
                    "server_name": "mcp_elitea_internal_applications",
                    "name": "Elitea Applications", "toolkit_name": "Elitea Applications",
                    "selected_tools": [], "all_tools": true,
                    "toolkit_ref": {"toolkit_id": 0, "project_id": 1, "ref": "tkr1_0123456789abcdef0123456789abcdef"}
                }]
            })),
            &[],
        )
        .unwrap();
        assert_eq!(admitted.remote_tools.len(), 1);
        let spec = &admitted.remote_tools[0];
        assert_eq!(spec.toolkit_id, 0);
        assert_eq!(spec.tool_name, None);
        assert_eq!(spec.exposed_name, "Elitea_Applications_call");
    }

    #[test]
    fn tool_names_are_sanitised_unique_and_avoid_local_names() {
        let mut specs = vec![
            RemoteToolSpec {
                exposed_name: String::new(),
                toolkit_id: 1,
                toolkit_ref: String::new(),
                toolkit_name: "read".into(),
                toolkit_type: String::new(),
                description: String::new(),
                tool_name: Some("file".into()),
            };
            2
        ];
        name_tools(&mut specs, &["read_file"]);
        assert_eq!(specs[0].exposed_name, "read_file_2");
        assert_eq!(specs[1].exposed_name, "read_file_3");
        assert_eq!(sanitize(&"x".repeat(80)).len(), 64);
    }
}
