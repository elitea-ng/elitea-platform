//! The reads: DQL searches, entity lookups, relationships, modules, fields
//! and versions.

use adk_core::AdkError;
use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::{PyValue, str_of};

use super::client::{QtestCall, QtestClientErrorCode, invalid_response};
use super::fields::{self, FieldDefinitions};
use super::schema::QtestToolKind;
use super::search::{OBJECT_TYPES, QTEST_ID, SEARCH_ONLY_TYPES, SearchOptions, quoted};
use super::tools::{
    Failure, Qtest, invalid_arguments, optional_bool, optional_integer, optional_text,
    required_text,
};

impl Qtest {
    pub(super) async fn read(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        match kind {
            QtestToolKind::SearchByDql => {
                let dql = required_text(arguments, "dql")?;
                // `extract_images`/`prompt` are accepted; images are removed.
                optional_bool(arguments, "extract_images")?;
                optional_text(arguments, "prompt")?;
                let options = SearchOptions {
                    max_results: optional_integer(arguments, "max_results")?
                        .unwrap_or(super::search::DEFAULT_MAX_RESULTS),
                    append_test_steps: optional_bool(arguments, "append_test_steps")?,
                    include_external_properties: optional_bool(
                        arguments,
                        "include_external_properties",
                    )?,
                };
                Ok(Value::String(self.found_test_cases(dql, &options).await?))
            }
            QtestToolKind::FindTestCaseById => {
                let test_id = required_text(arguments, "test_id")?;
                optional_bool(arguments, "extract_images")?;
                optional_text(arguments, "prompt")?;
                let dql = format!("Id = {}", quoted(test_id)?);
                Ok(Value::String(
                    self.found_test_cases(&dql, &SearchOptions::default())
                        .await?,
                ))
            }
            QtestToolKind::GetModules => self.get_modules(arguments).await,
            QtestToolKind::GetAllTestCasesFieldsForProject => {
                if optional_bool(arguments, "force_refresh")? {
                    *self.fields.lock().await = None;
                }
                let definitions = self.field_definitions().await?;
                Ok(Value::String(fields::describe(
                    &definitions,
                    self.project_id,
                )))
            }
            QtestToolKind::SearchEntitiesByDql => self.search_entities(arguments).await,
            _ => {
                let entity_id = required_text(arguments, "entity_id")?;
                let prefix = entity_id
                    .split_once('-')
                    .map(|(prefix, _)| prefix.to_uppercase())
                    .unwrap_or_default();
                let Some((object_type, _, name)) =
                    OBJECT_TYPES.iter().find(|(_, known, _)| *known == prefix)
                else {
                    let mut prefixes = OBJECT_TYPES
                        .iter()
                        .map(|(_, prefix, _)| *prefix)
                        .collect::<Vec<_>>();
                    prefixes.sort_unstable();
                    return Err(Failure::Message(format!(
                        "Invalid entity ID format '{entity_id}'. Expected prefix to be one of: {}",
                        prefixes.join(", ")
                    )));
                };
                match self.entity(object_type, entity_id).await? {
                    Some(entity) => Ok(entity.to_json()),
                    None => Err(Failure::Message(format!(
                        "{name} '{entity_id}' not found in project {}",
                        self.project_id
                    ))),
                }
            }
        }
    }

    /// `search_by_dql`'s text: the count, then the first
    /// `no_of_tests_shown_in_dql_search` rows as a Python list.
    async fn found_test_cases(
        &self,
        dql: &str,
        options: &SearchOptions,
    ) -> Result<String, Failure> {
        let rows = self.search_test_cases(dql, options).await?;
        let shown = PyValue::List(rows.iter().take(self.shown_results).cloned().collect());
        Ok(format!(
            "Found {} Qtest test cases:\n{}",
            rows.len(),
            shown.repr()
        ))
    }

    async fn search_entities(&self, arguments: &Map<String, Value>) -> Result<Value, Failure> {
        let object_type = required_text(arguments, "object_type")?;
        let dql = required_text(arguments, "dql")?;
        let name = OBJECT_TYPES
            .iter()
            .find(|(kind, _, _)| *kind == object_type)
            .map(|(_, _, name)| *name)
            .or_else(|| {
                SEARCH_ONLY_TYPES
                    .iter()
                    .find(|(kind, _)| *kind == object_type)
                    .map(|(_, name)| *name)
            });
        let Some(name) = name else {
            let all = OBJECT_TYPES
                .iter()
                .map(|(kind, _, _)| *kind)
                .chain(SEARCH_ONLY_TYPES.iter().map(|(kind, _)| *kind))
                .collect::<Vec<_>>();
            return Err(Failure::Message(format!(
                "Invalid object_type '{object_type}'. Must be one of: {}",
                all.join(", ")
            )));
        };
        let response = self.search(object_type, dql, json!(["*"]), None).await?;
        let items = response
            .get("items")
            .and_then(Value::as_array)
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .map(|item| super::search::parse_entity(object_type, item).to_json())
            .collect::<Vec<_>>();
        Ok(json!({
            "object_type": object_type,
            "entity_name": name,
            "total": response.get("total").cloned().unwrap_or(json!(0)),
            "returned": items.len(),
            "items": items.into_iter().take(self.shown_results).collect::<Vec<_>>(),
        }))
    }

    async fn get_modules(&self, arguments: &Map<String, Value>) -> Result<Value, Failure> {
        let mut call = QtestCall::get("modules");
        if let Some(parent) = optional_integer(arguments, "parent_id")?.filter(|id| *id != 0) {
            call = call.query("parentId", parent.to_string());
        }
        if let Some(search) = optional_text(arguments, "search")?.filter(|text| !text.is_empty()) {
            call = call.query("search", search);
        }
        let modules = self.call(call).await?;
        let mut formatted = Vec::new();
        for module in modules.as_array().map_or(&[][..], Vec::as_slice) {
            format_module(module, 0, &mut formatted);
        }
        if formatted.is_empty() {
            return Ok(Value::String(
                "No modules found in the specified location.".to_owned(),
            ));
        }
        Ok(Value::String(format!(
            "Found {} module(s):\n{}",
            formatted.len(),
            PyValue::List(formatted).repr()
        )))
    }

    /// `__get_field_definitions_cached`: `GET /settings/test-cases/fields`,
    /// or on 403 the properties fallback, kept for the invocation.
    pub(super) async fn field_definitions(&self) -> Result<FieldDefinitions, Failure> {
        let mut cached = self.fields.lock().await;
        if let Some(definitions) = cached.as_ref() {
            return Ok(definitions.clone());
        }
        let definitions = match self
            .client
            .call(QtestCall::get("settings/test-cases/fields"))
            .await
        {
            Ok(fields) => {
                fields::from_field_resources(fields.as_array().map_or(&[][..], Vec::as_slice))
            }
            Err(error) if error.code() == QtestClientErrorCode::Authorization => {
                self.fields_from_properties().await?
            }
            Err(error) => return Err(Failure::Adk(error.into_adk())),
        };
        *cached = Some(definitions.clone());
        Ok(definitions)
    }

    /// `__get_field_definitions_from_properties_api`.
    async fn fields_from_properties(&self) -> Result<FieldDefinitions, Failure> {
        let options = SearchOptions::default();
        let paging = SearchOptions {
            max_results: 1,
            ..options
        };
        let response = self
            .search("test-cases", "", json!(["*"]), Some((1, 1, &paging)))
            .await
            .map_err(|error| {
                if error.category == adk_core::ErrorCategory::Forbidden {
                    Failure::Message(format!(
                        "Permission denied (HTTP 403) accessing test-cases in project {}. Ensure the API token has READ permission on test-cases.",
                        self.project_id
                    ))
                } else {
                    Failure::Message(format!(
                        "Cannot find any test case to query field definitions. Please create at least one test case in project {}",
                        self.project_id
                    ))
                }
            })?;
        let Some(test_case) = response
            .get("items")
            .and_then(|items| items.get(0))
            .and_then(|item| item.get("id"))
        else {
            return Err(Failure::Message(format!(
                "No test cases found in project {}. Please create at least one test case to retrieve field definitions.",
                self.project_id
            )));
        };
        let test_case = str_of(test_case);
        let properties_path = format!("test-cases/{test_case}/properties");
        let info_path = format!("test-cases/{test_case}/properties-info");
        let properties = self
            .call(QtestCall::get(&properties_path).query("calledBy", "testcase_properties"))
            .await?;
        let info = self.call(QtestCall::get(&info_path)).await?;
        Ok(fields::from_properties(
            properties.as_array().map_or(&[][..], Vec::as_slice),
            &info,
        ))
    }

    /// `_parse_modules`: every module under the project root with its full
    /// name (`"{pid} {name}"`), kept for the invocation.
    pub(super) async fn modules(&self) -> Result<Vec<(Value, String)>, AdkError> {
        let mut cached = self.modules.lock().await;
        if let Some(modules) = cached.as_ref() {
            return Ok(modules.clone());
        }
        let response = self
            .call(QtestCall::get("modules").query("expand", "descendants"))
            .await?;
        let mut modules = Vec::new();
        for module in response
            .as_array()
            .ok_or_else(|| invalid_response().into_adk())?
        {
            collect_modules(module, &mut modules);
        }
        *cached = Some(modules.clone());
        Ok(modules)
    }
}

fn collect_modules(module: &Value, modules: &mut Vec<(Value, String)>) {
    let pid = module.get("pid").map_or_else(|| "None".to_owned(), str_of);
    let name = module.get("name").map_or_else(|| "None".to_owned(), str_of);
    modules.push((
        module.get("id").cloned().unwrap_or(Value::Null),
        format!("{pid} {name}"),
    ));
    for child in module
        .get("children")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice)
    {
        collect_modules(child, modules);
    }
}

fn format_module(module: &Value, level: u64, formatted: &mut Vec<PyValue>) {
    let field = |key: &str| PyValue::from_json(module.get(key).unwrap_or(&Value::Null));
    let pid = module
        .get("pid")
        .and_then(Value::as_str)
        .filter(|pid| !pid.is_empty());
    let name = module
        .get("name")
        .and_then(Value::as_str)
        .filter(|name| !name.is_empty());
    let full_name = match (pid, name) {
        (Some(pid), Some(name)) => format!("{pid} {name}"),
        (_, Some(name)) => name.to_owned(),
        _ => String::new(),
    };
    let children = module
        .get("children")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice);
    formatted.push(PyValue::Dict(vec![
        ("id".to_owned(), field("id")),
        ("name".to_owned(), field("name")),
        ("pid".to_owned(), field("pid")),
        ("full_name".to_owned(), PyValue::text(full_name)),
        ("level".to_owned(), PyValue::Json(json!(level))),
        (
            "has_children".to_owned(),
            PyValue::Json(json!(!children.is_empty())),
        ),
    ]));
    for child in children {
        format_module(child, level + 1, formatted);
    }
}

/// The relationship reads and version listing.
impl Qtest {
    pub(super) async fn relation(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        match kind {
            QtestToolKind::FindTestCasesByRequirementId => {
                let requirement = required_text(arguments, "requirement_id")?;
                let details = optional_bool(arguments, "include_details")?;
                self.test_cases_of_requirement(requirement, details).await
            }
            QtestToolKind::FindRequirementsByTestCaseId
            | QtestToolKind::FindTestRunsByTestCaseId => {
                let test_case = required_text(arguments, "test_case_id")?;
                let internal = self.test_case_qtest_id(test_case).await?;
                let (prefix, object_type, key, empty) =
                    if kind == QtestToolKind::FindRequirementsByTestCaseId {
                        (
                            "RQ-",
                            "requirements",
                            "requirements",
                            "No requirements are linked to test case",
                        )
                    } else {
                        (
                            "TR-",
                            "test-runs",
                            "test_runs",
                            "No test runs are associated with test case",
                        )
                    };
                let linked = self.linked("test-cases", &internal, prefix).await?;
                if linked.is_empty() {
                    return Ok(json!({
                        "test_case_id": test_case,
                        "total": 0,
                        key: [],
                        "message": format!("{empty} '{test_case}'"),
                    }));
                }
                let entities = self.entities(object_type, &linked).await?;
                let mut result = json!({
                    "test_case_id": test_case,
                    "total": entities.len(),
                    key: entities,
                });
                if kind == QtestToolKind::FindTestRunsByTestCaseId {
                    result["hint"] = json!(
                        "To find defects, use find_defects_by_test_run_id for each test run."
                    );
                }
                Ok(result)
            }
            QtestToolKind::FindDefectsByTestRunId => {
                let test_run = required_text(arguments, "test_run_id")?;
                self.defects_of_test_run(test_run).await
            }
            _ => self.test_case_versions(arguments).await,
        }
    }

    /// Each linked pid's parsed entity, or the SDK's placeholder.
    async fn entities(
        &self,
        object_type: &str,
        linked: &[(String, Value)],
    ) -> Result<Vec<Value>, Failure> {
        let mut entities = Vec::with_capacity(linked.len());
        for (pid, _) in linked {
            entities.push(match self.entity(object_type, pid).await? {
                Some(entity) => entity.to_json(),
                None => {
                    json!({"Id": pid, QTEST_ID: null, "Name": "Unable to fetch", "Description": ""})
                }
            });
        }
        Ok(entities)
    }

    async fn test_cases_of_requirement(
        &self,
        requirement: &str,
        details: bool,
    ) -> Result<Value, Failure> {
        let internal = self.internal_id("requirements", requirement).await?;
        let linked = self.linked("requirements", &internal, "TC-").await?;
        if linked.is_empty() {
            return Ok(json!({
                "requirement_id": requirement,
                "total": 0,
                "test_cases": [],
                "message": format!("No test cases are linked to requirement '{requirement}'"),
            }));
        }
        let mut test_cases = Vec::with_capacity(linked.len());
        for (pid, id) in &linked {
            let dql = format!("Id = {}", quoted(pid)?);
            let rows = match self
                .search_test_cases(&dql, &SearchOptions::default())
                .await
            {
                Ok(rows) => rows,
                Err(_) if details => {
                    test_cases
                        .push(json!({"Id": pid, QTEST_ID: id, "error": "Unable to fetch details"}));
                    continue;
                }
                Err(_) => {
                    test_cases.push(json!({"Id": pid, QTEST_ID: id, "Name": "Unable to fetch", "Description": ""}));
                    continue;
                }
            };
            let Some(row) = rows.first() else {
                continue;
            };
            test_cases.push(if details {
                row.to_json()
            } else {
                json!({
                    "Id": pid,
                    QTEST_ID: id,
                    "Name": row.get("Name").map_or(Value::Null, PyValue::to_json),
                    "Description": row.get("Description").map_or(json!(""), PyValue::to_json),
                })
            });
        }
        Ok(json!({
            "requirement_id": requirement,
            "total": test_cases.len(),
            "test_cases": test_cases,
        }))
    }

    async fn defects_of_test_run(&self, test_run: &str) -> Result<Value, Failure> {
        let Some(run) = self.entity("test-runs", test_run).await? else {
            return Err(Failure::Message(format!("Test run '{test_run}' not found")));
        };
        let mut source = None;
        if let Some(test_case) = run
            .get("Test Case Id")
            .map(PyValue::to_json)
            .filter(|id| !id.is_null())
        {
            source = self.pid_of("test-cases", &test_case).await;
        }
        let Some(internal) = run
            .get(QTEST_ID)
            .map(PyValue::to_json)
            .filter(|id| !id.is_null())
        else {
            return Err(Failure::Message(format!(
                "QTest Id not found in test run data for '{test_run}'"
            )));
        };
        let linked = self.linked("test-runs", &internal, "DF-").await?;
        let mut result = if linked.is_empty() {
            json!({
                "test_run_id": test_run,
                "total": 0,
                "defects": [],
                "message": format!("No defects are associated with test run '{test_run}'"),
            })
        } else {
            let defects = self.entities("defects", &linked).await?;
            json!({"test_run_id": test_run, "total": defects.len(), "defects": defects})
        };
        if let Some(source) = source.filter(|source| !source.is_null()) {
            result["source_test_case_id"] = source;
        }
        Ok(result)
    }

    /// `__fetch_test_case_versions`.
    pub(super) async fn versions(&self, test_case: &Value) -> Result<Vec<Value>, AdkError> {
        let path = format!("test-cases/{}/versions", str_of(test_case));
        let response = self.call(QtestCall::get(&path)).await?;
        Ok(response
            .as_array()
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .filter(|item| item.is_object())
            .map(|item| {
                json!({
                    "version_id": item.get("test_case_version_id").cloned().unwrap_or(Value::Null),
                    "version": item.get("version").cloned().unwrap_or(Value::Null),
                    "name": item.get("name").cloned().unwrap_or(Value::Null),
                })
            })
            .collect())
    }

    async fn test_case_versions(&self, arguments: &Map<String, Value>) -> Result<Value, Failure> {
        let test_case = required_text(arguments, "test_case_id")?;
        let version_name = optional_text(arguments, "version_name")?;
        let internal =
            if !test_case.is_empty() && test_case.bytes().all(|byte| byte.is_ascii_digit()) {
                json!(
                    test_case
                        .parse::<u64>()
                        .map_err(|_| Failure::Adk(invalid_arguments()))?
                )
            } else {
                self.internal_id("test-cases", test_case).await?
            };
        let mut versions = self.versions(&internal).await?;
        if let Some(wanted) = version_name {
            let wanted = wanted.trim();
            let listed = versions
                .iter()
                .map(|version| {
                    version
                        .get("version")
                        .map_or_else(|| "None".to_owned(), str_of)
                })
                .collect::<Vec<_>>();
            versions.retain(|version| {
                version
                    .get("version")
                    .filter(|value| !value.is_null())
                    .map(str_of)
                    .unwrap_or_default()
                    .trim()
                    == wanted
            });
            if versions.is_empty() {
                let available = if listed.is_empty() {
                    "none".to_owned()
                } else {
                    listed.join(", ")
                };
                return Err(Failure::Message(format!(
                    "Version '{}' is not among the versions qTest lists for test case {test_case} in project {}. Listed versions: {available}. qTest omits intermediate minor versions, so '{}' may still exist and its ID remain usable if you already know it.",
                    version_name.unwrap_or_default(),
                    self.project_id,
                    version_name.unwrap_or_default()
                )));
            }
        }
        Ok(json!({
            "test_case_id": test_case,
            "qtest_test_case_id": internal,
            "total": versions.len(),
            "versions": versions,
        }))
    }
}
