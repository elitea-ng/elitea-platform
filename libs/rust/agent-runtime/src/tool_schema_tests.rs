//! One test per construct the providers refuse, and the round trip: every
//! schema the runtime can emit must normalise to a schema every profile
//! accepts. The toolkit families are covered by the SDK conformance gate
//! (`toolkits::sdk_conformance::assert_sdk_conformance` runs the same check
//! over every tool a family serves); this file covers the internal tools and
//! representative MCP schemas, which are external.

use std::sync::Arc;

use adk_tool::SimpleToolContext;
use serde_json::{Value, json};

use super::*;

/// What an MCP server may advertise: draft-07 markers, shared and recursive
/// definitions, nullable unions, `const`, `oneOf`, tuples, pattern maps,
/// lookaround patterns, huge bounds and an untyped property.
fn mcp_fixture() -> Value {
    json!({
        "$schema": "http://json-schema.org/draft-07/schema#",
        "type": "object",
        "properties": {
            "node": {"$ref": "#/$defs/node"},
            "mode": {"type": "integer", "enum": [1, 2, 3], "default": 1},
            "name": {
                "type": ["string", "null"],
                "format": "hostname",
                "pattern": "^(?=a)b$",
                "maxLength": 65536,
                "default": null
            },
            "tags": {
                "type": "array",
                "items": [{"type": "string"}, {"type": "integer"}],
                "uniqueItems": true,
                "minItems": 1,
                "maxItems": 4096
            },
            "env": {
                "type": "object",
                "patternProperties": {"^[A-Z]+$": {"type": "string"}},
                "additionalProperties": {"type": "string"}
            },
            "kind": {"const": "fixed"},
            "choice": {"oneOf": [
                {"type": "string"},
                {"type": "integer", "const": -1},
                {"type": "null"}
            ]},
            "limit": {"type": "integer", "exclusiveMinimum": 0, "multipleOf": 5, "minimum": 0},
            "anything": {},
            "flag": {"not": {"type": "null"}, "type": "boolean"},
            "merged": {"allOf": [
                {"$ref": "#/$defs/node"},
                {"properties": {"extra": {"type": "string"}}, "required": ["extra"]}
            ]}
        },
        "required": ["node", "ghost"],
        "$defs": {
            "node": {
                "type": "object",
                "properties": {
                    "value": {"type": "string"},
                    "child": {"$ref": "#/$defs/node"}
                },
                "required": ["value"]
            }
        },
        "additionalProperties": false
    })
}

fn property<'a>(schema: &'a Value, name: &str) -> &'a Value {
    &schema["properties"][name]
}

#[test]
fn a_root_any_of_of_objects_becomes_one_object() {
    let raw = json!({"anyOf": [
        {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]},
        {"type": "object", "properties": {"b": {"type": "integer"}}, "required": ["b"]}
    ]});
    let out = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(out["type"], "object");
    assert_eq!(property(&out, "a")["type"], "string");
    assert_eq!(property(&out, "b")["type"], "integer");
    assert!(out.get("anyOf").is_none());
    assert!(out.get("required").is_none(), "neither is required by both");
    let description = out["description"].as_str().expect("a note");
    assert!(description.contains("a, b"), "{description}");
    assert!(
        wire_violations(&raw, SchemaProfile::OpenAi)
            .iter()
            .any(|line| line.contains("root"))
    );
}

#[test]
fn a_name_every_root_variant_requires_stays_required() {
    let raw = json!({"anyOf": [
        {"type": "object", "properties": {"id": {"type": "string"}, "a": {"type": "string"}}, "required": ["id", "a"]},
        {"type": "object", "properties": {"id": {"type": "string"}, "b": {"type": "string"}}, "required": ["id"]}
    ]});
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        assert_eq!(out["required"], json!(["id"]), "{profile:?}");
    }
}

#[test]
fn references_are_inlined_and_definitions_dropped() {
    let raw = json!({
        "type": "object",
        "properties": {"p": {"$ref": "#/$defs/point", "description": "where"}},
        "$defs": {"point": {
            "type": "object",
            "properties": {"x": {"type": "number"}},
            "required": ["x"]
        }}
    });
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        let point = property(&out, "p");
        assert_eq!(point["description"], "where", "{profile:?}");
        assert_eq!(point["required"], json!(["x"]), "{profile:?}");
        assert_eq!(property(point, "x")["type"], "number", "{profile:?}");
        assert!(out.get("$defs").is_none());
        assert!(!out.to_string().contains("$ref"));
    }
}

#[test]
fn a_recursive_reference_ends_in_a_bare_object() {
    let raw = json!({
        "type": "object",
        "properties": {"tree": {"$ref": "#/$defs/n"}},
        "$defs": {"n": {"type": "object", "properties": {
            "kids": {"type": "array", "items": {"$ref": "#/$defs/n"}}
        }}}
    });
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        let inner = &property(property(&out, "tree"), "kids")["items"];
        assert_eq!(inner["type"], "object", "{profile:?}");
        assert!(inner.get("properties").is_none(), "{profile:?}");
        assert!(!out.to_string().contains("$ref"));
    }
    // The root itself as the target.
    let looping = json!({"type": "object", "properties": {"again": {"$ref": "#"}}});
    assert_wire_valid("root reference", &looping);
}

#[test]
fn a_reference_outside_the_schema_is_an_unconstrained_value() {
    let raw = json!({"type": "object", "properties": {
        "far": {"$ref": "https://example.com/schema.json"},
        "missing": {"$ref": "#/$defs/nowhere"}
    }});
    assert_wire_valid("outside reference", &raw);
    let out = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert!(property(&out, "far").get("type").is_none());
}

#[test]
fn a_shared_definition_that_expands_exponentially_is_bounded() {
    let mut defs = serde_json::Map::new();
    for level in 0..40 {
        let next = format!("#/$defs/d{}", level + 1);
        defs.insert(
            format!("d{level}"),
            json!({"type": "object", "properties": {
                "x": {"$ref": next.clone()},
                "y": {"$ref": next}
            }}),
        );
    }
    defs.insert("d40".to_owned(), json!({"type": "string"}));
    let raw = json!({
        "type": "object",
        "properties": {"z": {"$ref": "#/$defs/d0"}},
        "$defs": Value::Object(defs)
    });
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        assert!(wire_violations(&out, profile).is_empty(), "{profile:?}");
    }
}

#[test]
fn const_becomes_a_single_value_enum_where_the_provider_needs_it() {
    let raw = json!({"type": "object", "properties": {"k": {"const": "fixed"}}});
    let llama = normalize_tool_schema(&raw, SchemaProfile::LlamaCpp);
    assert_eq!(
        property(&llama, "k"),
        &json!({"enum": ["fixed"], "type": "string"})
    );
    let gemini = normalize_tool_schema(&raw, SchemaProfile::Gemini);
    assert_eq!(
        property(&gemini, "k"),
        &json!({"enum": ["fixed"], "type": "string"})
    );
    let openai = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(property(&openai, "k"), &json!({"const": "fixed"}));

    // Gemini has no enum of numbers: the value moves into the description.
    let number = json!({"type": "object", "properties": {"k": {"const": -1}}});
    let gemini = normalize_tool_schema(&number, SchemaProfile::Gemini);
    assert_eq!(
        property(&gemini, "k"),
        &json!({"type": "integer", "description": "(must equal -1)"})
    );
}

#[test]
fn one_of_becomes_any_of() {
    let raw = json!({"type": "object", "properties": {
        "v": {"oneOf": [{"type": "string"}, {"type": "integer"}]}
    }});
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        assert_eq!(
            property(&out, "v"),
            &json!({"anyOf": [{"type": "string"}, {"type": "integer"}]}),
            "{profile:?}"
        );
    }
}

#[test]
fn all_of_is_merged_and_keeps_every_required_name() {
    let raw = json!({"type": "object", "properties": {"a": {"allOf": [
        {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]},
        {"properties": {"y": {"type": "integer"}}, "required": ["y"]}
    ]}}});
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        let merged = property(&out, "a");
        assert_eq!(merged["required"], json!(["x", "y"]), "{profile:?}");
        assert_eq!(property(merged, "x")["type"], "string", "{profile:?}");
        assert_eq!(property(merged, "y")["type"], "integer", "{profile:?}");
        assert!(merged.get("allOf").is_none());
    }
}

#[test]
fn gemini_turns_null_into_nullable_and_others_keep_it() {
    let raw = json!({"type": "object", "properties": {
        "n": {"anyOf": [{"type": "integer"}, {"type": "null"}], "default": null},
        "s": {"type": ["string", "null"]}
    }});
    let gemini = normalize_tool_schema(&raw, SchemaProfile::Gemini);
    assert_eq!(
        property(&gemini, "n"),
        &json!({"nullable": true, "type": "integer"})
    );
    assert_eq!(
        property(&gemini, "s"),
        &json!({"nullable": true, "type": "string"})
    );
    let openai = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(
        property(&openai, "n"),
        &json!({"anyOf": [{"type": "integer"}, {"type": "null"}], "default": null})
    );
}

#[test]
fn gemini_drops_what_its_subset_refuses() {
    let raw = json!({"type": "object", "additionalProperties": false, "properties": {
        "m": {"type": "object", "additionalProperties": {"type": "string"}},
        "d": {"type": "string", "default": "x", "format": "email"},
        "e": {"type": "integer", "enum": [1, 2]},
        "any": {}
    }});
    let out = normalize_tool_schema(&raw, SchemaProfile::Gemini);
    assert!(out.get("additionalProperties").is_none());
    assert!(property(&out, "m").get("additionalProperties").is_none());
    assert!(
        property(&out, "m")["description"]
            .as_str()
            .is_some_and(|text| text.contains("extra keys hold string"))
    );
    let default = property(&out, "d");
    assert!(default.get("default").is_none() && default.get("format").is_none());
    let text = default["description"].as_str().expect("folded");
    assert!(
        text.contains("default \"x\"") && text.contains("format \"email\""),
        "{text}"
    );
    assert!(property(&out, "e").get("enum").is_none());
    assert_eq!(property(&out, "any")["type"], "string");
    assert_eq!(
        wire_violations(&out, SchemaProfile::Gemini),
        Vec::<String>::new()
    );
}

#[test]
fn llama_cpp_keeps_only_anchored_simple_patterns() {
    let raw = json!({"type": "object", "properties": {
        "ok": {"type": "string", "pattern": "^[a-z]+$"},
        "open": {"type": "string", "pattern": "[a-z]+"},
        "class": {"type": "string", "pattern": "^\\d+$"},
        "look": {"type": "string", "pattern": "^(?=a)b$"},
        "wide": {"type": "string", "pattern": "^a{1,500}$"}
    }});
    let out = normalize_tool_schema(&raw, SchemaProfile::LlamaCpp);
    assert_eq!(property(&out, "ok")["pattern"], "^[a-z]+$");
    for name in ["open", "class", "look", "wide"] {
        let node = property(&out, name);
        assert!(node.get("pattern").is_none(), "{name}");
        assert!(
            node["description"]
                .as_str()
                .is_some_and(|text| text.contains("pattern")),
            "{name}"
        );
    }
    // The others carry them as written.
    let openai = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(property(&openai, "look")["pattern"], "^(?=a)b$");
    // Gemini's RE2 refuses only lookaround.
    let gemini = normalize_tool_schema(&raw, SchemaProfile::Gemini);
    assert!(property(&gemini, "look").get("pattern").is_none());
    assert_eq!(property(&gemini, "class")["pattern"], "^\\d+$");
}

#[test]
fn large_bounds_are_folded_for_llama_cpp_only() {
    let raw = json!({"type": "object", "properties": {
        "s": {"type": "string", "minLength": 1, "maxLength": 65536},
        "o": {"type": "object", "maxProperties": 4}
    }});
    let llama = normalize_tool_schema(&raw, SchemaProfile::LlamaCpp);
    assert_eq!(property(&llama, "s")["minLength"], 1);
    assert!(property(&llama, "s").get("maxLength").is_none());
    assert!(
        property(&llama, "s")["description"]
            .as_str()
            .is_some_and(|text| text.contains("maxLength 65536"))
    );
    assert!(property(&llama, "o").get("maxProperties").is_none());
    for profile in [
        SchemaProfile::OpenAi,
        SchemaProfile::Anthropic,
        SchemaProfile::Gemini,
    ] {
        let out = normalize_tool_schema(&raw, profile);
        assert_eq!(property(&out, "s")["maxLength"], 65536, "{profile:?}");
    }
}

#[test]
fn a_required_name_without_a_property_gets_one() {
    let raw = json!({"type": "object", "required": ["ghost"]});
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        assert_eq!(out["required"], json!(["ghost"]), "{profile:?}");
        assert!(out["properties"].get("ghost").is_some(), "{profile:?}");
    }
}

#[test]
fn an_empty_or_missing_schema_is_an_object_with_no_arguments() {
    for raw in [json!({}), json!(true), json!(null)] {
        assert_wire_valid("empty parameters", &raw);
        let out = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
        assert_eq!(out["type"], "object");
        assert_eq!(out["properties"], json!({}));
    }
}

#[test]
fn the_mcp_fixture_is_refused_raw_and_valid_normalised_for_every_profile() {
    let raw = mcp_fixture();
    for profile in SchemaProfile::ALL {
        assert!(
            !wire_violations(&raw, profile).is_empty(),
            "{profile:?} should refuse the raw fixture"
        );
    }
    assert_wire_valid("mcp fixture", &raw);
}

#[test]
fn no_property_or_required_name_is_lost() {
    let raw = mcp_fixture();
    let names: Vec<&String> = raw["properties"]
        .as_object()
        .expect("props")
        .keys()
        .collect();
    for profile in SchemaProfile::ALL {
        let out = normalize_tool_schema(&raw, profile);
        for name in &names {
            assert!(
                out["properties"].get(name.as_str()).is_some(),
                "{profile:?} {name}"
            );
        }
        assert_eq!(out["required"], json!(["node", "ghost"]), "{profile:?}");
        // A name the extra branch requires survives the merge too.
        assert_eq!(
            property(&out, "merged")["required"],
            json!(["value", "extra"]),
            "{profile:?}"
        );
    }
}

#[test]
fn the_model_name_picks_the_profile() {
    let pick = SchemaProfile::for_model;
    assert_eq!(pick("gemini-2.5-pro"), SchemaProfile::Gemini);
    assert_eq!(pick("Claude-Sonnet-5"), SchemaProfile::Anthropic);
    assert_eq!(pick("gpt-5"), SchemaProfile::OpenAi);
    assert_eq!(pick("o3"), SchemaProfile::OpenAi);
    assert_eq!(pick("o3-mini"), SchemaProfile::OpenAi);
    assert_eq!(pick("amazon.nova-pro"), SchemaProfile::Bedrock);
    assert_eq!(pick("orca-mini"), SchemaProfile::LlamaCpp);
    assert_eq!(pick("qwen3-0.6b"), SchemaProfile::LlamaCpp);
}

#[tokio::test]
async fn the_planner_tools_are_valid_for_every_profile() {
    let tools = crate::planner::toolset()
        .tools(Arc::new(SimpleToolContext::new("schema-inventory")))
        .await
        .expect("planner tools");
    assert!(!tools.is_empty());
    for tool in tools {
        let schema = tool.parameters_schema().unwrap_or_else(|| json!({}));
        assert_wire_valid(tool.name(), &schema);
    }
}

#[test]
fn a_percent_encoded_reference_resolves() {
    let raw = json!({
        "type": "object",
        "properties": {"t": {"$ref": "#/$defs/My%20Type"}},
        "$defs": {"My Type": {"type": "string"}}
    });
    let out = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(property(&out, "t")["type"], "string");
}

#[test]
fn all_of_enums_intersect() {
    let raw = json!({"type": "object", "properties": {"e": {"allOf": [
        {"type": "string", "enum": ["a", "b"]},
        {"enum": ["b", "c"]}
    ]}}});
    let out = normalize_tool_schema(&raw, SchemaProfile::OpenAi);
    assert_eq!(property(&out, "e")["enum"], json!(["b"]));
}

#[test]
fn a_folded_constraint_survives_a_single_member_union() {
    let raw = json!({"type": "object", "properties": {"u": {
        "description": "A link.",
        "anyOf": [{"type": "string", "format": "uri"}, {"type": "null"}]
    }}});
    let out = normalize_tool_schema(&raw, SchemaProfile::Gemini);
    let text = property(&out, "u")["description"].as_str().expect("text");
    assert!(text.starts_with("A link."), "{text}");
    assert!(text.contains("format \"uri\""), "{text}");
}

#[test]
fn a_provider_prefixed_model_name_picks_the_provider() {
    assert_eq!(
        SchemaProfile::for_model("openai/gpt-4o"),
        SchemaProfile::OpenAi
    );
    assert_eq!(
        SchemaProfile::for_model("azure/gpt-4.1"),
        SchemaProfile::OpenAi
    );
    assert_eq!(
        SchemaProfile::for_model("vertex_ai/gemini-2.5-flash"),
        SchemaProfile::Gemini
    );
}
