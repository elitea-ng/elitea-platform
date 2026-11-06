//! The `browser` entry of elitea-main's `rust_only_toolkit_tools.json`
//! (`new_types`): the type has no SDK counterpart, so the toolkit editor's
//! form and tool list come from here, and every schema from the browser
//! tool contract (`elitea_browser_tools::TOOLS`).

use serde_json::{Value, json};

use elitea_browser_tools::TOOLS;

pub(crate) fn browser_type_entry() -> Value {
    let mut names: Vec<&str> = TOOLS.iter().map(|spec| spec.name).collect();
    names.sort_unstable();
    let schemas = TOOLS
        .iter()
        .map(|spec| {
            let mut schema = (spec.parameters)();
            schema["title"] = json!(spec.name);
            (spec.name.to_owned(), schema)
        })
        .collect::<serde_json::Map<String, Value>>();
    json!({
        "metadata": {
            "categories": ["other"],
            "check_connection_supported": false,
            "extra_categories": ["browser", "web", "browse", "automation"],
            "has_function_validators": false,
            "icon_url": null,
            "label": "Cloud Browser",
        },
        "settings": {
            "properties": {
                "selected_tools": {
                    "default": [],
                    "items": {"enum": names, "type": "string"},
                    "title": "Selected Tools",
                    "type": "array",
                },
            },
            "required": [],
            "title": "browser",
            "type": "object",
        },
        "args_schemas": Value::Object(schemas),
    })
}
