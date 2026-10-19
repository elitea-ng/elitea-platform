//! FU-229: a Code node that declares dependencies on a runtime without a
//! preparation profile is a typed refusal, decided before any I/O.
use super::preparation::{declares_dependencies, refuse_unavailable_preparation};
use super::*;
use crate::agents::graph::code_runtime::CodePreparationFailure;
use crate::config::SandboxRuntimeConfig;
use serde_json::json;

const LANGUAGES: [(&str, CodeLanguage); 4] = [
    ("python", CodeLanguage::Python),
    ("javascript", CodeLanguage::JavaScript),
    ("typescript", CodeLanguage::TypeScript),
    ("rust", CodeLanguage::Rust),
];

fn config(language: &str, preparation: bool) -> SandboxRuntimeConfig {
    let mut value = json!({
        "language": language, "target": "sandbox.internal:9446", "audience": "pure-owner",
        "image_digest": format!("sha256:{}", "a".repeat(64)),
        "policy_revision": "isolated-v1", "timeout_seconds": 30
    });
    if preparation {
        value["preparation"] = json!({
            "target": "preparation.internal:9448", "audience": "preparation-owner",
            "image_digest": format!("sha256:{}", "b".repeat(64)),
            "policy_revision": "preparation-v1", "timeout_seconds": 120
        });
    }
    serde_json::from_value(value).unwrap()
}

fn client(audience: &str) -> SandboxClient {
    SandboxClient::from_channel(
        tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
        audience.into(),
        Duration::from_secs(30),
    )
    .unwrap()
}

/// Every peer is unreachable: any journal, Main or supervisor call fails.
fn runtime(language: &str, preparation: bool) -> RemoteCodeRuntime {
    let control = Arc::new(
        AgentControlClient::from_channel(
            tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let state = crate::test_pg::pool_options()
        .max_connections(1)
        .acquire_timeout(Duration::from_millis(200))
        .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
        .unwrap();
    let preparation_client = preparation.then(|| client("preparation-owner"));
    let factory = CodeRuntimeFactory::new(
        control,
        vec![(
            config(language, preparation),
            client("pure-owner"),
            preparation_client,
        )],
        state,
    );
    RemoteCodeRuntime {
        saved_child_scope: None,
        intent_content: None,
        control: factory.control,
        authority: Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture()),
        profiles: factory.profiles,
        journal: factory.journal,
        compiled_profile: None,
        debug_sink: None,
    }
}

/// The smallest saved node of each language that declares a dependency.
fn with_dependencies(language: CodeLanguage) -> (&'static str, Option<&'static str>) {
    match language {
        CodeLanguage::Python => ("import micropip\nreturn 1", None),
        CodeLanguage::JavaScript | CodeLanguage::TypeScript => {
            ("import { z } from \"npm:zod@3\";\nreturn 1;", None)
        }
        CodeLanguage::Rust => ("fn main() {}", Some("[dependencies]\nserde = \"1\"\n")),
    }
}

fn invocation<'a>(
    language: CodeLanguage,
    source: &'a str,
    dependencies_toml: Option<&'a str>,
) -> CodeInvocation<'a> {
    CodeInvocation {
        activation: [7; 32],
        language,
        platform_client: false,
        source,
        dependencies_toml,
        provenance: CodeProvenance::SavedLiteral,
        input_json: b"{}".to_vec(),
        trace: None,
        original: None,
        debug: None,
        workspace: None,
    }
}

#[test]
fn declared_dependencies_are_recognised_per_language_without_false_positives() {
    use CodeLanguage::{JavaScript, Python, Rust, TypeScript};
    for (language, source, toml, declared) in [
        (Python, "import micropip", None, true),
        (Python, "await micropip.install('x')", None, true),
        (Python, "return 41", None, false),
        (Python, "my_micropip = 1\nmicropipe = 2", None, false),
        (JavaScript, "import x from 'npm:left-pad'", None, true),
        (
            JavaScript,
            "const m = await import(`jsr:@std/path`)",
            None,
            true,
        ),
        (
            TypeScript,
            "import { y } from \"jsr:@std/assert\"",
            None,
            true,
        ),
        (TypeScript, "// npm: is a scheme\nreturn 1", None, false),
        (JavaScript, "import x from './local.js'", None, false),
        (Rust, "fn main() {}", Some("[dependencies]\n"), true),
        (Rust, "// npm: micropip", None, false),
    ] {
        assert_eq!(
            declares_dependencies(language, source, toml),
            declared,
            "{language:?}: {source}"
        );
    }
}

#[tokio::test]
async fn declared_dependencies_without_preparation_are_refused_before_any_io() {
    for (name, language) in LANGUAGES {
        let runtime = runtime(name, false);
        let profile = &runtime.profiles[0];
        let (source, toml) = with_dependencies(language);
        let refused = tokio::time::timeout(
            Duration::from_secs(5),
            runtime.prepare_execution_job_diagnosed(&invocation(language, source, toml), profile),
        )
        .await
        .expect("a pure refusal never waits on a peer");
        assert!(
            matches!(refused, Err(CodePreparationFailure::Unavailable)),
            "{name}"
        );
        assert_eq!(
            CodePreparationFailure::Unavailable.code(),
            "pipeline.code_preparation_unavailable"
        );
    }
}

#[tokio::test]
async fn a_node_without_dependencies_keeps_its_existing_route() {
    for (name, language) in LANGUAGES {
        let runtime = runtime(name, false);
        let profile = &runtime.profiles[0];
        let plain = invocation(language, "return 1", None);
        assert_eq!(refuse_unavailable_preparation(&plain, profile), Ok(()));
        // Unchanged: it goes on to the dispatch journal (unreachable here).
        let result = runtime
            .prepare_execution_job_diagnosed(&plain, profile)
            .await;
        assert!(
            matches!(result, Err(CodePreparationFailure::Failed)),
            "{name}"
        );
    }
}

#[tokio::test]
async fn a_runtime_with_preparation_is_unchanged() {
    for (name, language) in LANGUAGES {
        let runtime = runtime(name, true);
        let (source, toml) = with_dependencies(language);
        assert_eq!(
            refuse_unavailable_preparation(
                &invocation(language, source, toml),
                &runtime.profiles[0]
            ),
            Ok(()),
            "{name}"
        );
    }
}
