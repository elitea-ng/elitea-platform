use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;

use ring::digest;
use tokio_stream::StreamExt as _;
use tonic::{Code, Request, Response, Status};

use super::input_content::content_fake::{ContentFake, Frames, claim_of, frames};
use super::input_content::{
    InputContentClient, InputContentConfig, InputContentError, InputContentTransportError,
};
use crate::protocol::control::{ClaimBoundInputAuthority, test_lease_monitored_input_execution};
use crate::protocol::elitea::runtime::v1::{
    GetInputRequestV1, GetInputResponseV1, InputContentHeaderV1, get_input_response_v1::Part,
};

const MEDIA_TYPE: &str = "application/vnd.elitea.agent-execution-input.v1+protobuf";
const TOOLKIT_MEDIA_TYPE: &str = "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf";
const SOURCE: &[u8] = b"source {{secret}}";
const CHUNK_BYTES: usize = 64 * 1024;
static SOURCE_SHA256: LazyLock<[u8; 32]> = LazyLock::new(|| sha256(SOURCE));

/// What the fake content service answers one `GetInput` with.
enum Reply {
    Frames(Vec<Result<GetInputResponseV1, Status>>),
    /// These frames, then a stream that never ends.
    Stalled(Vec<Result<GetInputResponseV1, Status>>),
    Refused(Status),
    /// A call that is never answered.
    Hang,
}

struct FakeRpc {
    captured: Arc<Mutex<Vec<CapturedRequest>>>,
    reply: Mutex<Option<Reply>>,
}

#[derive(Debug, Eq, PartialEq)]
struct CapturedRequest {
    execution_id: String,
    generation: u64,
    content_id: String,
    immutable_version: String,
    claim: String,
    fence: Vec<u8>,
}

#[tonic::async_trait]
impl ContentFake for FakeRpc {
    async fn get_input(
        &self,
        request: Request<GetInputRequestV1>,
    ) -> Result<Response<Frames<GetInputResponseV1>>, Status> {
        let (claim, fence) = claim_of(&request);
        let request = request.into_inner();
        let scope = request.scope.expect("scope");
        self.captured
            .lock()
            .expect("captured requests")
            .push(CapturedRequest {
                execution_id: scope.execution_id,
                generation: scope.generation,
                content_id: request.content_id,
                immutable_version: request.immutable_version,
                claim,
                fence,
            });
        let reply = self
            .reply
            .lock()
            .expect("fake reply")
            .take()
            .expect("one fake reply");
        match reply {
            Reply::Frames(items) => Ok(Response::new(frames(items))),
            Reply::Stalled(items) => Ok(Response::new(Box::pin(
                tokio_stream::iter(items).chain(tokio_stream::pending()),
            ))),
            Reply::Refused(status) => Err(status),
            Reply::Hang => std::future::pending().await,
        }
    }
}

fn materialized() -> Vec<u8> {
    b"materialized-in-memory".to_vec()
}

fn sha256(value: &[u8]) -> [u8; 32] {
    digest::digest(&digest::SHA256, value)
        .as_ref()
        .try_into()
        .expect("SHA-256 length")
}

fn reference() -> ClaimBoundInputAuthority<'static> {
    ClaimBoundInputAuthority {
        execution_id: "execution-one",
        generation: 2,
        content_id: "settings id",
        immutable_version: "v/1",
        claim_id: "claim-1",
        fence_token: &[b'f'; 32],
        expected_source_length: SOURCE.len() as u64,
        expected_source_sha256: SOURCE_SHA256.as_slice(),
        media_type: MEDIA_TYPE,
    }
}

/// The request the default reference produces, as the service receives it:
/// identifiers raw, the claim and fence as metadata.
fn expected_request() -> CapturedRequest {
    CapturedRequest {
        execution_id: "execution-one".to_owned(),
        generation: 2,
        content_id: "settings id".to_owned(),
        immutable_version: "v/1".to_owned(),
        claim: "claim-1".to_owned(),
        fence: vec![b'f'; 32],
    }
}

struct TestClient(InputContentClient);

impl TestClient {
    async fn fetch_materialized(
        &self,
        reference: ClaimBoundInputAuthority<'_>,
    ) -> Result<super::input_content::MaterializedInput, InputContentError> {
        self.0.fetch_test_authority(reference).await
    }
}

fn config(deadline: Duration, max_materialized_bytes: usize) -> InputContentConfig {
    InputContentConfig {
        origin: "https://content.internal".to_owned(),
        deadline,
        max_materialized_bytes,
    }
}

/// The header Main sends for `body` materialized from `SOURCE`.
fn header(body: &[u8]) -> InputContentHeaderV1 {
    InputContentHeaderV1 {
        media_type: MEDIA_TYPE.to_owned(),
        length: body.len() as u64,
        sha256: sha256(body).to_vec(),
        source_sha256: SOURCE_SHA256.to_vec(),
        source_length: SOURCE.len() as u64,
        source_immutable_version: "v/1".to_owned(),
    }
}

#[allow(
    clippy::unnecessary_wraps,
    reason = "a fixture reply is the Result the fake service answers with"
)]
fn header_frame(header: InputContentHeaderV1) -> Result<GetInputResponseV1, Status> {
    Ok(GetInputResponseV1 {
        part: Some(Part::Header(header)),
    })
}

#[allow(
    clippy::unnecessary_wraps,
    reason = "a fixture reply is the Result the fake service answers with"
)]
fn chunk_frame(chunk: &[u8]) -> Result<GetInputResponseV1, Status> {
    Ok(GetInputResponseV1 {
        part: Some(Part::Chunk(chunk.to_vec())),
    })
}

/// One stream: `header`, then `body` in chunks of at most `chunk_bytes`.
fn stream(header: InputContentHeaderV1, body: &[u8], chunk_bytes: usize) -> Reply {
    let mut items = vec![header_frame(header)];
    items.extend(body.chunks(chunk_bytes).map(chunk_frame));
    Reply::Frames(items)
}

fn served(body: &[u8]) -> Reply {
    stream(header(body), body, CHUNK_BYTES)
}

fn fake_client(
    reply: Reply,
    deadline: Duration,
    max_materialized_bytes: usize,
) -> (TestClient, Arc<Mutex<Vec<CapturedRequest>>>) {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = InputContentClient::with_rpc(
        FakeRpc {
            captured: Arc::clone(&captured),
            reply: Mutex::new(Some(reply)),
        },
        config(deadline, max_materialized_bytes),
    )
    .expect("input content client");
    (TestClient(client), captured)
}

async fn fetch(reply: Reply) -> Result<Vec<u8>, InputContentError> {
    let (client, _) = fake_client(reply, Duration::from_secs(1), 1024);
    client
        .fetch_materialized(reference())
        .await
        .map(|input| input.as_bytes().to_vec())
}

#[tokio::test(flavor = "current_thread")]
async fn claim_bound_materialization_carries_scope_and_claim_metadata() {
    let body = materialized();
    let (client, captured) = fake_client(served(&body), Duration::from_secs(1), 1024);

    let execution = test_lease_monitored_input_execution(SOURCE.len() as u64, *SOURCE_SHA256);
    let result = client
        .0
        .fetch_materialized(&execution)
        .await
        .expect("materialized input");

    assert_eq!(result.as_bytes(), body);
    assert_eq!(
        *captured.lock().expect("captured requests"),
        [expected_request()]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn chunks_assemble_in_order_whatever_their_size() {
    let body = materialized();
    for chunk_bytes in [1, 3, body.len()] {
        assert_eq!(
            fetch(stream(header(&body), &body, chunk_bytes))
                .await
                .expect("assembled"),
            body
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn source_identity_and_response_digest_are_both_required() {
    let body = materialized();
    let mut wrong_source = header(&body);
    wrong_source.source_immutable_version = "wrong".to_owned();
    assert!(matches!(
        fetch(stream(wrong_source, &body, CHUNK_BYTES)).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));

    let mut wrong_body_digest = header(&body);
    wrong_body_digest.sha256 = vec![9; 32];
    assert!(matches!(
        fetch(stream(wrong_body_digest, &body, CHUNK_BYTES)).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut wrong_source_digest = header(&body);
    wrong_source_digest.source_sha256 = vec![7; 32];
    assert!(matches!(
        fetch(stream(wrong_source_digest, &body, CHUNK_BYTES)).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));

    let mut wrong_source_length = header(&body);
    wrong_source_length.source_length = 999;
    assert!(matches!(
        fetch(stream(wrong_source_length, &body, CHUNK_BYTES)).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn admitted_long_history_materializes_without_the_old_256_kib_ceiling() {
    for length in [256 * 1024 + 1, 1024 * 1024] {
        let body = vec![b'x'; length];
        let digest = sha256(&body);
        let mut header = header(&body);
        header.source_length = length as u64;
        header.source_sha256 = digest.to_vec();
        let authority = ClaimBoundInputAuthority {
            expected_source_length: length as u64,
            expected_source_sha256: &digest,
            ..reference()
        };
        let (client, _) = fake_client(
            stream(header, &body, CHUNK_BYTES),
            Duration::from_secs(1),
            1024 * 1024,
        );
        assert_eq!(
            client
                .fetch_materialized(authority)
                .await
                .unwrap()
                .as_bytes(),
            body
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn refusals_map_to_the_variants_the_http_statuses_produced() {
    for (code, expected) in [
        (Code::Unauthenticated, "input_content.authorization_failed"),
        (Code::PermissionDenied, "input_content.authorization_failed"),
        (Code::NotFound, "input_content.invalid_response"),
        (Code::Unimplemented, "input_content.invalid_response"),
        (Code::InvalidArgument, "input_content.invalid_response"),
        (Code::FailedPrecondition, "input_content.invalid_response"),
        (Code::ResourceExhausted, "input_content.resource_exhausted"),
        (Code::DeadlineExceeded, "input_content.timeout"),
        (Code::Unavailable, "input_content.dependency_unavailable"),
        (Code::Internal, "input_content.dependency_unavailable"),
        (Code::Aborted, "input_content.dependency_unavailable"),
    ] {
        let Err(error) = fetch(Reply::Refused(Status::new(code, "refused"))).await else {
            panic!("{code:?} must fail")
        };
        assert_eq!(error.code(), expected, "{code:?}");
    }
    // An overloaded or unreachable service is the channel's failure, kept
    // with its status for operator diagnostics.
    assert!(matches!(
        fetch(Reply::Refused(Status::unavailable("overloaded"))).await,
        Err(InputContentError::Transport(
            InputContentTransportError::Status(_)
        ))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn the_stream_opens_with_exactly_one_well_formed_header() {
    let body = materialized();
    let chunk_first = Reply::Frames(vec![chunk_frame(&body), header_frame(header(&body))]);
    let repeated = Reply::Frames(vec![
        header_frame(header(&body)),
        header_frame(header(&body)),
        chunk_frame(&body),
    ]);
    let empty_message = Reply::Frames(vec![
        header_frame(header(&body)),
        Ok(GetInputResponseV1 { part: None }),
    ]);
    let no_header = Reply::Frames(Vec::new());
    for reply in [chunk_first, repeated, empty_message, no_header] {
        assert!(matches!(
            fetch(reply).await,
            Err(InputContentError::InvalidInput(_))
        ));
    }

    let mut short_digest = header(&body);
    short_digest.sha256 = vec![1; 31];
    let mut short_source_digest = header(&body);
    short_source_digest.source_sha256 = vec![1; 33];
    let mut wrong_media = header(&body);
    wrong_media.media_type = "application/json".to_owned();
    let mut no_length = header(&body);
    no_length.length = 0;
    let mut no_source_length = header(&body);
    no_source_length.source_length = 0;
    for header in [
        short_digest,
        short_source_digest,
        wrong_media,
        no_length,
        no_source_length,
    ] {
        assert!(matches!(
            fetch(stream(header, &body, CHUNK_BYTES)).await,
            Err(InputContentError::InvalidInput(_))
        ));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn streaming_length_bounds_fail_closed() {
    let body = materialized();
    let (client, _) = fake_client(served(&body), Duration::from_secs(1), body.len() - 1);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::ResourceExhausted(_))
    ));

    // Bytes past the declared length are a protocol violation.
    let oversized_stream = vec![b'x'; body.len() + 1];
    assert!(matches!(
        fetch(stream(header(&body), &oversized_stream, 4)).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut overstated = header(&body);
    overstated.length = body.len() as u64 + 1;
    assert!(matches!(
        fetch(stream(overstated, &body, CHUNK_BYTES)).await,
        Err(InputContentError::InvalidInput(_))
    ));

    // A stream the service ends with an error after the header is not an
    // input: the bytes so far are dropped.
    let interrupted = Reply::Frames(vec![
        header_frame(header(&body)),
        chunk_frame(&body[..4]),
        Err(Status::unavailable("interrupted")),
    ]);
    assert!(matches!(
        fetch(interrupted).await,
        Err(InputContentError::Transport(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn exact_body_limit_and_media_type_parameters_are_accepted() {
    let body = vec![b'x'; 1024];
    let (client, _) = fake_client(served(&body), Duration::from_secs(1), body.len());
    let exact_limit = client
        .fetch_materialized(reference())
        .await
        .expect("body at exact configured limit");
    assert_eq!(exact_limit.as_bytes(), body);

    let mixed_case_body = materialized();
    let mut mixed_case_media = header(&mixed_case_body);
    mixed_case_media.media_type =
        "Application/Vnd.Elitea.Agent-Execution-Input.V1+Protobuf; charset=binary".to_owned();
    assert_eq!(
        fetch(stream(mixed_case_media, &mixed_case_body, CHUNK_BYTES))
            .await
            .expect("case-insensitive media type"),
        mixed_case_body
    );
}

#[tokio::test(flavor = "current_thread")]
async fn direct_toolkit_input_media_type_is_exactly_admitted() {
    let body = materialized();
    let mut toolkit_header = header(&body);
    toolkit_header.media_type = TOOLKIT_MEDIA_TYPE.to_owned();
    let (client, captured) = fake_client(
        stream(toolkit_header, &body, CHUNK_BYTES),
        Duration::from_secs(1),
        1024,
    );
    let mut toolkit_reference = reference();
    toolkit_reference.media_type = TOOLKIT_MEDIA_TYPE;

    let materialized = client
        .fetch_materialized(toolkit_reference)
        .await
        .expect("direct toolkit materialized input");

    assert_eq!(materialized.as_bytes(), body);
    assert_eq!(captured.lock().expect("captured requests").len(), 1);

    let (client, captured) = fake_client(served(&body), Duration::from_secs(1), 1024);
    let mut lookalike = reference();
    lookalike.media_type = "application/vnd.elitea.toolkit-execute-read-input.v2+protobuf";
    assert!(matches!(
        client.fetch_materialized(lookalike).await,
        Err(InputContentError::InvalidInput(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn oversized_admitted_source_fails_before_request_emission() {
    let body = materialized();
    let (client, captured) = fake_client(served(&body), Duration::from_secs(1), SOURCE.len() - 1);

    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::ResourceExhausted(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());
}

#[test]
fn errors_expose_safe_actionable_codes_and_retry_classification() {
    let authorization = InputContentError::AuthorizationFailed(
        "the materialized input source digest does not match the admitted descriptor",
    );
    assert_eq!(authorization.code(), "input_content.authorization_failed");
    assert!(!authorization.retryable());
    assert_eq!(
        authorization.to_string(),
        "the materialized input source digest does not match the admitted descriptor"
    );

    for unavailable in [
        InputContentError::Transport(InputContentTransportError::Unavailable),
        InputContentError::transport(Status::unavailable("secret detail")),
    ] {
        assert_eq!(unavailable.code(), "input_content.dependency_unavailable");
        assert!(unavailable.retryable());
        assert_eq!(
            unavailable.to_string(),
            "the input content service is unavailable"
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn whole_request_deadline_bounds_a_stalled_service() {
    let (client, _) = fake_client(Reply::Hang, Duration::from_millis(20), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::Timeout(_))
    ));

    let body = materialized();
    let stalled = Reply::Stalled(vec![header_frame(header(&body)), chunk_frame(&body)]);
    let (client, _) = fake_client(stalled, Duration::from_millis(20), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::Timeout(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn configuration_and_reference_bounds_reject_unsafe_authority() {
    let rpc = || FakeRpc {
        captured: Arc::new(Mutex::new(Vec::new())),
        reply: Mutex::new(None),
    };
    assert!(matches!(
        InputContentClient::with_rpc(
            rpc(),
            InputContentConfig {
                origin: "http://content.internal".to_owned(),
                deadline: Duration::from_secs(1),
                max_materialized_bytes: 1024,
            }
        ),
        Err(InputContentError::InvalidConfiguration(_))
    ));
    let (client, captured) = fake_client(served(&materialized()), Duration::from_secs(1), 1024);
    let malformed = ClaimBoundInputAuthority {
        claim_id: "claim\nforged",
        ..reference()
    };
    assert!(matches!(
        client.fetch_materialized(malformed).await,
        Err(InputContentError::InvalidInput(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());

    for origin in [
        "https://CONTENT.internal:8443",
        "https://CONTENT.internal:443",
        "https://[::1]:8443",
    ] {
        assert!(
            InputContentClient::with_rpc(
                rpc(),
                InputContentConfig {
                    origin: origin.to_owned(),
                    deadline: Duration::from_secs(1),
                    max_materialized_bytes: 1024,
                }
            )
            .is_ok()
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn checkpoint_request_preserves_claim_binding_and_rejects_changed_source() {
    for changed_source in [false, true] {
        let inspection = crate::protocol::control::test_model_checkpoint_inspection(i64::MAX);
        let (pending, _lease) = inspection.into_lease_supervision();
        let live = pending.into_live();
        let body = materialized();
        let mut header = header(&body);
        header.source_length = 1;
        header.source_sha256 = if changed_source {
            vec![0x62; 32]
        } else {
            vec![0x61; 32]
        };
        let (client, captured) = fake_client(
            stream(header, &body, CHUNK_BYTES),
            Duration::from_secs(1),
            1024,
        );
        let result = client.0.fetch_checkpoint_request(&live).await;
        if changed_source {
            assert!(matches!(
                result,
                Err(InputContentError::AuthorizationFailed(_))
            ));
        } else {
            assert_eq!(result.expect("frozen request").as_bytes(), body);
        }
        assert_eq!(
            *captured.lock().expect("captured requests"),
            [expected_request()]
        );
    }
}

mod security_context {
    use std::path::PathBuf;
    use std::time::Duration;

    use serde_json::Value;

    use super::{CapturedRequest, Reply, chunk_frame, fake_client, header_frame};
    use crate::protocol::control::test_governed_lease_monitored_execution;
    use crate::protocol::elitea::runtime::v1::{
        DigestAlgorithmV1, DigestV1, ExecutionSecurityContextReferenceV1, InputContentHeaderV1,
        WorkerCommandV1,
    };
    use crate::protocol::security_context::{self, SecurityContextReference};
    use crate::transport::InputContentError;

    struct Vector {
        context: Vec<u8>,
        digest: Vec<u8>,
    }

    fn vector(name: &str) -> Vector {
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../testdata/proto/runtime/v1/security-context/vectors.json");
        let corpus: Value =
            serde_json::from_slice(&std::fs::read(path).expect("corpus")).expect("corpus JSON");
        let vector = corpus["vectors"]
            .as_array()
            .expect("vectors")
            .iter()
            .find(|vector| vector["name"] == name)
            .unwrap_or_else(|| panic!("no corpus vector named {name:?}"));
        let hex = |field: &str| {
            let text = vector[field].as_str().expect("hex");
            (0..text.len())
                .step_by(2)
                .map(|at| u8::from_str_radix(&text[at..at + 2], 16).expect("hex"))
                .collect::<Vec<u8>>()
        };
        Vector {
            context: hex("context_hex"),
            digest: hex("reference_digest_hex"),
        }
    }

    fn reference(execution_id: &str, digest: &[u8]) -> SecurityContextReference {
        SecurityContextReference::from_command(&WorkerCommandV1 {
            execution_id: execution_id.to_owned(),
            security_context_ref: Some(ExecutionSecurityContextReferenceV1 {
                content_entry_id: security_context::content_id(execution_id),
                digest: Some(DigestV1 {
                    algorithm: DigestAlgorithmV1::Sha256 as i32,
                    value: digest.to_vec(),
                }),
                schema_revision: security_context::SCHEMA_REVISION_V1.to_owned(),
            }),
            ..WorkerCommandV1::default()
        })
        .expect("reference")
        .expect("governed")
    }

    /// What Main's content server sends for stored context bytes.
    fn served(bytes: &[u8], digest: &[u8], version: &str) -> Reply {
        Reply::Frames(vec![
            header_frame(InputContentHeaderV1 {
                media_type: security_context::MEDIA_TYPE.to_owned(),
                length: bytes.len() as u64,
                sha256: digest.to_vec(),
                source_sha256: digest.to_vec(),
                source_length: bytes.len() as u64,
                source_immutable_version: version.to_owned(),
            }),
            chunk_frame(bytes),
        ])
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_governed_execution_fetches_and_verifies_its_context_under_claim() {
        let accepted = vector("valid governed context");
        let reference = reference("execution-1", &accepted.digest);
        let version = reference.immutable_version();
        assert!(version.starts_with("sha256:"));
        let (client, captured) = fake_client(
            served(&accepted.context, &accepted.digest, &version),
            Duration::from_secs(1),
            1024,
        );
        let execution = test_governed_lease_monitored_execution("execution-1", Some(reference));
        let context = client
            .0
            .fetch_security_context(&execution)
            .await
            .expect("verified")
            .expect("governed");
        assert_eq!(context.message().agent_binding_epoch, 3);
        assert_eq!(
            *captured.lock().expect("captured"),
            [CapturedRequest {
                execution_id: "execution-1".to_owned(),
                generation: 2,
                content_id: "elitea.security-context/execution-1".to_owned(),
                immutable_version: version,
                claim: "claim-1".to_owned(),
                fence: vec![b'f'; 32],
            }]
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_legacy_execution_never_asks_for_a_context() {
        let (client, captured) = fake_client(Reply::Hang, Duration::from_secs(1), 1024);
        let execution = test_governed_lease_monitored_execution("execution-1", None);
        assert!(
            client
                .0
                .fetch_security_context(&execution)
                .await
                .expect("legacy")
                .is_none()
        );
        assert!(captured.lock().expect("captured").is_empty());
    }

    #[tokio::test(flavor = "current_thread")]
    async fn every_refused_context_fails_the_execution_closed() {
        let accepted = vector("valid governed context");
        let mismatch = vector("bytes do not match the reference digest");
        let unknown = vector("unknown field 18");
        // (execution of the claim, reference, served bytes, served digest)
        let cases = [
            // The served bytes do not hash to the signed reference.
            (
                "execution-1",
                reference("execution-1", &accepted.digest),
                mismatch.context.clone(),
                accepted.digest.clone(),
            ),
            // A context with an unknown field 18, digest and all consistent.
            (
                "execution-1",
                reference("execution-1", &unknown.digest),
                unknown.context.clone(),
                unknown.digest.clone(),
            ),
            // The reference names another execution's context.
            (
                "execution-2",
                reference("execution-1", &accepted.digest),
                accepted.context.clone(),
                accepted.digest.clone(),
            ),
            // The header restates another digest.
            (
                "execution-1",
                reference("execution-1", &accepted.digest),
                accepted.context.clone(),
                vec![9; 32],
            ),
        ];
        for (execution_id, reference, bytes, served_digest) in cases {
            let version = reference.immutable_version();
            let (client, _) = fake_client(
                served(&bytes, &served_digest, &version),
                Duration::from_secs(1),
                1024,
            );
            let execution = test_governed_lease_monitored_execution(execution_id, Some(reference));
            let error = client
                .0
                .fetch_security_context(&execution)
                .await
                .expect_err("refused");
            assert!(
                matches!(error, InputContentError::AuthorizationFailed(_)),
                "{error:?}"
            );
            assert!(!error.retryable());
        }
    }
}
