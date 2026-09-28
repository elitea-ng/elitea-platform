//! Stored Code-node compatibility contract, independent of sandbox deployment.

#![allow(dead_code)] // Execution binding remains gated on sandbox admission.

use std::collections::{BTreeMap, BTreeSet};

use adk_rust::graph::State;
use ring::digest;
use serde::{Deserialize, Serialize};

use super::compiler::reserved_user_state_key;
use super::yaml::{valid_graph_id, valid_output_key};

const MAX_NODE_BYTES: usize = 512 * 1024;
const MAX_SOURCE_BYTES: usize = 256 * 1024;
const MAX_VARIABLES: usize = 256;

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
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct CodeNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    language: CodeLanguage,
    #[serde(deserialize_with = "deserialize_code_source")]
    code: CodeSource,
    #[serde(default)]
    input: Vec<String>,
    #[serde(default)]
    output: Vec<String>,
    #[serde(default)]
    structured_output: bool,
    #[serde(default)]
    debug: bool,
    #[serde(default)]
    transition: Option<String>,
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
}

pub(super) struct ResolvedCode<'a> {
    pub(super) source: &'a str,
    pub(super) provenance: CodeProvenance,
}

impl CodeNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, &'static str> {
        if yaml.is_empty() || yaml.len() > MAX_NODE_BYTES {
            return Err("the Code node exceeds its configuration size limit");
        }
        let node: Self = serde_yaml_ng::from_str(yaml).map_err(
            |_| "the Code node has malformed fields or an unsupported language or source mapping",
        )?;
        if node.node_type != "code" || !valid_graph_id(&node.id) {
            return Err("the Code node type or identifier is invalid");
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
        }
        Ok(node)
    }

    pub(super) fn language(&self) -> CodeLanguage {
        self.language
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
                source,
                provenance: CodeProvenance::SavedLiteral,
            }),
            CodeSource::Variable(key) => {
                if key != "input" && declared_types.get(key).map(String::as_str) != Some("str") {
                    return Err("the Code source variable must be declared as a string");
                }
                let source = state
                    .get(key)
                    .and_then(serde_json::Value::as_str)
                    .ok_or("the Code source variable is missing or is not a string")?;
                validate_source(source)?;
                Ok(ResolvedCode {
                    source,
                    provenance: CodeProvenance::StateVariable,
                })
            }
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

fn validate_source(source: &str) -> Result<(), &'static str> {
    if source.trim().is_empty() || source.contains('\0') {
        return Err("the Code source is empty or contains a null byte");
    }
    if source.len() > MAX_SOURCE_BYTES {
        return Err("the Code source exceeds its 256 KiB limit");
    }
    Ok(())
}
