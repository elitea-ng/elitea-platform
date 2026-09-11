use std::collections::HashMap;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{Content, LlmResponseStream};

use super::*;

#[derive(Default)]
struct Model {
    tools: Mutex<BTreeSet<String>>,
}

#[async_trait]
impl Llm for Model {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        *self.tools.lock().unwrap() = request.tools.into_keys().collect();
        Ok(Box::pin(adk_rust::futures::stream::empty()))
    }
}

struct Operation {
    name: String,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Tool for Operation {
    fn name(&self) -> &str {
        &self.name
    }
    fn description(&self) -> &'static str {
        "Read fixture data."
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(json!({"read": true}))
    }
}

fn requirement(
    family: &str,
    alias: &str,
    configuration: &str,
) -> DelegatedAuthorizationRequirement {
    DelegatedAuthorizationRequirement::new(
        alias.to_owned(),
        family.to_owned(),
        "https://resource.example.invalid".to_owned(),
        None,
        None,
    )
    .unwrap()
    .with_resource_metadata(json!({"configuration_uuid": configuration,
            "authorization_servers": ["https://issuer.example.invalid"],
            "provided_settings": {"mcp_client_id": "public-client"}}))
    .unwrap()
}

async fn enumerate(toolsets: Vec<Arc<dyn Toolset>>) -> BTreeMap<String, Arc<dyn Tool>> {
    let mut tools = BTreeMap::new();
    for toolset in toolsets {
        for tool in toolset
            .tools(Arc::new(SimpleToolContext::new("fixture")))
            .await
            .unwrap()
        {
            assert!(tools.insert(tool.name().to_owned(), tool).is_none());
        }
    }
    tools
}

async fn request(model: &dyn Llm, tools: &BTreeMap<String, Arc<dyn Tool>>) {
    let _ = model
        .generate_content(
            LlmRequest {
                model: "fixture".to_owned(),
                contents: vec![Content::new("user").with_text("Read fixture data")],
                config: None,
                previous_response_id: None,
                tools: tools
                    .iter()
                    .map(|(name, tool)| (name.clone(), tool.declaration()))
                    .collect::<HashMap<_, _>>(),
            },
            false,
        )
        .await
        .unwrap();
}

#[tokio::test]
async fn undiscovered_toolkits_keep_exact_guards_through_merge_binding_and_skip() {
    let first = requirement("mcp", "first", "configuration-a");
    let other = requirement("mcp", "other", "configuration-b");
    let mut catalog = DelegatedAuthorizationCatalog::default();
    catalog.insert_discovery_requirement(first.clone()).unwrap();
    let mut nested = DelegatedAuthorizationCatalog::default();
    nested.insert_discovery_requirement(other.clone()).unwrap();
    catalog.merge(nested).unwrap();
    let binding = crate::toolkits::bind_toolsets(Vec::new(), &BTreeSet::new(), "fixture")
        .await
        .unwrap();
    let mut catalog = catalog.bind_provider_names(&binding).unwrap();
    assert!(
        catalog
            .requirement_for(&first.authorization_tool_name())
            .unwrap()
            .same_authority(&first)
    );
    assert!(catalog.requirement_for("unknown_operation").is_none());
    let fresh = catalog.clone();
    catalog.decline(&first);
    let scope = catalog.encode_declined_scope().unwrap().unwrap();
    let decoded = super::super::decode_declined_authorization_scope(&scope).unwrap();
    assert_eq!(decoded.len(), 1);
    assert!(decoded[0].same_authority(&first));

    let recorder = Arc::new(Model::default());
    let (model, tools) =
        bind_authorization_model_tools(recorder.clone(), Vec::new(), &mut catalog).unwrap();
    let tools = enumerate(tools).await;
    request(model.as_ref(), &tools).await;
    assert_eq!(
        *recorder.tools.lock().unwrap(),
        BTreeSet::from([
            first.authorization_tool_name(),
            other.authorization_tool_name(),
        ])
    );
    let declined = tools[&first.authorization_tool_name()]
        .execute(Arc::new(SimpleToolContext::new("fixture")), json!({}))
        .await
        .unwrap();
    assert_eq!(declined["status"], "declined");
    assert_eq!(declined["scope"], "current_run");
    assert!(declined.get("auth_context").is_none());
    assert!(
        tools[&other.authorization_tool_name()]
            .execute(Arc::new(SimpleToolContext::new("fixture")), json!({}),)
            .await
            .is_err()
    );
    assert_eq!(
        catalog.tool_names().collect::<Vec<_>>(),
        vec![other.authorization_tool_name()]
    );

    let (_, next_turn) =
        bind_authorization_model_tools(recorder, Vec::new(), &mut fresh.clone()).unwrap();
    let next_turn = enumerate(next_turn).await;
    assert!(
        next_turn[&first.authorization_tool_name()]
            .execute(Arc::new(SimpleToolContext::new("fixture")), json!({}),)
            .await
            .is_err(),
        "Skip must not authorize or decline a later invocation"
    );
}

#[tokio::test]
async fn discovery_guard_rejects_a_remote_operation_using_its_internal_identity() {
    let requirement = requirement("mcp", "first", "configuration-a");
    let mut catalog = DelegatedAuthorizationCatalog::default();
    catalog
        .insert_discovery_requirement(requirement.clone())
        .unwrap();
    let operation = Arc::new(Operation {
        name: requirement.authorization_tool_name(),
        calls: Arc::new(AtomicUsize::new(0)),
    }) as Arc<dyn Tool>;
    let binding = crate::toolkits::bind_toolsets(
        vec![Arc::new(BasicToolset::new("unrelated", vec![operation])) as Arc<dyn Toolset>],
        &BTreeSet::new(),
        "fixture",
    )
    .await
    .unwrap();
    assert!(catalog.bind_provider_names(&binding).is_err());
}

#[tokio::test]
async fn authorization_proxy_cardinality_and_skip_scope_are_family_independent() {
    for family in ["mcp", "openapi", "sharepoint"] {
        for count in 1..=16 {
            let first = requirement(family, "first", "configuration-a");
            let other = requirement(family, "other", "configuration-b");
            let calls = Arc::new(AtomicUsize::new(0));
            let mut catalog = DelegatedAuthorizationCatalog::default();
            let mut tools: Vec<Arc<dyn Tool>> = Vec::new();
            for index in 0..count {
                let name = format!("operation_{index}");
                catalog.insert(&name, first.clone()).unwrap();
                tools.push(Arc::new(Operation {
                    name,
                    calls: Arc::clone(&calls),
                }));
            }
            catalog.insert("other_operation", other.clone()).unwrap();
            tools.push(Arc::new(Operation {
                name: "other_operation".to_owned(),
                calls: Arc::clone(&calls),
            }));
            tools.push(Arc::new(Operation {
                name: "unrelated_public_read".to_owned(),
                calls: Arc::clone(&calls),
            }));
            let toolsets = vec![Arc::new(BasicToolset::new("fixture", tools)) as Arc<dyn Toolset>];
            let recorder = Arc::new(Model::default());
            let (model, initial) = bind_authorization_model_tools(
                recorder.clone(),
                toolsets.clone(),
                &mut catalog.clone(),
            )
            .unwrap();
            let initial = enumerate(initial).await;
            request(model.as_ref(), &initial).await;
            assert_eq!(
                *recorder.tools.lock().unwrap(),
                BTreeSet::from([
                    first.authorization_tool_name(),
                    other.authorization_tool_name(),
                    "unrelated_public_read".to_owned(),
                ])
            );
            assert!(
                initial[&first.authorization_tool_name()]
                    .execute(Arc::new(SimpleToolContext::new("fixture")), json!({}))
                    .await
                    .is_err()
            );
            assert_eq!(calls.load(Ordering::Acquire), 0);

            catalog.decline(&first);
            let scope = catalog.encode_declined_scope().unwrap().unwrap();
            assert_eq!(
                super::super::decode_declined_authorization_scope(&scope)
                    .unwrap()
                    .len(),
                1
            );
            let (model, skipped) =
                bind_authorization_model_tools(recorder.clone(), toolsets, &mut catalog).unwrap();
            let skipped = enumerate(skipped).await;
            // Exercise concurrent calls and a subsequent sequential retry.
            for _ in 0..2 {
                let results = adk_rust::futures::future::join_all((0..count).map(|index| {
                    skipped[&format!("operation_{index}")]
                        .execute(Arc::new(SimpleToolContext::new("fixture")), json!({}))
                }))
                .await;
                assert!(
                    results
                        .into_iter()
                        .all(|result| result.unwrap()["status"] == "declined")
                );
            }
            assert!(
                catalog
                    .tool_names()
                    .all(|name| !name.starts_with("operation_"))
            );
            assert!(
                catalog
                    .tool_names()
                    .any(|name| name == other.authorization_tool_name())
            );
            assert!(!catalog.is_declined("other_operation"));
            request(model.as_ref(), &skipped).await;
            assert!(!recorder.tools.lock().unwrap().contains("operation_0"));
            assert_eq!(calls.load(Ordering::Acquire), 0);
        }
    }
}

#[test]
fn a_different_configuration_on_the_same_resource_does_not_inherit_skip() {
    let first = requirement("openapi", "same alias", "configuration-a");
    let other = requirement("openapi", "same alias", "configuration-b");
    assert_ne!(
        first.authorization_tool_name(),
        other.authorization_tool_name()
    );
    let mut catalog = DelegatedAuthorizationCatalog::default();
    catalog.insert("read_other", other).unwrap();
    catalog.decline(&first);
    assert!(!catalog.is_declined("read_other"));
    assert!(catalog.encode_declined_scope().unwrap().is_none());
}

#[test]
fn skip_result_is_run_scoped_and_has_no_discovery_metadata() {
    for family in ["mcp", "openapi", "sharepoint"] {
        let requirement = requirement(family, "fixture toolkit", "configuration-a");
        let result = delegated_authorization_declined_result(&requirement, "authorize_fixture");
        assert_eq!(result["type"], "mcp_auth_decision");
        assert_eq!(result["status"], "declined");
        assert_eq!(result["scope"], "current_run");
        assert_eq!(result["toolkit_name"], "fixture toolkit");
        assert_eq!(result["next_step"], "use_other_tools_or_report");
        let encoded = result.to_string();
        for excluded in [
            "auth_context",
            "server_url",
            "resource_metadata",
            "provided_settings",
            "explicitly asks",
        ] {
            assert!(
                !encoded.contains(excluded),
                "unexpected field or instruction: {excluded}"
            );
        }
        assert!(
            result["message"]
                .as_str()
                .unwrap()
                .contains("later user turn")
        );
    }
}
