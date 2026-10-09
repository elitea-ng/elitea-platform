//! `PostgreSQL` and runtime-double checks; these are not deployed runtime acceptance.
use super::super::{MAX_HYDRATION_AGE_SECONDS, Reconciliation};
use super::*;
use crate::{
    protocol::{
        command::Ed25519PublicKeyResolver,
        elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
        sandbox_grant::{AuthorizedContent, GrantVerifier},
    },
    sandbox::{
        dependency_bundle::{PythonDependencyBundle, hex},
        dependency_content::DependencyContentClient,
        ledger::{JobLedger, JobRecord, LedgerError},
        request::Language,
        runtime::CodeJobRuntime,
    },
    state::postgres_session_tests::IsolatedPostgres,
};
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, docker::CodeJobIdentity},
};
use prost::Message as _;
use ring::signature::{Ed25519KeyPair, KeyPair as _};
use sqlx::PgPool;
use std::{
    collections::BTreeMap,
    os::unix::fs::PermissionsExt as _,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};
use tokio::{
    io::{AsyncReadExt as _, AsyncWriteExt as _},
    sync::Notify,
};

const OWNER: &str = "hydration-owner";
const RUNTIME_ID: &str = "original-runtime";
const POLICY: &str = "hydration-test-v1";
const WAIT: Duration = Duration::from_secs(25);

async fn database() -> IsolatedPostgres {
    let database = IsolatedPostgres::create(
        &std::env::var("ELITEA_TEST_DATABASE_URL").expect("ELITEA_TEST_DATABASE_URL is required"),
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

fn request() -> PreparedJob {
    PreparedJob::new(
        Language::Python,
        "print(42)".into(),
        BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        POLICY.into(),
        30,
    )
    .unwrap()
}

fn scope(request: &PreparedJob, key: u8) -> JobScope {
    request
        .scope("hydration-tenant".into(), 2, [key; 32])
        .unwrap()
}

fn bundle() -> DependencyBundle {
    let content = format!(
        r#"{{"revision":1,"runtime":"pyodide-0.29.0","requirements":[],"files":[{{"name":"elitea-python-lock.json","bytes":2,"sha256":"{}"}}]}}"#,
        hex(ring::digest::digest(&ring::digest::SHA256, b"{}").as_ref())
    );
    let root = hex(ring::digest::digest(&ring::digest::SHA256, content.as_bytes()).as_ref());
    let record = format!("{},\"digest\":\"{root}\"}}", &content[..content.len() - 1]);
    DependencyBundle::PythonV1(PythonDependencyBundle::parse_record(record.as_bytes()).unwrap())
}

async fn age(pool: &PgPool, seconds: i32) {
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_bound_at=clock_timestamp()-make_interval(secs => $1)")
        .bind(f64::from(seconds)).execute(pool).await.unwrap();
}

async fn expire_leases(pool: &PgPool) {
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second'",
    )
    .execute(pool)
    .await
    .unwrap();
}

async fn bound(supervisor: &DockerSupervisor, scope: &JobScope) -> JobLease {
    supervisor.ledger.reserve(scope).await.unwrap();
    let lease = supervisor
        .ledger
        .claim(scope, supervisor.owner.clone(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    supervisor
        .ledger
        .bind_runtime(&lease, RUNTIME_ID)
        .await
        .unwrap();
    lease
}

fn record(outcome: Reconciliation) -> JobRecord {
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = outcome
    else {
        panic!("original allocation must become terminal")
    };
    assert!(!cleanup_pending);
    assert_eq!(record.runtime_id.as_deref(), Some(RUNTIME_ID));
    assert!(record.result_json.is_none());
    record
}

fn expired(outcome: Reconciliation) {
    let record = record(outcome);
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.hydration_deadline_exceeded")
    );
}

#[derive(Clone)]
struct Runtime(Arc<RuntimeState>);
struct RuntimeState {
    image: String,
    exists: AtomicBool,
    prepares: AtomicUsize,
    dispatches: AtomicUsize,
    exports: AtomicUsize,
    imports: AtomicUsize,
    terminations: AtomicUsize,
    cleanups: AtomicUsize,
    files: Mutex<BTreeMap<String, Vec<u8>>>,
    block_export: AtomicBool,
    export_started: Notify,
    export_release: Notify,
    block_termination: AtomicBool,
    termination_fails: AtomicBool,
    termination_started: Notify,
    termination_release: Notify,
}
impl Runtime {
    fn new(present: bool) -> Self {
        Self(Arc::new(RuntimeState {
            image: format!("sha256:{}", "a".repeat(64)),
            exists: AtomicBool::new(present),
            prepares: AtomicUsize::new(0),
            dispatches: AtomicUsize::new(0),
            exports: AtomicUsize::new(0),
            imports: AtomicUsize::new(0),
            terminations: AtomicUsize::new(0),
            cleanups: AtomicUsize::new(0),
            files: Mutex::new(BTreeMap::from([(
                "elitea-python-lock.json".into(),
                b"{}".to_vec(),
            )])),
            block_export: AtomicBool::new(false),
            export_started: Notify::new(),
            export_release: Notify::new(),
            block_termination: AtomicBool::new(false),
            termination_fails: AtomicBool::new(false),
            termination_started: Notify::new(),
            termination_release: Notify::new(),
        }))
    }
    fn check(identity: &CodeJobIdentity) {
        assert_eq!(identity.runtime_id(), Some(RUNTIME_ID));
    }
    fn assert_expired(&self) {
        assert_eq!(self.0.prepares.load(Ordering::SeqCst), 0);
        assert_eq!(self.0.dispatches.load(Ordering::SeqCst), 0);
        assert_eq!(self.0.terminations.load(Ordering::SeqCst), 1);
        assert_eq!(self.0.cleanups.load(Ordering::SeqCst), 1);
    }
}
#[async_trait::async_trait]
impl CodeJobRuntime for Runtime {
    fn image_digest(&self) -> &str {
        &self.0.image
    }
    fn code_compilation_enabled(&self) -> bool {
        false
    }
    fn code_job_timeout(&self) -> Duration {
        Duration::from_secs(30)
    }
    async fn instance(&self, _identity: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        Ok(self
            .0
            .exists
            .load(Ordering::SeqCst)
            .then(|| RUNTIME_ID.into()))
    }
    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        if identity.runtime_id().is_some() {
            Self::check(identity);
        }
        Ok(self.0.exists.load(Ordering::SeqCst))
    }
    async fn prepare(
        &self,
        identity: &CodeJobIdentity,
        _manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        assert!(identity.runtime_id().is_none());
        assert!(
            !self.0.exists.swap(true, Ordering::SeqCst),
            "never replace an original runtime"
        );
        self.0.prepares.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        Self::check(identity);
        Ok(true)
    }
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Self::check(identity);
        self.0.dispatches.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        Self::check(identity);
        Ok(Some(br#"{"status":"completed"}"#.to_vec()))
    }
    async fn export_execution_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Self::check(identity);
        self.0.exports.fetch_add(1, Ordering::SeqCst);
        self.0.export_started.notify_one();
        if self.0.block_export.load(Ordering::SeqCst) {
            self.0.export_release.notified().await;
        }
        let content = self
            .0
            .files
            .lock()
            .unwrap()
            .get(name)
            .cloned()
            .expect("fixture execution content exists");
        assert_eq!(content.len() as u64, bytes);
        writer
            .write_all(&content)
            .await
            .map_err(|error| SandboxError::ExecutionFailed(error.to_string()))
    }
    async fn import_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Self::check(identity);
        self.0.imports.fetch_add(1, Ordering::SeqCst);
        let mut content = Vec::new();
        reader.read_to_end(&mut content).await.unwrap();
        assert_eq!(content.len() as u64, bytes);
        self.0.files.lock().unwrap().insert(name.into(), content);
        Ok(())
    }
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Self::check(identity);
        self.0.terminations.fetch_add(1, Ordering::SeqCst);
        self.0.termination_started.notify_one();
        if self.0.block_termination.load(Ordering::SeqCst) {
            self.0.termination_release.notified().await;
        }
        if self.0.termination_fails.load(Ordering::SeqCst) {
            return Err(SandboxError::ExecutionFailed(
                "Original runtime termination remains unconfirmed".into(),
            ));
        }
        Ok(())
    }
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Self::check(identity);
        self.0.cleanups.fetch_add(1, Ordering::SeqCst);
        self.0.exists.store(false, Ordering::SeqCst);
        Ok(())
    }
}
fn supervisor(pool: PgPool, runtime: Runtime, owner: &str) -> DockerSupervisor {
    DockerSupervisor::new(JobLedger::new(pool), runtime, owner.into(), 1)
        .unwrap()
        .with_admission_policy(POLICY.into(), vec![Language::Python])
        .unwrap()
}

struct Keys([u8; 32]);
impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
        (id == "fixture-key").then_some(self.0)
    }
}
struct DeliveryFixture {
    _staging: tempfile::TempDir,
    client: DependencyContentClient,
    grant: SignedSandboxJobGrantV1,
    authority: AuthorizedContent,
    execution: AuthorizedJob,
}
impl DeliveryFixture {
    fn new(request: &PreparedJob, bundle: &DependencyBundle) -> Self {
        let key = Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
        let now = chrono::Utc::now().timestamp_millis();
        let mut claims = SandboxJobGrantClaimsV1 {
            revision: 1,
            cancel_only: false,
            tenant_id: "hydration-tenant".into(),
            project_id: 2,
            execution_id: "execution".into(),
            activation_id: "hydration-1".into(),
            request_digest: request.fingerprint().unwrap().to_vec(),
            submitter_workload_identity: "worker".into(),
            audience: "execution".into(),
            issued_at_unix_millis: now,
            expires_at_unix_millis: now + 30_000,
            generation: 1,
            dependency_bundle_sha256: vec![],
        };
        let sign = |claims: &SandboxJobGrantClaimsV1| {
            let bytes = claims.encode_to_vec();
            let mut input = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
            input.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
            input.extend_from_slice(&bytes);
            SignedSandboxJobGrantV1 {
                key_id: "fixture-key".into(),
                claims_bytes: bytes,
                signature: key.sign(&input).as_ref().to_vec(),
            }
        };
        let verifier = GrantVerifier::new(
            Keys(key.public_key().as_ref().try_into().unwrap()),
            "execution".into(),
        )
        .unwrap();
        let execution = verifier
            .verify(&sign(&claims), "worker", request, now)
            .unwrap();
        claims.revision = 3;
        claims.dependency_bundle_sha256 = (0..32)
            .map(|i| u8::from_str_radix(&bundle.root()[i * 2..i * 2 + 2], 16).unwrap())
            .collect();
        let grant = sign(&claims);
        let authority = verifier.verify_content(&grant, "worker", now).unwrap();
        let tls = std::path::PathBuf::from(
            std::env::var("ELITEA_TEST_BUNDLE_TLS").expect("ELITEA_TEST_BUNDLE_TLS is required"),
        );
        let staging = tempfile::Builder::new()
            .permissions(std::fs::Permissions::from_mode(0o700))
            .tempdir()
            .unwrap();
        let client = DependencyContentClient::new(
            "https://content.invalid:9445",
            &std::fs::read(tls.join("ca.pem")).unwrap(),
            &std::fs::read(tls.join("client-combined.pem")).unwrap(),
            staging.path(),
            1,
            Duration::from_secs(30),
        )
        .unwrap();
        Self {
            _staging: staging,
            client,
            grant,
            authority,
            execution,
        }
    }
    fn delivery<'a>(&'a self, bundle: &'a DependencyBundle) -> DependencyDelivery<'a> {
        DependencyDelivery {
            client: &self.client,
            grant: &self.grant,
            authorization: &self.authority,
            bundle,
        }
    }
}

fn assert_no_hydration_effects(runtime: &Runtime) {
    assert_eq!(runtime.0.prepares.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.imports.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.exports.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.dispatches.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.cleanups.load(Ordering::SeqCst), 0);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_retained_dispatch_is_ready_with_the_only_execution_slot_occupied() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let scope = delivery.execution.scope();
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&supervisor, scope).await;
    supervisor.ledger.mark_dispatched(&lease).await.unwrap();
    let _execution_owner = supervisor.capacity.try_acquire().unwrap();

    assert!(
        supervisor
            .hydrate_authorized(
                &delivery.execution,
                &request,
                delivery.delivery(&bundle),
                &bundle,
                0,
            )
            .await
            .unwrap()
    );
    let retained = supervisor.ledger.read(scope).await.unwrap();
    assert_eq!(retained.phase, Phase::Dispatched);
    assert_eq!(retained.runtime_id.as_deref(), Some(RUNTIME_ID));
    assert!(retained.result_json.is_none());
    assert!(retained.failure_code.is_none());
    assert_eq!(supervisor.capacity.available_permits(), 0);
    assert_no_hydration_effects(&runtime);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_retained_dispatch_keeps_authority_index_and_digest_refusals() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let scope = delivery.execution.scope();
    let conflicting =
        JobScope::new(scope.tenant.clone(), scope.project, scope.key, [0xff; 32]).unwrap();
    let lease = bound(&supervisor, &conflicting).await;
    supervisor.ledger.mark_dispatched(&lease).await.unwrap();
    let _execution_owner = supervisor.capacity.try_acquire().unwrap();

    // These refuse before retained state can acknowledge readiness.
    for index in [u32::try_from(bundle.file_count()).unwrap() + 1, u32::MAX] {
        assert!(matches!(
            supervisor
                .hydrate_authorized(
                    &delivery.execution,
                    &request,
                    delivery.delivery(&bundle),
                    &bundle,
                    index,
                )
                .await,
            Err(SupervisorError::Invalid)
        ));
    }
    let changed = PreparedJob::new(
        Language::Python,
        "print(43)".into(),
        BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        POLICY.into(),
        30,
    )
    .unwrap()
    .with_python_dependency_bundle(bundle.root().into())
    .unwrap();
    assert!(matches!(
        supervisor
            .hydrate_authorized(
                &delivery.execution,
                &changed,
                delivery.delivery(&bundle),
                &bundle,
                0,
            )
            .await,
        Err(SupervisorError::Invalid)
    ));
    assert!(matches!(
        supervisor
            .hydrate_authorized(
                &delivery.execution,
                &request,
                delivery.delivery(&bundle),
                &bundle,
                0,
            )
            .await,
        Err(SupervisorError::Ledger(LedgerError::Conflict))
    ));
    assert_no_hydration_effects(&runtime);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_missing_and_reserved_jobs_remain_capacity_bound() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let runtime = Runtime::new(false);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let _execution_owner = supervisor.capacity.try_acquire().unwrap();
    for reserved in [false, true] {
        if reserved {
            supervisor
                .ledger
                .reserve(delivery.execution.scope())
                .await
                .unwrap();
        }
        assert!(matches!(
            supervisor
                .hydrate_authorized(
                    &delivery.execution,
                    &request,
                    delivery.delivery(&bundle),
                    &bundle,
                    0,
                )
                .await,
            Err(SupervisorError::Busy)
        ));
    }
    assert_no_hydration_effects(&runtime);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_fresh_binding_preserves_old_reservation() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 1);
    let runtime = Runtime::new(false);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    supervisor.ledger.reserve(&scope).await.unwrap();
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET created_at=clock_timestamp()-interval '2 hours'",
    )
    .execute(&database.pool)
    .await
    .unwrap();
    let lease = supervisor
        .ledger
        .claim(&scope, OWNER.into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    assert!(
        supervisor
            .expire_hydration_if_needed(&scope, &lease)
            .await
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        supervisor
            .provision_inert(&scope, &lease, &request.manifest().unwrap())
            .await
            .unwrap(),
        InertJob::Ready(_)
    ));
    assert!(
        supervisor
            .ledger
            .hydration_age_seconds(&scope)
            .await
            .unwrap()
            < 60
    );
    assert!(
        supervisor
            .expire_hydration_if_needed(&scope, &lease)
            .await
            .unwrap()
            .is_none()
    );
    assert_eq!(runtime.0.prepares.load(Ordering::SeqCst), 1);
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_ready_original_expires_before_any_index_transfer() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let scope = delivery.execution.scope();
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, scope).await;
    age(&database.pool, 3691).await;
    expire_leases(&database.pool).await;
    assert!(
        supervisor
            .hydrate_authorized(
                &delivery.execution,
                &request,
                delivery.delivery(&bundle),
                &bundle,
                0
            )
            .await
            .unwrap()
    );
    let result = supervisor.ledger.read(scope).await.unwrap();
    assert_eq!(result.phase, Phase::Failed);
    assert_eq!(
        result.failure_code.as_deref(),
        Some("sandbox.hydration_deadline_exceeded")
    );
    let dispatched: Option<chrono::DateTime<chrono::Utc>> =
        sqlx::query_scalar("SELECT dispatched_at FROM elitea_runtime.sandbox_jobs")
            .fetch_one(&database.pool)
            .await
            .unwrap();
    assert!(dispatched.is_none());
    assert_eq!(runtime.0.imports.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.exports.load(Ordering::SeqCst), 0);
    runtime.assert_expired();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_binding_age_survives_takeover_and_legacy_null_clock() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 3);
    let runtime = Runtime::new(true);
    let first = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&first, &scope).await;
    age(&database.pool, 3691).await;
    let original: chrono::DateTime<chrono::Utc> =
        sqlx::query_scalar("SELECT runtime_bound_at FROM elitea_runtime.sandbox_jobs")
            .fetch_one(&database.pool)
            .await
            .unwrap();
    first.ledger.renew(&lease, LEASE_SECONDS).await.unwrap();
    expire_leases(&database.pool).await;
    let replacement = supervisor(database.pool.clone(), runtime.clone(), "replacement-owner");
    let lease = replacement
        .ledger
        .claim(&scope, "replacement-owner".into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    let retained: chrono::DateTime<chrono::Utc> =
        sqlx::query_scalar("SELECT runtime_bound_at FROM elitea_runtime.sandbox_jobs")
            .fetch_one(&database.pool)
            .await
            .unwrap();
    assert_eq!(original, retained);
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_bound_at=NULL,created_at=$1")
        .bind(original)
        .execute(&database.pool)
        .await
        .unwrap();
    assert!(
        replacement
            .ledger
            .hydration_age_seconds(&scope)
            .await
            .unwrap()
            >= MAX_HYDRATION_AGE_SECONDS
    );
    expired(
        replacement
            .expire_hydration_if_needed(&scope, &lease)
            .await
            .unwrap()
            .unwrap(),
    );
    runtime.assert_expired();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_pre_dispatch_proof_cannot_outlive_retention() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let scope = delivery.execution.scope();
    let runtime = Runtime::new(true);
    runtime.0.files.lock().unwrap().insert(
        "elitea-python-bundle.json".into(),
        bundle.record_json().to_vec(),
    );
    runtime.0.block_export.store(true, Ordering::SeqCst);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&supervisor, scope).await;
    let progress = async {
        runtime.0.export_started.notified().await;
        age(&database.pool, 3691).await;
        runtime.0.export_release.notify_one();
    };
    let (result, ()) = tokio::time::timeout(WAIT, async {
        tokio::join!(
            supervisor.run_owned(
                scope,
                &lease,
                Some(&request),
                Some(delivery.delivery(&bundle))
            ),
            progress
        )
    })
    .await
    .unwrap();
    expired(result.unwrap());
    runtime.assert_expired();
    assert_eq!(runtime.0.exports.load(Ordering::SeqCst), 1);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS"]
async fn hydration_final_index_checks_expiry_after_metadata_import() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let scope = delivery.execution.scope();
    let runtime = Runtime::new(true);
    runtime.0.block_export.store(true, Ordering::SeqCst);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, scope).await;
    expire_leases(&database.pool).await;
    let progress = async {
        runtime.0.export_started.notified().await;
        age(&database.pool, 3691).await;
        runtime.0.export_release.notify_one();
    };
    let (result, ()) = tokio::time::timeout(WAIT, async {
        tokio::join!(
            supervisor.hydrate_authorized(
                &delivery.execution,
                &request,
                delivery.delivery(&bundle),
                &bundle,
                u32::try_from(bundle.file_count()).unwrap()
            ),
            progress
        )
    })
    .await
    .unwrap();
    assert!(result.unwrap());
    assert_eq!(
        supervisor.ledger.read(scope).await.unwrap().phase,
        Phase::Failed
    );
    assert_eq!(runtime.0.imports.load(Ordering::SeqCst), 1);
    runtime.assert_expired();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL and ELITEA_TEST_BUNDLE_TLS; exercises the real 20-second heartbeat"]
async fn hydration_heartbeat_expires_blocked_original_transfer() {
    let database = database().await;
    let bundle = bundle();
    let request = request()
        .with_python_dependency_bundle(bundle.root().into())
        .unwrap();
    let delivery = DeliveryFixture::new(&request, &bundle);
    let scope = delivery.execution.scope();
    let runtime = Runtime::new(true);
    runtime.0.block_export.store(true, Ordering::SeqCst);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, scope).await;
    expire_leases(&database.pool).await;
    let progress = async {
        runtime.0.export_started.notified().await;
        age(&database.pool, 3691).await;
    };
    let (result, ()) = tokio::time::timeout(WAIT, async {
        tokio::join!(
            supervisor.hydrate_authorized(
                &delivery.execution,
                &request,
                delivery.delivery(&bundle),
                &bundle,
                u32::try_from(bundle.file_count()).unwrap()
            ),
            progress
        )
    })
    .await
    .unwrap();
    assert!(result.unwrap());
    assert_eq!(
        supervisor.ledger.read(scope).await.unwrap().phase,
        Phase::Failed
    );
    assert_eq!(runtime.0.imports.load(Ordering::SeqCst), 0);
    runtime.assert_expired();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_idle_sweep_is_partitioned_bounded_and_not_starved_by_execution() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 7);
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, &scope).await;
    age(&database.pool, 3691).await;
    expire_leases(&database.pool).await;
    let other = scope_for(&request, 8);
    supervisor.ledger.reserve(&other).await.unwrap();
    let lease = supervisor
        .ledger
        .claim(&other, "other-owner".into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    supervisor
        .ledger
        .bind_runtime(&lease, "other-runtime")
        .await
        .unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_bound_at=clock_timestamp()-interval '2 hours',lease_until=clock_timestamp()-interval '1 second' WHERE owner_id='other-owner'").execute(&database.pool).await.unwrap();
    let unbound = scope_for(&request, 9);
    supervisor.ledger.reserve(&unbound).await.unwrap();
    supervisor
        .ledger
        .claim(&unbound, OWNER.into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET created_at=clock_timestamp()-interval '2 hours',lease_until=clock_timestamp()-interval '1 second' WHERE runtime_id IS NULL").execute(&database.pool).await.unwrap();
    let candidates = supervisor
        .ledger
        .expired_hydrations(OWNER, MAX_HYDRATION_AGE_SECONDS, 1)
        .await
        .unwrap();
    assert_eq!(candidates.len(), 1);
    assert!(candidates[0] == scope);
    assert!(matches!(
        supervisor
            .ledger
            .expired_hydrations(OWNER, MAX_HYDRATION_AGE_SECONDS, 33)
            .await,
        Err(LedgerError::Invalid)
    ));
    let _busy_execution = supervisor.capacity.try_acquire().unwrap();
    supervisor.reconcile_expired_hydrations().await.unwrap();
    runtime.assert_expired();
    assert_eq!(
        supervisor.ledger.read(&scope).await.unwrap().phase,
        Phase::Failed
    );
    assert_eq!(
        supervisor.ledger.read(&other).await.unwrap().phase,
        Phase::Reserved
    );
    assert_eq!(
        supervisor.ledger.read(&unbound).await.unwrap().phase,
        Phase::Reserved
    );
}
fn scope_for(request: &PreparedJob, key: u8) -> JobScope {
    scope(request, key)
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_sweep_candidate_racing_to_dispatched_is_not_killed() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 10);
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, &scope).await;
    age(&database.pool, 3691).await;
    expire_leases(&database.pool).await;
    let candidates = supervisor
        .ledger
        .expired_hydrations(OWNER, MAX_HYDRATION_AGE_SECONDS, 32)
        .await
        .unwrap();
    assert_eq!(candidates.len(), 1);
    let winner = supervisor
        .ledger
        .claim(&scope, "dispatch-owner".into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap();
    supervisor.ledger.mark_dispatched(&winner).await.unwrap();
    expire_leases(&database.pool).await;
    supervisor
        .reconcile_expired_hydration(&candidates[0])
        .await
        .unwrap();
    assert_eq!(
        supervisor.ledger.read(&scope).await.unwrap().phase,
        Phase::Dispatched
    );
    assert!(
        supervisor
            .ledger
            .execution_age_seconds(&scope)
            .await
            .unwrap()
            < 60
    );
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.cleanups.load(Ordering::SeqCst), 0);
    assert!(
        supervisor
            .ledger
            .expired_hydrations(OWNER, MAX_HYDRATION_AGE_SECONDS, 32)
            .await
            .unwrap()
            .is_empty()
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_stop_wins_during_expiry_termination() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 11);
    let runtime = Runtime::new(true);
    runtime.0.block_termination.store(true, Ordering::SeqCst);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&supervisor, &scope).await;
    age(&database.pool, 3691).await;
    let stop = async {
        runtime.0.termination_started.notified().await;
        supervisor
            .ledger
            .request_cancellation(&scope, OWNER)
            .await
            .unwrap();
        runtime.0.termination_release.notify_one();
    };
    let (result, ()) = tokio::time::timeout(WAIT, async {
        tokio::join!(supervisor.expire_hydration_if_needed(&scope, &lease), stop)
    })
    .await
    .unwrap();
    let terminal = record(result.unwrap().unwrap());
    assert_eq!(terminal.phase, Phase::Cancelled);
    assert_eq!(terminal.failure_code.as_deref(), Some("sandbox.cancelled"));
    runtime.assert_expired();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_preparer_and_live_owner_are_not_swept() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 12);
    let runtime = Runtime::new(true);
    let execution = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&execution, &scope).await;
    age(&database.pool, 3691).await;
    execution.reconcile_expired_hydrations().await.unwrap();
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
    execution.ledger.release(&lease).await.unwrap();
    let preparer = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap()
    .with_preparation_policy(POLICY.into())
    .unwrap();
    preparer.reconcile_expired_hydrations().await.unwrap();
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
    assert_eq!(
        preparer.ledger.read(&scope).await.unwrap().phase,
        Phase::Reserved
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_fenced_owner_cannot_terminate_the_original() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 13);
    let runtime = Runtime::new(true);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    let lease = bound(&supervisor, &scope).await;
    age(&database.pool, 3691).await;
    expire_leases(&database.pool).await;
    assert!(matches!(
        supervisor.expire_hydration_if_needed(&scope, &lease).await,
        Err(SupervisorError::Ledger(LedgerError::Fenced))
    ));
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 0);
    assert_eq!(
        supervisor.ledger.read(&scope).await.unwrap().phase,
        Phase::Reserved
    );
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL through ELITEA_TEST_DATABASE_URL"]
async fn hydration_unknown_termination_remains_reserved_and_retries_original_cleanup() {
    let database = database().await;
    let request = request();
    let scope = scope(&request, 14);
    let runtime = Runtime::new(true);
    runtime.0.termination_fails.store(true, Ordering::SeqCst);
    let supervisor = supervisor(database.pool.clone(), runtime.clone(), OWNER);
    bound(&supervisor, &scope).await;
    age(&database.pool, 3691).await;
    expire_leases(&database.pool).await;
    supervisor.reconcile_expired_hydrations().await.unwrap();
    let pending = supervisor.ledger.read(&scope).await.unwrap();
    assert_eq!(pending.phase, Phase::Reserved);
    assert!(pending.failure_code.is_none());
    assert_eq!(pending.runtime_id.as_deref(), Some(RUNTIME_ID));
    let dispatched: Option<chrono::DateTime<chrono::Utc>> =
        sqlx::query_scalar("SELECT dispatched_at FROM elitea_runtime.sandbox_jobs")
            .fetch_one(&database.pool)
            .await
            .unwrap();
    assert!(dispatched.is_none());
    assert_eq!(runtime.0.cleanups.load(Ordering::SeqCst), 0);
    runtime.0.termination_fails.store(false, Ordering::SeqCst);
    supervisor.reconcile_expired_hydrations().await.unwrap();
    let finished = supervisor.ledger.read(&scope).await.unwrap();
    assert_eq!(finished.phase, Phase::Failed);
    assert_eq!(
        finished.failure_code.as_deref(),
        Some("sandbox.hydration_deadline_exceeded")
    );
    assert_eq!(runtime.0.prepares.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.dispatches.load(Ordering::SeqCst), 0);
    assert_eq!(runtime.0.terminations.load(Ordering::SeqCst), 2);
    assert_eq!(runtime.0.cleanups.load(Ordering::SeqCst), 1);
}
