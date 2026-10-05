//! Operator admission and route compatibility. No transport or secret file is opened.
use super::*;
use serde_json::{Value, json};

fn example() -> Value {
    serde_json::from_str::<Value>(include_str!(
        "../../../deploy/runtime/code-platform-startup.example.json"
    ))
    .unwrap()["worker"]
        .clone()
}

fn accepted(value: Value) -> Result<RuntimeDeployConfig, ()> {
    serde_json::from_value::<RuntimeDeployConfig>(value)
        .map_err(|_| ())?
        .validate()
        .map_err(|_| ())
}

#[test]
fn code_platform_startup_preserves_omitted_profiles_and_exact_policy() {
    let mut value = example();
    for runtime in value["sandbox_runtimes"].as_array_mut().unwrap() {
        runtime.as_object_mut().unwrap().remove("platform_client");
    }
    let pure = accepted(value).unwrap();
    assert!(
        pure.sandbox_runtimes
            .iter()
            .all(|p| p.platform_client.is_none())
    );
    let enabled = accepted(example()).unwrap();
    assert_eq!(enabled.sandbox_runtimes.len(), 4);
    for (pure, enabled) in pure.sandbox_runtimes.iter().zip(&enabled.sandbox_runtimes) {
        let mut unchanged = enabled.clone();
        unchanged.platform_client = None;
        assert_eq!(*pure, unchanged);
        let selected = enabled.platform_client.as_ref().unwrap();
        assert_eq!(
            selected
                .policy()
                .unwrap()
                .binding()
                .unwrap()
                .policy_sha256(),
            "a299856c1b28b5aee8488da91e8dd2cb33251c89b677733b63447179144d03da"
        );
        assert!(selected.compiled_snapshot.is_none());
        let execution = selected.execution_config(enabled);
        assert_eq!(execution.language, enabled.language);
        assert_eq!(execution.preparation, enabled.preparation);
        assert!(execution.platform_client.is_none());
    }
}

#[test]
fn code_platform_startup_requires_original_attempt_storage_owner() {
    for (field, value) in [
        ("agent_node_recovery", json!(false)),
        ("agent_checkpoint_connection_path", Value::Null),
    ] {
        let mut input = example();
        input[field] = value;
        assert!(accepted(input).is_err(), "{field}");
    }
}

#[test]
fn code_platform_startup_rejects_unbounded_or_authored_policy_fields() {
    for (field, value) in [
        ("target", json!("http://sandbox.internal:9446")),
        ("audience", json!("dns:owner with-space")),
        ("image_digest", json!("runner:latest")),
        ("timeout_seconds", json!(0)),
        ("timeout_seconds", json!(3601)),
        ("max_calls", json!(0)),
        ("max_calls", json!(4097)),
        ("max_total_bytes", json!(0)),
        ("max_total_bytes", json!(67_108_865)),
        ("policy_sha256", json!("a".repeat(64))),
        ("ca_path", json!("/authored/trust.pem")),
        ("purpose", json!("preparation")),
        ("compiled_snapshot", Value::Null),
    ] {
        let mut input = example();
        input["sandbox_runtimes"][0]["platform_client"][field] = value;
        assert!(accepted(input).is_err(), "{field}");
    }
    for value in [Value::Null, json!(true)] {
        let mut input = example();
        input["sandbox_runtimes"][0]["platform_client"] = value;
        assert!(accepted(input).is_err());
    }
}

#[test]
fn code_platform_startup_keeps_exact_audience_stop_routing() {
    let mut input = example();
    input["sandbox_runtimes"][0]["platform_client"]["target"] =
        json!("other-supervisor.internal:9446");
    assert!(accepted(input).is_err());
    let mut input = example();
    input["sandbox_runtimes"][3]["platform_client"]["audience"] = json!("dns:rust-pure");
    assert!(accepted(input).is_err());
}

#[test]
fn code_platform_startup_keeps_rust_broker_and_cache_profiles_explicit() {
    let mut input = example();
    input["sandbox_runtimes"][3]["platform_client"]["policy_revision"] = json!("cargo-execute-v2");
    assert!(accepted(input).is_err());
    let settings = json!({
        "profiles_file":"/run/elitea-runtime/broker-compiled-profiles.json",
        "profiles_sha256":"a".repeat(64), "dependency_bundle_sha256":""
    });
    let mut input = example();
    input["sandbox_runtimes"][3]["platform_client"]["compiled_snapshot"] = settings.clone();
    assert!(accepted(input).is_ok());
    let mut input = example();
    input["sandbox_runtimes"][0]["platform_client"]["compiled_snapshot"] = settings;
    assert!(accepted(input).is_err());
}
