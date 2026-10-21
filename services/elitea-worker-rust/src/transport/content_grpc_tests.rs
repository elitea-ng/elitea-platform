//! The content client against a real tonic server over real mutual TLS: the
//! generated codec, metadata, status codes and chunked streaming as they cross
//! the wire, not as an in-process fake hands them over.

use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use rcgen::{
    BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa, KeyPair,
    KeyUsagePurpose,
};
use ring::digest;
use tokio::sync::oneshot;
use tonic::transport::{Certificate, Identity, Server, ServerTlsConfig};
use tonic::{Request, Response, Status};

use super::content_fake::{ContentFake, Frames, Served, claim_of, frames};
use super::{InputContentClient, InputContentConfig, InputContentError};
use crate::protocol::control::ClaimBoundInputAuthority;
use crate::protocol::elitea::runtime::v1::runtime_content_service_server::RuntimeContentServiceServer;
use crate::protocol::elitea::runtime::v1::{
    GetInputRequestV1, GetInputResponseV1, InputContentHeaderV1, get_input_response_v1::Part,
};

const MEDIA_TYPE: &str = "application/vnd.elitea.agent-execution-input.v1+protobuf";
const SOURCE: &[u8] = b"source document";
/// Larger than one 64 KiB chunk, so the body crosses the wire in several.
const BODY_BYTES: usize = 150 * 1024;
const FENCE: [u8; 32] = [0xf0; 32];

/// A throwaway CA, a server certificate for `localhost` and a client
/// certificate, all PEM.
struct Pki {
    ca: String,
    server: (String, String),
    client: (String, String),
}

fn leaf(
    issuer: &CertifiedIssuer<'_, KeyPair>,
    names: &[&str],
    usage: ExtendedKeyUsagePurpose,
) -> (String, String) {
    let key = KeyPair::generate().expect("leaf key");
    let mut params = CertificateParams::new(
        names
            .iter()
            .map(|name| (*name).to_owned())
            .collect::<Vec<_>>(),
    )
    .expect("leaf params");
    params.extended_key_usages = vec![usage];
    let certificate = params.signed_by(&key, issuer).expect("leaf certificate");
    (certificate.pem(), key.serialize_pem())
}

fn authority() -> CertifiedIssuer<'static, KeyPair> {
    let mut params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    CertifiedIssuer::self_signed(params, KeyPair::generate().expect("ca key")).expect("ca")
}

fn pki() -> Pki {
    let ca = authority();
    Pki {
        ca: ca.pem(),
        server: leaf(&ca, &["localhost"], ExtendedKeyUsagePurpose::ServerAuth),
        client: leaf(&ca, &[], ExtendedKeyUsagePurpose::ClientAuth),
    }
}

/// How the server answers the one `GetInput` it is asked.
#[derive(Clone)]
enum Script {
    Serve,
    Refuse(Status),
    /// The header and one chunk, then the stream fails with this status.
    FailMidStream(Status),
}

/// What the server saw of the call.
#[derive(Default)]
struct Seen {
    claim: Option<(String, Vec<u8>)>,
    peer_certificates: usize,
}

struct Main {
    script: Script,
    seen: Arc<Mutex<Seen>>,
}

fn body() -> Vec<u8> {
    (0..BODY_BYTES)
        .map(|index| u8::try_from(index % 251).expect("below 251"))
        .collect()
}

fn sha256(value: &[u8]) -> Vec<u8> {
    digest::digest(&digest::SHA256, value).as_ref().to_vec()
}

fn header(body: &[u8]) -> InputContentHeaderV1 {
    InputContentHeaderV1 {
        media_type: MEDIA_TYPE.to_owned(),
        length: body.len() as u64,
        sha256: sha256(body),
        source_sha256: sha256(SOURCE),
        source_length: SOURCE.len() as u64,
        source_immutable_version: "v1".to_owned(),
    }
}

#[allow(
    clippy::unnecessary_wraps,
    reason = "a fixture reply is the Result the fake service answers with"
)]
fn frame(part: Part) -> Result<GetInputResponseV1, Status> {
    Ok(GetInputResponseV1 { part: Some(part) })
}

#[tonic::async_trait]
impl ContentFake for Main {
    async fn get_input(
        &self,
        request: Request<GetInputRequestV1>,
    ) -> Result<Response<Frames<GetInputResponseV1>>, Status> {
        {
            let mut seen = self.seen.lock().expect("seen");
            seen.claim = Some(claim_of(&request));
            seen.peer_certificates = request
                .peer_certs()
                .map_or(0, |certificates| certificates.len());
        }
        let body = body();
        let mut items = vec![frame(Part::Header(header(&body)))];
        match self.script.clone() {
            Script::Refuse(status) => return Err(status),
            Script::Serve => items.extend(
                body.chunks(64 * 1024)
                    .map(|chunk| frame(Part::Chunk(chunk.to_vec()))),
            ),
            Script::FailMidStream(status) => {
                items.push(frame(Part::Chunk(body[..1024].to_vec())));
                items.push(Err(status));
            }
        }
        Ok(Response::new(frames(items)))
    }
}

/// A content server on loopback that requires a client certificate under the
/// CA. Dropping the sender stops it.
async fn serve(pki: &Pki, script: Script) -> (SocketAddr, Arc<Mutex<Seen>>, oneshot::Sender<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("loopback listener");
    let address = listener.local_addr().expect("listener address");
    let seen = Arc::new(Mutex::new(Seen::default()));
    let tls = ServerTlsConfig::new()
        .identity(Identity::from_pem(&pki.server.0, &pki.server.1))
        .client_ca_root(Certificate::from_pem(&pki.ca));
    let (stop, stopped) = oneshot::channel();
    let service = RuntimeContentServiceServer::new(Served(Main {
        script,
        seen: Arc::clone(&seen),
    }));
    let router = Server::builder()
        .tls_config(tls)
        .expect("server TLS")
        .add_service(service);
    tokio::spawn(async move {
        router
            .serve_with_incoming_shutdown(
                tokio_stream::wrappers::TcpListenerStream::new(listener),
                async {
                    let _ = stopped.await;
                },
            )
            .await
            .expect("content server");
    });
    (address, seen, stop)
}

async fn client(pki: &Pki, address: SocketAddr, client: &(String, String)) -> InputContentClient {
    InputContentClient::connect(
        InputContentConfig {
            origin: format!("https://localhost:{}", address.port()),
            deadline: Duration::from_secs(5),
            max_materialized_bytes: BODY_BYTES,
        },
        Certificate::from_pem(&pki.ca),
        Identity::from_pem(&client.0, &client.1),
    )
    .await
    .expect("content client")
}

async fn fetch(client: &InputContentClient) -> Result<Vec<u8>, InputContentError> {
    let source_sha256 = sha256(SOURCE);
    client
        .fetch_test_authority(ClaimBoundInputAuthority {
            execution_id: "execution-one",
            generation: 2,
            content_id: "settings",
            immutable_version: "v1",
            claim_id: "claim-1",
            fence_token: &FENCE,
            expected_source_length: SOURCE.len() as u64,
            expected_source_sha256: &source_sha256,
            media_type: MEDIA_TYPE,
        })
        .await
        .map(|input| input.as_bytes().to_vec())
}

#[tokio::test]
async fn an_authorized_call_streams_verified_chunks_with_the_claim_intact() {
    let pki = pki();
    let (address, seen, _stop) = serve(&pki, Script::Serve).await;

    let fetched = fetch(&client(&pki, address, &pki.client).await)
        .await
        .expect("served input");

    assert_eq!(fetched, body());
    let seen = seen.lock().expect("seen");
    assert_eq!(
        seen.claim,
        Some(("claim-1".to_owned(), FENCE.to_vec())),
        "the ascii claim id and the raw 32-byte fence cross the wire unchanged"
    );
    assert_eq!(
        seen.peer_certificates, 1,
        "the call was mutually authenticated"
    );
}

#[tokio::test]
async fn a_refused_claim_is_an_authorization_failure() {
    let pki = pki();
    let (address, _, _stop) = serve(
        &pki,
        Script::Refuse(Status::permission_denied("stale claim")),
    )
    .await;

    let Err(error) = fetch(&client(&pki, address, &pki.client).await).await else {
        panic!("a refused claim must not materialize input")
    };

    assert!(
        matches!(error, InputContentError::AuthorizationFailed(_)),
        "{error:?}"
    );
    assert!(!error.retryable());
}

#[tokio::test]
async fn a_server_deadline_is_a_timeout() {
    let pki = pki();
    let (address, _, _stop) = serve(
        &pki,
        Script::Refuse(Status::deadline_exceeded("main ran out of time")),
    )
    .await;

    let Err(error) = fetch(&client(&pki, address, &pki.client).await).await else {
        panic!("an expired call must not materialize input")
    };

    assert!(matches!(error, InputContentError::Timeout(_)), "{error:?}");
    assert!(error.retryable());
}

#[tokio::test]
async fn a_stream_cancelled_mid_body_is_a_retryable_dependency_failure() {
    let pki = pki();
    let (address, _, _stop) = serve(
        &pki,
        Script::FailMidStream(Status::cancelled("main shut down")),
    )
    .await;

    let Err(error) = fetch(&client(&pki, address, &pki.client).await).await else {
        panic!("a truncated stream must not materialize input")
    };

    assert_eq!(
        error.code(),
        "input_content.dependency_unavailable",
        "{error:?}"
    );
    assert!(error.retryable());
}

#[tokio::test]
async fn a_client_certificate_from_another_authority_is_refused_by_the_handshake() {
    let pki = pki();
    let (address, seen, _stop) = serve(&pki, Script::Serve).await;
    let stranger = leaf(&authority(), &[], ExtendedKeyUsagePurpose::ClientAuth);

    let Err(error) = fetch(&client(&pki, address, &stranger).await).await else {
        panic!("a client outside the runtime CA must not be served")
    };

    assert_eq!(
        error.code(),
        "input_content.dependency_unavailable",
        "{error:?}"
    );
    assert!(
        seen.lock().expect("seen").claim.is_none(),
        "no call reached the service"
    );
}
