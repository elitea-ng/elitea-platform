//! Test-case field definitions and the SDK's user-format to
//! `PropertyResource` mapping (`__map_properties_to_api_format`).

use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::str_of;

/// The keys `create`/`update` handle themselves rather than as properties.
pub(super) const CORE_FIELDS: [&str; 6] = [
    "Name",
    "Description",
    "Precondition",
    "Steps",
    "Id",
    "QTest Id",
];
/// System fields the SDK never submits.
const EXCLUDED_FIELDS: [&str; 2] = ["Shared", "Projects Shared to"];
/// `UserListDataType` and the other multi-value types of `properties-info`.
const MULTI_SELECT_TYPES: [&str; 3] = [
    "UserListDataType",
    "MultiSelectionDataType",
    "CheckListDataType",
];

/// One project field: `{field_id, required, data_type, multiple, values}`.
#[derive(Clone, Debug, PartialEq)]
pub(super) struct FieldDefinition {
    pub(super) field_id: Value,
    pub(super) required: bool,
    /// `5` marks a user field.
    pub(super) data_type: Option<i64>,
    pub(super) multiple: bool,
    /// Allowed value label to value id, in the provider's order.
    pub(super) values: Vec<(String, Value)>,
}

impl FieldDefinition {
    fn value_id(&self, label: &str) -> Option<&Value> {
        self.values
            .iter()
            .find(|(name, _)| name == label)
            .map(|(_, id)| id)
    }

    fn insert_value(&mut self, label: String, id: Value) {
        if let Some(existing) = self.values.iter_mut().find(|(name, _)| *name == label) {
            existing.1 = id;
        } else {
            self.values.push((label, id));
        }
    }

    fn sorted_labels(&self) -> Vec<&str> {
        let mut labels = self
            .values
            .iter()
            .map(|(name, _)| name.as_str())
            .collect::<Vec<_>>();
        labels.sort_unstable();
        labels
    }
}

/// Field name to definition, in the provider's order.
pub(super) type FieldDefinitions = Vec<(String, FieldDefinition)>;

fn insert(definitions: &mut FieldDefinitions, name: String, definition: FieldDefinition) {
    if let Some(existing) = definitions.iter_mut().find(|(field, _)| *field == name) {
        existing.1 = definition;
    } else {
        definitions.push((name, definition));
    }
}

fn lookup<'a>(definitions: &'a FieldDefinitions, name: &str) -> Option<&'a FieldDefinition> {
    definitions
        .iter()
        .find(|(field, _)| field == name)
        .map(|(_, definition)| definition)
}

fn truthy(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(number)) => number.as_f64().is_some_and(|number| number != 0.0),
        Some(Value::String(text)) => !text.is_empty(),
        Some(Value::Array(items)) => !items.is_empty(),
        Some(Value::Object(object)) => !object.is_empty(),
    }
}

/// `GET /settings/test-cases/fields`: each field by label, with only the
/// allowed values marked active.
pub(super) fn from_field_resources(fields: &[Value]) -> FieldDefinitions {
    let mut definitions = FieldDefinitions::new();
    for field in fields {
        let Some(label) = field.get("label").map(str_of) else {
            continue;
        };
        let mut definition = FieldDefinition {
            field_id: field.get("id").cloned().unwrap_or(Value::Null),
            required: truthy(field.get("required")),
            data_type: field.get("data_type").and_then(Value::as_i64),
            multiple: truthy(field.get("multiple")),
            values: Vec::new(),
        };
        for allowed in field
            .get("allowed_values")
            .and_then(Value::as_array)
            .map_or(&[][..], Vec::as_slice)
        {
            // The generated model reads an absent `is_active` as `None`,
            // which the SDK's `not is_active` skips.
            if allowed.get("is_active").and_then(Value::as_bool) != Some(true) {
                continue;
            }
            let label = allowed
                .get("label")
                .map_or_else(|| "None".to_owned(), str_of);
            definition.insert_value(label, allowed.get("value").cloned().unwrap_or(Value::Null));
        }
        insert(&mut definitions, label, definition);
    }
    definitions
}

/// The fallback without Field Management permission: `/properties` (names,
/// ids, required) merged with `/properties-info` (types, allowed values).
pub(super) fn from_properties(properties: &[Value], info: &Value) -> FieldDefinitions {
    let metadata = info
        .get("metadata")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice);
    let mut definitions = FieldDefinitions::new();
    for property in properties {
        let Some(name) = property
            .get("name")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty() && !EXCLUDED_FIELDS.contains(name))
        else {
            continue;
        };
        let field_id = property.get("id").cloned().unwrap_or(Value::Null);
        let item = metadata
            .iter()
            .find(|item| item.get("id") == Some(&field_id));
        let data_type = item
            .and_then(|item| item.get("data_type"))
            .and_then(Value::as_str);
        let mut definition = FieldDefinition {
            field_id,
            required: truthy(property.get("required")),
            data_type: (data_type == Some("UserListDataType")).then_some(5),
            multiple: data_type.is_some_and(|kind| MULTI_SELECT_TYPES.contains(&kind)),
            values: Vec::new(),
        };
        for allowed in item
            .and_then(|item| item.get("allowed_values"))
            .and_then(Value::as_array)
            .map_or(&[][..], Vec::as_slice)
        {
            let text = allowed.get("value_text");
            let id = allowed.get("id");
            if truthy(text) && truthy(id) {
                definition.insert_value(
                    text.map(str_of).unwrap_or_default(),
                    id.cloned().unwrap_or(Value::Null),
                );
            }
        }
        insert(&mut definitions, name.to_owned(), definition);
    }
    definitions
}

/// `__format_field_info_for_display`.
pub(super) fn describe(definitions: &FieldDefinitions, project_id: u64) -> String {
    let mut output = vec![format!(
        "Available Test Case Fields for Project {project_id}:\n"
    )];
    let mut sorted = definitions.iter().collect::<Vec<_>>();
    sorted.sort_by(|(left, _), (right, _)| left.cmp(right));
    for (name, definition) in sorted {
        let required = if definition.required {
            " (Required)"
        } else {
            ""
        };
        let has_values = !definition.values.is_empty();
        let kind = if !has_values {
            "Text"
        } else if definition.multiple {
            "Multi-select"
        } else {
            "Single-select"
        };
        output.push(format!("\n{name} ({kind}{required}):"));
        if has_values {
            for label in definition.sorted_labels() {
                output.push(format!("  - {label}"));
            }
        } else {
            output.push("  Free text input. Set to null to clear.".to_owned());
        }
    }
    output.push("\n\n--- Field Type Guide ---".to_owned());
    output.push("\nText fields: Use null to clear, provide string value to set.".to_owned());
    output.push(
        "\nSingle-select: Provide exact value name from the list above. Cannot be cleared via API."
            .to_owned(),
    );
    output.push(
        "\nMulti-select: Provide value as array [\"val1\", \"val2\"]. Use null to clear."
            .to_owned(),
    );
    output.join("\n")
}

struct Property {
    id: Value,
    name: String,
    value: Value,
    value_name: Option<Value>,
}

/// `__map_properties_to_api_format` without base properties: the request's
/// `properties` list, or the SDK's complete validation report.
pub(super) fn map_properties(
    test_case: &[(String, Value)],
    definitions: &FieldDefinitions,
) -> Result<Vec<Value>, String> {
    let mut properties: Vec<Property> = Vec::new();
    let mut errors = Vec::new();
    for (name, value) in test_case {
        if CORE_FIELDS.contains(&name.as_str()) || value.as_str() == Some("") {
            continue;
        }
        let Some(definition) = lookup(definitions, name) else {
            errors.push(format!(
                "❌ Unknown field '{name}' - not defined in project configuration"
            ));
            continue;
        };
        let property = if value.is_null() {
            match clear(name, definition) {
                Ok(property) => property,
                Err(error) => {
                    errors.push(error);
                    continue;
                }
            }
        } else if definition.values.is_empty() {
            Property {
                id: definition.field_id.clone(),
                name: name.clone(),
                value: value.clone(),
                value_name: value.is_string().then(|| value.clone()),
            }
        } else {
            match select(name, value, definition, &mut errors) {
                Some(property) => property,
                None => continue,
            }
        };
        if let Some(existing) = properties.iter_mut().find(|item| item.name == *name) {
            *existing = property;
        } else {
            properties.push(property);
        }
    }
    if !errors.is_empty() {
        let mut available = definitions
            .iter()
            .map(|(name, _)| name.as_str())
            .collect::<Vec<_>>();
        available.sort_unstable();
        return Err(format!(
            "Found {} validation error(s) in test case properties:\n\n{}\n\n📋 Available fields for this project: {}\n\n💡 Tip: Use 'get_all_test_cases_fields_for_project' tool to see all fields with their allowed values.",
            errors.len(),
            errors.join("\n"),
            available.join(", ")
        ));
    }
    Ok(properties
        .into_iter()
        .filter(|property| !EXCLUDED_FIELDS.contains(&property.name.as_str()))
        .map(|property| {
            let mut resource = Map::new();
            resource.insert("field_id".to_owned(), property.id);
            resource.insert("field_name".to_owned(), Value::String(property.name));
            resource.insert("field_value".to_owned(), property.value);
            // The generated client drops `None` attributes.
            if let Some(name) = property.value_name.filter(|name| !name.is_null()) {
                resource.insert("field_value_name".to_owned(), name);
            }
            Value::Object(resource)
        })
        .collect())
}

/// A `null` value clears a text or multi-select field; the API cannot clear
/// a single-select one.
fn clear(name: &str, definition: &FieldDefinition) -> Result<Property, String> {
    if definition.values.is_empty() {
        return Ok(Property {
            id: definition.field_id.clone(),
            name: name.to_owned(),
            value: json!(""),
            value_name: Some(json!("")),
        });
    }
    if definition.multiple {
        return Ok(Property {
            id: definition.field_id.clone(),
            name: name.to_owned(),
            value: json!("[]"),
            value_name: None,
        });
    }
    let available = definition
        .values
        .iter()
        .map(|(label, _)| label.as_str())
        .collect::<Vec<_>>();
    let available = if available.is_empty() {
        "none".to_owned()
    } else {
        available.join(", ")
    };
    Err(format!(
        "⚠️ Cannot clear single-select field '{name}' - this is a QTest API limitation (clearing is possible from UI but not exposed via API). Please select an alternative value instead. Available values: {available}"
    ))
}

/// A dropdown, multi-select or user value, validated against the allowed
/// labels.
fn select(
    name: &str,
    value: &Value,
    definition: &FieldDefinition,
    errors: &mut Vec<String>,
) -> Option<Property> {
    let candidates = match value {
        Value::Array(items) if definition.multiple => items.clone(),
        other => vec![other.clone()],
    };
    let mut ids = Vec::new();
    let mut names = Vec::new();
    for candidate in &candidates {
        let id = candidate
            .as_str()
            .and_then(|label| definition.value_id(label));
        let Some(id) = id else {
            errors.push(format!(
                "❌ Invalid value '{}' for field '{name}'. Allowed values: {}",
                str_of(candidate),
                definition.sorted_labels().join(", ")
            ));
            continue;
        };
        ids.push(id.clone());
        names.push(str_of(candidate));
    }
    if ids.is_empty() {
        return None;
    }
    let (field_value, value_name) = if definition.multiple && ids.len() == 1 {
        let label = if definition.data_type == Some(5) {
            format!("[{}]", names[0])
        } else {
            names[0].clone()
        };
        (json!(format!("[{}]", str_of(&ids[0]))), json!(label))
    } else if definition.multiple {
        let joined = ids.iter().map(str_of).collect::<Vec<_>>().join(",");
        (json!(format!("[{joined}]")), json!(names.join(", ")))
    } else {
        (ids[0].clone(), json!(names[0]))
    };
    Some(Property {
        id: definition.field_id.clone(),
        name: name.to_owned(),
        value: field_value,
        value_name: Some(value_name),
    })
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{describe, from_field_resources, map_properties};

    #[test]
    fn maps_values_and_reports_every_error() {
        let definitions = from_field_resources(&[
            json!({"id":1,"label":"Priority","allowed_values":[
                {"label":"High","value":11,"is_active":true},
                {"label":"Old","value":12,"is_active":false}
            ]}),
            json!({"id":2,"label":"Team","multiple":true,"allowed_values":[
                {"label":"A","value":21,"is_active":true},{"label":"B","value":22,"is_active":true}
            ]}),
            json!({"id":3,"label":"Notes","required":true}),
        ]);
        let mapped = map_properties(
            &[
                ("Name".to_owned(), json!("ignored")),
                ("Priority".to_owned(), json!("High")),
                ("Team".to_owned(), json!(["A", "B"])),
                ("Notes".to_owned(), json!(null)),
            ],
            &definitions,
        )
        .expect("valid properties");
        assert_eq!(
            mapped,
            [
                json!({"field_id":1,"field_name":"Priority","field_value":11,"field_value_name":"High"}),
                json!({"field_id":2,"field_name":"Team","field_value":"[21,22]","field_value_name":"A, B"}),
                json!({"field_id":3,"field_name":"Notes","field_value":"","field_value_name":""}),
            ]
        );
        let report = map_properties(
            &[
                ("Priority".to_owned(), json!("Old")),
                ("Colour".to_owned(), json!("red")),
                ("Priority".to_owned(), json!(null)),
            ],
            &definitions,
        )
        .expect_err("invalid properties");
        assert!(report.starts_with("Found 3 validation error(s)"));
        assert!(
            report.contains("❌ Invalid value 'Old' for field 'Priority'. Allowed values: High")
        );
        assert!(report.contains("❌ Unknown field 'Colour'"));
        assert!(report.contains("Cannot clear single-select field 'Priority'"));
        assert!(report.contains("Available fields for this project: Notes, Priority, Team"));
        let text = describe(&definitions, 7);
        assert!(text.starts_with(
            "Available Test Case Fields for Project 7:\n\n\nNotes (Text (Required)):"
        ));
        assert!(text.contains("\nTeam (Multi-select):\n  - A\n  - B"));
    }
}
