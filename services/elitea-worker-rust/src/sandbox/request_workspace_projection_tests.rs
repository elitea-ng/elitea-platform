use super::*;

#[test]
fn removing_workspace_preserves_exact_original_dependency_and_broker_bytes() {
    let legacy = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_legacy_v1.json"
    )
    .as_slice();
    let python = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_python_v2.json"
    )
    .as_slice();
    let native = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_native_v3.json"
    )
    .as_slice();
    let workspace = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_only_v4.json"
    )
    .as_slice();
    let broker = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_broker_v5.json"
    )
    .as_slice();
    let both = include_bytes!(
        "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_both_v5.json"
    )
    .as_slice();
    for (original, expected) in [
        (legacy, legacy),
        (python, python),
        (native, native),
        (workspace, legacy),
        (broker, broker),
        (both, broker),
    ] {
        let job = PreparedJob::from_transport(original).unwrap();
        assert_eq!(job.to_transport().unwrap(), original);
        let base = job.pre_workspace().unwrap();
        assert!(base.workspace().is_none());
        assert_eq!(base.to_transport().unwrap(), expected);
        assert_eq!(
            base.fingerprint().unwrap(),
            PreparedJob::from_transport(expected)
                .unwrap()
                .fingerprint()
                .unwrap()
        );
        if let Some(binding) = job.workspace() {
            assert_eq!(
                base.with_workspace(binding.clone())
                    .unwrap()
                    .to_transport()
                    .unwrap(),
                original
            );
        }
    }
}
