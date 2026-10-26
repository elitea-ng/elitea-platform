//! The `web` and `host_intel` entries of elitea-main's
//! `rust_only_toolkit_tools.json` (`new_types`): neither type has an SDK
//! counterpart, so the toolkit editor's form and tool list come from here.
//! `families::rust_only_catalogue_document` compares them with the file.

use serde_json::{Value, json};

use super::capability::{fetch, host_intel, web_search};

fn titled(mut schema: Value, title: &str) -> Value {
    schema["title"] = json!(title);
    schema
}

fn schemas(tools: &[(&str, Value)]) -> Value {
    Value::Object(
        tools
            .iter()
            .map(|(name, schema)| ((*name).to_owned(), schema.clone()))
            .collect(),
    )
}

fn metadata(label: &str, extra_categories: &[&str]) -> Value {
    json!({
        "categories": ["other"],
        "check_connection_supported": false,
        "extra_categories": extra_categories,
        "has_function_validators": false,
        "icon_url": null,
        "label": label,
    })
}

/// Sorted, as the Office entries are, so the document does not depend on
/// `preserve_order`.
fn selected_tools(names: &[&str]) -> Value {
    let mut names = names.to_vec();
    names.sort_unstable();
    json!({
        "default": [],
        "items": {"enum": names, "type": "string"},
        "title": "Selected Tools",
        "type": "array",
    })
}

pub(crate) fn web_type_entry() -> Value {
    json!({
        "metadata": metadata("Web Search", &["web", "search", "fetch", "browse", "documentation"]),
        "settings": {
            "properties": {
                "default_fetch_backend": {
                    "default": "direct",
                    "description": "How web_fetch reads a page when a call names no provider: direct (through the platform's egress guard) or a provider's extract API.",
                    "enum": ["direct", "tavily", "firecrawl"],
                    "title": "Default Fetch Backend",
                    "type": "string",
                },
                "default_search_provider": {
                    "anyOf": [
                        {"enum": ["tavily", "firecrawl", "brave", "ddg_instant"], "type": "string"},
                        {"type": "null"},
                    ],
                    "default": null,
                    "description": "The search provider this toolkit uses when a call names none. Empty uses the platform default.",
                    "title": "Default Search Provider",
                },
                "selected_tools": selected_tools(&[web_search::NAME, fetch::NAME]),
                "strict_options": {
                    "default": true,
                    "description": "Refuse a search option the provider cannot honour. Off, the option is dropped and listed in ignored_options.",
                    "title": "Strict Options",
                    "type": "boolean",
                },
            },
            "required": [],
            "title": "web",
            "type": "object",
        },
        "args_schemas": schemas(&[
            (web_search::NAME, titled(web_search::schema(), "WebSearch")),
            (fetch::NAME, titled(fetch::schema(), "WebFetch")),
        ]),
    })
}

pub(crate) fn host_intel_type_entry() -> Value {
    json!({
        "metadata": metadata("Host Intelligence", &["shodan", "hosts", "ports", "cve", "security"]),
        "settings": {
            "properties": {
                "selected_tools": selected_tools(&[host_intel::SEARCH, host_intel::LOOKUP]),
            },
            "required": [],
            "title": "host_intel",
            "type": "object",
        },
        "args_schemas": schemas(&[
            (host_intel::SEARCH, titled(host_intel::search_schema(), "InternetHostSearch")),
            (host_intel::LOOKUP, titled(host_intel::lookup_schema(), "InternetHostLookup")),
        ]),
    })
}
