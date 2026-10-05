//! Startup admission for both runtime backends. No runtime or database is contacted.
use super::*;
use serde_json::{Value, json};

fn examples() -> Vec<Value> {
    serde_json::from_str::<Value>(include_str!(
        "../../../../deploy/runtime/code-platform-startup.example.json"
    ))
    .unwrap()["supervisors"]
        .as_array()
        .unwrap()
        .clone()
}

fn backends(input: &Value) -> [Value; 2] {
    [
        json!({"kind":"docker"}),
        json!({
            "kind":"kubernetes", "cluster":"sandbox-cluster",
            "namespace":"code-execution", "runtime_class":"sandbox",
            "node_selector":{"sandbox":"true"},
            "image":format!("registry.example/code@{}",input["image_digest"].as_str().unwrap())
        }),
    ]
}

#[test]
fn code_platform_startup_defaults_to_refusal_and_preserves_runtime_cap() {
    let config: Config = serde_json::from_str(include_str!(
        "../../../../deploy/runtime/sandbox-supervisor.example.json"
    ))
    .unwrap();
    assert!(!config.code_platform_profile);
    assert!(config.code_owner_requester.is_none());
    config.validate().unwrap();
    assert_eq!(MAX_RUNTIME_PROFILES, 8);
    let mut value = examples().remove(0);
    value["code_platform_profile"] = Value::Null;
    assert!(serde_json::from_value::<Config>(value).is_err());
}

#[test]
fn code_platform_startup_admits_explicit_execution_on_both_backends() {
    for mut input in examples() {
        for backend in backends(&input) {
            input["backend"] = backend;
            let config: Config = serde_json::from_value(input.clone()).unwrap();
            config.validate().unwrap();
            assert!(config.code_platform_profile);
            assert!(config.purpose == Purpose::Execution);
            assert_eq!(
                config.code_owner_requester.as_deref(),
                Some("dns:elitea-main")
            );
        }
    }
}

#[test]
fn code_platform_startup_denies_missing_owner_preparation_and_network() {
    for mut input in examples() {
        for backend in backends(&input) {
            input["backend"] = backend;
            for (field, value) in [
                ("code_owner_requester", Value::Null),
                ("code_owner_requester", json!("dns:another owner")),
                ("purpose", json!("preparation")),
                ("preparation_network", json!("resolver-network")),
            ] {
                let mut invalid = input.clone();
                invalid[field] = value;
                let config: Config = serde_json::from_value(invalid).unwrap();
                assert!(config.validate().is_err(), "{field}");
            }
        }
    }
}

#[test]
fn code_platform_startup_denies_mutable_unbounded_or_wrong_rust_profile() {
    for input in examples() {
        for (field, value) in [
            ("image_digest", json!("runner:latest")),
            ("memory_bytes", json!(0)),
            ("cpu_limit", json!(0)),
            ("timeout_seconds", json!(3601)),
        ] {
            let mut invalid = input.clone();
            invalid[field] = value;
            let config: Config = serde_json::from_value(invalid).unwrap();
            assert!(config.validate().is_err(), "{field}");
        }
        let mut config: Config = serde_json::from_value(input).unwrap();
        config.cpu_limit = f64::INFINITY;
        assert!(config.validate().is_err());
    }
    let mut value = examples().remove(1);
    value["policy_revision"] = json!("cargo-execute-v2");
    let config: Config = serde_json::from_value(value).unwrap();
    assert!(config.validate().is_err());
}
