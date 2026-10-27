//! A sixth, synthetic provider registered from THIS module, with no edit to
//! the governed core (`toolkits/governed/mod.rs`, `governed/client.rs`):
//! the seam is open to a new adapter (#1120 acceptance).

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{Tool, Toolset};
use adk_tool::{BasicToolset, FunctionTool};
use serde_json::{Map, Value, json};

use super::governed::adapters::reference_registry_builder;
use super::governed::client::GovernedToolkitSpec;
use super::governed::{
    AuthAdapter, CredentialClass, CredentialEnvelope, CredentialRedeemer, EnvelopeKind,
    GovernedBuild, GovernedError, GovernedOperationClient, TargetKind, ToolOperation,
    canonical_target,
};
use super::governed_tests::{RecordingRedeemer, Respond, answering, context, policy, tool};

const TARGET: &str = "https://widgets.example.test";

/// Counts the provider clients built, and names each by a serial number the
/// tool returns, so a test can tell two clients apart.
#[derive(Default)]
struct SyntheticAdapter {
    built: AtomicUsize,
    instances: Mutex<Vec<usize>>,
}

const SYNTHETIC_TOOLS: &[ToolOperation] = &[ToolOperation {
    tool: "read_widget",
    operation_id: "synthetic.widget.read",
    resource_kind: "widget",
    id_argument: Some("widget_id"),
    unbound_with: &[],
}];

impl AuthAdapter for SyntheticAdapter {
    fn provider_type(&self) -> &'static str {
        "synthetic"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::BaseUrl
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        match class {
            CredentialClass::StaticSecret => &["token"],
            CredentialClass::OauthDelegated => &["access_token"],
        }
    }

    fn probe_operation(&self) -> &'static str {
        "synthetic.widget.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        SYNTHETIC_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        settings
            .get("base_url")
            .and_then(Value::as_str)
            .and_then(|raw| reqwest::Url::parse(raw).ok())
            .map(|url| canonical_target(&url))
            .ok_or(GovernedError::InvalidConfiguration)
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        let serial = self.built.fetch_add(1, Ordering::SeqCst);
        self.instances.lock().expect("instances").push(serial);
        let token = envelope
            .material("token")
            .or_else(|| envelope.material("access_token"))
            .ok_or(GovernedError::MaterialNotAllowed)?
            .to_owned();
        let read: Arc<dyn Tool> = Arc::new(FunctionTool::new(
            "read_widget",
            "Read one widget",
            move |_context, arguments| {
                let token = token.clone();
                async move {
                    Ok(json!({"client": serial, "token": token, "widget": arguments["widget_id"]}))
                }
            },
        ));
        let write: Arc<dyn Tool> = Arc::new(FunctionTool::new(
            "write_widget",
            "Write one widget (not mapped)",
            |_context, _arguments| async { Ok(json!({"written": true})) },
        ));
        Ok(Arc::new(BasicToolset::new(
            build.toolkit_name(),
            vec![read, write],
        )))
    }
}

fn settings() -> Map<String, Value> {
    json!({"base_url": "https://widgets.example.test/"})
        .as_object()
        .cloned()
        .expect("settings")
}

fn run(
    adapter: &Arc<SyntheticAdapter>,
    respond: Box<Respond>,
) -> (Arc<dyn Toolset>, Arc<RecordingRedeemer>) {
    let registry = reference_registry_builder()
        .register(Arc::clone(adapter) as Arc<dyn AuthAdapter>)
        .expect("a sixth provider type")
        .build();
    let redeemer = RecordingRedeemer::new(respond);
    let client = Arc::new(GovernedOperationClient::new(
        Arc::new(registry),
        Arc::clone(&redeemer) as Arc<dyn CredentialRedeemer>,
    ));
    let settings = settings();
    let toolset = client
        .toolset(&GovernedToolkitSpec::new(
            "widgets",
            "synthetic",
            Some(9),
            &settings,
            &policy(),
        ))
        .expect("synthetic toolset");
    (toolset, redeemer)
}

#[tokio::test]
async fn a_sixth_adapter_registers_and_serves_a_governed_call() {
    let adapter = Arc::new(SyntheticAdapter::default());
    let (toolset, redeemer) = run(
        &adapter,
        answering(EnvelopeKind::Static, &[("token", "envelope-token")], TARGET),
    );
    let declarations = adapter.built.load(Ordering::SeqCst);
    let result = tool(&toolset, "read_widget")
        .await
        .execute(context(), json!({"widget_id": "W-1"}))
        .await
        .expect("served");
    assert_eq!(result["token"], "envelope-token");
    assert_eq!(result["widget"], "W-1");
    assert_eq!(adapter.built.load(Ordering::SeqCst), declarations + 1);
    let calls = redeemer.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].operation_id, "synthetic.widget.read");
    assert_eq!(calls[0].resource_ref, "synthetic:widget/W-1");

    // A duplicate provider type is refused at composition.
    assert!(
        reference_registry_builder()
            .register(Arc::new(SyntheticAdapter::default()))
            .and_then(|builder| builder.register(Arc::new(SyntheticAdapter::default())))
            .is_err()
    );
}

#[tokio::test]
async fn an_unmapped_synthetic_tool_never_redeems() {
    let adapter = Arc::new(SyntheticAdapter::default());
    let (toolset, redeemer) = run(
        &adapter,
        answering(EnvelopeKind::Static, &[("token", "t")], TARGET),
    );
    let error = tool(&toolset, "write_widget")
        .await
        .execute(context(), json!({}))
        .await
        .expect_err("not mapped");
    assert_eq!(error.code, "governed.operation_not_mapped");
    assert_eq!(redeemer.calls().len(), 0);
}

/// An envelope minted for target A is refused for toolkit B before a
/// provider client exists: nothing is built, nothing is called.
#[tokio::test]
async fn an_envelope_for_another_target_builds_no_client() {
    let adapter = Arc::new(SyntheticAdapter::default());
    let (toolset, redeemer) = run(
        &adapter,
        answering(
            EnvelopeKind::Static,
            &[("token", "t")],
            "https://other.example.test",
        ),
    );
    let declarations = adapter.built.load(Ordering::SeqCst);
    let error = tool(&toolset, "read_widget")
        .await
        .execute(context(), json!({"widget_id": "W-1"}))
        .await
        .expect_err("another target");
    assert_eq!(error.code, "governed.target_mismatch");
    assert_eq!(redeemer.calls().len(), 1);
    assert_eq!(
        adapter.built.load(Ordering::SeqCst),
        declarations,
        "no client built"
    );
}

/// Two governed runs with different security contexts (different
/// redeemers, different envelopes) never share a provider client, and
/// neither do two calls of one run: every invocation builds its own.
#[tokio::test]
async fn governed_runs_never_share_a_client() {
    let adapter = Arc::new(SyntheticAdapter::default());
    let (first, _) = run(
        &adapter,
        answering(EnvelopeKind::Static, &[("token", "context-a")], TARGET),
    );
    let (second, _) = run(
        &adapter,
        answering(
            EnvelopeKind::OauthAccess,
            &[("access_token", "context-b")],
            TARGET,
        ),
    );
    let mut clients = Vec::new();
    for (toolset, token) in [
        (&first, "context-a"),
        (&second, "context-b"),
        (&first, "context-a"),
    ] {
        let result = tool(toolset, "read_widget")
            .await
            .execute(context(), json!({"widget_id": 1}))
            .await
            .expect("served");
        assert_eq!(result["token"], token);
        clients.push(result["client"].as_u64().expect("client serial"));
    }
    let mut distinct = clients.clone();
    distinct.sort_unstable();
    distinct.dedup();
    assert_eq!(distinct.len(), clients.len(), "{clients:?}");
}
