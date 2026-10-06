use super::*;
use crate::rust_profile::rust_prepare_archive::digest;
use crate::rust_profile::{compose_manifest_profile, parse_dependencies};

#[test]
fn only_exact_broker_policy_and_capability_select_the_broker_profile() {
    assert_eq!(Profile::for_policy(BROKER_POLICY).unwrap(), Profile::Broker);
    assert_eq!(
        Profile::for_execution(BROKER_POLICY, true).unwrap(),
        Profile::Broker
    );
    assert!(Profile::for_execution(BROKER_POLICY, false).is_err());
    for policy in ["cargo-execute-v1", "rust-v1", "custom-operator-legacy.v12"] {
        assert_eq!(
            Profile::for_execution(policy, false).unwrap(),
            Profile::Legacy
        );
        assert!(Profile::for_execution(policy, true).is_err());
    }
    for policy in [
        "cargo-broker-execute-v10",
        "cargo-broker_execute-v1",
        "Cargo-broker-execute-v1",
        "",
        "/tmp/profile",
        "cargo broker",
    ] {
        assert!(Profile::for_execution(policy, true).is_err());
    }
}

#[test]
fn fixed_broker_assets_keep_their_measured_interface() {
    assert_eq!(
        digest(Profile::Broker.template().as_bytes()),
        "63ca94313d30b01cb504e78b3d96d9710b61cb3dde4f2eb54bdde5ab2acae8b9"
    );
    let expected = [
        (
            "src/main.rs",
            "ae2b26b7b0b06a7626db61f79aa65b33675c7e2f7919dbddc61a0bb0d11e0634",
        ),
        (
            "src/platform.rs",
            "e2e42c258c1da7107e3e87e42ea12396b6599c3850a8042c58ecdf5287e18ad1",
        ),
        (
            "src/platform_client.rs",
            "693a6f815af46fb707290778af42e10c9438081b8ad8ab8c4442fabc88347ba3",
        ),
        (
            "src/platform_pipe.rs",
            "08d5a8dca7fb6e8d4499292465b70db66d993096c6974634456ad1ad7d9cb629",
        ),
    ];
    assert_eq!(Profile::Broker.sources().len(), expected.len());
    for ((path, bytes), (expected_path, expected_digest)) in
        Profile::Broker.sources().iter().zip(expected)
    {
        assert_eq!(*path, expected_path);
        assert_eq!(digest(bytes.as_bytes()), expected_digest);
    }
    let manifest = compose_manifest_profile(
        Profile::Broker,
        parse_dependencies(b"[dependencies]\nitoa='=1.0.18'\n").unwrap(),
    )
    .unwrap();
    let value: toml::Value = toml::from_str(std::str::from_utf8(&manifest).unwrap()).unwrap();
    assert_eq!(
        value["dependencies"]["serde"]["version"].as_str(),
        Some("=1.0.229")
    );
    assert_eq!(
        value["dependencies"]["serde"]["features"]
            .as_array()
            .unwrap(),
        &[toml::Value::String("derive".into())]
    );
    assert_eq!(
        value["dependencies"]["serde_json"].as_str(),
        Some("=1.0.151")
    );
}

#[test]
fn ordinary_profile_keeps_original_template_wrapper_and_source_set() {
    assert_eq!(Profile::Legacy.sources(), LEGACY_SOURCES);
    assert_eq!(Profile::Legacy.sources().len(), 1);
    assert_eq!(
        digest(Profile::Legacy.template().as_bytes()),
        "ef28d29a000d697efbac8902fecaf26bbd8f4d2c315f346eec6d416420d9361c"
    );
    assert_eq!(
        digest(Profile::Legacy.wrapper().as_bytes()),
        "f15316395ae72f7fbb58abf8a4430ccb83c5d5ec0b99ce6de6785db42086cbeb"
    );
    // These image bytes are independent golden inputs, not broker-profile normalization.
    assert_eq!(Profile::Legacy.template(), TEMPLATE);
    assert_eq!(Profile::Legacy.wrapper(), WRAPPER);
}

#[test]
fn broker_manifest_refuses_direct_and_renamed_serde_overrides() {
    for declaration in [
        "[dependencies]\nserde='1'\n",
        "[dependencies]\nalias={version='1',package='serde'}\n",
    ] {
        let dependencies = parse_dependencies(declaration.as_bytes()).unwrap();
        assert!(compose_manifest_profile(Profile::Legacy, dependencies.clone()).is_ok());
        assert_eq!(
            compose_manifest_profile(Profile::Broker, dependencies)
                .unwrap_err()
                .code,
            ErrorCode::InvalidDeclaration
        );
    }
}
