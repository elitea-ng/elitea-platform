use super::*;
use crate::{
    protocol::{
        command::Ed25519PublicKeyResolver,
        elitea::runtime::v1::SandboxJobGrantClaimsV1,
        sandbox_grant::{AuthorizedCancellation, GrantVerifier},
    },
    sandbox::{
        ledger::{JobLedger, LedgerError},
        request::{Language, PreparedJob},
        runtime::CodeJobRuntime,
    },
    state::postgres_session_tests::IsolatedPostgres,
};
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, ManifestEntry, docker::CodeJobIdentity},
};
use prost::Message as _;
use ring::signature::{Ed25519KeyPair, KeyPair as _};
use serde_json::{Value, json};
use sqlx::postgres::PgPoolOptions;
use std::sync::{Arc, Mutex};

const DATABASE_URL: &str = "ELITEA_TEST_DATABASE_URL";
const POLICY: &str = "python-preparation-v1";
const OWNER: &str = "preparation-owner";

fn job() -> PreparationJob {
    PreparationJob::new(
        "raise RuntimeError('must never execute')".into(),
        image(),
        POLICY.into(),
        30,
    )
    .unwrap()
}
fn image() -> String {
    format!("sha256:{}", "a".repeat(64))
}
fn bundle() -> DependencyBundle {
    let content = format!(
        r#"{{"revision":1,"runtime":"pyodide-0.29.0","requirements":[],"files":[{{"name":"elitea-python-lock.json","bytes":2,"sha256":"{}"}}]}}"#,
        content_root(ring::digest::digest(&ring::digest::SHA256, b"{}").as_ref())
    );
    let root =
        content_root(ring::digest::digest(&ring::digest::SHA256, content.as_bytes()).as_ref());
    let record = format!("{},\"digest\":\"{root}\"}}", &content[..content.len() - 1]);
    DependencyBundle::parse_record(record.as_bytes()).unwrap()
}
fn marker(request: &PreparationJob) -> Value {
    let now = chrono::Utc::now().timestamp_millis();
    json!({
        "revision":1, "status":"resolved",
        "source_sha256":content_root(ring::digest::digest(&ring::digest::SHA256, request.source().as_bytes()).as_ref()),
        "preparer_image_digest":request.image_digest(), "policy_revision":request.policy_revision(),
        "timeout_seconds":request.timeout_seconds(), "started_unix_ms":now-1000,
        "deadline_unix_ms":now-1000+i64::from(request.timeout_seconds())*1000,
        "bundle":serde_json::from_slice::<Value>(bundle().record_json()).unwrap()
    })
}

struct Keys([u8; 32]);
impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, key_id: &str) -> Option<[u8; 32]> {
        (key_id == "fixture-key").then_some(self.0)
    }
}
fn signed(
    request: &PreparationJob,
    cancel: bool,
) -> (GrantVerifier<Keys>, SignedSandboxJobGrantV1) {
    signed_digest(request.fingerprint().unwrap(), cancel)
}
fn signed_digest(
    fingerprint: [u8; 32],
    cancel: bool,
) -> (GrantVerifier<Keys>, SignedSandboxJobGrantV1) {
    let key = Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
    let now = chrono::Utc::now().timestamp_millis();
    let claims = SandboxJobGrantClaimsV1 {
        revision: if cancel { 2 } else { 1 },
        cancel_only: cancel,
        tenant_id: "tenant".into(),
        project_id: 2,
        execution_id: "execution".into(),
        activation_id: "preparation-1".into(),
        request_digest: fingerprint.to_vec(),
        submitter_workload_identity: "worker".into(),
        audience: "preparation".into(),
        issued_at_unix_millis: now,
        expires_at_unix_millis: now + 30_000,
        generation: 1,
        dependency_bundle_sha256: vec![],
    }
    .encode_to_vec();
    let mut input = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
    input.extend_from_slice(&(claims.len() as u64).to_be_bytes());
    input.extend_from_slice(&claims);
    let grant = SignedSandboxJobGrantV1 {
        key_id: "fixture-key".into(),
        claims_bytes: claims,
        signature: key.sign(&input).as_ref().to_vec(),
    };
    let verifier = GrantVerifier::new(
        Keys(key.public_key().as_ref().try_into().unwrap()),
        "preparation".into(),
    )
    .unwrap();
    (verifier, grant)
}
fn authority(request: &PreparationJob) -> AuthorizedPreparation {
    let (verifier, grant) = signed(request, false);
    verifier
        .verify_preparation(
            &grant,
            "worker",
            request,
            chrono::Utc::now().timestamp_millis(),
        )
        .unwrap()
}
fn cancellation(request: &PreparationJob) -> AuthorizedCancellation {
    let (verifier, grant) = signed(request, true);
    verifier
        .verify_cancellation(&grant, "worker", chrono::Utc::now().timestamp_millis())
        .unwrap()
}

#[derive(Default)]
#[expect(
    clippy::struct_excessive_bools,
    reason = "Independent runtime and injected failure flags model recovery combinations"
)]
struct RuntimeState {
    present: bool,
    running: bool,
    prepares: usize,
    dispatches: usize,
    terminations: usize,
    cleanups: usize,
    marker: Option<Vec<u8>>,
    termination_error: bool,
    termination_pending: bool,
    marker_observation_error: bool,
    termination_runtime_ids: Vec<Option<String>>,
    cleanup_runtime_ids: Vec<Option<String>>,
    readiness_pending: bool,
    execution_files: std::collections::BTreeMap<String, Vec<u8>>,
    execution_receipt: Option<Vec<u8>>,
}
#[derive(Clone)]
struct Runtime {
    image: String,
    state: Arc<Mutex<RuntimeState>>,
    dispatched: Arc<tokio::sync::Notify>,
    readiness_observed: Arc<tokio::sync::Notify>,
    termination_started: Arc<tokio::sync::Notify>,
    termination_released: Arc<tokio::sync::Notify>,
}
impl Runtime {
    fn new(request: &PreparationJob, resolved: bool) -> Self {
        Self {
            image: request.image_digest().into(),
            state: Arc::new(Mutex::new(RuntimeState {
                marker: resolved.then(|| serde_json::to_vec(&marker(request)).unwrap()),
                ..RuntimeState::default()
            })),
            dispatched: Arc::new(tokio::sync::Notify::new()),
            readiness_observed: Arc::new(tokio::sync::Notify::new()),
            termination_started: Arc::new(tokio::sync::Notify::new()),
            termination_released: Arc::new(tokio::sync::Notify::new()),
        }
    }
    fn check(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        if !self.state.lock().unwrap().present || identity.runtime_id() != Some("original-runtime")
        {
            return Err(SandboxError::ExecutionFailed(
                "Original runtime is missing or replaced".into(),
            ));
        }
        Ok(())
    }
}
#[async_trait::async_trait]
impl CodeJobRuntime for Runtime {
    fn image_digest(&self) -> &str {
        &self.image
    }
    fn code_compilation_enabled(&self) -> bool {
        false
    }
    fn code_job_timeout(&self) -> Duration {
        Duration::from_secs(30)
    }
    async fn instance(&self, identity: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        if identity
            .runtime_id()
            .is_some_and(|id| id != "original-runtime")
        {
            return Err(SandboxError::ExecutionFailed(
                "Runtime binding changed".into(),
            ));
        }
        Ok(self
            .state
            .lock()
            .unwrap()
            .present
            .then(|| "original-runtime".into()))
    }
    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        Ok(self.instance(identity).await?.is_some())
    }
    async fn prepare(&self, _: &CodeJobIdentity, manifest: &Manifest) -> Result<(), SandboxError> {
        assert_eq!(manifest.entries.len(), 2);
        let mut state = self.state.lock().unwrap();
        assert!(!state.present);
        state.present = true;
        state.running = true;
        state.prepares += 1;
        Ok(())
    }
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        self.check(identity)?;
        let ready = !self.state.lock().unwrap().readiness_pending;
        self.readiness_observed.notify_one();
        Ok(ready)
    }
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.check(identity)?;
        let mut state = self.state.lock().unwrap();
        state.dispatches += 1;
        if state.execution_receipt.is_some() {
            state.running = false;
        }
        drop(state);
        self.dispatched.notify_one();
        Ok(())
    }
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        self.check(identity)?;
        let state = self.state.lock().unwrap();
        if state.dispatches == 0 {
            Ok(None)
        } else {
            Ok(state.execution_receipt.clone())
        }
    }
    async fn export_execution_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        use tokio::io::AsyncWriteExt as _;
        self.check(identity)?;
        let content = self
            .state
            .lock()
            .unwrap()
            .execution_files
            .get(name)
            .cloned()
            .ok_or_else(|| SandboxError::ExecutionFailed("Execution file is missing".into()))?;
        if content.len() as u64 != bytes {
            return Err(SandboxError::ExecutionFailed(
                "Execution file length changed".into(),
            ));
        }
        writer
            .write_all(&content)
            .await
            .map_err(|_| SandboxError::ExecutionFailed("Execution export failed".into()))
    }
    async fn import_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        use tokio::io::AsyncReadExt as _;
        self.check(identity)?;
        let mut content = Vec::new();
        reader
            .take(bytes + 1)
            .read_to_end(&mut content)
            .await
            .map_err(|_| SandboxError::ExecutionFailed("Execution import failed".into()))?;
        if content.len() as u64 != bytes {
            return Err(SandboxError::ExecutionFailed(
                "Execution import length changed".into(),
            ));
        }
        let mut state = self.state.lock().unwrap();
        if let Some(recorded) = state.execution_files.get(name) {
            if *recorded != content {
                return Err(SandboxError::ExecutionFailed(
                    "Execution file is immutable".into(),
                ));
            }
        } else {
            state.execution_files.insert(name.into(), content);
        }
        Ok(())
    }
    async fn preparation_marker(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        self.check(identity)?;
        let state = self.state.lock().unwrap();
        if state.marker_observation_error {
            return Err(SandboxError::ExecutionFailed(
                "Preparation marker observation is unavailable".into(),
            ));
        }
        Ok(state.marker.clone())
    }
    async fn bundle_preparation_marker(
        &self,
        identity: &CodeJobIdentity,
        _: bool,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        self.preparation_marker(identity).await
    }
    async fn release_preparation(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.check(identity)
    }
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        let pending = {
            let mut state = self.state.lock().unwrap();
            state
                .termination_runtime_ids
                .push(identity.runtime_id().map(str::to_owned));
            state.termination_pending
        };
        if pending {
            self.termination_started.notify_one();
            self.termination_released.notified().await;
        }
        let mut state = self.state.lock().unwrap();
        if state.termination_error {
            return Err(SandboxError::ExecutionFailed(
                "Termination remains unconfirmed".into(),
            ));
        }
        state.running = false;
        state.terminations += 1;
        Ok(())
    }
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        let mut state = self.state.lock().unwrap();
        state
            .cleanup_runtime_ids
            .push(identity.runtime_id().map(str::to_owned));
        assert!(
            !state.running,
            "A holding runtime cannot be removed before termination"
        );
        state.present = false;
        state.cleanups += 1;
        Ok(())
    }
}
fn supervisor(pool: sqlx::PgPool, runtime: Runtime) -> DockerSupervisor {
    DockerSupervisor::new(JobLedger::new(pool), runtime, OWNER.into(), 2)
        .unwrap()
        .with_preparation_policy(POLICY.into())
        .unwrap()
}
async fn database() -> IsolatedPostgres {
    let database = IsolatedPostgres::create(
        &std::env::var(DATABASE_URL)
            .expect("Set ELITEA_TEST_DATABASE_URL for preparation lifecycle tests"),
    )
    .await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&database.pool)
        .await
        .unwrap();
    for migration in [
        include_str!("../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0006_sandbox_stop_reconciliation.sql"
        ),
        include_str!("../../../elitea-main/migrations/agentstate/0008_sandbox_runtime_binding.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0009_sandbox_preparation_bundle.sql"
        ),
        include_str!("../../../elitea-main/migrations/agentstate/0010_sandbox_phase_deadlines.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0013_sandbox_whole_code_recovery.sql"
        ),
    ] {
        sqlx::raw_sql(migration)
            .execute(&database.pool)
            .await
            .unwrap();
    }
    database
}
fn pending(outcome: PreparationReconciliation) -> DependencyBundle {
    let PreparationReconciliation::Pending {
        bundle: Some(bundle),
    } = outcome
    else {
        panic!("Resolution must remain pending until publication")
    };
    bundle
}

#[test]
fn native_marker_emitted_by_linux_preparer_is_accepted_without_normalization() {
    let request =
        PreparationJob::from_transport(include_bytes!("native-deno-preparation-job-v2.json"))
            .unwrap();
    let emitted = include_bytes!("native-deno-preparation-marker-v2.json");
    let clock: Value = serde_json::from_slice(emitted).unwrap();
    let now = clock["started_unix_ms"].as_i64().unwrap() + 1;
    let bundle = parse_marker(emitted, &request, now).unwrap();
    assert!(request.matches_bundle(&bundle));
    assert_eq!(bundle.root(), clock["bundle"]["digest"].as_str().unwrap());
    assert_eq!(bundle.file_count(), 2);

    let raw = std::str::from_utf8(emitted).unwrap();
    let duplicate_fields = [
        ("\"revision\":2", "\"revision\":2,\"revision\":2"),
        ("\"platform\":{", "\"platform\":{},\"platform\":{"),
        ("\"payload\":{", "\"payload\":{},\"payload\":{"),
        ("\"files\":[", "\"files\":[],\"files\":["),
    ];
    for (field, replacement) in duplicate_fields {
        let changed = raw.replacen(field, replacement, 1);
        assert_ne!(changed, raw);
        assert!(parse_marker(changed.as_bytes(), &request, now).is_err());
    }
    let record: crate::sandbox::native_bundle::NativeRecord =
        serde_json::from_slice(bundle.record_json()).unwrap();
    let noncanonical = serde_json::to_string(&record).unwrap();
    let changed = raw.replace(
        std::str::from_utf8(bundle.record_json()).unwrap(),
        &noncanonical,
    );
    assert_ne!(changed, raw);
    assert!(parse_marker(changed.as_bytes(), &request, now).is_err());
}

#[test]
fn marker_binds_source_runtime_policy_deadline_and_exact_bundle() {
    let request = job();
    let now = chrono::Utc::now().timestamp_millis();
    let original = marker(&request);
    assert_eq!(
        parse_marker(&serde_json::to_vec(&original).unwrap(), &request, now)
            .unwrap()
            .root(),
        bundle().root()
    );
    for (key, value) in [
        ("revision", json!(2)),
        ("status", json!("completed")),
        ("source_sha256", json!("b".repeat(64))),
        (
            "preparer_image_digest",
            json!(format!("sha256:{}", "b".repeat(64))),
        ),
        ("policy_revision", json!("changed")),
        ("timeout_seconds", json!(31)),
        ("started_unix_ms", json!(0)),
        ("started_unix_ms", json!(now + 1)),
        ("deadline_unix_ms", json!(now)),
        ("deadline_unix_ms", json!(i64::MAX)),
        ("bundle", json!(null)),
        ("unknown", json!(true)),
    ] {
        let mut changed = original.clone();
        changed[key] = value;
        assert!(
            parse_marker(&serde_json::to_vec(&changed).unwrap(), &request, now).is_err(),
            "{key}"
        );
    }
    let mut changed = original.clone();
    changed["bundle"]["digest"] = json!("c".repeat(64));
    assert!(parse_marker(&serde_json::to_vec(&changed).unwrap(), &request, now).is_err());
    let duplicate = serde_json::to_string(&original)
        .unwrap()
        .replacen('{', "{\"revision\":1,", 1);
    assert!(parse_marker(duplicate.as_bytes(), &request, now).is_err());
    let nested_duplicate = serde_json::to_string(&original)
        .unwrap()
        .replace("\"runtime\":", "\"revision\":1,\"runtime\":");
    assert!(parse_marker(nested_duplicate.as_bytes(), &request, now).is_err());
    assert!(parse_marker(&vec![b' '; MARKER_LIMIT + 1], &request, now).is_err());
}

#[test]
fn trusted_manifest_carries_source_only_as_data_and_matches_the_selected_runtime() {
    let request = job();
    assert!(request.matches_runtime(&image(), POLICY));
    assert!(request.matches_runtime(&format!("registry.example/preparer@{}", image()), POLICY));
    assert!(!request.matches_runtime(&format!("sha256:{}", "b".repeat(64)), POLICY));
    assert!(!request.matches_runtime(&image(), "changed"));
    assert!(request.within_timeout(Duration::from_secs(30)));
    assert!(!request.within_timeout(Duration::from_secs(29)));
    let manifest = request.manifest().unwrap();
    let ManifestEntry::File { path, content } = &manifest.entries[0] else {
        panic!("Prepared request file")
    };
    assert_eq!(path, ".elitea-code.json");
    assert_eq!(content, &request.to_transport().unwrap());
    let ManifestEntry::File { path, content } = &manifest.entries[1] else {
        panic!("Trusted runner file")
    };
    assert_eq!(path, ".elitea-job.json");
    let runner: Value = serde_json::from_slice(content).unwrap();
    assert_eq!(runner["argv"][0], "/usr/local/bin/deno");
    assert_eq!(
        runner["argv"].as_array().unwrap().last().unwrap(),
        "/opt/elitea-code/python_preparation_job.mjs"
    );
    assert_eq!(runner["timeout_seconds"], 30);
    assert!(
        !String::from_utf8(content.clone())
            .unwrap()
            .contains(request.source())
    );
}

#[tokio::test]
async fn preparation_and_execution_admission_are_mutually_exclusive() {
    let pool = PgPoolOptions::new()
        .connect_lazy("postgres://fixture@127.0.0.1:1/fixture")
        .unwrap();
    let request = job();
    let runtime = Runtime::new(&request, true);
    let preparer = supervisor(pool.clone(), runtime.clone());
    assert!(
        preparer
            .with_admission_policy(POLICY.into(), vec![Language::Python])
            .is_err()
    );
    let executor = DockerSupervisor::new(
        JobLedger::new(pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_admission_policy(POLICY.into(), vec![Language::Python])
    .unwrap();
    assert!(executor.with_preparation_policy(POLICY.into()).is_err());
    for invalid in ["", "space separated", &"x".repeat(129)] {
        assert!(
            DockerSupervisor::new(
                JobLedger::new(pool.clone()),
                runtime.clone(),
                OWNER.into(),
                1
            )
            .unwrap()
            .with_preparation_policy(invalid.into())
            .is_err()
        );
    }
    let unconfigured =
        DockerSupervisor::new(JobLedger::new(pool), runtime, OWNER.into(), 1).unwrap();
    assert!(matches!(
        unconfigured
            .prepare_authorized(&authority(&request), &request)
            .await,
        Err(SupervisorError::Invalid)
    ));
    let mut wrong = serde_json::from_slice::<Value>(&request.to_transport().unwrap()).unwrap();
    wrong["policy_revision"] = json!("wrong");
    let wrong = PreparationJob::from_transport(&serde_json::to_vec(&wrong).unwrap()).unwrap();
    let preparer = supervisor(
        PgPoolOptions::new()
            .connect_lazy("postgres://fixture@127.0.0.1:1/fixture")
            .unwrap(),
        Runtime::new(&wrong, true),
    );
    assert!(matches!(
        preparer
            .prepare_authorized(&authority(&wrong), &wrong)
            .await,
        Err(SupervisorError::Invalid)
    ));
}

#[tokio::test]
async fn root_bound_execution_requires_content_authority_before_ledger_admission() {
    let preparation = job();
    let request = PreparedJob::new(
        Language::Python,
        "print('executed')".into(),
        std::collections::BTreeMap::new(),
        image(),
        POLICY.into(),
        30,
    )
    .unwrap()
    .with_python_dependency_bundle(bundle().root().into())
    .unwrap();
    let (verifier, grant) = signed_digest(request.fingerprint().unwrap(), false);
    let auth = verifier
        .verify(
            &grant,
            "worker",
            &request,
            chrono::Utc::now().timestamp_millis(),
        )
        .unwrap();
    let pool = PgPoolOptions::new()
        .connect_lazy("postgres://fixture@127.0.0.1:1/fixture")
        .unwrap();
    let executor = DockerSupervisor::new(
        JobLedger::new(pool),
        Runtime::new(&preparation, true),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_admission_policy(POLICY.into(), vec![Language::Python])
    .unwrap();
    assert!(matches!(
        executor.submit_authorized(&auth, &request).await,
        Err(SupervisorError::Invalid)
    ));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn preparation_receipt_replacement_preserves_root_and_holding_files() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    let root = pending(first.prepare_authorized(&auth, &request).await.unwrap());
    let ledger = JobLedger::new(database.pool.clone());
    assert_eq!(
        ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    assert_eq!(
        ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .unwrap()
            .root(),
        root.root()
    );
    {
        let state = runtime.state.lock().unwrap();
        assert!(state.running);
        assert_eq!(
            (
                state.prepares,
                state.dispatches,
                state.terminations,
                state.cleanups
            ),
            (1, 1, 0, 0)
        );
    }
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    assert_eq!(
        pending(
            replacement
                .prepare_authorized(&authority(&request), &request)
                .await
                .unwrap()
        )
        .root(),
        root.root()
    );
    assert_eq!(runtime.state.lock().unwrap().prepares, 1);
    assert_eq!(runtime.state.lock().unwrap().dispatches, 1);
    let changed =
        PreparationJob::new("print('changed')".into(), image(), POLICY.into(), 30).unwrap();
    assert!(matches!(
        replacement
            .prepare_authorized(&authority(&changed), &changed)
            .await,
        Err(SupervisorError::Ledger(LedgerError::Conflict))
    ));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn preparation_lease_handoff_fences_old_owners_and_preserves_the_record() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let ledger = JobLedger::new(database.pool.clone());
    ledger.reserve(auth.scope()).await.unwrap();
    let first = ledger
        .claim(auth.scope(), OWNER.into(), 60)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&first).await.unwrap();
    ledger
        .record_preparation_bundle(&first, &bundle())
        .await
        .unwrap();
    ledger.release(&first).await.unwrap();
    assert!(matches!(
        ledger.renew(&first, 60).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger.record_preparation_bundle(&first, &bundle()).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger
            .finish(&first, Phase::Completed, Some("{}"), None)
            .await,
        Err(LedgerError::Fenced)
    ));
    let next = ledger
        .claim(auth.scope(), OWNER.into(), 60)
        .await
        .unwrap()
        .unwrap();
    ledger
        .record_preparation_bundle(&next, &bundle())
        .await
        .unwrap();
    assert!(matches!(
        ledger.release(&first).await,
        Err(LedgerError::Fenced)
    ));
    ledger.release(&next).await.unwrap();
    assert_eq!(
        ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .unwrap()
            .root(),
        bundle().root()
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn active_publication_lease_cannot_acknowledge_an_unpublished_index() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    pending(first.prepare_authorized(&auth, &request).await.unwrap());
    let held = first.claim_indexed_transfer(auth.scope()).await.unwrap();
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        replacement.claim_indexed_transfer(auth.scope()).await,
        Err(SupervisorError::Ledger(LedgerError::Fenced))
    ));
    let record = first.ledger.read(auth.scope()).await.unwrap();
    assert_eq!(record.phase, Phase::Dispatched);
    assert!(record.result_json.is_none());
    assert_eq!(runtime.state.lock().unwrap().dispatches, 1);
    first.ledger.release(&held).await.unwrap();
    let next = replacement
        .claim_indexed_transfer(auth.scope())
        .await
        .unwrap();
    replacement.ledger.release(&next).await.unwrap();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn preparation_stop_waits_for_confirmed_termination_and_never_resolves_again() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    pending(first.prepare_authorized(&auth, &request).await.unwrap());
    runtime.state.lock().unwrap().termination_error = true;
    assert!(matches!(
        first.cancel_authorized(&cancellation(&request)).await,
        Err(SupervisorError::Runtime(_))
    ));
    let ledger = JobLedger::new(database.pool.clone());
    assert!(ledger.cancellation_requested(auth.scope()).await.unwrap());
    assert_eq!(
        ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    runtime.state.lock().unwrap().termination_error = false;
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    let outcome = replacement
        .cancel_authorized(&cancellation(&request))
        .await
        .unwrap();
    assert!(matches!(
        outcome,
        Reconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Cancelled,
                ..
            },
            ..
        }
    ));
    assert_eq!(runtime.state.lock().unwrap().prepares, 1);
    assert!(
        ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_some()
    );
    let prepared = replacement
        .prepare_authorized(&authority(&request), &request)
        .await
        .unwrap();
    assert!(matches!(
        prepared,
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Cancelled,
                ..
            },
            ..
        }
    ));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn stopped_preparation_cannot_start_native_resolution() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    JobLedger::new(database.pool.clone())
        .request_cancellation(auth.scope(), OWNER)
        .await
        .unwrap();
    let outcome = first.prepare_authorized(&auth, &request).await.unwrap();
    assert!(matches!(
        outcome,
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Cancelled,
                ..
            },
            ..
        }
    ));
    assert_eq!(runtime.state.lock().unwrap().prepares, 0);
    assert_eq!(runtime.state.lock().unwrap().dispatches, 0);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn lost_original_preparer_cannot_be_recreated_after_dispatch() {
    let database = database().await;
    let request = job();
    let runtime = Runtime::new(&request, false);
    let first = supervisor(database.pool.clone(), runtime.clone());
    let auth = authority(&request);
    let mut running = Box::pin(first.prepare_authorized(&auth, &request));
    tokio::time::timeout(Duration::from_secs(5), async {
        tokio::select! {
            result = &mut running => panic!("Unresolved preparation returned: {}",result.is_ok()),
            () = runtime.dispatched.notified() => {},
        }
    })
    .await
    .expect("The original preparation must dispatch within its fixture deadline");
    drop(running);
    runtime.state.lock().unwrap().present = false;
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second'",
    )
    .execute(&database.pool)
    .await
    .unwrap();
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        replacement
            .prepare_authorized(&authority(&request), &request)
            .await,
        Err(SupervisorError::Runtime(_))
    ));
    assert_eq!(runtime.state.lock().unwrap().prepares, 1);
    assert_eq!(runtime.state.lock().unwrap().dispatches, 1);
    assert_eq!(
        JobLedger::new(database.pool.clone())
            .read(auth.scope())
            .await
            .unwrap()
            .phase,
        Phase::Dispatched
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn publication_completion_requires_original_runtime_termination() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    let resolved = pending(first.prepare_authorized(&auth, &request).await.unwrap());
    let lease = first
        .ledger
        .claim(auth.scope(), OWNER.into(), 60)
        .await
        .unwrap()
        .unwrap();
    let identity = first.bound_identity(auth.scope()).await.unwrap();
    runtime.state.lock().unwrap().termination_error = true;
    // This component check begins after successful shared metadata publication.
    assert!(matches!(
        first
            .complete_preparation(auth.scope(), &lease, &identity, &resolved)
            .await,
        Err(SupervisorError::Runtime(_))
    ));
    assert_eq!(
        first.ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    assert_eq!(runtime.state.lock().unwrap().cleanups, 0);
    runtime.state.lock().unwrap().termination_error = false;
    let completed = first
        .complete_preparation(auth.scope(), &lease, &identity, &resolved)
        .await
        .unwrap();
    let PreparationReconciliation::Terminal {
        record,
        cleanup_pending,
    } = completed
    else {
        panic!("Confirmed publication must complete")
    };
    assert_eq!(record.phase, Phase::Completed);
    assert!(!cleanup_pending);
    assert_eq!(
        record.result_json.unwrap().as_bytes(),
        resolved.record_json()
    );
    assert!(!runtime.state.lock().unwrap().running);
    assert!(matches!(
        first
            .prepare_authorized(&authority(&request), &request)
            .await
            .unwrap(),
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Completed,
                ..
            },
            ..
        }
    ));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn published_bundle_completion_survives_loss_of_original_files() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    let resolved = pending(first.prepare_authorized(&auth, &request).await.unwrap());
    {
        let mut state = runtime.state.lock().unwrap();
        state.present = false;
        state.running = false;
    }
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    let lease = replacement
        .claim_indexed_transfer(auth.scope())
        .await
        .unwrap();
    let identity = replacement.bound_identity(auth.scope()).await.unwrap();
    // The private content client independently confirms committed shared metadata before this transition.
    let completed = replacement
        .complete_preparation(auth.scope(), &lease, &identity, &resolved)
        .await
        .unwrap();
    assert!(matches!(
        completed,
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Completed,
                ..
            },
            ..
        }
    ));
    assert_eq!(runtime.state.lock().unwrap().prepares, 1);
    assert_eq!(runtime.state.lock().unwrap().dispatches, 1);
    assert_eq!(
        replacement
            .ledger
            .read(auth.scope())
            .await
            .unwrap()
            .result_json
            .unwrap()
            .as_bytes(),
        resolved.record_json()
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn indexed_execution_provisioning_remains_reserved_and_retains_its_original_runtime() {
    let database = database().await;
    let preparation = job();
    let runtime = Runtime::new(&preparation, true);
    let request = PreparedJob::new(
        Language::Python,
        "print('executed')".into(),
        std::collections::BTreeMap::new(),
        image(),
        POLICY.into(),
        30,
    )
    .unwrap()
    .with_python_dependency_bundle(bundle().root().into())
    .unwrap();
    let scope = request.scope("tenant".into(), 2, [3; 32]).unwrap();
    let executor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_admission_policy(POLICY.into(), vec![Language::Python])
    .unwrap();
    executor.ledger.reserve(&scope).await.unwrap();
    let first = executor.claim_indexed_transfer(&scope).await.unwrap();
    let InertJob::Ready(identity) = executor
        .provision_inert(&scope, &first, &request.manifest().unwrap())
        .await
        .unwrap()
    else {
        panic!("Indexed hydration must provision an inert runtime")
    };
    assert_eq!(identity.runtime_id(), Some("original-runtime"));
    assert_eq!(
        executor.ledger.read(&scope).await.unwrap().phase,
        Phase::Reserved
    );
    executor.ledger.release(&first).await.unwrap();
    let next = executor.claim_indexed_transfer(&scope).await.unwrap();
    assert!(matches!(
        executor
            .provision_inert(&scope, &next, &request.manifest().unwrap())
            .await
            .unwrap(),
        InertJob::Ready(_)
    ));
    executor.ledger.release(&next).await.unwrap();
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 0);
    assert!(state.running);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS with ca.pem and client-combined.pem"]
#[expect(
    clippy::too_many_lines,
    reason = "Keep final hydration, pre-dispatch refusal, and immutable submission together"
)]
async fn final_hydration_and_submission_verify_execution_files_before_dispatch() {
    use crate::sandbox::docker_supervisor::DependencyDelivery;
    use std::os::unix::fs::PermissionsExt as _;
    let database = database().await;
    let runtime = Runtime::new(&job(), false);
    let resolved = bundle();
    let request = PreparedJob::new(
        Language::Python,
        "result = 7".into(),
        std::collections::BTreeMap::new(),
        image(),
        POLICY.into(),
        30,
    )
    .unwrap()
    .with_python_dependency_bundle(resolved.root().into())
    .unwrap();
    let (verifier, execution_grant) = signed_digest(request.fingerprint().unwrap(), false);
    let mut claims =
        SandboxJobGrantClaimsV1::decode(execution_grant.claims_bytes.as_slice()).unwrap();
    claims.revision = 3;
    claims.dependency_bundle_sha256 = (0..32)
        .map(|index| u8::from_str_radix(&resolved.root()[index * 2..index * 2 + 2], 16).unwrap())
        .collect();
    let claims = claims.encode_to_vec();
    let key = Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
    let mut input = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
    input.extend_from_slice(&(claims.len() as u64).to_be_bytes());
    input.extend_from_slice(&claims);
    let content_grant = SignedSandboxJobGrantV1 {
        key_id: "fixture-key".into(),
        claims_bytes: claims,
        signature: key.sign(&input).as_ref().to_vec(),
    };
    let now = chrono::Utc::now().timestamp_millis();
    let execution_authority = verifier
        .verify(&execution_grant, "worker", &request, now)
        .unwrap();
    let content_authority = verifier
        .verify_content(&content_grant, "worker", now)
        .unwrap();
    let tls = std::path::PathBuf::from(std::env::var("ELITEA_TEST_BUNDLE_TLS").unwrap());
    let staging = tempfile::Builder::new()
        .permissions(std::fs::Permissions::from_mode(0o700))
        .tempdir()
        .unwrap();
    // The final index and submission perform no HTTP transfer.
    let content = DependencyContentClient::new(
        "https://content.invalid:9445",
        &std::fs::read(tls.join("ca.pem")).unwrap(),
        &std::fs::read(tls.join("client-combined.pem")).unwrap(),
        staging.path(),
        1,
        Duration::from_secs(30),
    )
    .unwrap();
    let delivery = DependencyDelivery {
        client: &content,
        grant: &content_grant,
        authorization: &content_authority,
        bundle: &resolved,
    };
    let executor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_admission_policy(POLICY.into(), vec![Language::Python])
    .unwrap();
    // Earlier file indexes imported this lock. No preparation export exists in this runtime.
    runtime
        .state
        .lock()
        .unwrap()
        .execution_files
        .insert("elitea-python-lock.json".into(), b"{]".to_vec());
    let final_index = u32::try_from(resolved.file_count()).unwrap();
    assert!(matches!(
        executor
            .hydrate_authorized(
                &execution_authority,
                &request,
                delivery,
                &resolved,
                final_index
            )
            .await,
        Err(SupervisorError::Content(DependencyContentError::Integrity))
    ));
    assert!(
        !runtime
            .state
            .lock()
            .unwrap()
            .execution_files
            .contains_key("elitea-python-bundle.json")
    );
    assert_eq!(
        executor
            .ledger
            .read(execution_authority.scope())
            .await
            .unwrap()
            .phase,
        Phase::Reserved
    );
    assert_eq!(runtime.state.lock().unwrap().dispatches, 0);
    runtime
        .state
        .lock()
        .unwrap()
        .execution_files
        .insert("elitea-python-lock.json".into(), b"{}".to_vec());
    assert!(
        executor
            .hydrate_authorized(
                &execution_authority,
                &request,
                delivery,
                &resolved,
                final_index
            )
            .await
            .unwrap()
    );
    assert_eq!(
        runtime.state.lock().unwrap().execution_files["elitea-python-bundle.json"],
        resolved.record_json()
    );
    assert_eq!(
        executor
            .ledger
            .read(execution_authority.scope())
            .await
            .unwrap()
            .phase,
        Phase::Reserved
    );
    assert_eq!(runtime.state.lock().unwrap().dispatches, 0);

    let mut changed = resolved.record_json().to_vec();
    changed[0] ^= 1;
    runtime
        .state
        .lock()
        .unwrap()
        .execution_files
        .insert("elitea-python-bundle.json".into(), changed);
    assert!(matches!(
        executor
            .submit_authorized_with_dependencies(&execution_authority, &request, Some(delivery))
            .await,
        Err(SupervisorError::Content(DependencyContentError::Integrity))
    ));
    assert_eq!(runtime.state.lock().unwrap().dispatches, 0);
    assert_eq!(
        executor
            .ledger
            .read(execution_authority.scope())
            .await
            .unwrap()
            .phase,
        Phase::Reserved
    );
    // Expire only this fixture's uncertain submission lease before retry.
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE request_digest=$1")
        .bind(request.fingerprint().unwrap().as_slice()).execute(&database.pool).await.unwrap();
    {
        let mut state = runtime.state.lock().unwrap();
        state.execution_files.insert(
            "elitea-python-bundle.json".into(),
            resolved.record_json().to_vec(),
        );
        state.execution_receipt = Some(
            serde_json::to_vec(&json!({
                "revision":1,"status":"completed","exit_code":0,"stdout":"7","stderr":""
            }))
            .unwrap(),
        );
    }
    for _ in 0..2 {
        assert!(matches!(
            executor
                .submit_authorized_with_dependencies(&execution_authority, &request, Some(delivery))
                .await
                .unwrap(),
            Reconciliation::Terminal {
                record: JobRecord {
                    phase: Phase::Completed,
                    ..
                },
                cleanup_pending: false
            }
        ));
    }
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert!(!state.running);
}

fn invalid_marker_runtime(request: &PreparationJob) -> Runtime {
    let runtime = Runtime::new(request, false);
    runtime.state.lock().unwrap().marker = Some(b"{]".to_vec());
    runtime
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn invalid_native_marker_terminates_original_runtime_without_reresolution() {
    let database = database().await;
    let request =
        PreparationJob::from_transport(include_bytes!("native-deno-preparation-job-v2.json"))
            .unwrap();
    let mut emitted: Value =
        serde_json::from_slice(include_bytes!("native-deno-preparation-marker-v2.json")).unwrap();
    let native_record: crate::sandbox::native_bundle::NativeRecord =
        serde_json::from_value(emitted["bundle"].clone()).unwrap();
    let noncanonical = serde_json::to_string(&native_record).unwrap();
    let now = chrono::Utc::now().timestamp_millis();
    emitted["started_unix_ms"] = json!(now - 1);
    emitted["deadline_unix_ms"] = json!(now - 1 + i64::from(request.timeout_seconds()) * 1000);
    emitted["bundle"] = Value::Null;
    let bytes = serde_json::to_string(&emitted)
        .unwrap()
        .replace("\"bundle\":null", &format!("\"bundle\":{noncanonical}"))
        .into_bytes();
    assert!(matches!(
        parse_marker(&bytes, &request, now),
        Err(SupervisorError::Receipt)
    ));
    let runtime = Runtime::new(&request, false);
    runtime.state.lock().unwrap().marker = Some(bytes);
    let first = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_preparation_policy(request.policy_revision().into())
    .unwrap()
    .with_native_profile(
        request.platform().unwrap().clone(),
        vec![request.language()],
    )
    .unwrap();
    let auth = authority(&request);
    let mut previous_clocks = None;
    for _ in 0..2 {
        let PreparationReconciliation::Terminal {
            record,
            cleanup_pending,
        } = first.prepare_authorized(&auth, &request).await.unwrap()
        else {
            panic!("An invalid immutable marker must be terminal after termination")
        };
        assert_eq!(record.phase, Phase::Failed);
        assert_eq!(
            record.failure_code.as_deref(),
            Some("sandbox.preparation_invalid_receipt")
        );
        assert_eq!(record.runtime_id.as_deref(), Some("original-runtime"));
        assert!(record.result_json.is_none());
        assert!(!cleanup_pending);
        assert!(
            first
                .ledger
                .read_preparation_bundle(auth.scope())
                .await
                .unwrap()
                .is_none()
        );
        let clocks: (chrono::DateTime<chrono::Utc>, chrono::DateTime<chrono::Utc>) =
            sqlx::query_as("SELECT runtime_bound_at,dispatched_at FROM elitea_runtime.sandbox_jobs WHERE request_digest=$1")
                .bind(request.fingerprint().unwrap().as_slice())
                .fetch_one(&database.pool)
                .await
                .unwrap();
        assert!(clocks.0 <= clocks.1);
        if let Some(previous) = previous_clocks {
            assert_eq!(clocks, previous);
        } else {
            previous_clocks = Some(clocks);
        }
    }
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert_eq!(state.terminations, 1);
    assert_eq!(
        state.termination_runtime_ids,
        [Some("original-runtime".into())]
    );
    assert!(
        state
            .cleanup_runtime_ids
            .iter()
            .all(|id| id.as_deref() == Some("original-runtime"))
    );
    assert!(!state.present);
    assert!(!state.running);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn invalid_marker_termination_failure_retains_original_job_for_reconciliation() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = invalid_marker_runtime(&request);
    runtime.state.lock().unwrap().termination_error = true;
    let first = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        first.prepare_authorized(&auth, &request).await,
        Err(SupervisorError::Runtime(_))
    ));
    let record = first.ledger.read(auth.scope()).await.unwrap();
    assert_eq!(record.phase, Phase::Dispatched);
    assert!(record.failure_code.is_none());
    assert!(record.result_json.is_none());
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    {
        let mut state = runtime.state.lock().unwrap();
        assert!(state.present && state.running);
        assert_eq!(state.cleanups, 0);
        assert_eq!(state.terminations, 0);
        state.termination_error = false;
    }
    let replacement = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        replacement
            .prepare_authorized(&auth, &request)
            .await
            .unwrap(),
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Failed,
                ..
            },
            cleanup_pending: false,
        }
    ));
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert_eq!(state.terminations, 1);
    assert_eq!(
        state.termination_runtime_ids,
        [
            Some("original-runtime".into()),
            Some("original-runtime".into())
        ]
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn invalid_marker_fenced_lease_cannot_terminate_original_runtime() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = invalid_marker_runtime(&request);
    runtime.state.lock().unwrap().termination_error = true;
    let first = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        first.prepare_authorized(&auth, &request).await,
        Err(SupervisorError::Runtime(_))
    ));
    let expired_lease = first
        .ledger
        .claim(auth.scope(), OWNER.into(), 60)
        .await
        .unwrap()
        .unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE request_digest=$1")
        .bind(request.fingerprint().unwrap().as_slice()).execute(&database.pool).await.unwrap();
    let current = first
        .ledger
        .claim(auth.scope(), "replacement-owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    runtime.state.lock().unwrap().termination_error = false;
    assert!(matches!(
        first
            .observe_preparer(auth.scope(), &expired_lease, &request)
            .await,
        Err(SupervisorError::Ledger(LedgerError::Fenced))
    ));
    assert_eq!(
        first.ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    first.ledger.renew(&current, 60).await.unwrap();
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.termination_runtime_ids.len(), 1);
    assert_eq!(state.terminations, 0);
    assert_eq!(state.cleanups, 0);
    assert!(state.running);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn invalid_marker_fenced_during_termination_cannot_write_failed_receipt() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = invalid_marker_runtime(&request);
    runtime.state.lock().unwrap().termination_pending = true;
    let first = supervisor(database.pool.clone(), runtime.clone());
    let mut running = Box::pin(first.prepare_authorized(&auth, &request));
    tokio::time::timeout(Duration::from_secs(5), async {
        tokio::select! {
            result = &mut running => panic!("Termination must remain pending: {}", result.is_ok()),
            () = runtime.termination_started.notified() => {},
        }
    })
    .await
    .expect("Invalid marker must reach original runtime termination");
    assert_eq!(
        first.ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE request_digest=$1")
        .bind(request.fingerprint().unwrap().as_slice()).execute(&database.pool).await.unwrap();
    let current = first
        .ledger
        .claim(auth.scope(), "replacement-owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    runtime.termination_released.notify_one();
    assert!(matches!(
        tokio::time::timeout(Duration::from_secs(5), running)
            .await
            .unwrap(),
        Err(SupervisorError::Ledger(LedgerError::Fenced))
    ));
    let record = first.ledger.read(auth.scope()).await.unwrap();
    assert_eq!(record.phase, Phase::Dispatched);
    assert!(record.failure_code.is_none());
    assert!(record.result_json.is_none());
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    first.ledger.renew(&current, 60).await.unwrap();
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.terminations, 1);
    assert_eq!(state.cleanups, 0);
    assert_eq!(
        state.termination_runtime_ids,
        [Some("original-runtime".into())]
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn invalid_marker_concurrent_cancellation_writes_cancelled_after_termination() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = invalid_marker_runtime(&request);
    runtime.state.lock().unwrap().termination_pending = true;
    let first = supervisor(database.pool.clone(), runtime.clone());
    let mut running = Box::pin(first.prepare_authorized(&auth, &request));
    tokio::time::timeout(Duration::from_secs(5), async {
        tokio::select! {
            result = &mut running => panic!("Termination must remain pending: {}", result.is_ok()),
            () = runtime.termination_started.notified() => {},
        }
    })
    .await
    .expect("Invalid marker must reach original runtime termination");
    first
        .ledger
        .request_cancellation(auth.scope(), OWNER)
        .await
        .unwrap();
    assert_eq!(
        first.ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    runtime.termination_released.notify_one();
    let PreparationReconciliation::Terminal {
        record,
        cleanup_pending,
    } = tokio::time::timeout(Duration::from_secs(5), running)
        .await
        .unwrap()
        .unwrap()
    else {
        panic!("Concurrent stop must become terminal after confirmed termination")
    };
    assert_eq!(record.phase, Phase::Cancelled);
    assert_eq!(record.failure_code.as_deref(), Some("sandbox.cancelled"));
    assert!(!cleanup_pending);
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        first.prepare_authorized(&auth, &request).await.unwrap(),
        PreparationReconciliation::Terminal {
            record: JobRecord {
                phase: Phase::Cancelled,
                ..
            },
            ..
        }
    ));
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert_eq!(state.terminations, 1);
    assert_eq!(
        state.termination_runtime_ids,
        [Some("original-runtime".into())]
    );
    assert!(!state.present);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn marker_observation_error_retains_preparation_for_same_runtime_reconciliation() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    runtime.state.lock().unwrap().marker_observation_error = true;
    let first = supervisor(database.pool.clone(), runtime.clone());
    assert!(matches!(
        first.prepare_authorized(&auth, &request).await,
        Err(SupervisorError::Runtime(_))
    ));
    assert_eq!(
        first.ledger.read(auth.scope()).await.unwrap().phase,
        Phase::Dispatched
    );
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    runtime.state.lock().unwrap().marker_observation_error = false;
    pending(first.prepare_authorized(&auth, &request).await.unwrap());
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert_eq!(state.terminations, 0);
    assert_eq!(state.cleanups, 0);
    assert!(state.present && state.running);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn preparation_bundle_persistence_error_retains_original_runtime_for_reconciliation() {
    let database = database().await;
    let request = job();
    let auth = authority(&request);
    let runtime = Runtime::new(&request, true);
    let first = supervisor(database.pool.clone(), runtime.clone());
    sqlx::raw_sql("CREATE FUNCTION elitea_runtime.reject_preparation_bundle() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.preparation_bundle_json IS NOT NULL THEN RAISE EXCEPTION 'Injected preparation persistence failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_preparation_bundle BEFORE UPDATE ON elitea_runtime.sandbox_jobs FOR EACH ROW EXECUTE FUNCTION elitea_runtime.reject_preparation_bundle()")
        .execute(&database.pool).await.unwrap();
    assert!(matches!(
        first.prepare_authorized(&auth, &request).await,
        Err(SupervisorError::Ledger(_))
    ));
    let record = first.ledger.read(auth.scope()).await.unwrap();
    assert_eq!(record.phase, Phase::Dispatched);
    assert!(record.failure_code.is_none());
    assert!(
        first
            .ledger
            .read_preparation_bundle(auth.scope())
            .await
            .unwrap()
            .is_none()
    );
    sqlx::raw_sql("DROP TRIGGER reject_preparation_bundle ON elitea_runtime.sandbox_jobs; DROP FUNCTION elitea_runtime.reject_preparation_bundle()")
        .execute(&database.pool).await.unwrap();
    pending(first.prepare_authorized(&auth, &request).await.unwrap());
    let state = runtime.state.lock().unwrap();
    assert_eq!(state.prepares, 1);
    assert_eq!(state.dispatches, 1);
    assert_eq!(state.terminations, 0);
    assert_eq!(state.cleanups, 0);
    assert!(state.present && state.running);
}

#[tokio::test]
async fn frozen_lookup_succeeds_while_execution_capacity_is_exhausted() {
    let pool = PgPoolOptions::new()
        .connect_lazy("postgres://fixture@127.0.0.1:1/fixture")
        .unwrap();
    let request = job();
    let supervisor = DockerSupervisor::new(
        JobLedger::new(pool),
        Runtime::new(&request, true),
        OWNER.into(),
        1,
    )
    .unwrap();
    let _execution = supervisor.capacity.try_acquire().unwrap();
    assert!(supervisor.capacity.try_acquire().is_err());
    // A saturated execution slot does not refuse the metadata lookup...
    let mut held = Vec::new();
    for _ in 0..super::super::FROZEN_LOOKUP_CONCURRENCY {
        held.push(supervisor.admit_frozen_lookup().unwrap());
    }
    // ...and lookups remain bounded by their own limit.
    assert!(matches!(
        supervisor.admit_frozen_lookup(),
        Err(SupervisorError::Busy)
    ));
    held.pop();
    assert!(supervisor.admit_frozen_lookup().is_ok());
}
