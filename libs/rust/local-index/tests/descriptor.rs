//! The index tools take the cloud Inventory tools' arguments: every tool's
//! property names, required flags and defaults equal the descriptor's
//! (`services/elitea-subapp-host/internal/apps/inventory/descriptor.json`),
//! for the toolkit whose handler it is routed to. A change on either side
//! fails here until both agree.

use elitea_local_index::tools::TOOLS;
use serde_json::Value;
use std::collections::BTreeMap;

fn descriptor() -> Value {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../services/elitea-subapp-host/internal/apps/inventory/descriptor.json"
    );
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{path}: {e}"));
    serde_json::from_str(&text).unwrap()
}

/// `name → (required, default)` of a cloud `args_schema`.
fn cloud(args: &Value) -> BTreeMap<String, (bool, Option<Value>)> {
    args.as_object()
        .unwrap()
        .iter()
        .map(|(name, arg)| {
            (
                name.clone(),
                (
                    arg.get("required")
                        .and_then(Value::as_bool)
                        .unwrap_or(false),
                    arg.get("default").cloned(),
                ),
            )
        })
        .collect()
}

/// The same of a JSON Schema.
fn local(schema: &Value) -> BTreeMap<String, (bool, Option<Value>)> {
    let required: Vec<&str> = schema["required"]
        .as_array()
        .map(|names| names.iter().filter_map(Value::as_str).collect())
        .unwrap_or_default();
    schema["properties"]
        .as_object()
        .unwrap()
        .iter()
        .map(|(name, property)| {
            (
                name.clone(),
                (
                    required.contains(&name.as_str()),
                    property.get("default").cloned(),
                ),
            )
        })
        .collect()
}

fn cloud_type(kind: &str) -> &'static str {
    match kind {
        "String" => "string",
        "Integer" => "integer",
        "Boolean" => "boolean",
        "Number" | "Float" => "number",
        other => panic!("an argument type the drift test does not know: {other}"),
    }
}

#[test]
fn every_index_tool_takes_the_cloud_tools_arguments() {
    let descriptor = descriptor();
    let toolkits = descriptor["provided_toolkits"].as_array().unwrap();
    for spec in &TOOLS {
        let toolkit = toolkits
            .iter()
            .find(|toolkit| toolkit["name"] == spec.family)
            .unwrap_or_else(|| panic!("no {} toolkit in the descriptor", spec.family));
        let tool = toolkit["provided_tools"]
            .as_array()
            .unwrap()
            .iter()
            .find(|tool| tool["name"] == spec.name)
            .unwrap_or_else(|| panic!("{} is not a {} tool", spec.name, spec.family));
        let schema = (spec.parameters)();
        assert_eq!(
            local(&schema),
            cloud(&tool["args_schema"]),
            "{}: names, required flags and defaults",
            spec.name
        );
        for (name, arg) in tool["args_schema"].as_object().unwrap() {
            assert_eq!(
                schema["properties"][name]["type"],
                cloud_type(arg["type"].as_str().unwrap()),
                "{}.{name}: type",
                spec.name
            );
        }
    }
}
