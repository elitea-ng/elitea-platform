//! Stored Code-node compatibility contract, independent of sandbox deployment.

#![allow(dead_code)] // Execution binding remains gated on sandbox admission.

use std::borrow::Cow;
use std::collections::{BTreeMap, BTreeSet};

use adk_rust::graph::State;
use ring::digest;
use serde::{Deserialize, Serialize};

use super::compiler::reserved_user_state_key;
use super::yaml::{valid_graph_id, valid_output_key};

const MAX_NODE_BYTES: usize = 512 * 1024;
const MAX_SOURCE_BYTES: usize = 256 * 1024;
const MAX_VARIABLES: usize = 256;
const MAX_DEPENDENCY_BYTES: usize = 64 * 1024;
const MAX_DEPENDENCIES: usize = 128;

#[derive(Clone, Copy, Default, Deserialize, Serialize, PartialEq, Eq, Debug)]
#[serde(rename_all = "lowercase")]
pub(super) enum CodeLanguage {
    #[default]
    Python,
    JavaScript,
    TypeScript,
    Rust,
}

// Deliberately omit Debug: source and state-variable names can contain private data.
#[derive(Clone, Deserialize, Serialize)]
#[serde(
    tag = "type",
    content = "value",
    rename_all = "lowercase",
    deny_unknown_fields
)]
enum CodeSource {
    Fixed(String),
    Variable(String),
    Fstring(String),
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct CodeNodeDefinition {
    id: String,
    #[serde(skip)]
    digest: [u8; 32],
    #[serde(skip)]
    debug_definition: Option<super::code_debug::CodeDebugDefinitionPin>,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    language: CodeLanguage,
    #[serde(
        default,
        deserialize_with = "deserialize_dependencies",
        skip_serializing_if = "Option::is_none"
    )]
    dependencies: Option<String>,
    #[serde(deserialize_with = "deserialize_code_source")]
    code: CodeSource,
    #[serde(
        default,
        deserialize_with = "deserialize_workspace",
        skip_serializing_if = "Option::is_none"
    )]
    workspace: Option<crate::sandbox::workspace::WorkspaceSelection>,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
    #[serde(default)]
    structured_output: bool,
    #[serde(default)]
    debug: bool,
    #[serde(default, skip_serializing_if = "code_platform_false")]
    platform_client: bool,
    #[serde(default)]
    transition: Option<String>,
}

#[allow(
    clippy::trivially_copy_pass_by_ref,
    reason = "Preserve the existing caller and protocol conversion signature."
)]
fn code_platform_false(value: &bool) -> bool {
    !*value
}

fn deserialize_workspace<'de, D>(
    deserializer: D,
) -> Result<Option<crate::sandbox::workspace::WorkspaceSelection>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    // Only omission preserves no-workspace semantics. Null cannot remove saved policy.
    crate::sandbox::workspace::WorkspaceSelection::deserialize(deserializer).map(Some)
}

fn deserialize_dependencies<'de, D>(deserializer: D) -> Result<Option<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    // Only omission means no declaration. Null cannot discard saved dependencies.
    String::deserialize(deserializer).map(Some)
}

fn deserialize_code_source<'de, D>(deserializer: D) -> Result<CodeSource, D::Error>
where
    D: serde::Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum StoredSource {
        Mapping(CodeSource),
        Legacy(String),
    }
    Ok(match StoredSource::deserialize(deserializer)? {
        StoredSource::Mapping(source) => source,
        StoredSource::Legacy(source) => CodeSource::Fixed(source),
    })
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum CodeProvenance {
    SavedLiteral,
    StateVariable,
    StateTemplate,
}

/// Immutable bytes resolved from the frozen mapping and one graph state snapshot.
/// No Debug or serialization: source stays on the authorized execution data plane.
pub(super) struct ResolvedCode<'a> {
    source: Cow<'a, str>,
    provenance: CodeProvenance,
}

impl ResolvedCode<'_> {
    pub(super) fn source(&self) -> &str {
        &self.source
    }

    pub(super) fn provenance(&self) -> CodeProvenance {
        self.provenance
    }
}

impl CodeNodeDefinition {
    pub(super) fn id(&self) -> &str {
        &self.id
    }
    pub(super) fn input_keys(&self) -> &[String] {
        &self.input
    }
    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }
    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }
    pub(super) fn bind_debug_definition(&mut self, definition: [u8; 32], yaml: [u8; 32]) {
        self.debug_definition =
            Some(super::code_debug::CodeDebugDefinitionPin { definition, yaml });
    }
    pub(super) fn refresh_debug_compiler_digest(&mut self, definition: [u8; 32]) {
        if let Some(original) = self.debug_definition.as_mut() {
            original.definition = definition;
        }
    }
    pub(super) fn original_declaration(
        &self,
        context: &adk_rust::graph::NodeContext,
    ) -> Result<Option<super::code_workspace::CodeOriginalDeclaration>, &'static str> {
        let Some(pin) = self.debug_definition.as_ref() else {
            return Ok(None);
        };
        Ok(Some(super::code_workspace::CodeOriginalDeclaration {
            node_id: self.id.clone(),
            graph_thread_id: context.config.thread_id.clone(),
            graph_step: u64::try_from(context.step).map_err(|_| "Code graph step is invalid")?,
            configuration_json: serde_json::to_string(self)
                .map_err(|_| "Code declaration configuration is invalid")?,
            definition: pin.definition,
            yaml: pin.yaml,
        }))
    }
    pub(super) fn workspace_selection(
        &self,
        context: &adk_rust::graph::NodeContext,
    ) -> Result<Option<super::code_workspace::CodeWorkspaceInvocation>, &'static str> {
        let Some(selection) = self.workspace.as_ref() else {
            return Ok(None);
        };
        selection
            .validate_declaration()
            .map_err(|_| "Code workspace declaration is invalid")?;
        let declaration = self
            .original_declaration(context)?
            .ok_or("Code workspace has no original saved declaration")?;
        Ok(Some(super::code_workspace::CodeWorkspaceInvocation {
            selection: selection.clone(),
            declaration,
        }))
    }
    pub(super) fn debug_selection(
        &self,
        context: &adk_rust::graph::NodeContext,
    ) -> Result<Option<super::code_debug::CodeDebugSelection>, &'static str> {
        if !self.debug {
            return Ok(None);
        }
        Ok(Some(super::code_debug::CodeDebugSelection {
            node_id: self.id.clone(),
            graph_thread_id: context.config.thread_id.clone(),
            graph_step: context.step.to_string(),
            configuration_json: serde_json::to_string(self)
                .map_err(|_| "Code debug configuration is invalid")?,
            original: self.debug_definition.clone(),
        }))
    }
    pub(super) fn platform_client(&self) -> bool {
        self.platform_client
    }

    pub(super) fn structured_output(&self) -> bool {
        self.structured_output
    }

    pub(super) fn from_yaml(yaml: &str) -> Result<Self, &'static str> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_BYTES {
            return Err("the Code node exceeds its configuration size limit");
        }
        let mut node: Self = serde_yaml_ng::from_str(yaml).map_err(
            |_| "the Code node has malformed fields or an unsupported language or source mapping",
        )?;
        if node.node_type != "code" || !valid_graph_id(&node.id) {
            return Err("the Code node type or identifier is invalid");
        }
        if let Some(dependencies) = &node.dependencies {
            if node.language != CodeLanguage::Rust {
                return Err("Cargo dependencies require the Rust Code language");
            }
            validate_dependencies(dependencies)?;
        }
        if node
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err("the Code node transition is invalid");
        }
        for selection in [&node.input, &node.output] {
            let mut seen = BTreeSet::new();
            if selection.len() > MAX_VARIABLES
                || selection
                    .iter()
                    .any(|key| !valid_output_key(key) || !seen.insert(key))
            {
                return Err("the Code node variable selection is invalid or duplicated");
            }
        }
        match &node.code {
            CodeSource::Fixed(source) => validate_source(source)?,
            CodeSource::Variable(key) => {
                if !valid_output_key(key) || reserved_user_state_key(key) || key == "messages" {
                    return Err("the Code source variable is invalid or reserved");
                }
            }
            CodeSource::Fstring(template) => {
                validate_source(template)?;
                visit_template(template, |_| Ok(()))?;
            }
        }
        if let Some(workspace) = &node.workspace {
            workspace
                .validate_declaration()
                .map_err(|_| "Code workspace declaration is invalid")?;
        }
        node.digest = node.config_digest()?;
        Ok(node)
    }

    pub(super) fn validated_digest(&self) -> [u8; 32] {
        self.digest
    }

    pub(super) fn language(&self) -> CodeLanguage {
        self.language
    }

    pub(super) fn dependencies_toml(&self) -> Option<&str> {
        self.dependencies.as_deref()
    }

    /// Preserve source provenance when deciding whether an execution needs approval.
    /// A saved variable mapping does not make its current value saved literal code.
    pub(super) fn resolve_source<'a>(
        &'a self,
        state: &'a State,
        declared_types: &BTreeMap<String, String>,
    ) -> Result<ResolvedCode<'a>, &'static str> {
        match &self.code {
            CodeSource::Fixed(source) => Ok(ResolvedCode {
                source: Cow::Borrowed(source),
                provenance: CodeProvenance::SavedLiteral,
            }),
            CodeSource::Variable(key) => {
                validate_source_variable(key, declared_types)?;
                let source = state
                    .get(key)
                    .and_then(serde_json::Value::as_str)
                    .ok_or("the Code source variable is missing or is not a string")?;
                validate_source(source)?;
                Ok(ResolvedCode {
                    source: Cow::Borrowed(source),
                    provenance: CodeProvenance::StateVariable,
                })
            }
            CodeSource::Fstring(template) => {
                let mut rendered = String::new();
                visit_template(template, |part| {
                    let value = match part {
                        TemplatePart::Literal(value) => Cow::Borrowed(value),
                        TemplatePart::Variable(key) => {
                            resolve_template_variable(key, state, declared_types)?
                        }
                    };
                    if value.len() > MAX_SOURCE_BYTES.saturating_sub(rendered.len()) {
                        return Err("the Code template expansion exceeds its 256 KiB limit");
                    }
                    rendered.push_str(&value);
                    Ok(())
                })?;
                validate_source(&rendered)?;
                Ok(ResolvedCode {
                    source: Cow::Owned(rendered),
                    provenance: CodeProvenance::StateTemplate,
                })
            }
        }
    }

    /// Validate source references during graph binding, before any runtime effects.
    pub(super) fn validate_source_types(
        &self,
        declared_types: &BTreeMap<String, String>,
    ) -> Result<(), &'static str> {
        match &self.code {
            CodeSource::Fixed(_) => Ok(()),
            CodeSource::Variable(key) => validate_source_variable(key, declared_types),
            CodeSource::Fstring(template) => visit_template(template, |part| match part {
                TemplatePart::Literal(_) => Ok(()),
                TemplatePart::Variable(key) => validate_template_variable(key, declared_types),
            }),
        }
    }

    /// Include language, mapping kind, and all execution-relevant YAML settings.
    pub(super) fn config_digest(&self) -> Result<[u8; 32], &'static str> {
        let bytes =
            serde_json::to_vec(self).map_err(|_| "the Code configuration cannot be encoded")?;
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(b"elitea.graph.code.config.v1\0");
        context.update(&bytes);
        let mut result = [0; 32];
        result.copy_from_slice(context.finish().as_ref());
        Ok(result)
    }
}

fn validate_source_variable(
    key: &str,
    declared_types: &BTreeMap<String, String>,
) -> Result<(), &'static str> {
    if key != "input" && declared_types.get(key).map(String::as_str) != Some("str") {
        return Err("the Code source variable must be declared as a string");
    }
    if key == "input" && declared_types.get(key).is_some_and(|kind| kind != "str") {
        return Err("the Code source variable must be declared as a string");
    }
    Ok(())
}

fn validate_template_variable(
    key: &str,
    declared_types: &BTreeMap<String, String>,
) -> Result<(), &'static str> {
    if key == "input" {
        return validate_source_variable(key, declared_types);
    }
    if !declared_types
        .get(key)
        .is_some_and(|kind| matches!(kind.as_str(), "str" | "int"))
    {
        return Err("the Code template variable must be declared as a string or integer");
    }
    Ok(())
}

fn resolve_template_variable<'a>(
    key: &str,
    state: &'a State,
    declared_types: &BTreeMap<String, String>,
) -> Result<Cow<'a, str>, &'static str> {
    const INVALID: &str =
        "the Code template variable is missing or has an invalid string or integer value";
    validate_template_variable(key, declared_types)?;
    let value = state.get(key).ok_or(INVALID)?;
    if key != "input" && declared_types.get(key).is_some_and(|kind| kind == "int") {
        if value.as_i64().is_none() && value.as_u64().is_none() {
            return Err(INVALID);
        }
        // Native JSON integers have the same decimal text as SDK str(int).
        Ok(Cow::Owned(value.to_string()))
    } else {
        value.as_str().map(Cow::Borrowed).ok_or(INVALID)
    }
}

enum TemplatePart<'a> {
    Literal(&'a str),
    Variable(&'a str),
}

/// Support named fields and escaped braces without expression evaluation.
/// Bound every field before state access; never keep unresolved executable fields.
fn visit_template(
    template: &str,
    mut visit: impl FnMut(TemplatePart<'_>) -> Result<(), &'static str>,
) -> Result<(), &'static str> {
    const INVALID: &str = "the Code template has malformed braces or an unsupported field";
    let mut cursor = 0;
    let mut fields = 0;
    while let Some(relative) = template[cursor..].find(['{', '}']) {
        let index = cursor + relative;
        visit(TemplatePart::Literal(&template[cursor..index]))?;
        let brace = template.as_bytes()[index];
        if template.as_bytes().get(index + 1) == Some(&brace) {
            visit(TemplatePart::Literal(&template[index..=index]))?;
            cursor = index + 2;
            continue;
        }
        if brace == b'}' {
            return Err(INVALID);
        }
        let end = index + 1 + template[index + 1..].find('}').ok_or(INVALID)?;
        let key = &template[index + 1..end];
        if !valid_output_key(key)
            || !key.bytes().enumerate().all(|(index, byte)| {
                byte == b'_' || byte.is_ascii_alphabetic() || (index != 0 && byte.is_ascii_digit())
            })
        {
            return Err(INVALID);
        }
        if reserved_user_state_key(key) || key == "messages" {
            return Err("the Code template field is reserved");
        }
        fields += 1;
        if fields > MAX_VARIABLES {
            return Err("the Code template exceeds its 256-field limit");
        }
        visit(TemplatePart::Variable(key))?;
        cursor = end + 1;
    }
    visit(TemplatePart::Literal(&template[cursor..]))
}

fn validate_dependencies(declaration: &str) -> Result<(), &'static str> {
    const INVALID: &str = "Cargo dependencies must contain a bounded [dependencies] table with registry versions and supported options";
    if declaration.trim().is_empty()
        || declaration.len() > MAX_DEPENDENCY_BYTES
        || declaration.contains('\0')
    {
        return Err(INVALID);
    }
    let manifest: toml::Table = declaration.parse().map_err(|_| INVALID)?;
    let dependencies = manifest
        .get("dependencies")
        .and_then(toml::Value::as_table)
        .ok_or(INVALID)?;
    if manifest.len() != 1 || dependencies.is_empty() || dependencies.len() > MAX_DEPENDENCIES {
        return Err(INVALID);
    }
    for (name, dependency) in dependencies {
        if !valid_dependency_name(name) || name.replace('-', "_") == "serde_json" {
            return Err(INVALID);
        }
        match dependency {
            toml::Value::String(version) if valid_dependency_text(version) => {}
            toml::Value::Table(options) => validate_dependency_options(options).ok_or(INVALID)?,
            _ => return Err(INVALID),
        }
    }
    Ok(())
}

fn validate_dependency_options(options: &toml::Table) -> Option<()> {
    let version = options.get("version")?.as_str()?;
    if !valid_dependency_text(version) {
        return None;
    }
    for (key, value) in options {
        match key.as_str() {
            "version" => {}
            "package" => {
                let package = value.as_str()?;
                if !valid_dependency_name(package) || package.replace('-', "_") == "serde_json" {
                    return None;
                }
            }
            "default-features" => {
                value.as_bool()?;
            }
            "features" => {
                let features = value.as_array()?;
                if features.len() > MAX_DEPENDENCIES
                    || features
                        .iter()
                        .any(|feature| !feature.as_str().is_some_and(valid_dependency_feature))
                {
                    return None;
                }
            }
            _ => return None,
        }
    }
    Some(())
}

fn valid_dependency_feature(value: &str) -> bool {
    !value.is_empty() && value.len() <= 128 && value.bytes().all(|byte| byte.is_ascii_graphic())
}

fn valid_dependency_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name.as_bytes()[0].is_ascii_alphabetic()
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
}

fn valid_dependency_text(value: &str) -> bool {
    !value.trim().is_empty()
        && value.len() <= 256
        && value.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
}

fn validate_source(source: &str) -> Result<(), &'static str> {
    if source.trim().is_empty() || source.contains('\0') {
        return Err("the Code source is empty or contains a null byte");
    }
    if source.len() > MAX_SOURCE_BYTES {
        return Err("the Code source exceeds its 256 KiB limit");
    }
    Ok(())
}
