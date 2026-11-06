//! An agent's soul document (ADR-0050 decision 1a, issue 1604).
//!
//! The soul is the JSON form of a `SOUL.md` file: YAML front matter
//! (`schema: elitea.soul/v1`, `display_name`, `pronouns`, `role`, `voice`,
//! `values`, `boundaries`, `relationships`, `proactive_goals`) and a
//! markdown body. The schema is `AgentSoulV1` in
//! `services/elitea-main/api/openapi/v2.yaml`; Main checks a soul against it
//! before it stores one. [`Soul::admit`] checks it again, the same way, and
//! [`Soul::render`] writes the prompt text.
//!
//! **A soul grants nothing.** [`Soul`] has no field that could name a tool,
//! a credential or a permission, and any unknown key is refused, so a soul
//! can never be read as a grant.
//!
//! **Prompt order.** Platform system text and organization guardrails (the
//! preamble, ADR-0043), then the soul, then the instructions, then recalled
//! memory. [`with_soul`] puts the rendered soul before the instructions; the
//! preamble already precedes both and the memory follows them.
//!
//! [`Soul::to_markdown`] and [`Soul::from_markdown`] are the lossless
//! `SOUL.md` form the desktop reads and writes.

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// The only `schema` value of this version.
pub const SCHEMA_ID: &str = "elitea.soul/v1";
/// The file name of a local soul, next to `AGENTS.md`.
pub const FILE_NAME: &str = "SOUL.md";
/// The most UTF-8 bytes of text (every string value, summed) one soul holds.
pub const MAX_TEXT_BYTES: usize = 16 * 1_024;

const MAX_DISPLAY_NAME: usize = 64;
const MAX_PRONOUNS: usize = 32;
const MAX_ROLE: usize = 160;
const MAX_VOICE: (usize, usize) = (16, 64);
const MAX_VALUES: (usize, usize) = (32, 256);
const MAX_BOUNDARIES: (usize, usize) = (32, 512);
const MAX_RELATIONSHIPS: usize = 32;
const MAX_AGENT_REF: usize = 128;
const MAX_STANCE: usize = 512;
const MAX_GOALS: (usize, usize) = (16, 512);

/// One relationship: how the agent treats another agent or a person.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Relationship {
    /// A registry id or a name.
    pub agent: String,
    pub stance: String,
}

/// An admitted soul document.
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Soul {
    pub schema: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pronouns: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub role: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub voice: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub values: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub boundaries: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub relationships: Vec<Relationship>,
    /// Read only by the heartbeat (ADR-0050 decision 2); not rendered.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub proactive_goals: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub body: String,
}

/// Why a soul was refused. The message names the rule and the field, never
/// a value.
#[derive(Clone, Debug, Eq, PartialEq, thiserror::Error)]
pub enum SoulError {
    #[error("the soul is not an AgentSoulV1 document")]
    Shape,
    #[error("the soul names an unsupported schema")]
    Schema,
    #[error("the soul field {0} is out of bounds")]
    Bounds(&'static str),
    #[error("the soul holds more than {MAX_TEXT_BYTES} bytes of text")]
    TooLarge,
    #[error("the SOUL.md front matter is malformed")]
    FrontMatter,
}

fn bounded(value: &str, max: usize) -> bool {
    !value.is_empty() && value.len() <= max && !value.contains('\0')
}

fn check_optional(
    value: Option<&String>,
    max: usize,
    field: &'static str,
) -> Result<(), SoulError> {
    match value {
        Some(text) if !bounded(text, max) => Err(SoulError::Bounds(field)),
        _ => Ok(()),
    }
}

fn check_list(
    items: &[String],
    (count, each): (usize, usize),
    field: &'static str,
) -> Result<(), SoulError> {
    if items.len() > count || items.iter().any(|item| !bounded(item, each)) {
        return Err(SoulError::Bounds(field));
    }
    Ok(())
}

impl Soul {
    /// Admit a soul from the JSON Main sends (`version_details.soul`).
    /// `None` and JSON null are "no soul".
    ///
    /// # Errors
    ///
    /// [`SoulError`] when the value is not an `AgentSoulV1` document.
    pub fn admit(value: Option<&Value>) -> Result<Option<Self>, SoulError> {
        match value {
            None | Some(Value::Null) => Ok(None),
            Some(value @ Value::Object(_)) => {
                let soul: Self =
                    serde_json::from_value(value.clone()).map_err(|_| SoulError::Shape)?;
                soul.check()?;
                Ok(Some(soul))
            }
            Some(_) => Err(SoulError::Shape),
        }
    }

    /// The `AgentSoulV1` bounds.
    ///
    /// # Errors
    ///
    /// The first bound the soul breaks.
    pub fn check(&self) -> Result<(), SoulError> {
        if self.schema != SCHEMA_ID {
            return Err(SoulError::Schema);
        }
        check_optional(self.display_name.as_ref(), MAX_DISPLAY_NAME, "display_name")?;
        check_optional(self.pronouns.as_ref(), MAX_PRONOUNS, "pronouns")?;
        check_optional(self.role.as_ref(), MAX_ROLE, "role")?;
        check_list(&self.voice, MAX_VOICE, "voice")?;
        check_list(&self.values, MAX_VALUES, "values")?;
        check_list(&self.boundaries, MAX_BOUNDARIES, "boundaries")?;
        check_list(&self.proactive_goals, MAX_GOALS, "proactive_goals")?;
        if self.relationships.len() > MAX_RELATIONSHIPS
            || self
                .relationships
                .iter()
                .any(|r| !bounded(&r.agent, MAX_AGENT_REF) || !bounded(&r.stance, MAX_STANCE))
        {
            return Err(SoulError::Bounds("relationships"));
        }
        if self.body.contains('\0') || self.body.len() > MAX_TEXT_BYTES {
            return Err(SoulError::Bounds("body"));
        }
        if self.text_bytes() > MAX_TEXT_BYTES {
            return Err(SoulError::TooLarge);
        }
        Ok(())
    }

    /// The UTF-8 bytes of every string value, summed (Main's measure).
    #[must_use]
    pub fn text_bytes(&self) -> usize {
        let lists = [
            &self.voice,
            &self.values,
            &self.boundaries,
            &self.proactive_goals,
        ];
        self.schema.len()
            + [&self.display_name, &self.pronouns, &self.role]
                .iter()
                .map(|value| value.as_ref().map_or(0, String::len))
                .sum::<usize>()
            + lists
                .iter()
                .flat_map(|list| list.iter())
                .map(String::len)
                .sum::<usize>()
            + self
                .relationships
                .iter()
                .map(|r| r.agent.len() + r.stance.len())
                .sum::<usize>()
            + self.body.len()
    }

    /// The prompt text of the soul: a `## Persona` section. Empty when the
    /// soul carries nothing to render (`proactive_goals` is not rendered).
    #[must_use]
    pub fn render(&self) -> String {
        let mut lines: Vec<String> = Vec::new();
        match (&self.display_name, &self.pronouns) {
            (Some(name), Some(pronouns)) => lines.push(format!("You are {name} ({pronouns}).")),
            (Some(name), None) => lines.push(format!("You are {name}.")),
            (None, Some(pronouns)) => lines.push(format!("Your pronouns are {pronouns}.")),
            (None, None) => {}
        }
        if let Some(role) = &self.role {
            lines.push(format!("Role: {role}"));
        }
        if !self.voice.is_empty() {
            lines.push(format!("Voice: {}", self.voice.join(", ")));
        }
        let list = |lines: &mut Vec<String>, heading: &str, items: &[String]| {
            if !items.is_empty() {
                lines.push(format!("{heading}:"));
                lines.extend(items.iter().map(|item| format!("- {item}")));
            }
        };
        list(&mut lines, "Values", &self.values);
        list(&mut lines, "Boundaries", &self.boundaries);
        if !self.relationships.is_empty() {
            lines.push("Relationships:".to_owned());
            lines.extend(
                self.relationships
                    .iter()
                    .map(|r| format!("- {}: {}", r.agent, r.stance)),
            );
        }
        let body = self.body.trim();
        if lines.is_empty() && body.is_empty() {
            return String::new();
        }
        let mut text = String::from("## Persona\n\n");
        text.push_str(&lines.join("\n"));
        if !body.is_empty() {
            if !lines.is_empty() {
                text.push_str("\n\n");
            }
            text.push_str(body);
        }
        text
    }

    /// The `SOUL.md` text: YAML front matter (every field but `body`), then
    /// the body. [`Self::from_markdown`] reads it back to an equal soul.
    #[must_use]
    pub fn to_markdown(&self) -> String {
        let front = Self {
            body: String::new(),
            ..self.clone()
        };
        let yaml = serde_yaml_ng::to_string(&front).unwrap_or_default();
        let mut text = format!("---\n{yaml}---\n");
        if !self.body.is_empty() {
            text.push('\n');
            text.push_str(&self.body);
            if !self.body.ends_with('\n') {
                text.push('\n');
            }
        }
        text
    }

    /// Read a `SOUL.md`: front matter between `---` lines, then the body.
    ///
    /// # Errors
    ///
    /// [`SoulError::FrontMatter`] for a file with no closed front matter or
    /// malformed YAML; any [`Self::check`] refusal.
    pub fn from_markdown(text: &str) -> Result<Self, SoulError> {
        if text.len() > MAX_TEXT_BYTES * 4 {
            return Err(SoulError::TooLarge);
        }
        let rest = text
            .strip_prefix("---\n")
            .or_else(|| text.strip_prefix("---\r\n"))
            .ok_or(SoulError::FrontMatter)?;
        let (yaml, after) = split_front_matter(rest).ok_or(SoulError::FrontMatter)?;
        let mut soul: Self = crate::bounded_yaml::from_str(
            yaml,
            crate::bounded_yaml::YamlBudget {
                nodes: 1_024,
                scalar_bytes: MAX_TEXT_BYTES,
                depth: 4,
            },
        )
        .map_err(|_| SoulError::FrontMatter)?;
        if !soul.body.is_empty() {
            // The body is the text after the front matter, never a key.
            return Err(SoulError::Shape);
        }
        let body = after
            .strip_prefix("\r\n")
            .or_else(|| after.strip_prefix('\n'))
            .unwrap_or(after);
        body.strip_suffix('\n')
            .unwrap_or(body)
            .clone_into(&mut soul.body);
        soul.check()?;
        Ok(soul)
    }
}

/// The YAML before the closing `---` line, and the text after that line.
fn split_front_matter(rest: &str) -> Option<(&str, &str)> {
    let mut offset = 0;
    for line in rest.split_inclusive('\n') {
        if line.trim_end_matches(['\r', '\n']) == "---" {
            return Some((&rest[..offset], &rest[offset + line.len()..]));
        }
        offset += line.len();
    }
    None
}

/// The system text of an agent with a soul: the rendered soul, then the
/// instructions after a blank line. Without a soul (or with one that renders
/// nothing) the instructions unchanged.
#[must_use]
pub fn with_soul(soul: Option<&Soul>, instructions: &str) -> String {
    let rendered = soul.map(Soul::render).unwrap_or_default();
    match (rendered.is_empty(), instructions.is_empty()) {
        (true, _) => instructions.to_owned(),
        (false, true) => rendered,
        (false, false) => format!("{rendered}\n\n{instructions}"),
    }
}

#[cfg(test)]
#[path = "soul_tests.rs"]
mod tests;
