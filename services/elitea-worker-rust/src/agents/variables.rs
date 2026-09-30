//! Agent-variable substitution over a stored version's `instructions`.
//!
//! # The contract this ports, measured rather than assumed
//!
//! The SDK worker resolves variables with REAL Jinja2, not `{{name}}` string
//! replacement. Measured at the revision this repository pins —
//! `services/elitea-worker-python/elitea-sdk.lock.json` names elitea-sdk
//! `0.9.8` @ `b5113a12`, whose two applied patches touch MCP only:
//!
//! * `elitea_sdk/runtime/langchain/assistant.py:557-576` builds
//!   `prompt_variables` from `data['variables']` — a LIST of `{name, value}`
//!   rows — keeping only entries that are dicts with a truthy `name` and a
//!   `value` that is neither `None` nor `''` ("empty values are runtime
//!   placeholders"). It then `update()`s that map from `meta['variables']`,
//!   but ONLY when that key holds a dict.
//! * `elitea_sdk/runtime/clients/client.py:822-825` first folds the request's
//!   own `application.variables` into `data['variables']` by name:
//!   `if var['name'] in application_variables: var.update(...)`. Names that
//!   are not already in the version's list are ignored — the request can
//!   re-VALUE a declared variable, never declare one.
//! * `assistant.py:597-657` (`_resolve_jinja2_variables`) then renders:
//!   * a FAST PATH returns the string untouched when it contains no `{{` —
//!     so a template made only of `{% %}` blocks or `{# #}` comments is never
//!     rendered at all, and this port reproduces that exactly;
//!   * the context is `{'current_date': '%Y-%m-%d'}` first, then the captured
//!     variables — so a variable actually NAMED `current_date` wins;
//!   * `SandboxedEnvironment(undefined=DebugUndefined)` — an undefined name
//!     prints as the literal `{{ name }}` (Jinja2's `DebugUndefined` renders
//!     its own canonical spacing, whatever the source spelling was), it does
//!     not blank out and it does not raise;
//!   * EVERY exception is caught and the ORIGINAL, unrendered template is
//!     returned. A malformed template therefore reaches the model verbatim
//!     instead of ending the turn.
//! * `assistant.py:794` calls it for the react-agent path (`agent`/`predict`,
//!   which is both the stored-agent and the ad-hoc profile), and
//!   `assistant.py:886-905` (`pipeline()`) does NOT: pipeline `instructions`
//!   carry the graph YAML and are handed to `create_graph` unrendered. This
//!   port keeps that asymmetry.
//!
//! # Why `minijinja`, and why that is not a new dependency
//!
//! Because the measured contract is Jinja2, a hand-rolled `{{name}}` replacer
//! would be a different language: it would mis-handle `{% if %}`/`{% for %}`
//! bodies, filters and whitespace control the moment an author uses them, and
//! would silently disagree with the SDK worker on the same stored agent.
//! `minijinja` is already a direct dependency of this crate (pinned
//! `=2.23.0`, `Cargo.toml`), already used by the pipeline graph's
//! `state_modifier`/`router` nodes, so this adds ZERO new crates to the tree —
//! it reuses the one that is there. It is configured here to match Jinja2 for
//! the constructs the product actually authors:
//!
//! | Jinja2 (SDK)                     | here                                        |
//! |----------------------------------|---------------------------------------------|
//! | `autoescape=False`               | template name `<string>` ⇒ `AutoEscape::None` |
//! | `keep_trailing_newline=False`    | minijinja's default is the same             |
//! | `undefined=DebugUndefined`       | every undeclared name is pre-seeded with the |
//! |                                  | literal `{{ name }}` (see `render_with_date`) |
//! | unbounded evaluation             | `set_fuel` + a bounded writer, and an        |
//! |                                  | exhausted bound takes the SDK's own          |
//! |                                  | "return the template unrendered" branch      |
//!
//! DISCLOSED DIVERGENCE, deliberately small: Jinja2's `DebugUndefined` is
//! FALSY, while the `{{ name }}` string this port seeds for an undefined name
//! is truthy, so `{% if undefined_name %}` takes the other branch. Nothing the
//! variables panel or `contextResolver` (`apps/elitea-web/src/shared/lib/
//! string.ts`, which derives names from `{{name}}` occurrences alone) can
//! author reaches that construct; printing is the whole product surface, and
//! printing is exact.
//!
//! # Where the values come from on THIS platform
//!
//! The authored rows have no COLUMN: they live in `application_versions.meta`
//! under `variables`, as an ARRAY, written by `internal/api/v2/applications/
//! handler.go:511-512` (create) and `:915-933` (update), and read back out
//! under the `variables` key by that handler's own `versionDetailsResponse`
//! (:547, :568). This module therefore reads BOTH keys, and that is not
//! belt-and-braces: for a long time only ONE of them was populated.
//!
//! HISTORY, because it explains the shape of `admit`. Until
//! `application_version_details_json` was fixed,
//! `services/elitea-main/internal/db/queries/agent_chat.sql` built
//! `'variables', '[]'::jsonb` unconditionally, so `meta.variables` was the
//! only place this runtime could find them — and the SDK worker, whose
//! `meta['variables']` branch is guarded by `isinstance(..., dict)`
//! (`assistant.py:574`), could not read the array at all and substituted
//! nothing. That projection now carries the version's real list, which is
//! what makes the SDK worker substitute unpatched
//! (`services/elitea-worker-python/tests/unit/test_agent_variables.py`), and
//! `apps/elitea-web/e2e/streaming/chat.variables.spec.ts` now pins the SAME
//! journal assertion on both runtimes rather than on this one alone.
//!
//! Nothing here changed for it: both keys resolve to the same rows, `meta` is
//! still applied last, and this module keeps reproducing the SDK's SEMANTICS
//! over the values the platform really stores.

use std::collections::BTreeMap;
use std::io;

use chrono::Local;
use minijinja::{Environment, UndefinedBehavior};
use serde_json::{Map, Value};

use super::assembly::{invalid_profile, resource_exhausted_profile};
use super::runtime::NativeAgentAssemblyError;

/// One version cannot declare more variables than the edit page can show.
const MAX_VARIABLES: usize = 256;
const MAX_VARIABLE_NAME_BYTES: usize = 256;
/// `instructions` itself is bounded at 64 KiB, so one substituted value that
/// is larger than the whole prompt is already outside the authored shape.
const MAX_VARIABLE_VALUE_BYTES: usize = 64 * 1_024;
/// Rendering may legitimately grow the prompt (a loop over a value), but not
/// without bound: the rendered string becomes a provider request body.
const MAX_RENDERED_BYTES: usize = super::request::MAX_AGENT_INSTRUCTION_BYTES;
const TEMPLATE_FUEL: u64 = 250_000;

/// The captured `{name: value}` context for one version's instructions.
#[derive(Clone, Debug, Default)]
pub(crate) struct AgentVariables {
    values: BTreeMap<String, String>,
}

impl AgentVariables {
    /// Admit one version's variables in the SDK's own precedence order.
    ///
    /// `participant` is the request-level `application.variables` array —
    /// per-conversation VALUES for variables the version already declares
    /// (`chat_participant_mapping.entity_settings -> 'variables'`, projected
    /// by `agent_chat.sql:8`). Exactly as `client.py:822-825` does, a name the
    /// version does not declare is ignored rather than introduced, and
    /// `meta.variables` is applied last so it still wins — that is the order
    /// `assistant.py:562-575` builds the same map in.
    pub(super) fn admit(
        version: &Map<String, Value>,
        participant: Option<&Value>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let mut values = BTreeMap::new();
        for (name, value) in capture_variables(version.get("variables"))? {
            values.insert(name, value);
        }
        for (name, value) in capture_variables(participant)? {
            if let Some(declared) = values.get_mut(&name) {
                *declared = value;
            }
        }
        for (name, value) in capture_variables(meta_variables(version.get("meta"))?)? {
            values.insert(name, value);
        }
        Ok(Self { values })
    }

    /// Render one stored `instructions` string, or return it untouched.
    ///
    /// Never fails: every failure mode the SDK catches (`assistant.py:656`)
    /// yields the original template, because a prompt that reaches the model
    /// with its braces intact is what the SDK worker would have sent, and it
    /// is strictly better than ending the turn over an author's typo.
    pub(super) fn render(&self, instructions: &str) -> String {
        self.render_with_date(instructions, &Local::now().format("%Y-%m-%d").to_string())
    }

    fn render_with_date(&self, instructions: &str, current_date: &str) -> String {
        // `assistant.py:620-622`: no `{{`, no rendering — not even parsing.
        if !instructions.contains("{{") {
            return instructions.to_owned();
        }
        match self.try_render(instructions, current_date) {
            Ok(rendered) => rendered,
            Err(reason) => {
                tracing::warn!(
                    event = "agent_variable_render_skipped",
                    reason_code = reason,
                    "the agent instructions could not be rendered; sending them unsubstituted"
                );
                instructions.to_owned()
            }
        }
    }

    fn try_render(&self, instructions: &str, current_date: &str) -> Result<String, &'static str> {
        let mut environment = Environment::new();
        environment.set_undefined_behavior(UndefinedBehavior::Lenient);
        environment.set_fuel(Some(TEMPLATE_FUEL));
        let template = environment
            .template_from_str(instructions)
            .map_err(|_| "template_syntax")?;
        let mut context = BTreeMap::new();
        // `DebugUndefined` equivalence: a name nothing defines prints as the
        // literal `{{ name }}` rather than blanking out. Seeded FIRST so any
        // name that is actually defined below replaces its own placeholder.
        for name in template.undeclared_variables(false) {
            let placeholder = format!("{{{{ {name} }}}}");
            context.insert(name, placeholder);
        }
        context.insert("current_date".to_owned(), current_date.to_owned());
        for (name, value) in &self.values {
            context.insert(name.clone(), value.clone());
        }
        let mut writer = BoundedWriter::new(MAX_RENDERED_BYTES);
        template
            .render_captured_to(&context, &mut writer)
            .map_err(|_| "template_evaluation")?;
        String::from_utf8(writer.into_inner()).map_err(|_| "template_encoding")
    }
}

/// Frozen declared variables for one saved Agent's invocation-local bindings.
/// Empty defaults still declare a variable. Call values never introduce names.
#[derive(Clone, Debug)]
pub(super) struct AgentCallVariables {
    defaults: AgentVariables,
    names: std::collections::BTreeSet<String>,
}

impl AgentCallVariables {
    pub(super) fn admit(version: &Map<String, Value>) -> Result<Self, NativeAgentAssemblyError> {
        let defaults = AgentVariables::admit(version, None)?;
        let mut names = std::collections::BTreeSet::new();
        for collection in [
            version.get("variables"),
            meta_variables(version.get("meta"))?,
        ] {
            match collection {
                Some(Value::Array(rows)) => {
                    for row in rows {
                        if let Some(name) = row
                            .get("name")
                            .and_then(Value::as_str)
                            .filter(|name| bounded_variable_name(name))
                        {
                            names.insert(name.to_owned());
                        }
                    }
                }
                Some(Value::Object(rows)) => {
                    names.extend(
                        rows.keys()
                            .filter(|name| bounded_variable_name(name))
                            .cloned(),
                    );
                }
                _ => {}
            }
        }
        if names.len() > MAX_VARIABLES {
            return Err(resource_exhausted_profile());
        }
        // These are invocation controls, never variable overrides.
        names.retain(|name| !matches!(name.as_str(), "task" | "chat_history"));
        Ok(Self { defaults, names })
    }

    pub(super) fn schema_properties(&self) -> Map<String, Value> {
        self.names.iter().map(|name| (name.clone(), serde_json::json!({
            "type": ["string", "null"], "maxLength": MAX_VARIABLE_VALUE_BYTES,
            "description": "Optional saved-agent variable override. Null keeps the stored default."
        }))).collect()
    }

    pub(super) fn bind(
        &self,
        arguments: &Map<String, Value>,
    ) -> Result<AgentVariables, NativeAgentAssemblyError> {
        let mut bound = self.defaults.clone();
        for (name, value) in arguments {
            if name == "task" {
                continue;
            }
            if !self.names.contains(name) {
                return Err(invalid_profile());
            }
            match value {
                Value::Null => {}
                Value::String(value)
                    if value.len() <= MAX_VARIABLE_VALUE_BYTES && !value.contains('\0') =>
                {
                    bound.values.insert(name.clone(), value.clone());
                }
                _ => return Err(invalid_profile()),
            }
        }
        Ok(bound)
    }
}

/// Admit one stored variable collection without capturing it.
///
/// The shapes are Main's, not this runtime's invention: the CREATE path folds
/// variables into `meta` only when the request carried some, while the UPDATE
/// path writes the key whenever it is present, so that deleting the last
/// variable stays distinguishable from never having had one
/// (`internal/api/v2/applications/handler.go`). Every agent re-saved through
/// the edit page therefore carries `"variables": []`, and the dict spelling
/// is the one `assistant.py:574` reads, so both empty shapes and both
/// populated shapes are real.
pub(super) fn validate_variables(value: Option<&Value>) -> Result<(), NativeAgentAssemblyError> {
    capture_variables(value).map(|_| ())
}

/// Read `meta.variables` out of a version's `meta`, admitting an absent one.
pub(super) fn meta_variables(
    meta: Option<&Value>,
) -> Result<Option<&Value>, NativeAgentAssemblyError> {
    match meta {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(meta)) => Ok(meta.get("variables")),
        Some(_) => Err(invalid_profile()),
    }
}

/// Capture the entries that carry a usable value, refusing only real garbage.
///
/// SKIPPED rather than refused, following `assistant.py:564-569`: a row whose
/// `name` is missing or blank, and a row whose `value` is absent, null or
/// empty — "empty values are runtime placeholders", and the placeholder is
/// then left standing in the prompt. Rows written by Fork can genuinely carry
/// `"name": null` (`api/openapi/v2.yaml`'s `VersionVariable` documents both
/// keys as nullable precisely because of that path), so refusing them would
/// end the turn for an agent Main considers well-formed. A NON-STRING value
/// is skipped too, and says so: `VersionVariable.value` is a string, and
/// Python's `str()` of anything else does not agree with this renderer's.
fn capture_variables(
    value: Option<&Value>,
) -> Result<Vec<(String, String)>, NativeAgentAssemblyError> {
    match value {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(rows)) => {
            if rows.len() > MAX_VARIABLES {
                return Err(resource_exhausted_profile());
            }
            let mut captured = Vec::new();
            for row in rows {
                let row = row.as_object().ok_or_else(invalid_profile)?;
                let Some(name) = row
                    .get("name")
                    .and_then(Value::as_str)
                    .filter(|name| bounded_variable_name(name))
                else {
                    continue;
                };
                if let Some(value) = captured_value(name, row.get("value")) {
                    captured.push((name.to_owned(), value));
                }
            }
            Ok(captured)
        }
        Some(Value::Object(rows)) => {
            if rows.len() > MAX_VARIABLES {
                return Err(resource_exhausted_profile());
            }
            let mut captured = Vec::new();
            for (name, value) in rows {
                if !bounded_variable_name(name) {
                    continue;
                }
                if let Some(value) = captured_value(name, Some(value)) {
                    captured.push((name.clone(), value));
                }
            }
            Ok(captured)
        }
        Some(_) => Err(invalid_profile()),
    }
}

fn captured_value(name: &str, value: Option<&Value>) -> Option<String> {
    match value {
        None | Some(Value::Null) => None,
        Some(Value::String(value)) => {
            if value.is_empty() || value.len() > MAX_VARIABLE_VALUE_BYTES {
                None
            } else {
                Some(value.clone())
            }
        }
        Some(_) => {
            tracing::warn!(
                event = "agent_variable_skipped",
                reason_code = "variable_value_not_a_string",
                variable = name,
                "the stored variable value is not a string; its placeholder stays unsubstituted"
            );
            None
        }
    }
}

fn bounded_variable_name(name: &str) -> bool {
    !name.is_empty() && name.len() <= MAX_VARIABLE_NAME_BYTES && !name.contains('\0')
}

struct BoundedWriter {
    bytes: Vec<u8>,
    limit: usize,
}

impl BoundedWriter {
    fn new(limit: usize) -> Self {
        Self {
            bytes: Vec::new(),
            limit,
        }
    }

    fn into_inner(self) -> Vec<u8> {
        self.bytes
    }
}

impl io::Write for BoundedWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        let next =
            self.bytes.len().checked_add(buffer.len()).ok_or_else(|| {
                io::Error::other("rendered instructions exceed their resource bound")
            })?;
        if next > self.limit {
            return Err(io::Error::other(
                "rendered instructions exceed their resource bound",
            ));
        }
        self.bytes.extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::{AgentVariables, MAX_VARIABLES, capture_variables, validate_variables};
    use crate::agents::runtime::NativeAgentAssemblyErrorCode;
    use serde_json::{Map, Value, json};

    #[test]
    fn child_calls_override_empty_and_stored_defaults_without_cross_call_mutation() {
        let version = version(
            json!([{ "name": "audience", "value": "" }]),
            json!([{ "name": "tone", "value": "formal" }]),
        );
        let variables = super::AgentCallVariables::admit(&version).expect("declared variables");
        let first = variables
            .bind(
                json!({"task":"write", "audience":"operators", "tone":"brief"})
                    .as_object()
                    .unwrap(),
            )
            .unwrap();
        let second = variables
            .bind(
                json!({"task":"write", "audience":"users", "tone":null})
                    .as_object()
                    .unwrap(),
            )
            .unwrap();
        assert_eq!(
            first.render_with_date("{{audience}}|{{tone}}", DATE),
            "operators|brief"
        );
        assert_eq!(
            second.render_with_date("{{audience}}|{{tone}}", DATE),
            "users|formal"
        );
        assert_eq!(
            variables
                .defaults
                .render_with_date("{{audience}}|{{tone}}", DATE),
            "{{ audience }}|formal"
        );
        assert!(variables.schema_properties().contains_key("audience"));
    }

    #[test]
    fn child_call_variables_reject_unknown_and_non_string_values() {
        let variables =
            super::AgentCallVariables::admit(&version(json!({"audience":"users"}), json!([])))
                .unwrap();
        for args in [
            json!({"unknown":"x"}),
            json!({"audience":3}),
            json!({"audience":"bad\u{0000}value"}),
        ] {
            assert!(variables.bind(args.as_object().unwrap()).is_err());
        }
        assert_eq!(
            variables
                .bind(json!({"audience":""}).as_object().unwrap())
                .unwrap()
                .render_with_date("{{audience}}", DATE),
            ""
        );
    }

    const DATE: &str = "2026-08-29";

    fn version(variables: Value, meta: Value) -> Map<String, Value> {
        let mut meta_map = Map::new();
        meta_map.insert("variables".to_owned(), meta);
        let mut version = Map::new();
        version.insert("variables".to_owned(), variables);
        version.insert("meta".to_owned(), Value::Object(meta_map));
        version
    }

    fn render(variables: Value, instructions: &str) -> String {
        AgentVariables::admit(&version(json!([]), variables), None)
            .expect("admitted variables")
            .render_with_date(instructions, DATE)
    }

    #[test]
    fn a_declared_variable_is_substituted_wherever_it_is_printed() {
        let variables = json!([{"name": "audience", "value": "ops"}]);
        assert_eq!(
            render(variables, "for {{audience}}, again {{ audience }}"),
            "for ops, again ops"
        );
    }

    #[test]
    fn several_variables_resolve_independently() {
        let variables = json!([
            {"name": "audience", "value": "ops"},
            {"name": "tone", "value": "terse"},
        ]);
        assert_eq!(
            render(variables, "{{tone}} for {{audience}}"),
            "terse for ops"
        );
    }

    /// Jinja2\'s `DebugUndefined` prints the name back in ITS canonical
    /// spacing, whatever the source used — so an unspaced source placeholder
    /// coming back spaced is the proof that a real template engine ran.
    #[test]
    fn an_undefined_name_prints_its_own_placeholder() {
        assert_eq!(render(json!([]), "hello {{nobody}}"), "hello {{ nobody }}");
        assert_eq!(
            render(json!([{"name": "a", "value": "1"}]), "{{a}}{{b}}"),
            "1{{ b }}"
        );
    }

    #[test]
    fn an_empty_collection_leaves_the_prompt_alone() {
        for empty in [json!([]), json!({}), Value::Null] {
            assert_eq!(render(empty, "plain instructions"), "plain instructions");
        }
    }

    /// `assistant.py:620-622`. A template with no `{{` is returned BEFORE the
    /// engine sees it, so `{% %}` blocks and `{# #}` comments stay literal —
    /// and, because nothing is parsed, a trailing newline stays too.
    #[test]
    fn a_prompt_without_double_braces_is_never_parsed() {
        for untouched in [
            "{% if x %}kept{% endif %}",
            "{# a comment #}",
            "trailing\n",
            "",
        ] {
            assert_eq!(render(json!([]), untouched), untouched);
        }
    }

    /// The other half of the same rule: once `{{` appears the engine DOES run,
    /// and Jinja2\'s `keep_trailing_newline=False` default — which minijinja
    /// shares — eats one trailing newline. Pinned because it is the one way
    /// rendering changes a prompt that declares no variables at all.
    #[test]
    fn rendering_eats_one_trailing_newline_exactly_as_jinja2_does() {
        assert_eq!(render(json!([]), "{{ 1 }}\n"), "1");
        assert_eq!(render(json!([]), "{{ 1 }}\n\n"), "1\n");
    }

    /// `assistant.py:655-657` catches EVERY exception and returns the template
    /// unrendered, so an author\'s typo reaches the model instead of ending the
    /// turn. Both halves matter: the braces survive, and nothing refuses.
    #[test]
    fn a_malformed_template_reaches_the_model_verbatim() {
        for malformed in [
            "{{ unclosed",
            "{% for x in %}{{ x }}{% endfor %}",
            "{{ 1 / }}",
        ] {
            assert_eq!(render(json!([]), malformed), malformed);
        }
    }

    /// A runaway template takes the same branch: bounded here where Jinja2 is
    /// unbounded, and the bound is reported by keeping the original.
    #[test]
    fn a_runaway_template_keeps_the_original() {
        let runaway = "{% for i in range(400000) %}{{ i }}{% endfor %}";
        assert_eq!(render(json!([]), runaway), runaway);
    }

    #[test]
    fn current_date_is_defined_and_a_variable_of_that_name_still_wins() {
        assert_eq!(
            render(json!([]), "on {{current_date}}"),
            format!("on {DATE}")
        );
        assert_eq!(
            render(
                json!([{"name": "current_date", "value": "always"}]),
                "{{current_date}}"
            ),
            "always"
        );
    }

    /// `meta.variables` is applied after the version\'s own list, matching the
    /// `update()` at `assistant.py:575`.
    #[test]
    fn meta_variables_win_over_the_version_list() {
        let version = version(
            json!([{"name": "tone", "value": "terse"}]),
            json!({"tone": "formal"}),
        );
        assert_eq!(
            AgentVariables::admit(&version, None)
                .expect("admitted")
                .render_with_date("{{tone}}", DATE),
            "formal"
        );
    }

    #[test]
    fn a_value_that_is_not_a_string_is_skipped_rather_than_guessed() {
        let variables = json!([
            {"name": "count", "value": 7},
            {"name": "flag", "value": true},
            {"name": "text", "value": "kept"},
        ]);
        assert_eq!(
            render(variables, "{{count}}|{{flag}}|{{text}}"),
            "{{ count }}|{{ flag }}|kept"
        );
    }

    #[test]
    fn a_collection_that_is_not_a_collection_is_malformed_input() {
        for malformed in [json!("audience"), json!(7), json!(true), json!(["name"])] {
            assert_eq!(
                validate_variables(Some(&malformed))
                    .expect_err("a malformed variable collection")
                    .code(),
                NativeAgentAssemblyErrorCode::InvalidInput
            );
        }
    }

    #[test]
    fn an_oversized_collection_is_a_resource_bound_not_a_refusal_of_the_feature() {
        let rows = (0..=MAX_VARIABLES)
            .map(|index| json!({"name": format!("v{index}"), "value": "x"}))
            .collect::<Vec<_>>();
        assert_eq!(
            validate_variables(Some(&Value::Array(rows)))
                .expect_err("an oversized variable collection")
                .code(),
            NativeAgentAssemblyErrorCode::ResourceExhausted
        );
    }

    #[test]
    fn a_row_without_a_usable_name_or_value_is_skipped() {
        let captured = capture_variables(Some(&json!([
            {"name": null, "value": "orphan"},
            {"name": "", "value": "blank name"},
            {"name": "empty", "value": ""},
            {"name": "absent"},
            {"name": "kept", "value": "yes"},
        ])))
        .expect("nullable rows are skipped, not refused");
        assert_eq!(captured, vec![("kept".to_owned(), "yes".to_owned())]);
    }
}
