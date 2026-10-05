use super::super::code_runtime::{CodeInvocation, CodeSandboxRuntime};
use super::super::{code::CodeNodeDefinition, code_runtime::CodeNode};
use adk_rust::graph::{ExecutionConfig, GraphError, Node, NodeContext, State};
use async_trait::async_trait;
use serde_json::json;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};

fn source(workspace: &str) -> String {
    format!(
        "id: run\ntype: code\nlanguage: python\ncode: '7'\ninput: [count]\noutput: [count]\n{workspace}"
    )
}
fn selection() -> String {
    format!(
        "workspace:\n  toolkit_id: 12\n  toolkit_reference_sha256: '{}'\n  repository_id: '123'\n  commit: '{}'\n  mode: read\n  include: [src]\n",
        "1".repeat(64),
        "2".repeat(40)
    )
}
fn context() -> NodeContext {
    NodeContext::new(
        State::from([("count".into(), json!(2))]),
        ExecutionConfig::new("original-thread"),
        3,
    )
}

#[test]
fn original_declaration_exists_independently_of_debug_and_preserves_configuration() {
    let mut definition = CodeNodeDefinition::from_yaml(&source("")).unwrap();
    let before = serde_json::to_vec(&definition).unwrap();
    let digest = definition.config_digest().unwrap();
    assert!(
        definition
            .original_declaration(&context())
            .unwrap()
            .is_none()
    );
    definition.bind_debug_definition([3; 32], [4; 32]);
    let pin = definition
        .original_declaration(&context())
        .unwrap()
        .unwrap();
    assert_eq!(pin.node_id, "run");
    assert_eq!(pin.graph_thread_id, "original-thread");
    assert_eq!(pin.graph_step, 3);
    assert_eq!(pin.configuration_json.as_bytes(), before);
    assert_eq!(serde_json::to_vec(&definition).unwrap(), before);
    assert_eq!(definition.config_digest().unwrap(), digest);
    assert!(definition.debug_selection(&context()).unwrap().is_none());
    assert!(
        definition
            .workspace_selection(&context())
            .unwrap()
            .is_none()
    );
    let wire = serde_json::to_value(pin.binding()).unwrap();
    assert_eq!(wire["definition_sha256"], "03".repeat(32));
    assert_eq!(wire["yaml_sha256"], "04".repeat(32));
}

#[test]
fn workspace_is_exact_saved_policy_and_requires_the_owning_compiler_pin() {
    let mut definition = CodeNodeDefinition::from_yaml(&source(&selection())).unwrap();
    assert!(definition.workspace_selection(&context()).is_err());
    definition.bind_debug_definition([3; 32], [4; 32]);
    let call = definition.workspace_selection(&context()).unwrap().unwrap();
    assert_eq!(call.selection.toolkit_id, 12);
    assert_eq!(call.selection.commit, "2".repeat(40));
    assert_eq!(call.selection.include, ["src"]);
    assert!(
        call.declaration
            .configuration_json
            .contains("\"workspace\"")
    );
    assert!(definition.debug_selection(&context()).unwrap().is_none());
    for changed in [
        "workspace: null\n".to_owned(),
        selection().replace("include: [src]", "include: ['../outside']"),
        selection().replace("mode: read", "mode: write"),
        selection().replace(
            "include: [src]",
            "include: [src]\n  url: 'https://example.invalid'",
        ),
    ] {
        assert!(CodeNodeDefinition::from_yaml(&source(&changed)).is_err());
    }
}

struct Capture(Mutex<Vec<(String, String)>>);
#[async_trait]
impl CodeSandboxRuntime for Capture {
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        let call = invocation
            .workspace
            .ok_or_else(|| GraphError::Other("missing workspace".into()))?;
        self.0
            .lock()
            .unwrap()
            .push((call.declaration.graph_thread_id, call.selection.commit));
        Ok(br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":7}","stderr":""}"#.to_vec())
    }
}
#[tokio::test]
async fn production_node_passes_frozen_workspace_and_refuses_missing_pin_before_runtime() {
    let runtime = Arc::new(Capture(Mutex::new(Vec::new())));
    let types = BTreeMap::from([("count".into(), "int".into())]);
    let definition = CodeNodeDefinition::from_yaml(&source(&selection())).unwrap();
    let node = CodeNode::new(definition.clone(), types.clone(), runtime.clone()).unwrap();
    assert!(node.execute(&context()).await.is_err());
    assert!(runtime.0.lock().unwrap().is_empty());
    let mut pinned = definition;
    pinned.bind_debug_definition([3; 32], [4; 32]);
    let node = CodeNode::new(pinned, types, runtime.clone()).unwrap();
    node.execute(&context()).await.unwrap();
    assert_eq!(
        runtime.0.lock().unwrap().as_slice(),
        [("original-thread".into(), "2".repeat(40))]
    );
}
