//! The owner RPCs over a real mutual-TLS listener: the client certificate is
//! the only identity, and only the configured Main identity gets past authority.
use super::super::SupervisorService;
use super::*;
use crate::protocol::elitea::runtime::v1::{
    CodeOwnerRecoveryReadRequestV1,
    sandbox_supervisor_service_client::SandboxSupervisorServiceClient,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa, KeyPair,
    KeyUsagePurpose, SanType,
};
use ring::signature::{Ed25519KeyPair, KeyPair as _};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};

const READ_DOMAIN: &[u8] = b"elitea.sandbox.node-code-recovery-grant.ed25519.v1\0";

struct Keys([u8; 32]);
impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
        (id == "fixture-key").then_some(self.0)
    }
}

struct Pki {
    ca: String,
    issuer: CertifiedIssuer<'static, KeyPair>,
}
impl Pki {
    fn new() -> Self {
        let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        let issuer = CertifiedIssuer::self_signed(params, KeyPair::generate().unwrap()).unwrap();
        Self {
            ca: issuer.pem(),
            issuer,
        }
    }
    /// A leaf whose only SAN is `dns`, as (certificate PEM, key PEM).
    fn leaf(&self, dns: &str, usage: ExtendedKeyUsagePurpose) -> (String, String) {
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![SanType::DnsName(dns.try_into().unwrap())];
        params.extended_key_usages = vec![usage];
        let certificate = params.signed_by(&key, &self.issuer).unwrap();
        (certificate.pem(), key.serialize_pem())
    }
}

/// A signed read grant for Main's identity on a fixed original job.
fn read_request(key: &Ed25519KeyPair) -> (String, Vec<u8>) {
    let execution = "1".repeat(32);
    let dispatch = "2".repeat(64);
    let job_key = crate::sandbox::code_recovery::original_job_key(&execution, &dispatch);
    let now = chrono::Utc::now().timestamp_millis();
    let claims = serde_json::to_vec(&serde_json::json!({
        "schema": "elitea.sandbox.node-code-recovery-grant.v1",
        "tenant_id": "code-owner-fixture",
        "project_id": 1,
        "execution_id": execution,
        "original_generation": 1,
        "claim_id": "7".repeat(32),
        "claim_attempt": 2,
        "lease_epoch": 2,
        "fence_sha256": "8".repeat(64),
        "activation_id": "5".repeat(64),
        "node_id": "code",
        "graph_thread": "owner-fixture-root",
        "step": 2,
        "attempt": 1,
        "expected_revision": 2,
        "receipt_sha256": "9".repeat(64),
        "dispatch_activation": dispatch,
        "job_key": job_key,
        "request_digest": "a".repeat(64),
        "binding_sha256": "b".repeat(64),
        "supervisor_audience": "dns:supervisor.fixture",
        "requester_workload_identity": "dns:main.fixture",
        "operation": "read",
        "issued_at_unix_millis": now,
        "expires_at_unix_millis": now + 30_000,
    }))
    .unwrap();
    let mut framed = READ_DOMAIN.to_vec();
    framed.extend_from_slice(&(claims.len() as u64).to_be_bytes());
    framed.extend_from_slice(&claims);
    let request = serde_json::to_vec(&serde_json::json!({
        "schema": "elitea.sandbox.node-code-recovery-request.v1",
        "grant": {
            "schema": "elitea.sandbox.node-code-recovery-signed-grant.v1",
            "key_id": "fixture-key",
            "claims_base64url": URL_SAFE_NO_PAD.encode(&claims),
            "signature_base64url": URL_SAFE_NO_PAD.encode(key.sign(&framed).as_ref()),
        },
    }))
    .unwrap();
    (job_key, request)
}

async fn client(
    port: u16,
    ca: &str,
    identity: Option<&(String, String)>,
) -> Result<SandboxSupervisorServiceClient<Channel>, tonic::transport::Error> {
    let mut tls = ClientTlsConfig::new()
        .domain_name("localhost")
        .ca_certificate(Certificate::from_pem(ca));
    if let Some((certificate, key)) = identity {
        tls = tls.identity(Identity::from_pem(certificate, key));
    }
    let channel = Endpoint::from_shared(format!("https://localhost:{port}"))
        .unwrap()
        .tls_config(tls)?
        .connect_timeout(Duration::from_secs(5))
        .connect()
        .await?;
    Ok(SandboxSupervisorServiceClient::new(channel))
}

#[tokio::test]
async fn owner_rpcs_admit_only_the_configured_main_certificate() {
    crate::diagnostics::install_tls_crypto_provider().unwrap();
    let pki = Pki::new();
    let server = pki.leaf("localhost", ExtendedKeyUsagePurpose::ServerAuth);
    let main = pki.leaf("main.fixture", ExtendedKeyUsagePurpose::ClientAuth);
    let other = pki.leaf("worker.fixture", ExtendedKeyUsagePurpose::ClientAuth);
    let signing = Ed25519KeyPair::from_seed_unchecked(&[9; 32]).unwrap();
    let verifier = GrantVerifier::new(
        Keys(signing.public_key().as_ref().try_into().unwrap()),
        "dns:supervisor.fixture".into(),
    )
    .unwrap()
    .with_code_owner_requester("dns:main.fixture".into())
    .unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let (stop, stopped) = tokio::sync::oneshot::channel::<()>();
    let serving = tokio::spawn(
        SupervisorService::new(verifier, super::tests::no_io_supervisor()).serve(
            listener,
            Identity::from_pem(&server.0, &server.1),
            Certificate::from_pem(&pki.ca),
            async move {
                let _ = stopped.await;
            },
        ),
    );
    let (job_key, request_json) = read_request(&signing);
    let request = || CodeOwnerRecoveryReadRequestV1 {
        job_key: job_key.clone(),
        request_json: request_json.clone(),
    };

    // Main's certificate and grant pass authority and reach the ledger, which
    // this fixture cannot open: the refusal is the ledger's, with no content.
    let status = client(port, &pki.ca, Some(&main))
        .await
        .unwrap()
        .code_owner_recovery_read(request())
        .await
        .unwrap_err();
    assert_eq!(status.code(), tonic::Code::Aborted, "{status:?}");
    assert_eq!(
        status.message(),
        "The job cannot be observed for this owner grant."
    );

    // The same grant from another workload identity is refused by authority.
    let status = client(port, &pki.ca, Some(&other))
        .await
        .unwrap()
        .code_owner_recovery_read(request())
        .await
        .unwrap_err();
    assert_eq!(status.code(), tonic::Code::PermissionDenied, "{status:?}");

    // Without a client certificate the handshake fails before any RPC runs.
    match client(port, &pki.ca, None).await {
        Err(_) => {}
        Ok(mut anonymous) => {
            let status = anonymous
                .code_owner_recovery_read(request())
                .await
                .unwrap_err();
            assert!(
                matches!(
                    status.code(),
                    tonic::Code::Unavailable | tonic::Code::Unknown
                ),
                "{status:?}"
            );
        }
    }

    stop.send(()).unwrap();
    serving.await.unwrap().unwrap();
}
