//! The `tavily` toolkit type's entry of elitea-main's
//! `rust_only_toolkit_tools.json` (`new_types`): the SDK's `tavily` toolkit
//! (elitea-sdk `elitea_sdk/tools/tavily`, commit 0c1ce249, 2026-10-19) is newer
//! than the SDK snapshot Main pins, so this crate owns its catalogue schema
//! (D1 of epic #2254, #2255).
//!
//! The document below is the SDK's `toolkit_config_schema()` and the argument
//! schema of its four tools, verbatim, as pydantic emits them. It is the worked
//! example of `docs/contributing/adding-types.md`. Main compares it with the
//! captured upstream fixture
//! (`services/elitea-main/internal/runtimecomposition/testdata/sdk-fixtures/tavily.json`),
//! and `families::rust_only_catalogue_document` compares it with the file Main
//! embeds.
//!
//! The family that builds the type is `tavily_toolkit.rs` (#2261, which also
//! adds `web_crawl` and `web_map` to the `web` family); it reads its tools'
//! argument schemas from this entry, so the catalogue and the worker cannot
//! disagree. `tavily` is in the worker capability snapshot, so the type is served.

use std::sync::OnceLock;

use serde_json::Value;

/// The entry, as the SDK declares it. `properties` is the annotation the
/// snapshot carries beside the settings: `tavily_configuration` is a reference
/// to a saved `tavily` credential.
const ENTRY: &str = r##"
{
  "args_schemas": {
    "web_crawl": {
      "properties": {
        "instructions": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "What to look for (costs more)",
          "title": "Instructions"
        },
        "limit": {
          "default": 10,
          "description": "Maximum pages",
          "maximum": 20,
          "minimum": 1,
          "title": "Limit",
          "type": "integer"
        },
        "max_depth": {
          "default": 1,
          "description": "Link depth to follow",
          "maximum": 2,
          "minimum": 1,
          "title": "Max Depth",
          "type": "integer"
        },
        "url": {
          "description": "Start URL (http/https)",
          "title": "Url",
          "type": "string"
        }
      },
      "required": [
        "url"
      ],
      "title": "TavilyWebCrawl",
      "type": "object"
    },
    "web_extract": {
      "properties": {
        "urls": {
          "description": "Page URLs (http/https), at most 5",
          "items": {
            "type": "string"
          },
          "title": "Urls",
          "type": "array"
        }
      },
      "required": [
        "urls"
      ],
      "title": "TavilyWebExtract",
      "type": "object"
    },
    "web_map": {
      "properties": {
        "instructions": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "What to look for (costs more)",
          "title": "Instructions"
        },
        "limit": {
          "default": 20,
          "description": "Maximum URLs",
          "maximum": 50,
          "minimum": 1,
          "title": "Limit",
          "type": "integer"
        },
        "max_depth": {
          "default": 1,
          "description": "Link depth to follow",
          "maximum": 2,
          "minimum": 1,
          "title": "Max Depth",
          "type": "integer"
        },
        "url": {
          "description": "Start URL (http/https)",
          "title": "Url",
          "type": "string"
        }
      },
      "required": [
        "url"
      ],
      "title": "TavilyWebMap",
      "type": "object"
    },
    "web_search": {
      "properties": {
        "query": {
          "description": "Search query",
          "maxLength": 400,
          "title": "Query",
          "type": "string"
        },
        "search_depth": {
          "anyOf": [
            {
              "enum": [
                "basic",
                "advanced"
              ],
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "'advanced' gives better results at double cost, if the toolkit allows it",
          "title": "Search Depth"
        },
        "time_range": {
          "anyOf": [
            {
              "enum": [
                "day",
                "week",
                "month",
                "year"
              ],
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Only results from this recent period",
          "title": "Time Range"
        },
        "topic": {
          "anyOf": [
            {
              "enum": [
                "general",
                "news",
                "finance"
              ],
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Search category",
          "title": "Topic"
        }
      },
      "required": [
        "query"
      ],
      "title": "TavilyWebSearch",
      "type": "object"
    }
  },
  "metadata": {
    "categories": [
      "other"
    ],
    "check_connection_supported": false,
    "extra_categories": [
      "tavily",
      "web",
      "search",
      "internet",
      "extract",
      "crawl"
    ],
    "has_function_validators": false,
    "icon_url": null,
    "label": "Tavily",
    "tool_spend": {
      "backend": "tavily",
      "chat_tools": [
        "web_search",
        "web_extract"
      ],
      "configuration_type": "tavily",
      "feature": "web_search",
      "label": "Web search",
      "max_units_per_call": 12,
      "operations": [
        "search",
        "extract",
        "crawl",
        "map"
      ],
      "unit": "credit"
    }
  },
  "properties": {
    "tavily_configuration": {
      "configuration_types": [
        "tavily"
      ]
    }
  },
  "settings": {
    "properties": {
      "allowed_domains": {
        "default": [],
        "description": "Only these domains (empty = any)",
        "items": {
          "type": "string"
        },
        "title": "Allowed Domains",
        "type": "array"
      },
      "blocked_domains": {
        "default": [],
        "description": "Never these domains",
        "items": {
          "type": "string"
        },
        "title": "Blocked Domains",
        "type": "array"
      },
      "extract_depth": {
        "default": "basic",
        "description": "Extraction depth ('advanced' costs double)",
        "enum": [
          "basic",
          "advanced"
        ],
        "title": "Extract Depth",
        "type": "string"
      },
      "max_page_chars": {
        "default": 20000,
        "description": "Maximum characters per page",
        "maximum": 50000,
        "minimum": 1000,
        "title": "Max Page Chars",
        "type": "integer"
      },
      "max_results": {
        "default": 5,
        "description": "Search results per query",
        "maximum": 10,
        "minimum": 1,
        "title": "Max Results",
        "type": "integer"
      },
      "max_search_depth": {
        "default": "basic",
        "description": "Deepest search the model may request (advanced costs 2 credits per query)",
        "enum": [
          "basic",
          "advanced"
        ],
        "title": "Max Search Depth",
        "type": "string"
      },
      "per_run_call_cap": {
        "default": 10,
        "description": "Maximum Tavily calls per run",
        "maximum": 100,
        "minimum": 1,
        "title": "Per Run Call Cap",
        "type": "integer"
      },
      "selected_tools": {
        "default": [],
        "items": {
          "enum": [
            "web_search",
            "web_extract",
            "web_crawl",
            "web_map"
          ],
          "type": "string"
        },
        "title": "Selected Tools",
        "type": "array"
      },
      "tavily_configuration": {
        "$ref": "#/$defs/TavilyConfiguration",
        "configuration_types": [
          "tavily"
        ],
        "description": "Tavily Configuration"
      }
    },
    "required": [
      "tavily_configuration"
    ],
    "title": "tavily",
    "type": "object"
  }
}
"##;

/// One tool's argument schema, as the SDK declares it. `None` only if the
/// entry above were not JSON, which its own test pins.
pub(crate) fn tool_args_schema(tool: &str) -> Option<Value> {
    static PARSED: OnceLock<Option<Value>> = OnceLock::new();
    PARSED
        .get_or_init(|| serde_json::from_str(ENTRY).ok())
        .as_ref()?
        .get("args_schemas")?
        .get(tool)
        .cloned()
}

#[cfg(test)]
pub(crate) fn tavily_type_entry() -> Value {
    serde_json::from_str(ENTRY).expect("the tavily catalogue entry is valid JSON")
}
