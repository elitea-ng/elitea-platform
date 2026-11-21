//! The `analyse_jira` toolkit type's entry of elitea-main's
//! `rust_only_toolkit_tools.json` (`new_types`), authored here because the
//! type has no schema Main pins (D1 of epic #2254, #2255, and D7: #2263).
//!
//! Provenance. Upstream's `elitea_sdk/community/__init__.py` maps
//! `analyse_jira` to `analysis.jira_analyse.AnalyseJira`, a package that is
//! not in any public branch or tag of EliteaAI/elitea-sdk, so this entry is
//! reconstructed, not captured. What survives upstream, and what this entry
//! keeps exactly:
//!
//! - the skipped integration test `tests/test_jira_analysis.py`: the tools
//!   `get_number_off_all_issues(project_keys, after_date)` and
//!   `get_jira_issues(project_keys, closed_issues_based_on, resolved_after,
//!   updated_after, created_after, add_filter)`, the wrapper settings
//!   `closed_status`, `defects_name`, `custom_fields` and `project_keys`, the
//!   artifact bucket (default `analyse-jira`), the result keys `projects` and
//!   `projects_summary`, and the sentence `Data has been extracted
//!   successfully.`;
//! - the behaviour of the library `elitea-analyse` 0.1.2 on `PyPI` that the wrapper
//!   called (the library declares no licence, so its code is not copied).
//!
//! The other tools (`get_jira_fields_overview`, `get_jira_sprints`,
//! `get_jira_statuses`, `calculate_jira_metrics`) cover the rest of what the
//! library does; they are this platform's, named in the same style. The
//! evidence the first two are held to is the test
//! `the_reconstructed_upstream_surface_is_kept` in `tests.rs`.
//!
//! The tools read their argument schemas from this entry, so the catalogue
//! and the worker cannot disagree.

use std::sync::OnceLock;

use serde_json::Value;

/// The entry. `properties` marks `jira_configuration` as a reference to a
/// saved `jira` credential: the toolkit reuses the Jira credential and adds
/// no credential type.
const ENTRY: &str = r##"
{
  "args_schemas": {
    "calculate_jira_metrics": {
      "properties": {
        "aggregate_by": {
          "anyOf": [
            {
              "items": {
                "enum": ["mean", "median", "min", "max", "sum", "count", "std"],
                "type": "string"
              },
              "type": "array"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Extra aggregations of the per-item values of each group (the mean is always calculated).",
          "title": "Aggregate By"
        },
        "group_by": {
          "anyOf": [
            {
              "items": {
                "type": "string"
              },
              "type": "array"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Columns to group by. Default: project_name, issue_type, and project_key and team when they hold values. year_month is added where the metric is monthly.",
          "title": "Group By"
        },
        "metric": {
          "description": "The metric: lead_time, cycle_time, life_time, throughput, flow_efficiency, flow_load, or a bundle: closed_items (lead time, cycle time, count), open_items (life time, count by status), queue (lead time, throughput, WIP), flow (efficiency, load, time, velocity).",
          "enum": [
            "lead_time",
            "cycle_time",
            "life_time",
            "throughput",
            "flow_efficiency",
            "flow_load",
            "closed_items",
            "open_items",
            "queue",
            "flow"
          ],
          "title": "Metric",
          "type": "string"
        },
        "statuses_file": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Artifact path /{bucket}/{filename} of the statuses file get_jira_issues wrote (map_statuses_*.csv). Needed by cycle_time, flow_efficiency, closed_items and flow.",
          "title": "Statuses File"
        },
        "work_items_file": {
          "description": "Artifact path /{bucket}/{filename} of the work-items file get_jira_issues wrote (data_work_items_*.csv).",
          "title": "Work Items File",
          "type": "string"
        }
      },
      "required": [
        "metric",
        "work_items_file"
      ],
      "title": "CalculateJiraMetricsArgs",
      "type": "object"
    },
    "get_jira_fields_overview": {
      "properties": {
        "project_keys": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Comma-separated project keys. Default: the toolkit's project keys.",
          "title": "Project Keys"
        },
        "updated_after": {
          "description": "Only issues updated on or after this date (YYYY-MM-DD).",
          "title": "Updated After",
          "type": "string"
        }
      },
      "required": [
        "updated_after"
      ],
      "title": "GetJiraFieldsOverviewArgs",
      "type": "object"
    },
    "get_jira_issues": {
      "properties": {
        "add_filter": {
          "default": "",
          "description": "Extra JQL condition added to the extraction with AND, without ORDER BY. Example: labels = \"release\".",
          "title": "Add Filter",
          "type": "string"
        },
        "closed_issues_based_on": {
          "description": "1: an issue is closed when its status is the toolkit's closed status. 2: an issue is closed when it has a resolution date.",
          "enum": [
            1,
            2
          ],
          "title": "Closed Issues Based On",
          "type": "integer"
        },
        "created_after": {
          "description": "Defects created on or after this date (YYYY-MM-DD) are listed as defects.",
          "title": "Created After",
          "type": "string"
        },
        "project_keys": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Comma-separated project keys. Default: the toolkit's project keys.",
          "title": "Project Keys"
        },
        "resolved_after": {
          "description": "Closed issues resolved on or after this date (YYYY-MM-DD).",
          "title": "Resolved After",
          "type": "string"
        },
        "updated_after": {
          "description": "Open issues, and closed ones when closed_issues_based_on is 1, updated on or after this date (YYYY-MM-DD). With 2, closed issues are chosen by resolved_after.",
          "title": "Updated After",
          "type": "string"
        }
      },
      "required": [
        "closed_issues_based_on",
        "resolved_after",
        "updated_after",
        "created_after"
      ],
      "title": "GetJiraIssuesArgs",
      "type": "object"
    },
    "get_jira_sprints": {
      "properties": {
        "project_keys": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Comma-separated project keys. Default: the toolkit's project keys.",
          "title": "Project Keys"
        }
      },
      "title": "GetJiraSprintsArgs",
      "type": "object"
    },
    "get_jira_statuses": {
      "properties": {},
      "title": "GetJiraStatusesArgs",
      "type": "object"
    },
    "get_number_off_all_issues": {
      "properties": {
        "after_date": {
          "description": "Count issues updated on or after this date (YYYY-MM-DD).",
          "title": "After Date",
          "type": "string"
        },
        "project_keys": {
          "anyOf": [
            {
              "type": "string"
            },
            {
              "type": "null"
            }
          ],
          "default": null,
          "description": "Comma-separated project keys. Default: the toolkit's project keys, or every project the credential can see.",
          "title": "Project Keys"
        }
      },
      "required": [
        "after_date"
      ],
      "title": "GetJiraFieldsArgs",
      "type": "object"
    }
  },
  "metadata": {
    "categories": [
      "project management"
    ],
    "check_connection_supported": false,
    "extra_categories": [
      "analysis",
      "jira",
      "atlassian",
      "flow metrics",
      "cycle time",
      "lead time",
      "throughput"
    ],
    "has_function_validators": false,
    "icon_url": "jira-icon.svg",
    "label": "Jira analysis"
  },
  "properties": {
    "jira_configuration": {
      "configuration_types": [
        "jira"
      ]
    }
  },
  "settings": {
    "properties": {
      "api_version": {
        "default": "Auto",
        "description": "REST API version used for all Jira reads.\n\n• **Auto** (default) — selected from the Hosting setting of the linked credential (Cloud → V3, Server → V2)\n• **V3** — Jira Cloud (*.atlassian.net)\n• **V2** — Jira Server / Data Center",
        "enum": [
          "Auto",
          "2",
          "3"
        ],
        "title": "Api Version",
        "type": "string"
      },
      "artifact_bucket_path": {
        "default": "analyse-jira",
        "description": "Artifact bucket the datasets are written to. It is created when it does not exist.",
        "title": "Artifact Bucket Path",
        "type": "string"
      },
      "closed_status": {
        "default": "Done",
        "description": "Status name that marks an issue closed when closed_issues_based_on is 1.",
        "title": "Closed Status",
        "type": "string"
      },
      "custom_fields": {
        "additionalProperties": {
          "type": "string"
        },
        "default": {
          "team": ""
        },
        "description": "Extra dataset columns taken from Jira fields: the column name, and the field name or comma-separated field names it reads (the first one with a value wins). Example: {\"team\": \"Team\", \"story_points\": \"Story Points\"}. The team column is always present.",
        "title": "Custom Fields",
        "type": "object"
      },
      "defects_name": {
        "default": "Defect",
        "description": "Issue type name, or comma-separated names, that count as defects.",
        "title": "Defects Name",
        "type": "string"
      },
      "jira_configuration": {
        "$ref": "#/$defs/JiraConfiguration",
        "configuration_types": [
          "jira"
        ],
        "description": "Select the credentials used to connect this toolkit to Jira.\n\n**Project credentials** are shared with the project team.\n**Private credentials** are visible only to you."
      },
      "max_issues": {
        "default": 5000,
        "description": "Most issues one extraction reads. An extraction that would read more writes nothing and says so: narrow the projects, the dates or the filter, or raise this.",
        "maximum": 50000,
        "minimum": 1,
        "title": "Max Issues",
        "type": "integer"
      },
      "project_keys": {
        "default": "",
        "description": "Comma-separated project keys the tools use when a call names none.",
        "title": "Project Keys",
        "type": "string"
      },
      "selected_tools": {
        "default": [],
        "items": {
          "enum": [
            "get_number_off_all_issues",
            "get_jira_issues",
            "get_jira_fields_overview",
            "get_jira_sprints",
            "get_jira_statuses",
            "calculate_jira_metrics"
          ],
          "type": "string"
        },
        "title": "Selected Tools",
        "type": "array"
      },
      "verify_ssl": {
        "default": true,
        "description": "Verify the TLS certificate of your Jira instance. Certificates are always verified by this platform; turning this off makes the toolkit unavailable.",
        "title": "Verify Ssl",
        "type": "boolean"
      }
    },
    "required": [
      "jira_configuration"
    ],
    "title": "analyse_jira",
    "type": "object"
  }
}
"##;

fn parsed() -> Option<&'static Value> {
    static PARSED: OnceLock<Option<Value>> = OnceLock::new();
    PARSED
        .get_or_init(|| serde_json::from_str(ENTRY).ok())
        .as_ref()
}

/// One tool's argument schema, as the entry declares it. `None` only if the
/// entry above were not JSON, which its own test pins.
pub(super) fn tool_args_schema(tool: &str) -> Option<Value> {
    parsed()?.get("args_schemas")?.get(tool).cloned()
}

#[cfg(test)]
pub(crate) fn analyse_jira_type_entry() -> Value {
    serde_json::from_str(ENTRY).expect("the analyse_jira catalogue entry is valid JSON")
}
