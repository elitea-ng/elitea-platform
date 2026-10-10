//! Work item tracking calls shared by `ado_boards` and `ado_plans`
//! (`tools/ado/work_item/ado_wrapper.py`, `WorkItemTrackingClient` v7.1).

use reqwest::Method;
use serde_json::{Map, Value, json};

use super::client::{
    AdoClient, AdoClientError, AdoRequest, AdoScope, invalid_response, response_shape_failure,
};
use super::format::as_dict;

/// `get/create/update/delete_work_item` route version in the v7.1 client.
pub(crate) const WORK_ITEMS_API: &str = "7.1-preview.3";
/// `get_work_item_type` route version.
pub(crate) const WORK_ITEM_TYPES_API: &str = "7.1-preview.2";

/// The fields `_parse_work_items` reads when the caller names none.
pub(crate) const DEFAULT_SEARCH_FIELDS: &[&str] = &[
    "System.Title",
    "System.State",
    "System.AssignedTo",
    "System.WorkItemType",
    "System.CreatedDate",
    "System.ChangedDate",
];

const COMMON_OPTIONAL_FIELDS: &[&str] = &[
    "System.AssignedTo",
    "System.AreaPath",
    "System.IterationPath",
    "Microsoft.VSTS.Common.Priority",
    "System.Tags",
    "System.State",
];

/// `GET {project}/_apis/wit/workitems/{id}` with the SDK's optional
/// `fields`, `asOf` and `$expand`.
pub(crate) async fn get_work_item(
    client: &AdoClient,
    id: u64,
    fields: Option<&[String]>,
    as_of: Option<&str>,
    expand: Option<&str>,
) -> Result<Value, AdoClientError> {
    let id = id.to_string();
    let segments = ["wit", "workitems", id.as_str()];
    let request = AdoRequest::get(&segments, WORK_ITEMS_API)
        .query_opt("fields", fields.map(|fields| fields.join(",")))
        .query_opt("asOf", as_of)
        .query_opt("$expand", expand);
    client.json(request, false).await
}

/// `get_work_item`'s result: `id`, the `_workitems/edit` URL, the requested
/// fields (`"N/A"` when absent) or every field, and `relations` when the
/// expand asked for them.
pub(crate) fn work_item_result(
    organization: &str,
    raw: &Value,
    fields: Option<&[String]>,
    expand: Option<&str>,
) -> Result<Value, AdoClientError> {
    let id = raw.get("id").cloned().ok_or_else(invalid_response)?;
    let url = format!("{organization}/_workitems/edit/{}", id_text(&id));
    let mut ordered = Map::new();
    ordered.insert("id".to_owned(), id);
    ordered.insert("url".to_owned(), Value::String(url));
    let field_values = raw.get("fields").and_then(Value::as_object);
    match fields {
        Some(fields) if !fields.is_empty() => {
            for field in fields {
                ordered.insert(
                    field.clone(),
                    field_values
                        .and_then(|values| values.get(field))
                        .cloned()
                        .unwrap_or_else(|| Value::String("N/A".to_owned())),
                );
            }
        }
        _ => {
            if let Some(values) = field_values {
                for (name, value) in values {
                    ordered.insert(name.clone(), value.clone());
                }
            }
        }
    }
    let wants_relations = expand
        .is_some_and(|expand| matches!(expand.to_ascii_lowercase().as_str(), "relations" | "all"));
    if wants_relations
        && let Some(relations) = raw
            .get("relations")
            .and_then(Value::as_array)
            .filter(|relations| !relations.is_empty())
    {
        ordered.insert(
            "relations".to_owned(),
            Value::Array(relations.iter().map(as_dict).collect()),
        );
    }
    Ok(Value::Object(ordered))
}

/// The id as the SDK prints it in a URL or message.
pub(crate) fn id_text(id: &Value) -> String {
    match id {
        Value::String(text) => text.clone(),
        other => other.to_string(),
    }
}

/// `_transform_work_item`: `{"fields": {...}}` becomes one JSON Patch `add`
/// per field. The error is the SDK's own model-visible text.
pub(crate) fn transform_work_item(work_item_json: &str) -> Result<Vec<Value>, String> {
    let parsed: Value = serde_json::from_str(work_item_json)
        .map_err(|error| format!("Issues during attempt to parse work_item_json: {error}"))?;
    let object = parsed.as_object().ok_or_else(|| {
        "Issues during attempt to parse work_item_json: the work_item_json must be a JSON object."
            .to_owned()
    })?;
    let fields = object
        .get("fields")
        .ok_or_else(|| "The 'fields' property is missing from the work_item_json.".to_owned())?
        .as_object()
        .ok_or_else(|| {
            "Issues during attempt to parse work_item_json: 'fields' must be a JSON object."
                .to_owned()
        })?;
    Ok(fields
        .iter()
        .map(|(field, value)| json!({"op":"add","path":format!("/fields/{field}"),"value":value}))
        .collect())
}

/// `POST {project}/_apis/wit/workitems/${type}` with a JSON Patch document.
pub(crate) async fn create_work_item(
    client: &AdoClient,
    document: Vec<Value>,
    work_item_type: &str,
) -> Result<Value, AdoClientError> {
    let route = format!("${work_item_type}");
    let segments = ["wit", "workitems", route.as_str()];
    let request = AdoRequest::new(Method::POST, AdoScope::Project, &segments, WORK_ITEMS_API)
        .json_patch(Value::Array(document));
    client.json(request, true).await
}

/// `PATCH {project}/_apis/wit/workitems/{id}` with a JSON Patch document.
pub(crate) async fn update_work_item(
    client: &AdoClient,
    id: &str,
    document: Vec<Value>,
) -> Result<Value, AdoClientError> {
    let segments = ["wit", "workitems", id];
    let request = AdoRequest::new(Method::PATCH, AdoScope::Project, &segments, WORK_ITEMS_API)
        .json_patch(Value::Array(document));
    client.json(request, true).await
}

/// One field of a work item type, as `_get_work_item_type_fields` keeps it.
#[derive(Clone, Debug)]
pub(crate) struct FieldDefinition {
    pub(crate) reference_name: String,
    pub(crate) name: Value,
    pub(crate) required: bool,
    pub(crate) allowed_values: Vec<Value>,
}

/// `GET {project}/_apis/wit/workitemtypes/{type}`; its `fields` keyed and
/// ordered by reference name.
pub(crate) async fn work_item_type_fields(
    client: &AdoClient,
    work_item_type: &str,
) -> Result<Vec<FieldDefinition>, AdoClientError> {
    let segments = ["wit", "workitemtypes", work_item_type];
    let value = client
        .json(AdoRequest::get(&segments, WORK_ITEM_TYPES_API), false)
        .await?;
    let fields = value
        .get("fields")
        .and_then(Value::as_array)
        .ok_or_else(|| response_shape_failure(false))?;
    let mut definitions: Vec<FieldDefinition> = Vec::with_capacity(fields.len());
    for field in fields {
        let Some(reference_name) = field.get("referenceName").and_then(Value::as_str) else {
            return Err(invalid_response());
        };
        let definition = FieldDefinition {
            reference_name: reference_name.to_owned(),
            name: field.get("name").cloned().unwrap_or(Value::Null),
            required: field
                .get("alwaysRequired")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            allowed_values: field
                .get("allowedValues")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default(),
        };
        // The SDK builds a dict, so a repeated reference name keeps the last.
        if let Some(existing) = definitions
            .iter_mut()
            .find(|existing| existing.reference_name == definition.reference_name)
        {
            *existing = definition;
        } else {
            definitions.push(definition);
        }
    }
    definitions.sort_by(|left, right| left.reference_name.cmp(&right.reference_name));
    Ok(definitions)
}

/// `_format_work_item_type_fields_for_display`, byte for byte.
///
/// The v7.1 `WorkItemTypeFieldInstance` model has no `type` attribute, so
/// the SDK's `hasattr(field, 'type')` is always false and every field prints
/// `Type: Unknown`; that is kept.
pub(crate) fn format_work_item_type_fields(
    project: &str,
    work_item_type: &str,
    definitions: &[FieldDefinition],
) -> String {
    if definitions.is_empty() {
        return format!(
            "Unable to retrieve field definitions for work item type '{work_item_type}'. Please check your Azure DevOps connection and permissions."
        );
    }
    let rule = "=".repeat(80);
    let dash = "-".repeat(80);
    let mut output = vec![
        format!("Available Fields for Work Item Type '{work_item_type}' in Project '{project}':\n"),
        rule.clone(),
    ];
    let (required, optional): (Vec<_>, Vec<_>) = definitions
        .iter()
        .partition(|definition| definition.required);
    if !required.is_empty() {
        output.push("\n📋 REQUIRED FIELDS:".to_owned());
        output.push(dash.clone());
        for field in &required {
            output.push(format!(
                "\n✓ {} (Reference: {})",
                python_text(&field.name),
                field.reference_name
            ));
            output.push("  Type: Unknown".to_owned());
            if !field.allowed_values.is_empty() {
                output.push(format!(
                    "  Allowed Values: {}",
                    joined_values(&field.allowed_values)
                ));
            }
        }
    }
    if !optional.is_empty() {
        output.push("\n\n📝 OPTIONAL FIELDS (Common):".to_owned());
        output.push(dash);
        for field in optional
            .iter()
            .filter(|field| COMMON_OPTIONAL_FIELDS.contains(&field.reference_name.as_str()))
        {
            output.push(format!(
                "\n  {} (Reference: {})",
                python_text(&field.name),
                field.reference_name
            ));
            output.push("    Type: Unknown".to_owned());
            if !field.allowed_values.is_empty() {
                output.push(format!(
                    "    Allowed Values: {}",
                    joined_values(&field.allowed_values)
                ));
            }
        }
    }
    output.push(format!("\n\n{rule}"));
    output.push("\n💡 Usage Instructions:".to_owned());
    output.push(
        "  • Use the 'Reference' name (e.g., 'System.Title') as the field key in work_item_json"
            .to_owned(),
    );
    output.push("  • Provide all required fields when creating work items".to_owned());
    output.push("  • For fields with allowed values, use exact value from the list".to_owned());
    output.push(format!(
        "  • Example for {work_item_type}: {{\"fields\": {{\"System.Title\": \"My title\", \"CustomField\": \"Value\"}}}}"
    ));
    output.join("\n")
}

/// Python `str()` of a scalar the SDK interpolates.
pub(crate) fn python_text(value: &Value) -> String {
    match value {
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::String(text) => text.clone(),
        other => crate::toolkits::families::python_repr::repr(other),
    }
}

fn joined_values(values: &[Value]) -> String {
    values
        .iter()
        .map(python_text)
        .collect::<Vec<_>>()
        .join(", ")
}
