//! Provider-aware tool parameter schemas (#2145, #2155).
//!
//! A tool declaration's `parameters` is a JSON Schema the runtime builds from
//! a toolkit family, an internal tool, a sub-agent tool or an MCP server's
//! advertisement. Providers accept different subsets of it, and one schema a
//! provider refuses fails the whole model call (the worker reports a provider
//! HTTP 4xx as `MODEL_REQUEST_REJECTED`), so a selected tool made the agent
//! unusable while the same agent with no tools worked.
//!
//! [`normalize_tool_schema`] is the ONE place that rewrites a declaration's
//! schema for a [`SchemaProfile`]. The model facades call it when they encode
//! a request, so the stored declarations (and every digest and checkpoint
//! comparison over them) are untouched, and the tool itself keeps validating
//! a call against its own full schema: only the wire form is relaxed.
//!
//! What it does, for every profile:
//!
//! * inlines `$ref` (local pointers) and drops `$defs`/`definitions`, with a
//!   cycle guard: a recursive reference becomes a bare object, a reference
//!   outside the schema becomes an unconstrained value;
//! * merges `allOf`, turns `oneOf` into `anyOf` (a relaxation), and flattens a
//!   root-level `anyOf` of objects into one object (providers refuse a
//!   combinator at the root);
//! * guarantees a root `type: object` with `properties`, and that every
//!   `required` name has a property (a required argument is never dropped);
//! * infers a missing `type` where the keywords imply it.
//!
//! On top of that, the restricted profiles ([`SchemaProfile::Gemini`], an
//! `OpenAPI` subset, and [`SchemaProfile::LlamaCpp`], whose JSON-schema to GBNF
//! converter is stricter still) drop or rewrite what those providers refuse:
//! `const` becomes a single-value `enum`, `type` arrays and `anyOf` with
//! `null` become `nullable` for Gemini, patterns llama.cpp cannot convert and
//! repetition bounds that explode its grammar are removed, and so on. Every
//! constraint that is dropped is folded into the property's `description`
//! text so the model still sees it.
//!
//! The output does not depend on map order: keys are visited sorted, and
//! nothing here relies on `serde_json`'s `preserve_order`.
//!
//! [`wire_violations`] is the independent checker the inventory tests run
//! over every tool the runtime can emit.

use std::collections::{BTreeSet, HashMap};
use std::sync::{Mutex, OnceLock};

use serde_json::{Map, Value, json};

type Object = Map<String, Value>;

/// Nodes one normalisation may visit: bounds a schema whose shared
/// definitions would expand exponentially once inlined.
const MAX_NODES: usize = 50_000;
const MAX_DEPTH: usize = 48;
/// llama.cpp expands a bound into repeated grammar rules and refuses a
/// repetition count above 2000 outright; far lower counts already make a
/// grammar too large to build for a tool list. Longer bounds are folded into
/// the description.
const LLAMA_MAX_REPETITION: u64 = 64;
const GEMINI_FORMATS: [&str; 6] = ["date-time", "int32", "int64", "float", "double", "enum"];
const LLAMA_FORMATS: [&str; 4] = ["date", "time", "date-time", "uuid"];
/// Keywords the restricted profiles cannot carry. Their meaning is folded
/// into the description, except pure annotations.
const RESTRICTED_FORBIDDEN: [&str; 19] = [
    "oneOf",
    "patternProperties",
    "propertyNames",
    "not",
    "if",
    "then",
    "else",
    "contains",
    "dependentRequired",
    "dependentSchemas",
    "dependencies",
    "prefixItems",
    "uniqueItems",
    "multipleOf",
    "unevaluatedItems",
    "unevaluatedProperties",
    "minContains",
    "maxContains",
    "additionalItems",
];

/// The provider family a tool schema is written for.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum SchemaProfile {
    /// Anthropic `input_schema`: a full JSON Schema, but the root must be a
    /// plain object (no root combinator) and a reference must be local.
    Anthropic,
    /// `OpenAI` function calling (non-strict): the same structural rules.
    OpenAi,
    /// Bedrock Converse `toolSpec.inputSchema`: the same structural rules.
    Bedrock,
    /// Google Gemini / Vertex: an `OpenAPI` subset. No `$ref`, `const`,
    /// `oneOf`, `additionalProperties`, `default`, type arrays or non-string
    /// enums, a type on every node, a short list of formats.
    Gemini,
    /// llama.cpp: the JSON-schema to GBNF converter. No `patternProperties`,
    /// anchored simple patterns only, bounded repetition, a short list of
    /// formats, no `not`/`if`/`multipleOf`/`uniqueItems`, and `const` and
    /// `oneOf` are rewritten.
    LlamaCpp,
}

impl SchemaProfile {
    /// Every profile, for a test that must hold for all of them.
    pub const ALL: [Self; 5] = [
        Self::Anthropic,
        Self::OpenAi,
        Self::Bedrock,
        Self::Gemini,
        Self::LlamaCpp,
    ];

    /// The profile for a model name, for the `OpenAI`-compatible gateway path
    /// where the provider behind the gateway is not otherwise known. An
    /// unknown name gets the strictest portable form ([`Self::LlamaCpp`]):
    /// local and self-hosted servers sit behind names nothing can recognise,
    /// and the tool still validates a call against its own full schema.
    #[must_use]
    pub fn for_model(model_name: &str) -> Self {
        // `openai/gpt-4o`, `azure/gpt-4.1`: the model is the last path segment.
        let lowered = model_name.to_ascii_lowercase();
        let name = lowered.rsplit('/').next().unwrap_or(lowered.as_str());
        if name.contains("gemini") {
            Self::Gemini
        } else if name.contains("claude") {
            Self::Anthropic
        } else if name.contains("nova") || name.contains("bedrock") {
            Self::Bedrock
        } else if name.starts_with("gpt-")
            || name.starts_with("chatgpt")
            || ["o1", "o3", "o4"].iter().any(|prefix| {
                name.strip_prefix(prefix)
                    .is_some_and(|rest| rest.is_empty() || rest.starts_with('-'))
            })
        {
            Self::OpenAi
        } else {
            Self::LlamaCpp
        }
    }

    /// The profile a catalogue entry names (`llm_settings.tool_schema_profile`):
    /// `anthropic`, `openai`, `bedrock`, `gemini` or `llama_cpp`. `None` for
    /// any other text.
    #[must_use]
    pub fn from_name(name: &str) -> Option<Self> {
        match name.trim().to_ascii_lowercase().as_str() {
            "anthropic" => Some(Self::Anthropic),
            "openai" => Some(Self::OpenAi),
            "bedrock" => Some(Self::Bedrock),
            "gemini" => Some(Self::Gemini),
            "llama_cpp" | "llamacpp" => Some(Self::LlamaCpp),
            _ => None,
        }
    }

    const fn gemini(self) -> bool {
        matches!(self, Self::Gemini)
    }

    const fn llama(self) -> bool {
        matches!(self, Self::LlamaCpp)
    }

    const fn restricted(self) -> bool {
        matches!(self, Self::Gemini | Self::LlamaCpp)
    }
}

/// Entries the memo of normalised schemas holds before it starts over.
const CACHE_CAPACITY: usize = 512;
/// A schema longer than this (serialised) is normalised every time rather
/// than held in the memo.
const CACHE_MAX_KEY_BYTES: usize = 64 * 1024;

type Memo = Mutex<HashMap<(SchemaProfile, String), Value>>;

fn memo() -> &'static Memo {
    static MEMO: OnceLock<Memo> = OnceLock::new();
    MEMO.get_or_init(|| Mutex::new(HashMap::new()))
}

/// The wire form of one tool's `parameters` schema for `profile`.
///
/// A tool's declaration is stable within a turn and between turns, yet every
/// model call of a turn encodes it again (#2181), so the result is memoised
/// per (profile, schema text). The output is a pure function of both, so a
/// hit is identical to a recomputation; the memo is bounded and starts over
/// when full.
#[must_use]
pub fn normalize_tool_schema(schema: &Value, profile: SchemaProfile) -> Value {
    let key = schema.to_string();
    if key.len() > CACHE_MAX_KEY_BYTES {
        return normalize_uncached(schema, profile);
    }
    let key = (profile, key);
    if let Ok(guard) = memo().lock()
        && let Some(hit) = guard.get(&key)
    {
        return hit.clone();
    }
    let out = normalize_uncached(schema, profile);
    if let Ok(mut guard) = memo().lock() {
        if guard.len() >= CACHE_CAPACITY {
            guard.clear();
        }
        guard.insert(key, out.clone());
    }
    out
}

thread_local! {
    static COMPUTED: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
}

/// How many normalisations this thread computed, as opposed to served from
/// the memo (tests: a repeated call must not raise it).
#[must_use]
pub fn computed_on_this_thread() -> usize {
    COMPUTED.with(std::cell::Cell::get)
}

fn normalize_uncached(schema: &Value, profile: SchemaProfile) -> Value {
    COMPUTED.with(|count| count.set(count.get() + 1));
    // A tool with no parameters: nothing to rewrite, nothing to note.
    if schema.as_object().is_none_or(Map::is_empty) {
        return json!({"type": "object", "properties": {}});
    }
    let mut normalizer = Normalizer {
        root: schema,
        profile,
        budget: MAX_NODES,
    };
    let node = normalizer.node(schema, &mut Vec::new(), 0);
    let root = if let Value::Object(object) = node {
        object
    } else {
        Object::new()
    };
    Value::Object(normalizer.root_object(root))
}

struct Normalizer<'a> {
    root: &'a Value,
    profile: SchemaProfile,
    budget: usize,
}

impl Normalizer<'_> {
    /// A schema that admits any value, in the form the profile accepts.
    fn any(&self, why: &str) -> Value {
        if self.profile.gemini() {
            json!({"type": "string", "description": why})
        } else {
            json!({"description": why})
        }
    }

    fn node(&mut self, schema: &Value, refs: &mut Vec<String>, depth: usize) -> Value {
        if self.budget == 0 || depth > MAX_DEPTH {
            return self.any("Nested value (not expanded).");
        }
        self.budget -= 1;
        let Some(object) = schema.as_object() else {
            return self.any("Any JSON value.");
        };
        if let Some(reference) = object.get("$ref").and_then(Value::as_str) {
            return self.reference(object, reference, refs, depth);
        }
        let mut base = object.clone();
        let all_of = take_list(&mut base, "allOf");
        let mut alternatives = take_list(&mut base, "anyOf");
        alternatives.extend(take_list(&mut base, "oneOf"));
        let (mut out, mut notes) = self.plain(base, refs, depth);
        for member in &all_of {
            if let Value::Object(member) = self.node(member, refs, depth + 1) {
                merge(&mut out, member, true);
            }
        }
        let mut normalized = Vec::with_capacity(alternatives.len());
        for alternative in &alternatives {
            normalized.push(self.node(alternative, refs, depth + 1));
        }
        let mut out = self.alternatives(out, normalized);
        self.finish(&mut out, &mut notes);
        Value::Object(out)
    }

    fn reference(
        &mut self,
        object: &Object,
        reference: &str,
        refs: &mut Vec<String>,
        depth: usize,
    ) -> Value {
        if refs.iter().any(|seen| seen == reference) {
            return json!({
                "type": "object",
                "description": "Same structure as an enclosing object (not expanded)."
            });
        }
        let Some(target) = lookup(self.root, reference).and_then(Value::as_object) else {
            return self.any("Value defined outside this schema.");
        };
        let mut merged = target.clone();
        for (key, value) in object {
            if key != "$ref" {
                merged.insert(key.clone(), value.clone());
            }
        }
        refs.push(reference.to_owned());
        let resolved = self.node(&Value::Object(merged), refs, depth + 1);
        refs.pop();
        resolved
    }

    /// The keywords of one node that are neither a reference nor a
    /// combinator, normalised, and the constraints folded out of it.
    fn plain(
        &mut self,
        mut object: Object,
        refs: &mut Vec<String>,
        depth: usize,
    ) -> (Object, Vec<String>) {
        let mut out = Object::new();
        let mut notes = Vec::new();
        let mut keys: Vec<String> = object.keys().cloned().collect();
        keys.sort();
        for key in keys {
            let Some(value) = object.remove(&key) else {
                continue;
            };
            match key.as_str() {
                "properties" => {
                    let properties = self.properties(&value, refs, depth);
                    out.insert(key, properties);
                }
                "items" => {
                    let items = self.items(&value, refs, depth);
                    out.insert(key, items);
                }
                "additionalProperties" => {
                    self.additional(&value, &mut out, &mut notes, refs, depth);
                }
                "required" => {
                    let names = strings(&value);
                    if !names.is_empty() {
                        out.insert(key, json!(names));
                    }
                }
                "description" | "title" => {
                    if value.is_string() {
                        out.insert(key, value);
                    }
                }
                "type" => self.type_keyword(&value, &mut out),
                "enum" => self.enum_keyword(value, &mut out, &mut notes),
                "const" => self.const_keyword(value, &mut out, &mut notes),
                "default" => self.default_keyword(value, &mut out, &mut notes),
                _ => self.other(&key, value, &mut out, &mut notes),
            }
        }
        (out, notes)
    }

    fn properties(&mut self, value: &Value, refs: &mut Vec<String>, depth: usize) -> Value {
        let Some(map) = value.as_object() else {
            return json!({});
        };
        let mut entries: Vec<(&String, &Value)> = map.iter().collect();
        entries.sort_by(|left, right| left.0.cmp(right.0));
        let mut out = Object::new();
        for (name, schema) in entries {
            out.insert(name.clone(), self.node(schema, refs, depth + 1));
        }
        Value::Object(out)
    }

    fn items(&mut self, value: &Value, refs: &mut Vec<String>, depth: usize) -> Value {
        if let Value::Array(members) = value {
            // A tuple form: the elements may be any of the listed schemas.
            self.node(&json!({"anyOf": members}), refs, depth + 1)
        } else {
            self.node(value, refs, depth + 1)
        }
    }

    fn additional(
        &mut self,
        value: &Value,
        out: &mut Object,
        notes: &mut Vec<String>,
        refs: &mut Vec<String>,
        depth: usize,
    ) {
        if self.profile.gemini() {
            if value.is_object() {
                let schema = self.node(value, refs, depth + 1);
                notes.push(format!("extra keys hold {}", brief(&schema)));
            }
            return;
        }
        match value {
            Value::Bool(_) => {
                out.insert("additionalProperties".to_owned(), value.clone());
            }
            Value::Object(_) => {
                let schema = self.node(value, refs, depth + 1);
                out.insert("additionalProperties".to_owned(), schema);
            }
            _ => {}
        }
    }

    fn type_keyword(&self, value: &Value, out: &mut Object) {
        match value {
            Value::String(_) => {
                out.insert("type".to_owned(), value.clone());
            }
            Value::Array(kinds) => {
                let names: Vec<&str> = kinds.iter().filter_map(Value::as_str).collect();
                let non_null: Vec<&str> = names
                    .iter()
                    .copied()
                    .filter(|name| *name != "null")
                    .collect();
                if self.profile.gemini() {
                    if non_null.len() != names.len() {
                        out.insert("nullable".to_owned(), Value::Bool(true));
                    }
                    match non_null.as_slice() {
                        [] => {}
                        [one] => {
                            out.insert("type".to_owned(), json!(one));
                        }
                        many => {
                            let members: Vec<Value> =
                                many.iter().map(|kind| json!({"type": kind})).collect();
                            out.insert("anyOf".to_owned(), Value::Array(members));
                        }
                    }
                } else if let [one] = names.as_slice() {
                    out.insert("type".to_owned(), json!(one));
                } else if !names.is_empty() {
                    out.insert("type".to_owned(), json!(names));
                }
            }
            _ => {}
        }
    }

    fn enum_keyword(&self, value: Value, out: &mut Object, notes: &mut Vec<String>) {
        let Value::Array(values) = value else {
            return;
        };
        if values.is_empty() {
            return;
        }
        if self.profile.gemini() && !values.iter().all(Value::is_string) {
            notes.push(format!("one of {}", compact(&Value::Array(values.clone()))));
            if let Some(kind) = scalar_type(&values) {
                out.insert("type".to_owned(), json!(kind));
            }
            return;
        }
        out.insert("enum".to_owned(), Value::Array(values));
    }

    fn const_keyword(&self, value: Value, out: &mut Object, notes: &mut Vec<String>) {
        if !self.profile.restricted() {
            out.insert("const".to_owned(), value);
        } else if self.profile.gemini() && !value.is_string() {
            notes.push(format!("must equal {}", compact(&value)));
            if let Some(kind) = scalar_type(std::slice::from_ref(&value)) {
                out.insert("type".to_owned(), json!(kind));
            }
        } else {
            out.insert("enum".to_owned(), json!([value]));
        }
    }

    fn default_keyword(&self, value: Value, out: &mut Object, notes: &mut Vec<String>) {
        if !self.profile.gemini() {
            out.insert("default".to_owned(), value);
        } else if matches!(value, Value::Bool(_) | Value::Number(_) | Value::String(_)) {
            notes.push(format!("default {}", compact(&value)));
        }
    }

    /// Every other keyword: bounds, formats, patterns, annotations and the
    /// keywords a restricted profile cannot carry.
    fn other(&self, key: &str, value: Value, out: &mut Object, notes: &mut Vec<String>) {
        // Dropped without a trace: markers that only name the draft or a
        // definition this pass removed, and, for the restricted profiles,
        // annotations the model gains nothing from.
        let silent = matches!(
            key,
            "$schema"
                | "$id"
                | "$anchor"
                | "$comment"
                | "$defs"
                | "definitions"
                | "$dynamicAnchor"
                | "$dynamicRef"
                | "$vocabulary"
        ) || (self.profile.restricted()
            && (matches!(
                key,
                "examples" | "deprecated" | "readOnly" | "writeOnly" | "contentSchema"
            ) || key.starts_with("x-")));
        if silent {
            return;
        }
        let keep = match key {
            "format" => self.format_ok(&value),
            "pattern" => self.pattern_ok(&value),
            "minLength" | "maxLength" | "minItems" | "maxItems" | "minProperties"
            | "maxProperties" => self.bound_ok(key, &value),
            "minimum" | "maximum" | "nullable" => true,
            "exclusiveMinimum" | "exclusiveMaximum" => {
                !self.profile.gemini() && (!self.profile.llama() || value.is_number())
            }
            // A schema-valued keyword is never kept when it points at a
            // definition this pass removed.
            _ if contains_ref(&value) => false,
            _ => !self.profile.restricted(),
        };
        if keep {
            out.insert(key.to_owned(), value);
        } else {
            notes.push(format!("{key} {}", compact(&value)));
        }
    }

    fn format_ok(&self, value: &Value) -> bool {
        match (self.profile, value.as_str()) {
            (SchemaProfile::Gemini, Some(format)) => GEMINI_FORMATS.contains(&format),
            (SchemaProfile::LlamaCpp, Some(format)) => LLAMA_FORMATS.contains(&format),
            _ => true,
        }
    }

    fn pattern_ok(&self, value: &Value) -> bool {
        let Some(pattern) = value.as_str() else {
            return false;
        };
        match self.profile {
            SchemaProfile::Gemini => re2_pattern_ok(pattern),
            SchemaProfile::LlamaCpp => llama_pattern_ok(pattern),
            _ => true,
        }
    }

    fn bound_ok(&self, key: &str, value: &Value) -> bool {
        if !self.profile.llama() {
            return true;
        }
        if matches!(key, "minProperties" | "maxProperties") {
            return false;
        }
        value
            .as_u64()
            .is_some_and(|bound| bound <= LLAMA_MAX_REPETITION)
    }

    /// Fold a node's `anyOf`/`oneOf` members into `out`.
    fn alternatives(&self, mut out: Object, alternatives: Vec<Value>) -> Object {
        if alternatives.is_empty() {
            return out;
        }
        let mut unique: Vec<Value> = Vec::new();
        for alternative in alternatives {
            if !unique.contains(&alternative) {
                unique.push(alternative);
            }
        }
        let (nulls, mut members): (Vec<Value>, Vec<Value>) =
            unique.into_iter().partition(is_null_schema);
        let nullable = !nulls.is_empty();
        if self.profile.gemini() && nullable {
            out.insert("nullable".to_owned(), Value::Bool(true));
        }
        if members.is_empty() {
            if !self.profile.gemini() {
                out.insert("anyOf".to_owned(), Value::Array(nulls));
            }
            return out;
        }
        if members.len() == 1 && (self.profile.gemini() || !nullable) {
            if let Some(Value::Object(member)) = members.pop() {
                merge(&mut out, member, true);
            }
            return out;
        }
        if !self.profile.gemini() {
            members.extend(nulls);
        }
        out.insert("anyOf".to_owned(), Value::Array(members));
        out
    }

    /// What every node needs once its keywords are normalised: a type where
    /// one is implied, a property for every required name, and the folded
    /// constraints in its description.
    fn finish(&self, out: &mut Object, notes: &mut Vec<String>) {
        if !out.contains_key("type") && !out.contains_key("anyOf") {
            let inferred = if ["properties", "required", "additionalProperties"]
                .iter()
                .any(|key| out.contains_key(*key))
            {
                Some("object")
            } else if out.contains_key("items") {
                Some("array")
            } else {
                out.get("enum")
                    .and_then(Value::as_array)
                    .and_then(|values| scalar_type(values))
            };
            if let Some(kind) = inferred {
                out.insert("type".to_owned(), json!(kind));
            } else if self.profile.gemini() && !out.contains_key("enum") {
                out.insert("type".to_owned(), json!("string"));
                notes.push("any JSON value; write it as JSON text".to_owned());
            }
        }
        // Gemini refuses an array that does not say what it holds.
        if self.profile.gemini()
            && out.get("type").and_then(Value::as_str) == Some("array")
            && !out.contains_key("items")
        {
            out.insert("items".to_owned(), self.any("Any JSON value."));
        }
        self.ensure_required_properties(out);
        fold(out, notes);
    }

    fn ensure_required_properties(&self, out: &mut Object) {
        let names = out.get("required").map_or_else(Vec::new, strings);
        if names.is_empty() {
            return;
        }
        let placeholder = self.any("Required argument (any JSON value).");
        let properties = out
            .entry("properties".to_owned())
            .or_insert_with(|| json!({}));
        if let Value::Object(properties) = properties {
            for name in names {
                properties
                    .entry(name)
                    .or_insert_with(|| placeholder.clone());
            }
        }
    }

    /// The root of a tool's parameters: always an object with properties.
    /// A root `anyOf` of objects becomes one object whose properties are the
    /// union of the variants'.
    fn root_object(&self, mut root: Object) -> Object {
        let mut notes = Vec::new();
        if let Some(Value::Array(variants)) = root.remove("anyOf") {
            fold_variants(&mut root, variants, &mut notes);
        }
        for key in ["enum", "const", "nullable", "default"] {
            root.remove(key);
        }
        root.insert("type".to_owned(), json!("object"));
        if !matches!(root.get("properties"), Some(Value::Object(_))) {
            root.insert("properties".to_owned(), json!({}));
        }
        self.ensure_required_properties(&mut root);
        fold(&mut root, &mut notes);
        root
    }
}

/// Merge a root `anyOf`'s variants into `root`. A property any variant
/// declares is kept; a name every variant requires is required; a name only
/// some variants require stays optional and is named in the description.
fn fold_variants(root: &mut Object, variants: Vec<Value>, notes: &mut Vec<String>) {
    let mut properties = if let Some(Value::Object(properties)) = root.remove("properties") {
        properties
    } else {
        Object::new()
    };
    let mut required = root.get("required").map_or_else(Vec::new, strings);
    let mut common: Option<BTreeSet<String>> = None;
    let mut any_required = BTreeSet::new();
    let mut skipped = Vec::new();
    for variant in variants {
        let Value::Object(variant) = variant else {
            skipped.push(brief(&variant));
            continue;
        };
        if !(variant.contains_key("properties")
            || variant.contains_key("required")
            || variant.get("type").and_then(Value::as_str) == Some("object"))
        {
            skipped.push(brief(&Value::Object(variant)));
            continue;
        }
        if let Some(Value::Object(declared)) = variant.get("properties") {
            for (name, schema) in declared {
                match properties.get(name) {
                    None => {
                        properties.insert(name.clone(), schema.clone());
                    }
                    Some(existing) if existing == schema => {}
                    Some(existing) => {
                        let combined = combine(existing, schema);
                        properties.insert(name.clone(), combined);
                    }
                }
            }
        }
        let names: BTreeSet<String> = variant
            .get("required")
            .map_or_else(Vec::new, strings)
            .into_iter()
            .collect();
        any_required.extend(names.iter().cloned());
        common = Some(match common {
            None => names,
            Some(previous) => previous.intersection(&names).cloned().collect(),
        });
    }
    for name in common.unwrap_or_default() {
        if !required.contains(&name) {
            required.push(name);
        }
    }
    let conditional: Vec<String> = any_required
        .into_iter()
        .filter(|name| !required.contains(name))
        .collect();
    if !conditional.is_empty() {
        notes.push(format!(
            "only some call shapes require: {}",
            conditional.join(", ")
        ));
    }
    if !skipped.is_empty() {
        notes.push(format!("also accepted: {}", skipped.join(" | ")));
    }
    root.insert("properties".to_owned(), Value::Object(properties));
    if !required.is_empty() {
        root.insert("required".to_owned(), json!(required));
    }
}

/// One schema admitting what either of two schemas admits.
fn combine(existing: &Value, other: &Value) -> Value {
    let mut list = existing
        .get("anyOf")
        .and_then(Value::as_array)
        .filter(|_| existing.as_object().is_some_and(|object| object.len() == 1))
        .cloned()
        .unwrap_or_else(|| vec![existing.clone()]);
    if !list.contains(other) {
        list.push(other.clone());
    }
    json!({"anyOf": list})
}

fn merge(target: &mut Object, other: Object, join_descriptions: bool) {
    for (key, value) in other {
        match key.as_str() {
            "properties" => match (target.get_mut("properties"), value) {
                (Some(Value::Object(mine)), Value::Object(theirs)) => {
                    for (name, schema) in theirs {
                        mine.entry(name).or_insert(schema);
                    }
                }
                (None, theirs) => {
                    target.insert(key, theirs);
                }
                _ => {}
            },
            "required" => {
                let mut names = target.get("required").map_or_else(Vec::new, strings);
                for name in strings(&value) {
                    if !names.contains(&name) {
                        names.push(name);
                    }
                }
                if !names.is_empty() {
                    target.insert(key, json!(names));
                }
            }
            "enum" if join_descriptions => {
                // `allOf` is an intersection: only a value both lists allow.
                if let (Some(Value::Array(mine)), Value::Array(theirs)) =
                    (target.get("enum"), &value)
                {
                    let shared: Vec<Value> = mine
                        .iter()
                        .filter(|candidate| theirs.contains(candidate))
                        .cloned()
                        .collect();
                    if !shared.is_empty() {
                        target.insert(key, Value::Array(shared));
                    }
                } else {
                    target.entry(key).or_insert(value);
                }
            }
            "description" if join_descriptions => {
                let joined = match (target.get("description").and_then(Value::as_str), &value) {
                    (Some(mine), Value::String(theirs)) if mine != theirs => {
                        format!("{mine} {theirs}")
                    }
                    (Some(mine), _) => mine.to_owned(),
                    (None, other) => other.as_str().unwrap_or_default().to_owned(),
                };
                target.insert(key, Value::String(joined));
            }
            _ => {
                target.entry(key).or_insert(value);
            }
        }
    }
}

/// Append the folded constraints to the description: the model still reads
/// what the provider could not carry.
fn fold(out: &mut Object, notes: &mut Vec<String>) {
    if notes.is_empty() {
        return;
    }
    let existing = out
        .get("description")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let separator = if existing.is_empty() { "" } else { " " };
    let text = format!("{existing}{separator}({})", notes.join("; "));
    out.insert("description".to_owned(), Value::String(text));
    notes.clear();
}

fn lookup<'a>(root: &'a Value, reference: &str) -> Option<&'a Value> {
    let pointer = reference.strip_prefix('#')?;
    if pointer.is_empty() {
        return Some(root);
    }
    root.pointer(&percent_decode(pointer))
}

/// A URI fragment's `%XX` escapes (`#/$defs/My%20Type`) as the pointer's text.
fn percent_decode(text: &str) -> String {
    let bytes = text.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        let decoded = if bytes[index] == b'%' {
            text.get(index + 1..index + 3)
                .and_then(|hex| u8::from_str_radix(hex, 16).ok())
        } else {
            None
        };
        if let Some(byte) = decoded {
            out.push(byte);
            index += 3;
        } else {
            out.push(bytes[index]);
            index += 1;
        }
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn take_list(object: &mut Object, key: &str) -> Vec<Value> {
    if let Some(Value::Array(items)) = object.remove(key) {
        items
    } else {
        Vec::new()
    }
}

fn strings(value: &Value) -> Vec<String> {
    value.as_array().map_or_else(Vec::new, |items| {
        items
            .iter()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect()
    })
}

fn is_null_schema(value: &Value) -> bool {
    value.as_object().is_some_and(|object| {
        object.get("type").and_then(Value::as_str) == Some("null")
            && object
                .keys()
                .all(|key| matches!(key.as_str(), "type" | "description" | "title"))
    })
}

fn contains_ref(value: &Value) -> bool {
    match value {
        Value::Object(object) => object.contains_key("$ref") || object.values().any(contains_ref),
        Value::Array(items) => items.iter().any(contains_ref),
        _ => false,
    }
}

/// The JSON type every value shares, if they share one.
fn scalar_type(values: &[Value]) -> Option<&'static str> {
    let kind = |value: &Value| match value {
        Value::String(_) => Some("string"),
        Value::Bool(_) => Some("boolean"),
        Value::Number(number) if number.is_i64() || number.is_u64() => Some("integer"),
        Value::Number(_) => Some("number"),
        _ => None,
    };
    let first = kind(values.first()?)?;
    let mut shared = first;
    for value in values {
        let next = kind(value)?;
        if next != shared {
            if matches!((shared, next), ("integer" | "number", "integer" | "number")) {
                shared = "number";
            } else {
                return None;
            }
        }
    }
    Some(shared)
}

/// A short description of a schema's type, for a folded note.
fn brief(schema: &Value) -> String {
    match schema.get("type") {
        Some(Value::String(kind)) => kind.clone(),
        Some(Value::Array(kinds)) => kinds
            .iter()
            .filter_map(Value::as_str)
            .collect::<Vec<_>>()
            .join("/"),
        _ => "JSON values".to_owned(),
    }
}

fn compact(value: &Value) -> String {
    let text = value.to_string();
    if text.chars().count() <= 80 {
        return text;
    }
    let cut: String = text.chars().take(77).collect();
    format!("{cut}...")
}

/// A pattern Gemini's RE2 engine accepts: no lookaround, no back-reference.
fn re2_pattern_ok(pattern: &str) -> bool {
    !["(?=", "(?!", "(?<=", "(?<!"]
        .iter()
        .any(|construct| pattern.contains(construct))
        && !pattern
            .as_bytes()
            .windows(2)
            .any(|pair| pair[0] == b'\\' && (b'1'..=b'9').contains(&pair[1]))
}

/// A pattern llama.cpp's converter takes: anchored at both ends, escapes of
/// punctuation only (no `\d`, `\b`, `\p`, back-reference), no `(?` group and
/// bounded repetition.
fn llama_pattern_ok(pattern: &str) -> bool {
    if !(pattern.starts_with('^') && pattern.ends_with('$')) || pattern.chars().count() > 200 {
        return false;
    }
    let mut chars = pattern.chars().peekable();
    while let Some(character) = chars.next() {
        match character {
            '\\' => match chars.next() {
                Some(escaped) if "^$.[]()|{}*+?\\/-".contains(escaped) => {}
                _ => return false,
            },
            '(' if chars.peek() == Some(&'?') => return false,
            '{' => {
                let body: String = chars.by_ref().take_while(|next| *next != '}').collect();
                if body
                    .split(',')
                    .filter_map(|count| count.trim().parse::<u64>().ok())
                    .any(|count| count > LLAMA_MAX_REPETITION)
                {
                    return false;
                }
            }
            _ => {}
        }
    }
    true
}

/// What `profile` would refuse in `schema`, as `path: rule` lines. Empty for
/// a schema [`normalize_tool_schema`] produced.
#[must_use]
pub fn wire_violations(schema: &Value, profile: SchemaProfile) -> Vec<String> {
    let mut found = Vec::new();
    let root = schema.as_object();
    if root
        .and_then(|object| object.get("type"))
        .and_then(Value::as_str)
        != Some("object")
    {
        found.push("$: the root is not `type: object`".to_owned());
    }
    if root.is_some_and(|object| {
        ["anyOf", "oneOf", "allOf", "not", "enum"]
            .iter()
            .any(|key| object.contains_key(*key))
    }) {
        found.push("$: a combinator or enum at the root".to_owned());
    }
    if !root.is_some_and(|object| matches!(object.get("properties"), Some(Value::Object(_)))) {
        found.push("$: the root has no `properties` object".to_owned());
    }
    check(schema, "$", profile, &mut found);
    found
}

fn check(node: &Value, path: &str, profile: SchemaProfile, found: &mut Vec<String>) {
    let Some(object) = node.as_object() else {
        found.push(format!("{path}: not an object schema"));
        return;
    };
    for key in ["$ref", "$defs", "definitions", "$schema", "$id", "allOf"] {
        if object.contains_key(key) {
            found.push(format!("{path}: `{key}` present"));
        }
    }
    if profile.restricted() {
        check_restricted(object, path, profile, found);
    }
    if let Some(required) = object.get("required") {
        let properties = object.get("properties").and_then(Value::as_object);
        let names = required.as_array().and_then(|items| {
            items
                .iter()
                .map(|item| item.as_str().map(str::to_owned))
                .collect::<Option<Vec<_>>>()
        });
        if let Some(names) = names {
            for name in names {
                if !properties.is_some_and(|declared| declared.contains_key(&name)) {
                    found.push(format!("{path}: required `{name}` has no property"));
                }
            }
        } else {
            found.push(format!("{path}: `required` is not a list of names"));
        }
    }
    if let Some(Value::Object(properties)) = object.get("properties") {
        for (name, schema) in properties {
            check(schema, &format!("{path}.properties.{name}"), profile, found);
        }
    }
    if let Some(items) = object.get("items") {
        check(items, &format!("{path}.items"), profile, found);
    }
    if let Some(Value::Array(members)) = object.get("anyOf") {
        for (index, member) in members.iter().enumerate() {
            check(member, &format!("{path}.anyOf[{index}]"), profile, found);
        }
    }
    if let Some(additional @ Value::Object(_)) = object.get("additionalProperties") {
        check(
            additional,
            &format!("{path}.additionalProperties"),
            profile,
            found,
        );
    }
}

fn check_restricted(object: &Object, path: &str, profile: SchemaProfile, found: &mut Vec<String>) {
    for key in RESTRICTED_FORBIDDEN.iter().chain(&["const"]) {
        if object.contains_key(*key) {
            found.push(format!("{path}: `{key}` is not accepted"));
        }
    }
    if let Some(Value::String(format)) = object.get("format") {
        let allowed: &[&str] = if profile.gemini() {
            &GEMINI_FORMATS
        } else {
            &LLAMA_FORMATS
        };
        if !allowed.contains(&format.as_str()) {
            found.push(format!("{path}: format `{format}` is not accepted"));
        }
    }
    if profile.gemini() {
        check_gemini(object, path, found);
    } else {
        check_llama(object, path, found);
    }
}

fn check_gemini(object: &Object, path: &str, found: &mut Vec<String>) {
    for key in [
        "additionalProperties",
        "default",
        "exclusiveMinimum",
        "exclusiveMaximum",
    ] {
        if object.contains_key(key) {
            found.push(format!("{path}: `{key}` is not accepted"));
        }
    }
    if matches!(object.get("type"), Some(Value::Array(_))) {
        found.push(format!("{path}: a `type` array is not accepted"));
    }
    if object.get("type").and_then(Value::as_str) == Some("array") && !object.contains_key("items")
    {
        found.push(format!("{path}: an array without `items`"));
    }
    if !object.contains_key("type") && !object.contains_key("anyOf") {
        found.push(format!("{path}: no `type`"));
    }
    if let Some(Value::Array(values)) = object.get("enum")
        && !values.iter().all(Value::is_string)
    {
        found.push(format!("{path}: a non-string `enum`"));
    }
    if let Some(Value::Array(members)) = object.get("anyOf")
        && members.iter().any(is_null_schema)
    {
        found.push(format!("{path}: a null `anyOf` member (use `nullable`)"));
    }
    if let Some(Value::String(pattern)) = object.get("pattern")
        && !re2_pattern_ok(pattern)
    {
        found.push(format!("{path}: pattern is not RE2"));
    }
}

fn check_llama(object: &Object, path: &str, found: &mut Vec<String>) {
    if let Some(Value::String(pattern)) = object.get("pattern")
        && !llama_pattern_ok(pattern)
    {
        found.push(format!("{path}: pattern is not convertible to a grammar"));
    }
    for key in ["minProperties", "maxProperties"] {
        if object.contains_key(key) {
            found.push(format!("{path}: `{key}` is not accepted"));
        }
    }
    for key in ["minLength", "maxLength", "minItems", "maxItems"] {
        if object
            .get(key)
            .is_some_and(|bound| bound.as_u64().is_none_or(|n| n > LLAMA_MAX_REPETITION))
        {
            found.push(format!(
                "{path}: `{key}` exceeds the grammar repetition bound"
            ));
        }
    }
}

/// Assert that every profile accepts `schema` once normalised, and that
/// normalising it again changes nothing. The inventory tests of the toolkit
/// families, the internal tools and the sub-agent tools call it per tool.
///
/// # Panics
///
/// When a profile would still refuse the normalised schema, or normalising
/// is not idempotent: the message names the profile and the construct.
pub fn assert_wire_valid(label: &str, schema: &Value) {
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(schema, profile);
        let found = wire_violations(&out, profile);
        assert!(
            found.is_empty(),
            "{label} for {profile:?}: {found:#?}\n{out}"
        );
        assert_eq!(
            normalize_tool_schema(&out, profile),
            out,
            "{label} for {profile:?}: normalising twice changed the schema"
        );
    }
}
