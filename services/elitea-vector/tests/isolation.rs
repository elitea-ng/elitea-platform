//! The isolation conformance suite (ADR-0031 Verification 1).
//!
//! A token bound to project A must not read, count, update or delete a point
//! of project B — with crafted filters, a forged project field, an expired or
//! replayed token, or no token at all.
//!
//! The suite runs the real service over real mTLS against a REAL Qdrant:
//! `ELITEA_VECTOR_TEST_QDRANT_URL` is its gRPC URL (for example
//! `http://127.0.0.1:6334`). Without it every test skips, unless
//! `ELITEA_VECTOR_REQUIRE_QDRANT=1` (CI), which turns the skip into a
//! failure. Each test writes into its own collection.

use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use elitea_vector::auth::{
    Authenticator, CachePolicy, CachingIntrospector, GrpcIntrospector, IntrospectError,
    Introspector, TokenGrant, unix_now,
};
use elitea_vector::pb;
use elitea_vector::pb::vector_service_client::VectorServiceClient;
use elitea_vector::pb::vector_service_server::VectorServiceServer;
use elitea_vector::service::{SpaceLimits, VectorService};
use elitea_vector::store::{CollectionSettings, Store};
use elitea_vector::tls::{self, ReloadingCertificate};
use qdrant_client::Qdrant;
use rcgen::{BasicConstraints, CertificateParams, IsCa, Issuer, KeyPair, KeyUsagePurpose};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity, Server};
use tonic::{Code, Request};

const PROJECT_A: i64 = 101;
const PROJECT_B: i64 = 202;
const TOKEN_A: &str = "token-a";
const TOKEN_B: &str = "token-b";
const TOKEN_EXPIRED: &str = "token-expired";
const TOKEN_SHORT: &str = "token-short";
/// Project A's `DeepWiki` callback token (source `deepwiki` only).
const TOKEN_A_WIKI: &str = "token-a-wiki";
/// Memory callers (worker claim tokens with the `memory` source): two users of
/// project A, the first again in a shared conversation, a claim naming no
/// user, and the first user's id in project B.
const TOKEN_ALICE: &str = "token-alice";
const TOKEN_BOB: &str = "token-bob";
const TOKEN_ALICE_SHARED: &str = "token-alice-shared";
const TOKEN_NO_USER: &str = "token-no-user";
const TOKEN_ALICE_IN_B: &str = "token-alice-in-b";
const ALICE: i64 = 11;
const NAMESPACE_M: &str = "3a1f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5c";
const NAMESPACE_M2: &str = "4b2f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5d";
const NAMESPACE_A: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b";
const NAMESPACE_B: &str = "6f1e2d3c-4b5a-4968-8776-5a4b3c2d1e0f";
const DIMENSION: u32 = 4;

fn qdrant_url() -> Option<String> {
    let url = std::env::var("ELITEA_VECTOR_TEST_QDRANT_URL")
        .ok()
        .filter(|url| !url.is_empty());
    assert!(
        url.is_some() || std::env::var("ELITEA_VECTOR_REQUIRE_QDRANT").as_deref() != Ok("1"),
        "ELITEA_VECTOR_REQUIRE_QDRANT=1 but ELITEA_VECTOR_TEST_QDRANT_URL is not set"
    );
    url
}

macro_rules! require_qdrant {
    () => {
        match qdrant_url() {
            Some(url) => url,
            None => {
                eprintln!("skipped: ELITEA_VECTOR_TEST_QDRANT_URL is not set");
                return;
            }
        }
    };
}

// ── certificates ────────────────────────────────────────────────────────────

struct Pki {
    ca_pem: String,
    server: (String, String),
    admin: (String, String),
    worker: (String, String),
}

fn pki() -> Pki {
    elitea_vector::install_crypto_provider();
    let ca_key = KeyPair::generate().expect("ca key");
    let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    let ca_pem = ca_params.self_signed(&ca_key).expect("ca").pem();
    let issuer = Issuer::new(ca_params, ca_key);
    let leaf = |name: &str| {
        let key = KeyPair::generate().expect("leaf key");
        let params = CertificateParams::new(vec![name.to_owned()]).expect("leaf params");
        let pem = params.signed_by(&key, &issuer).expect("leaf").pem();
        (pem, key.serialize_pem())
    };
    Pki {
        ca_pem,
        server: leaf("localhost"),
        admin: leaf("elitea-main"),
        worker: leaf("elitea-worker"),
    }
}

// ── the token oracle (elitea-main's introspection, in process) ─────────────

struct Tokens(HashMap<String, TokenGrant>);

#[async_trait]
impl Introspector for Tokens {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        Ok(self.0.get(token).cloned())
    }
}

/// A worker claim token: the toolkit index writer and reader.
fn grant(project_id: i64, expires_at_unix: i64) -> TokenGrant {
    TokenGrant {
        project_id,
        principal: format!("user:{project_id}"),
        kind: pb::TokenKind::WorkerClaim,
        expires_at_unix,
        sources: vec![pb::Source::ToolkitIndex],
        personal_memory_allowed: true,
    }
}

/// A worker claim token that may use memory, for `user`.
fn memory_grant(project_id: i64, user: &str, personal: bool, expires_at_unix: i64) -> TokenGrant {
    TokenGrant {
        principal: user.to_owned(),
        sources: vec![pb::Source::ToolkitIndex, pb::Source::Memory],
        personal_memory_allowed: personal,
        ..grant(project_id, expires_at_unix)
    }
}

/// A `DeepWiki` engine callback token: the wiki writer and reader.
fn wiki_grant(project_id: i64, expires_at_unix: i64) -> TokenGrant {
    TokenGrant {
        kind: pb::TokenKind::EngineCallback,
        sources: vec![pb::Source::Deepwiki],
        ..grant(project_id, expires_at_unix)
    }
}

fn oracle() -> Arc<dyn Introspector> {
    let now = unix_now();
    let tokens = Tokens(HashMap::from([
        (TOKEN_A.to_owned(), grant(PROJECT_A, now + 3600)),
        (TOKEN_B.to_owned(), grant(PROJECT_B, now + 3600)),
        (TOKEN_EXPIRED.to_owned(), grant(PROJECT_A, now - 1)),
        (TOKEN_SHORT.to_owned(), grant(PROJECT_A, now + 3)),
        (TOKEN_A_WIKI.to_owned(), wiki_grant(PROJECT_A, now + 3600)),
        (
            TOKEN_ALICE.to_owned(),
            memory_grant(PROJECT_A, "user:11", true, now + 3600),
        ),
        (
            TOKEN_BOB.to_owned(),
            memory_grant(PROJECT_A, "user:12", true, now + 3600),
        ),
        (
            TOKEN_ALICE_SHARED.to_owned(),
            memory_grant(PROJECT_A, "user:11", false, now + 3600),
        ),
        (
            TOKEN_NO_USER.to_owned(),
            memory_grant(PROJECT_A, "service:scheduler", true, now + 3600),
        ),
        (
            TOKEN_ALICE_IN_B.to_owned(),
            memory_grant(PROJECT_B, "user:11", true, now + 3600),
        ),
    ]));
    Arc::new(CachingIntrospector::new(
        Arc::new(tokens),
        CachePolicy::default(),
        Arc::new(unix_now),
    ))
}

// ── the harness ─────────────────────────────────────────────────────────────

/// Serialises the tests that write. Every test uses the same two projects,
/// and a `Delete` without a space or a `DropProject` spans EVERY collection of
/// its project, as it must: run side by side, one test's clean-up removes
/// another test's points.
static SERIAL: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

struct Harness {
    slug: String,
    worker: VectorServiceClient<Channel>,
    admin: VectorServiceClient<Channel>,
    qdrant: Qdrant,
    _serial: tokio::sync::MutexGuard<'static, ()>,
}

fn unique_slug(prefix: &str) -> String {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_nanos());
    format!(
        "{prefix}-{nanos}-{}",
        COUNTER.fetch_add(1, Ordering::SeqCst)
    )
}

async fn serve(url: &str, introspector: Arc<dyn Introspector>, pki: &Pki) -> SocketAddr {
    serve_with(
        url,
        introspector,
        pki,
        CollectionSettings {
            // The suite's collections pile up in a Qdrant that outlives a run.
            max_collections: 100_000,
            ..CollectionSettings::default()
        },
        SpaceLimits::default(),
    )
    .await
}

/// Serves through the production listener: [`tls::tls_incoming`] over a
/// [`ReloadingCertificate`].
async fn serve_with(
    url: &str,
    introspector: Arc<dyn Introspector>,
    pki: &Pki,
    settings: CollectionSettings,
    limits: SpaceLimits,
) -> SocketAddr {
    let store = Arc::new(Store::new(
        Qdrant::from_url(url)
            .skip_compatibility_check()
            .build()
            .expect("qdrant client"),
        settings,
    ));
    let auth = Authenticator::new(introspector, HashSet::from(["dns:elitea-main".to_owned()]));
    let service = VectorService::new(auth, store).with_limits(limits);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let dir = std::env::temp_dir().join(unique_slug("elitea-vector-suite"));
    std::fs::create_dir_all(&dir).expect("scratch dir");
    std::fs::write(dir.join("tls.crt"), &pki.server.0).expect("cert");
    std::fs::write(dir.join("tls.key"), &pki.server.1).expect("key");
    let certificate = ReloadingCertificate::load(&dir.join("tls.crt"), &dir.join("tls.key"))
        .expect("certificate");
    let config = tls::server_config(certificate, pki.ca_pem.as_bytes()).expect("tls config");
    tokio::spawn(
        Server::builder()
            .add_service(VectorServiceServer::new(service))
            .serve_with_incoming(tls::tls_incoming(listener, config)),
    );
    address
}

async fn client(
    address: SocketAddr,
    pki: &Pki,
    identity: &(String, String),
) -> VectorServiceClient<Channel> {
    let tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(&pki.ca_pem))
        .identity(Identity::from_pem(&identity.0, &identity.1))
        .domain_name("localhost");
    let channel = Channel::from_shared(format!("https://{address}"))
        .expect("endpoint")
        .tls_config(tls)
        .expect("client tls")
        .connect()
        .await
        .expect("connect");
    VectorServiceClient::new(channel)
}

async fn harness() -> Option<Harness> {
    harness_with(oracle).await
}

/// `introspector` is built after the serial lock is taken, so expiries it
/// computes from the clock do not age while the test waits its turn.
async fn harness_with(introspector: impl FnOnce() -> Arc<dyn Introspector>) -> Option<Harness> {
    harness_custom(introspector, |_| {
        (
            CollectionSettings {
                max_collections: 100_000,
                ..CollectionSettings::default()
            },
            SpaceLimits::default(),
        )
    })
    .await
}

/// As [`harness_with`], with collection settings and space limits chosen
/// after the serial lock is taken (they may depend on Qdrant's current
/// state, which only a lock holder can trust).
async fn harness_custom(
    introspector: impl FnOnce() -> Arc<dyn Introspector>,
    configure: impl FnOnce(&str) -> (CollectionSettings, SpaceLimits),
) -> Option<Harness> {
    let url = qdrant_url()?;
    let serial = SERIAL.lock().await;
    let pki = pki();
    let (settings, limits) = configure(&url);
    let address = serve_with(&url, introspector(), &pki, settings, limits).await;
    Some(Harness {
        _serial: serial,
        slug: unique_slug("iso"),
        worker: client(address, &pki, &pki.worker).await,
        admin: client(address, &pki, &pki.admin).await,
        qdrant: Qdrant::from_url(&url)
            .skip_compatibility_check()
            .build()
            .expect("qdrant"),
    })
}

fn with_token<T>(token: &str, message: T) -> Request<T> {
    let mut request = Request::new(message);
    request.metadata_mut().insert(
        "authorization",
        format!("Bearer {token}").parse().expect("metadata"),
    );
    request
}

impl Harness {
    fn space(&self) -> pb::EmbeddingSpace {
        pb::EmbeddingSpace {
            model_slug: self.slug.clone(),
            dimension: DIMENSION,
        }
    }

    fn namespace(namespace_id: &str) -> pb::Namespace {
        pb::Namespace {
            source: pb::Source::ToolkitIndex as i32,
            namespace_id: namespace_id.to_owned(),
            generation: String::new(),
            memory_scope: 0,
        }
    }

    fn point(document_key: &str, text: &str, vector: [f32; 4]) -> pb::Point {
        pb::Point {
            document_key: document_key.to_owned(),
            document_version: "1".to_owned(),
            chunk_id: "0".to_owned(),
            chunk_type: "text".to_owned(),
            acl: vec!["group:all".to_owned()],
            text: text.to_owned(),
            metadata_json: format!("{{\"owner\":\"{document_key}\"}}"),
            vector: vector.to_vec(),
            ..pb::Point::default()
        }
    }

    async fn upsert(
        &mut self,
        token: &str,
        project_id: i64,
        namespace: pb::Namespace,
        points: Vec<pb::Point>,
    ) -> Result<pb::UpsertResponse, tonic::Status> {
        let space = self.space();
        self.upsert_in(token, project_id, space, namespace, points)
            .await
    }

    async fn upsert_in(
        &mut self,
        token: &str,
        project_id: i64,
        space: pb::EmbeddingSpace,
        namespace: pb::Namespace,
        points: Vec<pb::Point>,
    ) -> Result<pb::UpsertResponse, tonic::Status> {
        let messages = vec![
            pb::UpsertRequest {
                message: Some(pb::upsert_request::Message::Header(pb::UpsertHeader {
                    project_id,
                    space: Some(space),
                    namespace: Some(namespace),
                })),
            },
            pb::UpsertRequest {
                message: Some(pb::upsert_request::Message::Batch(pb::UpsertBatch {
                    points,
                })),
            },
        ];
        self.worker
            .upsert(with_token(token, tokio_stream::iter(messages)))
            .await
            .map(tonic::Response::into_inner)
    }

    /// Project A and project B each write two documents. B writes into
    /// B's namespace and ALSO into A's namespace id, with A's document keys:
    /// a namespace id is only an argument and must not cross projects.
    async fn seed(&mut self) {
        for (token, namespace, prefix, vector) in [
            (TOKEN_A, NAMESPACE_A, "a", [1.0, 0.0, 0.0, 0.0]),
            (TOKEN_B, NAMESPACE_B, "b", [1.0, 0.0, 0.0, 0.0]),
            (TOKEN_B, NAMESPACE_A, "a", [1.0, 0.0, 0.0, 0.0]),
        ] {
            let text_owner = if token == TOKEN_A { "alpha" } else { "bravo" };
            let points = vec![
                Self::point(
                    &format!("{prefix}-doc-1"),
                    &format!("{text_owner} shared secret"),
                    vector,
                ),
                Self::point(
                    &format!("{prefix}-doc-2"),
                    &format!("{text_owner} other words"),
                    [0.0, 1.0, 0.0, 0.0],
                ),
            ];
            let written = self
                .upsert(token, 0, Self::namespace(namespace), points)
                .await
                .expect("seed upsert");
            assert_eq!(written.upserted, 2);
        }
    }

    fn scope(namespaces: &[&str]) -> pb::Scope {
        pb::Scope {
            project_id: 0,
            source: pb::Source::ToolkitIndex as i32,
            namespace_ids: namespaces.iter().map(|n| (*n).to_owned()).collect(),
            ..pb::Scope::default()
        }
    }

    async fn search(
        &mut self,
        token: &str,
        scope: pb::Scope,
        filter: Option<pb::Filter>,
    ) -> Result<Vec<pb::ScoredPoint>, tonic::Status> {
        self.worker
            .search(with_token(
                token,
                pb::SearchRequest {
                    scope: Some(scope),
                    space: Some(self.space()),
                    vector: vec![1.0, 0.0, 0.0, 0.0],
                    filter,
                    limit: 100,
                    score_threshold: None,
                },
            ))
            .await
            .map(|response| response.into_inner().points)
    }

    /// The number of points of `project` in the collection, read from Qdrant
    /// directly (the ground truth, not the service).
    async fn truth(&self, project: i64) -> u64 {
        let filter =
            qdrant_client::qdrant::Filter::must([qdrant_client::qdrant::Condition::matches(
                "project_id",
                project.to_string(),
            )]);
        self.qdrant
            .count(
                qdrant_client::qdrant::CountPointsBuilder::new(format!(
                    "emb_{}_{DIMENSION}",
                    self.slug
                ))
                .filter(filter)
                .exact(true),
            )
            .await
            .expect("count")
            .result
            .map_or(0, |result| result.count)
    }

    async fn texts_of_b(&self) -> Vec<String> {
        let filter =
            qdrant_client::qdrant::Filter::must([qdrant_client::qdrant::Condition::matches(
                "project_id",
                PROJECT_B.to_string(),
            )]);
        let scrolled = self
            .qdrant
            .scroll(
                qdrant_client::qdrant::ScrollPointsBuilder::new(format!(
                    "emb_{}_{DIMENSION}",
                    self.slug
                ))
                .filter(filter)
                .limit(100)
                .with_payload(true),
            )
            .await
            .expect("scroll");
        let mut texts: Vec<String> = scrolled
            .result
            .into_iter()
            .filter_map(|point| {
                point
                    .payload
                    .get("text")
                    .and_then(|value| value.as_str().cloned())
            })
            .collect();
        texts.sort();
        texts
    }
}

fn only_project(points: &[pb::ScoredPoint], project: i64) {
    assert!(!points.is_empty(), "expected some points");
    for point in points {
        assert_eq!(point.project_id, project, "leaked point {point:?}");
    }
}

// ── reads ───────────────────────────────────────────────────────────────────

#[tokio::test]
#[allow(clippy::too_many_lines)] // One scenario, every read RPC in turn.
async fn a_token_reads_only_its_own_project() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;

    // Its own namespace: A's points only, although B wrote into the same
    // namespace id with the same document keys.
    let points = h
        .search(TOKEN_A, Harness::scope(&[NAMESPACE_A]), None)
        .await
        .expect("search");
    only_project(&points, PROJECT_A);
    assert_eq!(points.len(), 2);
    assert!(points.iter().all(|point| point.text.starts_with("alpha")));

    // B's namespace: nothing.
    let points = h
        .search(TOKEN_A, Harness::scope(&[NAMESPACE_B]), None)
        .await
        .expect("search");
    assert!(points.is_empty(), "{points:?}");

    // Hybrid: A only, even for a term only B's text has.
    let hybrid = h
        .worker
        .hybrid_search(with_token(
            TOKEN_A,
            pb::HybridSearchRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A, NAMESPACE_B])),
                space: Some(h.space()),
                vector: vec![1.0, 0.0, 0.0, 0.0],
                text: "bravo secret".to_owned(),
                limit: 100,
                ..pb::HybridSearchRequest::default()
            },
        ))
        .await
        .expect("hybrid")
        .into_inner()
        .points;
    only_project(&hybrid, PROJECT_A);

    // ListIndexes: A's namespace only.
    let indexes = h
        .worker
        .list_indexes(with_token(TOKEN_A, pb::ListIndexesRequest::default()))
        .await
        .expect("list")
        .into_inner()
        .indexes;
    let mine: Vec<_> = indexes
        .iter()
        .filter(|index| {
            index
                .collection
                .ends_with(&format!("{}_{DIMENSION}", h.slug))
        })
        .collect();
    assert_eq!(mine.len(), 1, "{indexes:?}");
    assert_eq!(mine[0].namespace_id, NAMESPACE_A);
    assert_eq!(mine[0].point_count, 2);

    // ListDocuments: A's documents of the shared namespace id only, with
    // their versions and chunk counts; B's copies under the same keys are
    // invisible.
    let mut pages = h
        .worker
        .list_documents(with_token(
            TOKEN_A,
            pb::ListDocumentsRequest {
                project_id: 0,
                namespace: Some(Harness::namespace(NAMESPACE_A)),
                // This test's collection: the namespace id is shared with
                // other tests' collections on the same Qdrant.
                space: Some(h.space()),
                max_documents: 0,
            },
        ))
        .await
        .expect("list documents")
        .into_inner();
    let mut documents = Vec::new();
    while let Some(page) = pages.message().await.expect("page") {
        documents.extend(page.documents);
    }
    let listed: Vec<(String, String, u64)> = documents
        .iter()
        .map(|d| {
            (
                d.document_key.clone(),
                d.document_version.clone(),
                d.chunk_count,
            )
        })
        .collect();
    assert_eq!(
        listed,
        vec![
            ("a-doc-1".to_owned(), "1".to_owned(), 1),
            ("a-doc-2".to_owned(), "1".to_owned(), 1),
        ]
    );
    // B's token over B's namespace sees B's two documents, and an unknown
    // namespace lists nothing.
    let mut pages = h
        .worker
        .list_documents(with_token(
            TOKEN_B,
            pb::ListDocumentsRequest {
                project_id: 0,
                namespace: Some(Harness::namespace(NAMESPACE_B)),
                space: Some(h.space()),
                max_documents: 0,
            },
        ))
        .await
        .expect("list documents")
        .into_inner();
    let mut count = 0;
    while let Some(page) = pages.message().await.expect("page") {
        count += page.documents.len();
    }
    assert_eq!(count, 2);
}

/// `ListDocuments.max_documents` bounds the server's work (ADR-0030 round 2):
/// the listing is refused with `RESOURCE_EXHAUSTED` as soon as the distinct
/// documents read exceed it, before any page goes out; at or under the bound,
/// and without one, the listing is complete.
#[tokio::test]
async fn a_listing_over_its_document_bound_is_refused_before_the_first_page() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    // Project A holds two documents in namespace A (B's writes under the same
    // namespace id are B's and never counted for A).
    let list = |h: &mut Harness, max_documents: u64| {
        let request = pb::ListDocumentsRequest {
            project_id: 0,
            namespace: Some(Harness::namespace(NAMESPACE_A)),
            space: Some(h.space()),
            max_documents,
        };
        let mut worker = h.worker.clone();
        async move {
            let mut pages = worker
                .list_documents(with_token(TOKEN_A, request))
                .await?
                .into_inner();
            let mut documents = Vec::new();
            while let Some(page) = pages.message().await? {
                documents.extend(page.documents);
            }
            Ok::<_, tonic::Status>(documents)
        }
    };
    let refused = list(&mut h, 1)
        .await
        .expect_err("two documents exceed a bound of one");
    assert_eq!(refused.code(), Code::ResourceExhausted, "{refused:?}");
    assert!(
        refused.message().contains("more than 1 documents"),
        "{}",
        refused.message()
    );
    for bound in [2, 1000, 0] {
        let documents = list(&mut h, bound).await.expect("within the bound");
        assert_eq!(documents.len(), 2, "bound {bound}: {documents:?}");
    }
}

#[tokio::test]
async fn crafted_filters_cannot_widen_the_scope() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    let scope = Harness::scope(&[NAMESPACE_A, NAMESPACE_B]);

    // Naming a scope key is refused outright.
    for key in ["project_id", "source", "namespace_id", "generation", "text"] {
        let filter = pb::Filter {
            should: vec![pb::Condition {
                key: key.to_owned(),
                r#match: Some(pb::condition::Match::Keyword(PROJECT_B.to_string())),
            }],
            ..pb::Filter::default()
        };
        let status = h
            .search(TOKEN_A, scope.clone(), Some(filter))
            .await
            .expect_err(key);
        assert_eq!(status.code(), Code::InvalidArgument, "{key}");
    }

    // A `should` that matches only B's points, a `must_not` that excludes
    // every A point, and a metadata path: none adds a B point.
    let crafted = [
        pb::Filter {
            should: vec![pb::Condition {
                key: "document_key".to_owned(),
                r#match: Some(pb::condition::Match::Any(pb::StringList {
                    values: vec!["b-doc-1".to_owned(), "a-doc-1".to_owned()],
                })),
            }],
            ..pb::Filter::default()
        },
        pb::Filter {
            must_not: vec![pb::Condition {
                key: "metadata.owner".to_owned(),
                r#match: Some(pb::condition::Match::Keyword("a-doc-1".to_owned())),
            }],
            ..pb::Filter::default()
        },
        pb::Filter {
            should: vec![pb::Condition {
                key: "metadata.project_id".to_owned(),
                r#match: Some(pb::condition::Match::Keyword(PROJECT_B.to_string())),
            }],
            ..pb::Filter::default()
        },
    ];
    for filter in crafted {
        let points = h
            .search(TOKEN_A, scope.clone(), Some(filter.clone()))
            .await
            .expect("search");
        for point in &points {
            assert_eq!(point.project_id, PROJECT_A, "{filter:?} leaked {point:?}");
        }
        let counted = h
            .worker
            .count(with_token(
                TOKEN_A,
                pb::CountRequest {
                    scope: Some(scope.clone()),
                    space: Some(h.space()),
                    filter: Some(filter.clone()),
                },
            ))
            .await
            .expect("count")
            .into_inner()
            .count;
        assert!(counted <= 2, "{filter:?} counted {counted}");
    }
}

fn nested(filter: pb::Filter) -> pb::Condition {
    pb::Condition {
        key: String::new(),
        r#match: Some(pb::condition::Match::Filter(filter)),
    }
}

fn keyword(key: &str, value: &str) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Keyword(value.to_owned())),
    }
}

/// Nested `should`/`must_not` shapes that would match every B point if they
/// replaced the scope.
fn crafted_nested_filters() -> Vec<pb::Filter> {
    vec![
        // (not a-doc-1) or (not anything): true for every point.
        pb::Filter {
            should: vec![
                nested(pb::Filter {
                    must_not: vec![keyword("document_key", "a-doc-1")],
                    ..pb::Filter::default()
                }),
                nested(pb::Filter {
                    must_not: vec![keyword("chunk_type", "text")],
                    ..pb::Filter::default()
                }),
            ],
            ..pb::Filter::default()
        },
        // not (not b-doc-1): only b-doc-1, which exists in A too.
        pb::Filter {
            must_not: vec![nested(pb::Filter {
                must_not: vec![keyword("document_key", "b-doc-1")],
                ..pb::Filter::default()
            })],
            ..pb::Filter::default()
        },
        // metadata is empty or not: always true.
        pb::Filter {
            should: vec![
                pb::Condition {
                    key: "metadata.owner".to_owned(),
                    r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
                },
                nested(pb::Filter {
                    must_not: vec![pb::Condition {
                        key: "metadata.owner".to_owned(),
                        r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
                    }],
                    ..pb::Filter::default()
                }),
            ],
            ..pb::Filter::default()
        },
    ]
}

#[tokio::test]
async fn nested_filters_cannot_widen_the_scope_or_name_a_scope_key() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    let scope = Harness::scope(&[NAMESPACE_A, NAMESPACE_B]);

    // A scope key is refused at every depth, and as a presence check too.
    for key in ["project_id", "source", "namespace_id", "generation", "text"] {
        for leaf in [
            keyword(key, &PROJECT_B.to_string()),
            pb::Condition {
                key: key.to_owned(),
                r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
            },
        ] {
            let mut filter = pb::Filter {
                should: vec![leaf],
                ..pb::Filter::default()
            };
            for _ in 0..3 {
                filter = pb::Filter {
                    must_not: vec![nested(filter)],
                    ..pb::Filter::default()
                };
                let status = h
                    .search(TOKEN_A, scope.clone(), Some(filter.clone()))
                    .await
                    .expect_err(key);
                assert_eq!(status.code(), Code::InvalidArgument, "{key}");
            }
        }
    }

    // Deeper than 8 levels is refused.
    let mut deep = pb::Filter {
        must: vec![keyword("chunk_type", "text")],
        ..pb::Filter::default()
    };
    for _ in 0..8 {
        deep = pb::Filter {
            should: vec![nested(deep)],
            ..pb::Filter::default()
        };
    }
    let status = h
        .search(TOKEN_A, scope.clone(), Some(deep))
        .await
        .expect_err("too deep");
    assert_eq!(status.code(), Code::InvalidArgument);

    // Nested should/must_not shapes that would match every B point if they
    // replaced the scope: the scope still holds.
    let crafted = crafted_nested_filters();
    for filter in crafted {
        let points = h
            .search(TOKEN_A, scope.clone(), Some(filter.clone()))
            .await
            .expect("search");
        for point in &points {
            assert_eq!(point.project_id, PROJECT_A, "{filter:?} leaked {point:?}");
        }
        let counted = h
            .worker
            .count(with_token(
                TOKEN_A,
                pb::CountRequest {
                    scope: Some(scope.clone()),
                    space: Some(h.space()),
                    filter: Some(filter.clone()),
                },
            ))
            .await
            .expect("count")
            .into_inner()
            .count;
        assert!(counted <= 2, "{filter:?} counted {counted}");
    }
}

/// Filters over `metadata.year` and `metadata.lang`, and the document keys
/// each must select.
fn range_cases() -> Vec<(pb::Filter, Vec<&'static str>)> {
    let range = |range: pb::Range| pb::Condition {
        key: "metadata.year".to_owned(),
        r#match: Some(pb::condition::Match::Range(range)),
    };
    let empty = |key: &str| pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
    };
    vec![
        (
            pb::Filter {
                must: vec![range(pb::Range {
                    gte: Some(2024.0),
                    ..pb::Range::default()
                })],
                ..pb::Filter::default()
            },
            vec!["y2024", "y2025"],
        ),
        (
            pb::Filter {
                must: vec![range(pb::Range {
                    gt: Some(2020.0),
                    lt: Some(2025.0),
                    ..pb::Range::default()
                })],
                ..pb::Filter::default()
            },
            vec!["y2024"],
        ),
        // lang present and not "en": the SDK's $ne (a missing or null key
        // never matches).
        (
            pb::Filter {
                must_not: vec![empty("metadata.lang"), keyword("metadata.lang", "en")],
                ..pb::Filter::default()
            },
            vec!["y2024"],
        ),
        // (year < 2021) or (lang == de and year >= 2024)
        (
            pb::Filter {
                should: vec![
                    range(pb::Range {
                        lt: Some(2021.0),
                        ..pb::Range::default()
                    }),
                    nested(pb::Filter {
                        must: vec![
                            keyword("metadata.lang", "de"),
                            range(pb::Range {
                                gte: Some(2024.0),
                                ..pb::Range::default()
                            }),
                        ],
                        ..pb::Filter::default()
                    }),
                ],
                ..pb::Filter::default()
            },
            vec!["y2020", "y2024"],
        ),
        (
            pb::Filter {
                must: vec![pb::Condition {
                    key: "metadata.lang".to_owned(),
                    r#match: Some(pb::condition::Match::IsNull(pb::IsNull {})),
                }],
                ..pb::Filter::default()
            },
            vec!["none"],
        ),
    ]
}

#[tokio::test]
async fn range_presence_and_nested_conditions_match_like_qdrant() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let point = |key: &str, metadata: &str| pb::Point {
        metadata_json: metadata.to_owned(),
        ..Harness::point(key, key, [1.0, 0.0, 0.0, 0.0])
    };
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![
            point("y2020", r#"{"year": 2020, "lang": "en"}"#),
            point("y2024", r#"{"year": 2024, "lang": "de"}"#),
            point("y2025", r#"{"year": 2025.5}"#),
            point("none", r#"{"lang": null}"#),
        ],
    )
    .await
    .expect("upsert");
    let scope = Harness::scope(&[NAMESPACE_A]);
    for (filter, expected) in range_cases() {
        let points = h
            .worker
            .search(with_token(
                TOKEN_A,
                pb::SearchRequest {
                    scope: Some(scope.clone()),
                    space: Some(h.space()),
                    vector: vec![1.0, 0.0, 0.0, 0.0],
                    filter: Some(filter.clone()),
                    limit: 100,
                    score_threshold: None,
                },
            ))
            .await
            .expect("search")
            .into_inner()
            .points;
        let mut found: Vec<&str> = points.iter().map(|p| p.document_key.as_str()).collect();
        found.sort_unstable();
        assert_eq!(found, expected, "{filter:?}");
    }
}

#[tokio::test]
async fn active_generations_exclude_a_build_in_progress() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    for (generation, key) in [("g1", "published"), ("g2", "building"), ("", "plain")] {
        h.upsert(
            TOKEN_A,
            0,
            pb::Namespace {
                generation: generation.to_owned(),
                ..Harness::namespace(NAMESPACE_A)
            },
            vec![Harness::point(key, key, [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect("upsert");
    }
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_B),
        vec![Harness::point("other", "other", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("upsert");
    let scope = pb::Scope {
        active_generations: vec![pb::ActiveGeneration {
            namespace_id: NAMESPACE_A.to_owned(),
            generation: "g1".to_owned(),
        }],
        ..Harness::scope(&[NAMESPACE_A, NAMESPACE_B])
    };
    let points = h
        .search(TOKEN_A, scope.clone(), None)
        .await
        .expect("search");
    let mut found: Vec<&str> = points.iter().map(|p| p.document_key.as_str()).collect();
    found.sort_unstable();
    assert_eq!(found, ["other", "plain", "published"]);
    // An active generation for a namespace the scope does not list.
    let stray = pb::Scope {
        namespace_ids: vec![NAMESPACE_B.to_owned()],
        ..scope
    };
    let status = h.search(TOKEN_A, stray, None).await.expect_err("stray");
    assert_eq!(status.code(), Code::InvalidArgument);
}

#[tokio::test]
async fn hybrid_results_carry_their_dense_cosine_score() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![
            Harness::point("near", "nothing in common", [1.0, 0.0, 0.0, 0.0]),
            Harness::point("half", "nothing either", [1.0, 1.0, 0.0, 0.0]),
            Harness::point("far", "zebra quartz", [0.0, 0.0, 1.0, 0.0]),
        ],
    )
    .await
    .expect("upsert");
    let points = h
        .worker
        .hybrid_search(with_token(
            TOKEN_A,
            pb::HybridSearchRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A])),
                space: Some(h.space()),
                vector: vec![1.0, 0.0, 0.0, 0.0],
                text: "zebra".to_owned(),
                limit: 10,
                ..pb::HybridSearchRequest::default()
            },
        ))
        .await
        .expect("hybrid")
        .into_inner()
        .points;
    assert_eq!(points.len(), 3, "{points:?}");
    for point in &points {
        let dense = point
            .dense_score
            .expect("every hybrid point has a dense score");
        let expected = match point.document_key.as_str() {
            "near" => 1.0,
            "half" => std::f32::consts::FRAC_1_SQRT_2,
            _ => 0.0,
        };
        // float16 storage and int8 quantization are not used by exact search
        // over the originals, but the originals are float16.
        assert!((dense - expected).abs() < 0.01, "{point:?}");
        assert!(
            point.score < 0.1,
            "the fused score is rank-based: {point:?}"
        );
    }
    // A dense search reports the same score twice, and the hybrid dense
    // score equals it for every point: both are the cosine of the stored
    // (float16, normalised) vector against the query, one computed by
    // Qdrant and one by the service from the returned vector.
    let dense = h
        .search(TOKEN_A, Harness::scope(&[NAMESPACE_A]), None)
        .await
        .expect("search");
    assert_eq!(dense.len(), 3);
    assert!(dense.iter().all(|p| p.dense_score == Some(p.score)));
    for point in &points {
        let reference = dense
            .iter()
            .find(|p| p.id == point.id)
            .unwrap_or_else(|| panic!("{} not in the dense search", point.document_key));
        let delta = (point.dense_score.expect("dense") - reference.score).abs();
        assert!(delta < 1e-3, "{}: {delta} off", point.document_key);
    }
}

#[tokio::test]
async fn a_point_with_a_numeric_id_gets_its_hybrid_dense_score() {
    // Points the service writes have UUID ids, but Qdrant also allows
    // integer ids and the facade must not depend on the id's form: the
    // dense score comes from the point's own vector, not from a second
    // query by id.
    let url = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point(
            "written",
            "ordinary words",
            [1.0, 0.0, 0.0, 0.0],
        )],
    )
    .await
    .expect("upsert");
    let qdrant = raw_qdrant(&url);
    let collection = format!("emb_{}_{DIMENSION}", h.slug);
    let mut payload = qdrant_client::Payload::new();
    payload.insert("project_id", PROJECT_A.to_string());
    payload.insert("source", "toolkit_index");
    payload.insert("namespace_id", NAMESPACE_A);
    payload.insert("document_key", "numeric");
    payload.insert("document_version", "1");
    payload.insert("parent_id", "");
    payload.insert("chunk_id", "0");
    payload.insert("chunk_type", "text");
    payload.insert(
        "acl",
        qdrant_client::qdrant::Value::from(serde_json::json!(["group:all"])),
    );
    payload.insert("text", "zebra quartz");
    payload.insert(
        "metadata",
        qdrant_client::qdrant::Value::from(serde_json::json!({})),
    );
    let vectors = qdrant_client::qdrant::NamedVectors::default()
        .add_vector(
            "dense",
            qdrant_client::qdrant::Vector::new_dense(vec![1.0, 1.0, 0.0, 0.0]),
        )
        .add_vector(
            "bm25",
            qdrant_client::qdrant::Document::new("zebra quartz", "qdrant/bm25"),
        );
    qdrant
        .upsert_points(
            qdrant_client::qdrant::UpsertPointsBuilder::new(
                &collection,
                vec![qdrant_client::qdrant::PointStruct::new(
                    7_u64, vectors, payload,
                )],
            )
            .wait(true),
        )
        .await
        .expect("raw upsert");
    let points = h
        .worker
        .hybrid_search(with_token(
            TOKEN_A,
            pb::HybridSearchRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A])),
                space: Some(h.space()),
                vector: vec![1.0, 0.0, 0.0, 0.0],
                text: "zebra".to_owned(),
                limit: 10,
                ..pb::HybridSearchRequest::default()
            },
        ))
        .await
        .expect("hybrid")
        .into_inner()
        .points;
    assert_eq!(points.len(), 2, "{points:?}");
    let numeric = points
        .iter()
        .find(|p| p.document_key == "numeric")
        .expect("the numeric-id point");
    assert_eq!(numeric.id, "7");
    let dense = numeric.dense_score.expect("dense score");
    assert!(
        (dense - std::f32::consts::FRAC_1_SQRT_2).abs() < 0.01,
        "{numeric:?}"
    );
    let written = points
        .iter()
        .find(|p| p.document_key == "written")
        .expect("the written point");
    assert!((written.dense_score.expect("dense") - 1.0).abs() < 0.01);
}

// ── the forged project field ────────────────────────────────────────────────

#[tokio::test]
async fn a_forged_project_field_is_refused() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    let before = h.truth(PROJECT_B).await;

    let forged = pb::Scope {
        project_id: PROJECT_B,
        ..Harness::scope(&[NAMESPACE_B])
    };
    let status = h
        .search(TOKEN_A, forged.clone(), None)
        .await
        .expect_err("search");
    assert_eq!(status.code(), Code::PermissionDenied);

    let status = h
        .worker
        .count(with_token(
            TOKEN_A,
            pb::CountRequest {
                scope: Some(forged),
                space: None,
                filter: None,
            },
        ))
        .await
        .expect_err("count");
    assert_eq!(status.code(), Code::PermissionDenied);

    let status = h
        .worker
        .list_indexes(with_token(
            TOKEN_A,
            pb::ListIndexesRequest {
                project_id: PROJECT_B,
                source: 0,
            },
        ))
        .await
        .expect_err("list");
    assert_eq!(status.code(), Code::PermissionDenied);

    let status = h
        .upsert(
            TOKEN_A,
            PROJECT_B,
            Harness::namespace(NAMESPACE_B),
            vec![Harness::point(
                "b-doc-1",
                "overwritten",
                [0.0, 0.0, 1.0, 0.0],
            )],
        )
        .await
        .expect_err("upsert");
    assert_eq!(status.code(), Code::PermissionDenied);

    let status = h
        .worker
        .delete(with_token(
            TOKEN_A,
            pb::DeleteRequest {
                project_id: PROJECT_B,
                space: None,
                namespace: Some(Harness::namespace(NAMESPACE_B)),
                selector: Some(pb::delete_request::Selector::WholeNamespace(
                    pb::WholeNamespace {},
                )),
            },
        ))
        .await
        .expect_err("delete");
    assert_eq!(status.code(), Code::PermissionDenied);

    // The metadata cannot carry a project either: it stays metadata.
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_B),
        vec![pb::Point {
            metadata_json: format!("{{\"project_id\":\"{PROJECT_B}\"}}"),
            ..Harness::point("forged", "alpha forged", [0.0, 0.0, 0.0, 1.0])
        }],
    )
    .await
    .expect("own upsert");
    assert_eq!(h.truth(PROJECT_B).await, before);
    assert_eq!(h.truth(PROJECT_A).await, 3);
}

// ── updates and deletes ─────────────────────────────────────────────────────

/// Point ids carry the chunk type for every type, and are never recomputed to
/// find existing points: a document is replaced by a delete on its key and a
/// fresh upsert, an unchanged one is left alone. Points under a different id
/// rule coexist with new ones.
#[tokio::test(flavor = "multi_thread")]
async fn chunks_of_one_passage_are_separate_points_and_replacement_is_by_filter() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let typed = |key: &str, chunk_type: &str| {
        let mut point = Harness::point(key, chunk_type, [1.0, 0.0, 0.0, 0.0]);
        chunk_type.clone_into(&mut point.chunk_type);
        point.chunk_id = "3".to_owned();
        point
    };
    // The collection exists once something is written to it.
    h.seed().await;
    let before = h.truth(PROJECT_A).await;
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![
            typed("p-doc", "document"),
            typed("p-doc", "title"),
            typed("p-doc", "summary"),
            typed("p-doc", "anything-else"),
            typed("p-other", "document"),
        ],
    )
    .await
    .expect("upsert");
    assert_eq!(
        h.truth(PROJECT_A).await,
        before + 5,
        "five chunks, five points"
    );

    // Replace p-doc: delete by filter, upsert fewer chunks. p-other is not
    // rewritten and stays.
    h.worker
        .delete(with_token(
            TOKEN_A,
            pb::DeleteRequest {
                project_id: 0,
                space: None,
                namespace: Some(Harness::namespace(NAMESPACE_A)),
                selector: Some(pb::delete_request::Selector::Documents(pb::DocumentKeys {
                    document_keys: vec!["p-doc".to_owned()],
                })),
            },
        ))
        .await
        .expect("delete");
    assert_eq!(h.truth(PROJECT_A).await, before + 1);
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![typed("p-doc", "document"), typed("p-doc", "title")],
    )
    .await
    .expect("upsert");
    assert_eq!(h.truth(PROJECT_A).await, before + 3);
}

#[tokio::test]
async fn a_token_cannot_update_or_delete_another_projects_points() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    let b_texts = h.texts_of_b().await;
    assert_eq!(b_texts.len(), 4);

    // An upsert with B's namespace and B's document keys writes A's own
    // points; B's are not overwritten.
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_B),
        vec![Harness::point(
            "b-doc-1",
            "alpha overwrite",
            [0.0, 0.0, 1.0, 0.0],
        )],
    )
    .await
    .expect("upsert");
    assert_eq!(h.texts_of_b().await, b_texts);

    // Every delete selector, aimed at B's namespace and keys.
    let selectors = [
        pb::delete_request::Selector::WholeNamespace(pb::WholeNamespace {}),
        pb::delete_request::Selector::Documents(pb::DocumentKeys {
            document_keys: vec!["b-doc-1".to_owned(), "a-doc-1".to_owned()],
        }),
        pb::delete_request::Selector::Generations(pb::Generations {
            generations: vec!["g1".to_owned()],
        }),
        pb::delete_request::Selector::GenerationsExcept(pb::GenerationsExcept {
            keep: "nothing".to_owned(),
        }),
    ];
    for namespace in [NAMESPACE_B, NAMESPACE_A] {
        for selector in selectors.clone() {
            h.worker
                .delete(with_token(
                    TOKEN_A,
                    pb::DeleteRequest {
                        project_id: 0,
                        space: None,
                        namespace: Some(Harness::namespace(namespace)),
                        selector: Some(selector),
                    },
                ))
                .await
                .expect("own-scope delete");
        }
    }
    assert_eq!(h.texts_of_b().await, b_texts);
    assert_eq!(h.truth(PROJECT_A).await, 0, "A's own points are gone");

    // DropProject is for administrators only.
    let status = h
        .worker
        .drop_project(with_token(
            TOKEN_A,
            pb::DropProjectRequest {
                project_id: PROJECT_B,
            },
        ))
        .await
        .expect_err("drop");
    assert_eq!(status.code(), Code::PermissionDenied);
    assert_eq!(h.truth(PROJECT_B).await, 4);
}

// ── tokens ──────────────────────────────────────────────────────────────────

#[tokio::test]
async fn missing_expired_and_unknown_tokens_are_refused() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let scope = Harness::scope(&[NAMESPACE_A]);

    // A replayed token past its expiry: admitted, cached, then refused.
    h.search(TOKEN_SHORT, scope.clone(), None)
        .await
        .expect("fresh");
    tokio::time::sleep(Duration::from_millis(3200)).await;
    let status = h
        .search(TOKEN_SHORT, scope.clone(), None)
        .await
        .expect_err("replayed");
    assert_eq!(status.code(), Code::Unauthenticated);

    h.seed().await;

    // No token from a non-administrator certificate: every RPC refused.
    let status = h
        .worker
        .search(pb::SearchRequest {
            scope: Some(scope.clone()),
            space: Some(h.space()),
            vector: vec![1.0, 0.0, 0.0, 0.0],
            limit: 10,
            ..pb::SearchRequest::default()
        })
        .await
        .expect_err("search");
    assert_eq!(status.code(), Code::Unauthenticated);
    let status = h
        .worker
        .count(pb::CountRequest {
            scope: Some(pb::Scope {
                project_id: PROJECT_B,
                ..scope.clone()
            }),
            ..pb::CountRequest::default()
        })
        .await
        .expect_err("count");
    assert_eq!(status.code(), Code::Unauthenticated);
    let status = h
        .worker
        .drop_project(pb::DropProjectRequest {
            project_id: PROJECT_B,
        })
        .await
        .expect_err("drop");
    assert_eq!(status.code(), Code::Unauthenticated);
    let status = h
        .worker
        .upsert(tokio_stream::iter(Vec::<pb::UpsertRequest>::new()))
        .await
        .expect_err("upsert");
    assert_eq!(status.code(), Code::Unauthenticated);

    for token in [TOKEN_EXPIRED, "unknown-token"] {
        let status = h.search(token, scope.clone(), None).await.expect_err(token);
        assert_eq!(status.code(), Code::Unauthenticated, "{token}");
    }
}

#[tokio::test]
async fn an_administrator_names_the_project_and_cannot_search() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;

    // An administrator must name the project.
    let status = h
        .admin
        .drop_project(pb::DropProjectRequest { project_id: 0 })
        .await
        .expect_err("unnamed");
    assert_eq!(status.code(), Code::InvalidArgument);

    // It cannot search or write.
    let status = h
        .admin
        .search(pb::SearchRequest {
            scope: Some(pb::Scope {
                project_id: PROJECT_A,
                ..Harness::scope(&[NAMESPACE_A])
            }),
            space: Some(h.space()),
            vector: vec![1.0, 0.0, 0.0, 0.0],
            limit: 10,
            ..pb::SearchRequest::default()
        })
        .await
        .expect_err("search");
    assert_eq!(status.code(), Code::PermissionDenied);

    let counted = h
        .admin
        .count(pb::CountRequest {
            scope: Some(pb::Scope {
                project_id: PROJECT_B,
                ..Harness::scope(&[])
            }),
            space: Some(h.space()),
            filter: None,
        })
        .await
        .expect("count")
        .into_inner()
        .count;
    assert_eq!(counted, 4);

    // DropProject removes the named project only.
    h.admin
        .drop_project(pb::DropProjectRequest {
            project_id: PROJECT_A,
        })
        .await
        .expect("drop");
    assert_eq!(h.truth(PROJECT_A).await, 0);
    assert_eq!(h.truth(PROJECT_B).await, 4);
}

/// elitea-main's two deletions (ADR-0031 decision 4), as its vectorclient
/// sends them: a whole-namespace `Delete` of one toolkit index for the named
/// project (no space: every collection), and `DropProject`. Both are
/// idempotent — a namespace that holds nothing, a repeat, a project never
/// written — and a delete under one project never touches another project's
/// points in the SAME namespace id (the sweeper's tombstone names both).
#[tokio::test]
async fn an_administrator_deletes_a_namespace_and_a_project_idempotently() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    // A holds 2 points in NAMESPACE_A; B holds 2 in NAMESPACE_B and 2 in
    // NAMESPACE_A (its own, under the same namespace id).
    assert_eq!(h.truth(PROJECT_A).await, 2);
    assert_eq!(h.truth(PROJECT_B).await, 4);

    let whole = |project_id: i64, namespace: &str| pb::DeleteRequest {
        project_id,
        space: None,
        namespace: Some(Harness::namespace(namespace)),
        selector: Some(pb::delete_request::Selector::WholeNamespace(
            pb::WholeNamespace {},
        )),
    };

    // An administrator must name the project on Delete too.
    let status = h
        .admin
        .delete(whole(0, NAMESPACE_A))
        .await
        .expect_err("unnamed");
    assert_eq!(status.code(), Code::InvalidArgument);

    // A's index namespace goes; B's points under the same namespace id stay.
    let deleted = h
        .admin
        .delete(whole(PROJECT_A, NAMESPACE_A))
        .await
        .expect("delete")
        .into_inner();
    assert!(deleted.collections >= 1, "{deleted:?}");
    assert_eq!(h.truth(PROJECT_A).await, 0);
    assert_eq!(h.truth(PROJECT_B).await, 4);

    // Repeating it, and deleting a namespace that was never written, are
    // clean successes: the sweeper replays a tombstone it is not sure of.
    h.admin
        .delete(whole(PROJECT_A, NAMESPACE_A))
        .await
        .expect("repeat");
    h.admin
        .delete(whole(PROJECT_A, "7c9e6679-7425-40de-944b-e07fc1f90ae7"))
        .await
        .expect("unknown namespace");
    assert_eq!(h.truth(PROJECT_B).await, 4);

    // DropProject on a project that was never written is a clean success and
    // touches nobody; on B it removes every namespace of B.
    h.admin
        .drop_project(pb::DropProjectRequest {
            project_id: 999_999,
        })
        .await
        .expect("unknown project");
    assert_eq!(h.truth(PROJECT_B).await, 4);
    h.admin
        .drop_project(pb::DropProjectRequest {
            project_id: PROJECT_B,
        })
        .await
        .expect("drop b");
    assert_eq!(h.truth(PROJECT_B).await, 0);
    h.admin
        .drop_project(pb::DropProjectRequest {
            project_id: PROJECT_B,
        })
        .await
        .expect("repeat drop");
}

// ── shape ───────────────────────────────────────────────────────────────────

#[tokio::test]
async fn a_query_vector_of_another_dimension_is_refused() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    let status = h
        .worker
        .search(with_token(
            TOKEN_A,
            pb::SearchRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A])),
                space: Some(h.space()),
                vector: vec![1.0, 0.0, 0.0],
                limit: 10,
                ..pb::SearchRequest::default()
            },
        ))
        .await
        .expect_err("dimension");
    assert_eq!(status.code(), Code::InvalidArgument);
    let status = h
        .upsert(
            TOKEN_A,
            0,
            Harness::namespace(NAMESPACE_A),
            vec![pb::Point {
                vector: vec![1.0; 5],
                ..Harness::point("x", "x", [1.0, 0.0, 0.0, 0.0])
            }],
        )
        .await
        .expect_err("dimension");
    assert_eq!(status.code(), Code::InvalidArgument);
}

#[tokio::test]
async fn generations_publish_atomically_and_clean_up_lazily() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    for generation in ["g1", "g2"] {
        h.upsert(
            TOKEN_A,
            0,
            pb::Namespace {
                generation: generation.to_owned(),
                ..Harness::namespace(NAMESPACE_A)
            },
            vec![Harness::point("doc", generation, [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect("upsert");
    }
    // An incremental update written without a generation, before the
    // clean-up: it belongs to no build and the clean-up must keep it.
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point("incremental", "plain", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("upsert");
    let read = |generation: &str| pb::Scope {
        generation: generation.to_owned(),
        ..Harness::scope(&[NAMESPACE_A])
    };
    let g1 = h.search(TOKEN_A, read("g1"), None).await.expect("g1");
    assert_eq!(g1.len(), 1);
    assert_eq!(g1[0].text, "g1");
    assert_eq!(h.truth(PROJECT_A).await, 3);
    h.worker
        .delete(with_token(
            TOKEN_A,
            pb::DeleteRequest {
                project_id: 0,
                space: Some(h.space()),
                namespace: Some(Harness::namespace(NAMESPACE_A)),
                selector: Some(pb::delete_request::Selector::GenerationsExcept(
                    pb::GenerationsExcept {
                        keep: "g2".to_owned(),
                    },
                )),
            },
        ))
        .await
        .expect("cleanup");
    assert!(
        h.search(TOKEN_A, read("g1"), None)
            .await
            .expect("g1")
            .is_empty()
    );
    assert_eq!(
        h.search(TOKEN_A, read("g2"), None).await.expect("g2").len(),
        1
    );
    // The unversioned point survived: the toolkit scope (active generation
    // g2 plus unversioned points) reads both, and the collection holds
    // exactly those two.
    let published = pb::Scope {
        active_generations: vec![pb::ActiveGeneration {
            namespace_id: NAMESPACE_A.to_owned(),
            generation: "g2".to_owned(),
        }],
        ..Harness::scope(&[NAMESPACE_A])
    };
    let mut texts: Vec<String> = h
        .search(TOKEN_A, published, None)
        .await
        .expect("published")
        .into_iter()
        .map(|p| p.text)
        .collect();
    texts.sort();
    assert_eq!(texts, ["g2", "plain"]);
    assert_eq!(h.truth(PROJECT_A).await, 2, "g1 alone was removed");
}

// ── sources ─────────────────────────────────────────────────────────────────

fn wiki_namespace(namespace_id: &str) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::Deepwiki as i32,
        ..Harness::namespace(namespace_id)
    }
}

fn wiki_scope(namespaces: &[&str]) -> pb::Scope {
    pb::Scope {
        source: pb::Source::Deepwiki as i32,
        ..Harness::scope(namespaces)
    }
}

impl Harness {
    /// The points of `project` and `source`, read from Qdrant directly.
    async fn truth_of(&self, project: i64, source: &str) -> u64 {
        let filter = qdrant_client::qdrant::Filter::must([
            qdrant_client::qdrant::Condition::matches("project_id", project.to_string()),
            qdrant_client::qdrant::Condition::matches("source", source.to_owned()),
        ]);
        self.qdrant
            .count(
                qdrant_client::qdrant::CountPointsBuilder::new(format!(
                    "emb_{}_{DIMENSION}",
                    self.slug
                ))
                .filter(filter)
                .exact(true),
            )
            .await
            .expect("count")
            .result
            .map_or(0, |result| result.count)
    }

    /// Project A's worker writes a toolkit index and A's `DeepWiki` engine
    /// writes a wiki, under the SAME namespace id.
    async fn seed_both_sources(&mut self) {
        let points = || {
            vec![
                Self::point("doc-1", "alpha shared secret", [1.0, 0.0, 0.0, 0.0]),
                Self::point("doc-2", "alpha other words", [0.0, 1.0, 0.0, 0.0]),
            ]
        };
        self.upsert(TOKEN_A, 0, Self::namespace(NAMESPACE_A), points())
            .await
            .expect("toolkit index upsert");
        self.upsert(TOKEN_A_WIKI, 0, wiki_namespace(NAMESPACE_A), points())
            .await
            .expect("wiki upsert");
        assert_eq!(self.truth_of(PROJECT_A, "toolkit_index").await, 2);
        assert_eq!(self.truth_of(PROJECT_A, "deepwiki").await, 2);
    }

    /// Every way `token` can name `namespace`'s source is refused with
    /// `PERMISSION_DENIED`: write, search, hybrid search, count, delete and
    /// listing that one source.
    async fn assert_source_refused(
        &mut self,
        token: &str,
        namespace: pb::Namespace,
        scope: pb::Scope,
    ) {
        let mut statuses = vec![
            (
                "upsert",
                self.upsert(
                    token,
                    0,
                    namespace.clone(),
                    vec![Self::point("doc-1", "overwrite", [0.0, 0.0, 1.0, 0.0])],
                )
                .await
                .expect_err("upsert"),
            ),
            (
                "search",
                self.search(token, scope.clone(), None)
                    .await
                    .expect_err("search"),
            ),
        ];
        statuses.push((
            "hybrid",
            self.worker
                .hybrid_search(with_token(
                    token,
                    pb::HybridSearchRequest {
                        scope: Some(scope.clone()),
                        space: Some(self.space()),
                        vector: vec![1.0, 0.0, 0.0, 0.0],
                        text: "alpha".to_owned(),
                        limit: 10,
                        ..pb::HybridSearchRequest::default()
                    },
                ))
                .await
                .expect_err("hybrid"),
        ));
        statuses.push((
            "count",
            self.worker
                .count(with_token(
                    token,
                    pb::CountRequest {
                        scope: Some(scope.clone()),
                        ..pb::CountRequest::default()
                    },
                ))
                .await
                .expect_err("count"),
        ));
        statuses.push((
            "delete",
            self.worker
                .delete(with_token(
                    token,
                    pb::DeleteRequest {
                        project_id: 0,
                        space: None,
                        namespace: Some(namespace.clone()),
                        selector: Some(pb::delete_request::Selector::WholeNamespace(
                            pb::WholeNamespace {},
                        )),
                    },
                ))
                .await
                .expect_err("delete"),
        ));
        statuses.push((
            "list",
            self.worker
                .list_indexes(with_token(
                    token,
                    pb::ListIndexesRequest {
                        project_id: 0,
                        source: namespace.source,
                    },
                ))
                .await
                .expect_err("list"),
        ));
        for (operation, status) in statuses {
            assert_eq!(status.code(), Code::PermissionDenied, "{operation}");
        }
    }

    /// The sources `token` sees listed in this test's collection.
    async fn listed_sources(&mut self, token: &str) -> Vec<i32> {
        let indexes = self
            .worker
            .list_indexes(with_token(token, pb::ListIndexesRequest::default()))
            .await
            .expect("list")
            .into_inner()
            .indexes;
        let suffix = format!("{}_{DIMENSION}", self.slug);
        indexes
            .into_iter()
            .filter(|index| index.collection.ends_with(&suffix))
            .map(|index| index.source)
            .collect()
    }
}

#[tokio::test]
async fn a_worker_token_cannot_touch_deepwiki_points() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_both_sources().await;

    // Same project, same namespace id: only the source differs.
    h.assert_source_refused(
        TOKEN_A,
        wiki_namespace(NAMESPACE_A),
        wiki_scope(&[NAMESPACE_A]),
    )
    .await;
    assert_eq!(h.truth_of(PROJECT_A, "deepwiki").await, 2);

    // Its own source still works, and lists no wiki.
    let points = h
        .search(TOKEN_A, Harness::scope(&[NAMESPACE_A]), None)
        .await
        .expect("own source");
    assert_eq!(points.len(), 2);
    assert!(
        points
            .iter()
            .all(|point| point.source == pb::Source::ToolkitIndex as i32)
    );
    assert_eq!(
        h.listed_sources(TOKEN_A).await,
        vec![pb::Source::ToolkitIndex as i32]
    );
}

#[tokio::test]
async fn a_callback_token_cannot_touch_toolkit_index_points() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_both_sources().await;

    h.assert_source_refused(
        TOKEN_A_WIKI,
        Harness::namespace(NAMESPACE_A),
        Harness::scope(&[NAMESPACE_A]),
    )
    .await;
    assert_eq!(h.truth_of(PROJECT_A, "toolkit_index").await, 2);

    let points = h
        .search(TOKEN_A_WIKI, wiki_scope(&[NAMESPACE_A]), None)
        .await
        .expect("own source");
    assert_eq!(points.len(), 2);
    assert!(
        points
            .iter()
            .all(|point| point.source == pb::Source::Deepwiki as i32)
    );
    assert_eq!(
        h.listed_sources(TOKEN_A_WIKI).await,
        vec![pb::Source::Deepwiki as i32]
    );
}

// ── revocation ──────────────────────────────────────────────────────────────

/// elitea-main's introspection over live claims: a token is active until its
/// execution settles.
struct LiveClaims(std::sync::Mutex<HashMap<String, TokenGrant>>);

#[async_trait]
impl Introspector for LiveClaims {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        Ok(self.0.lock().expect("claims").get(token).cloned())
    }
}

#[tokio::test]
async fn a_token_for_a_settled_execution_is_refused() {
    const TOKEN_CLAIM: &str = "token-claim";
    let _ = require_qdrant!();
    let start = unix_now();
    let claims = Arc::new(LiveClaims(std::sync::Mutex::new(HashMap::from([(
        TOKEN_CLAIM.to_owned(),
        grant(PROJECT_A, start + 3600),
    )]))));
    let now = Arc::new(AtomicI64::new(start));
    let clock = {
        let now = Arc::clone(&now);
        Arc::new(move || now.load(Ordering::SeqCst))
    };
    let policy = CachePolicy::default();
    let introspector = Arc::new(CachingIntrospector::new(
        Arc::clone(&claims) as Arc<dyn Introspector>,
        policy,
        clock,
    ));
    let mut h = harness_with(move || introspector as Arc<dyn Introspector>)
        .await
        .expect("harness");

    h.upsert(
        TOKEN_CLAIM,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point(
            "doc-1",
            "alpha shared secret",
            [1.0, 0.0, 0.0, 0.0],
        )],
    )
    .await
    .expect("live claim writes");
    assert_eq!(h.truth(PROJECT_A).await, 1);

    // The execution settles: elitea-main now answers the token inactive. The
    // token is still an hour from its expiry, so only the worker-claim cache
    // cap stands between the settle and the refusal.
    claims.0.lock().expect("claims").remove(TOKEN_CLAIM);
    now.fetch_add(policy.worker_claim_max_ttl_seconds, Ordering::SeqCst);

    let status = h
        .upsert(
            TOKEN_CLAIM,
            0,
            Harness::namespace(NAMESPACE_A),
            vec![Harness::point("doc-2", "late write", [0.0, 1.0, 0.0, 0.0])],
        )
        .await
        .expect_err("settled upsert");
    assert_eq!(status.code(), Code::Unauthenticated);
    let status = h
        .search(TOKEN_CLAIM, Harness::scope(&[NAMESPACE_A]), None)
        .await
        .expect_err("settled search");
    assert_eq!(status.code(), Code::Unauthenticated);
    let status = h
        .worker
        .delete(with_token(
            TOKEN_CLAIM,
            pb::DeleteRequest {
                project_id: 0,
                space: None,
                namespace: Some(Harness::namespace(NAMESPACE_A)),
                selector: Some(pb::delete_request::Selector::WholeNamespace(
                    pb::WholeNamespace {},
                )),
            },
        ))
        .await
        .expect_err("settled delete");
    assert_eq!(status.code(), Code::Unauthenticated);
    assert_eq!(h.truth(PROJECT_A).await, 1, "nothing written or deleted");
}

// ── elitea-main unreachable ─────────────────────────────────────────────────

#[tokio::test]
async fn an_unreachable_introspection_service_fails_closed() {
    let url = require_qdrant!();
    let pki = pki();
    // Nothing listens on this port.
    let dead = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let dead_address = dead.local_addr().expect("address");
    drop(dead);
    let channel = Channel::from_shared(format!("https://{dead_address}"))
        .expect("endpoint")
        .tls_config(
            ClientTlsConfig::new()
                .ca_certificate(Certificate::from_pem(&pki.ca_pem))
                .identity(Identity::from_pem(&pki.worker.0, &pki.worker.1))
                .domain_name("localhost"),
        )
        .expect("tls")
        .connect_lazy();
    let introspector = Arc::new(GrpcIntrospector::new(channel, Duration::from_millis(500)));
    let address = serve(&url, introspector, &pki).await;
    let mut worker = client(address, &pki, &pki.worker).await;
    let status = worker
        .list_indexes(with_token(TOKEN_A, pb::ListIndexesRequest::default()))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), Code::Unavailable);
}

// ── review round 1: indexes, space limits, mid-stream revocation ───────────

fn raw_qdrant(url: &str) -> Qdrant {
    Qdrant::from_url(url)
        .skip_compatibility_check()
        .build()
        .expect("qdrant")
}

/// A collection in the service's layout but with NO payload index, as an
/// older build, or a run that stopped before indexing, leaves it.
async fn bare_collection(qdrant: &Qdrant, slug: &str) -> String {
    use qdrant_client::qdrant::{
        CreateCollectionBuilder, Datatype, Distance, Modifier, SparseVectorParamsBuilder,
        SparseVectorsConfigBuilder, VectorParamsBuilder, VectorsConfigBuilder,
    };
    let name = format!("emb_{slug}_{DIMENSION}");
    let mut vectors = VectorsConfigBuilder::default();
    vectors.add_named_vector_params(
        "dense",
        VectorParamsBuilder::new(u64::from(DIMENSION), Distance::Cosine)
            .datatype(Datatype::Float16),
    );
    let mut sparse = SparseVectorsConfigBuilder::default();
    sparse.add_named_vector_params(
        "bm25",
        SparseVectorParamsBuilder::default().modifier(Modifier::Idf),
    );
    qdrant
        .create_collection(
            CreateCollectionBuilder::new(&name)
                .vectors_config(vectors)
                .sparse_vectors_config(sparse),
        )
        .await
        .expect("bare collection");
    name
}

async fn indexed_fields(qdrant: &Qdrant, collection: &str) -> HashSet<String> {
    qdrant
        .collection_info(collection)
        .await
        .expect("info")
        .result
        .map(|info| info.payload_schema.into_keys().collect())
        .unwrap_or_default()
}

const REQUIRED_FIELDS: [&str; 10] = [
    "project_id",
    "source",
    "namespace_id",
    "generation",
    "document_key",
    "document_version",
    "parent_id",
    "chunk_type",
    "acl",
    "text",
];

#[tokio::test]
async fn an_existing_collection_without_indexes_gets_them_on_write_and_on_read() {
    let url = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let qdrant = raw_qdrant(&url);

    // Write path: the collection exists, bare.
    let written = bare_collection(&qdrant, &h.slug).await;
    assert!(indexed_fields(&qdrant, &written).await.is_empty());
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point("doc-1", "alpha", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("upsert into a bare collection");
    let fields = indexed_fields(&qdrant, &written).await;
    for field in REQUIRED_FIELDS {
        assert!(fields.contains(field), "{field} is indexed after a write");
    }

    // Read path: another bare collection, first touched by a search.
    let read_slug = unique_slug("bare-read");
    let read = bare_collection(&qdrant, &read_slug).await;
    let response = h
        .worker
        .search(with_token(
            TOKEN_A,
            pb::SearchRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A])),
                space: Some(pb::EmbeddingSpace {
                    model_slug: read_slug,
                    dimension: DIMENSION,
                }),
                vector: vec![1.0, 0.0, 0.0, 0.0],
                limit: 10,
                ..pb::SearchRequest::default()
            },
        ))
        .await
        .expect("search a bare collection")
        .into_inner();
    assert!(response.points.is_empty());
    let fields = indexed_fields(&qdrant, &read).await;
    for field in REQUIRED_FIELDS {
        assert!(fields.contains(field), "{field} is indexed after a read");
    }
}

#[tokio::test]
async fn the_collection_cap_refuses_a_new_space_but_not_an_existing_one() {
    let url = require_qdrant!();
    let mut h = harness_custom(oracle, |url| {
        let present = emb_collections(url);
        (
            CollectionSettings {
                max_collections: present + 1,
                ..CollectionSettings::default()
            },
            SpaceLimits::default(),
        )
    })
    .await
    .expect("harness");
    let qdrant = raw_qdrant(&url);
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point("doc-1", "alpha", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("the one allowed new space");

    let other = pb::EmbeddingSpace {
        model_slug: unique_slug("over-cap"),
        dimension: DIMENSION,
    };
    let status = h
        .upsert_in(
            TOKEN_A,
            0,
            other.clone(),
            Harness::namespace(NAMESPACE_A),
            vec![Harness::point("doc-1", "alpha", [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect_err("over the cap");
    assert_eq!(status.code(), Code::ResourceExhausted);
    assert!(status.message().contains("ELITEA_VECTOR_MAX_COLLECTIONS"));
    assert!(
        !qdrant
            .collection_exists(format!("emb_{}_{DIMENSION}", other.model_slug))
            .await
            .expect("exists"),
        "nothing was created"
    );
    // The space that exists keeps working.
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point("doc-2", "alpha", [0.0, 1.0, 0.0, 0.0])],
    )
    .await
    .expect("an existing space is not a new collection");
}

/// The `emb_*` collections Qdrant holds now, counted the way the service
/// counts them. `harness_custom`'s closure is synchronous, so this runs the
/// async call on a helper thread.
fn emb_collections(url: &str) -> usize {
    let qdrant = raw_qdrant(url);
    std::thread::spawn(move || {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(async move {
                qdrant
                    .list_collections()
                    .await
                    .expect("list")
                    .collections
                    .into_iter()
                    .filter(|collection| {
                        collection.name.starts_with("emb_")
                            && elitea_vector::layout::Space::from_collection(&collection.name)
                                .is_some()
                    })
                    .count()
            })
    })
    .join()
    .expect("counted")
}

#[tokio::test]
async fn the_space_allowlist_and_dimension_bound_are_checked_before_anything_is_made() {
    let url = require_qdrant!();
    let listed = unique_slug("listed");
    let allowed = elitea_vector::layout::Space::new(&listed, DIMENSION).expect("space");
    let mut h = harness_custom(oracle, move |_| {
        (
            CollectionSettings {
                max_collections: 100_000,
                ..CollectionSettings::default()
            },
            SpaceLimits {
                max_dimension: 4096,
                allowed: Some(HashSet::from([allowed])),
            },
        )
    })
    .await
    .expect("harness");
    let qdrant = raw_qdrant(&url);
    let point = || vec![Harness::point("doc-1", "alpha", [1.0, 0.0, 0.0, 0.0])];

    let status = h
        .upsert(TOKEN_A, 0, Harness::namespace(NAMESPACE_A), point())
        .await
        .expect_err("not on the allowlist");
    assert_eq!(status.code(), Code::PermissionDenied);
    assert!(
        !qdrant
            .collection_exists(format!("emb_{}_{DIMENSION}", h.slug))
            .await
            .expect("exists")
    );

    let listed_space = pb::EmbeddingSpace {
        model_slug: listed.clone(),
        dimension: DIMENSION,
    };
    h.upsert_in(
        TOKEN_A,
        0,
        listed_space,
        Harness::namespace(NAMESPACE_A),
        point(),
    )
    .await
    .expect("the listed space may be created");

    // Out-of-range dimensions never reach Qdrant.
    for dimension in [0, 4097, 65_536] {
        let slug = unique_slug("dim");
        let status = h
            .upsert_in(
                TOKEN_A,
                0,
                pb::EmbeddingSpace {
                    model_slug: slug.clone(),
                    dimension,
                },
                Harness::namespace(NAMESPACE_A),
                Vec::new(),
            )
            .await
            .expect_err("dimension out of range");
        assert_eq!(status.code(), Code::InvalidArgument, "{dimension}");
        assert!(
            !qdrant
                .collection_exists(format!("emb_{slug}_{dimension}"))
                .await
                .expect("exists")
        );
    }
}

#[tokio::test]
async fn a_non_finite_score_threshold_is_an_invalid_argument() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed().await;
    for threshold in [f32::NAN, f32::INFINITY, f32::NEG_INFINITY] {
        let status = h
            .worker
            .search(with_token(
                TOKEN_A,
                pb::SearchRequest {
                    scope: Some(Harness::scope(&[NAMESPACE_A])),
                    space: Some(h.space()),
                    vector: vec![1.0, 0.0, 0.0, 0.0],
                    limit: 10,
                    score_threshold: Some(threshold),
                    ..pb::SearchRequest::default()
                },
            ))
            .await
            .expect_err("refused");
        assert_eq!(status.code(), Code::InvalidArgument, "{threshold}");
    }
}

/// A token revoked while an Upsert stream is open is refused at the next
/// batch; what the earlier batch wrote stays.
#[tokio::test]
async fn a_token_revoked_mid_stream_is_refused_at_the_next_batch() {
    const TOKEN_CLAIM: &str = "token-claim-stream";
    let url = require_qdrant!();
    let start = unix_now();
    let claims = Arc::new(LiveClaims(std::sync::Mutex::new(HashMap::from([(
        TOKEN_CLAIM.to_owned(),
        grant(PROJECT_A, start + 3600),
    )]))));
    let now = Arc::new(AtomicI64::new(start));
    let policy = CachePolicy::default();
    let introspector = {
        let now = Arc::clone(&now);
        Arc::new(CachingIntrospector::new(
            Arc::clone(&claims) as Arc<dyn Introspector>,
            policy,
            Arc::new(move || now.load(Ordering::SeqCst)),
        ))
    };
    let h = harness_with(move || introspector as Arc<dyn Introspector>)
        .await
        .expect("harness");

    let (sender, receiver) = tokio::sync::mpsc::channel::<pb::UpsertRequest>(4);
    let header = pb::UpsertRequest {
        message: Some(pb::upsert_request::Message::Header(pb::UpsertHeader {
            project_id: 0,
            space: Some(h.space()),
            namespace: Some(Harness::namespace(NAMESPACE_A)),
        })),
    };
    let batch = |key: &str, vector: [f32; 4]| pb::UpsertRequest {
        message: Some(pb::upsert_request::Message::Batch(pb::UpsertBatch {
            points: vec![Harness::point(key, "streamed", vector)],
        })),
    };
    sender.send(header).await.expect("header");
    sender
        .send(batch("doc-1", [1.0, 0.0, 0.0, 0.0]))
        .await
        .expect("first batch");
    let qdrant = raw_qdrant(&url);
    let mut worker = h.worker.clone();
    let call = tokio::spawn(async move {
        worker
            .upsert(with_token(
                TOKEN_CLAIM,
                tokio_stream::wrappers::ReceiverStream::new(receiver),
            ))
            .await
    });
    // The first batch lands.
    let mut written = 0;
    // Creating the collection and its (now nine) payload indexes takes a few
    // seconds on a cold Qdrant; 20 s leaves room.
    for _ in 0..400 {
        // The collection appears with the first batch.
        if qdrant
            .collection_exists(format!("emb_{}_{DIMENSION}", h.slug))
            .await
            .expect("exists")
        {
            written = h.truth(PROJECT_A).await;
        }
        if written == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(written, 1, "the first batch was written");

    // The claim settles; the cache cap passes; the stream is still open.
    claims.0.lock().expect("claims").remove(TOKEN_CLAIM);
    now.fetch_add(policy.worker_claim_max_ttl_seconds, Ordering::SeqCst);
    sender
        .send(batch("doc-2", [0.0, 1.0, 0.0, 0.0]))
        .await
        .expect("second batch");
    let status = call
        .await
        .expect("joined")
        .expect_err("the stream is aborted");
    assert_eq!(status.code(), Code::Unauthenticated);
    assert_eq!(
        h.truth(PROJECT_A).await,
        1,
        "the refused batch wrote nothing"
    );
}

/// `ListIndexes` and a space-less `Count` fan out over every collection
/// concurrently; the answer stays sorted and complete.
#[tokio::test]
async fn fan_out_over_collections_is_complete_and_sorted() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let other = pb::EmbeddingSpace {
        model_slug: format!("{}-b", h.slug),
        dimension: DIMENSION,
    };
    let second_namespace = "1c2d3e4f-5a6b-4c7d-8e9f-a0b1c2d3e4f5";
    let points = || {
        vec![
            Harness::point("doc-1", "alpha shared secret", [1.0, 0.0, 0.0, 0.0]),
            Harness::point("doc-2", "alpha other words", [0.0, 1.0, 0.0, 0.0]),
        ]
    };
    h.upsert(TOKEN_A, 0, Harness::namespace(NAMESPACE_A), points())
        .await
        .expect("first space");
    h.upsert_in(
        TOKEN_A,
        0,
        other.clone(),
        Harness::namespace(second_namespace),
        points(),
    )
    .await
    .expect("second space");
    h.upsert_in(TOKEN_A, 0, other, Harness::namespace(NAMESPACE_A), points())
        .await
        .expect("second space, first namespace");

    let slug = h.slug.clone();
    let indexes: Vec<_> = h
        .worker
        .list_indexes(with_token(TOKEN_A, pb::ListIndexesRequest::default()))
        .await
        .expect("list")
        .into_inner()
        .indexes
        .into_iter()
        .filter(|index| index.collection.contains(&slug))
        .collect();
    assert_eq!(indexes.len(), 3, "{indexes:?}");
    let keys: Vec<_> = indexes
        .iter()
        .map(|index| {
            (
                index.collection.clone(),
                index.source,
                index.namespace_id.clone(),
            )
        })
        .collect();
    let mut sorted = keys.clone();
    sorted.sort();
    assert_eq!(keys, sorted, "ListIndexes is sorted");
    assert!(indexes.iter().all(|index| index.point_count == 2));

    // Without a space, Count adds up every collection (this test's two, at
    // least; other tests' collections hold other projects' points only).
    let counted = h
        .worker
        .count(with_token(
            TOKEN_A,
            pb::CountRequest {
                scope: Some(Harness::scope(&[NAMESPACE_A, second_namespace])),
                ..pb::CountRequest::default()
            },
        ))
        .await
        .expect("count")
        .into_inner()
        .count;
    assert!(counted >= 6, "{counted}");
}

// ── memory: owner enforcement ───────────────────────────────────────────────

const PERSONAL: i32 = pb::MemoryScope::Personal as i32;
const PROJECT: i32 = pb::MemoryScope::Project as i32;

fn memory_namespace(namespace_id: &str, scope: i32) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::Memory as i32,
        namespace_id: namespace_id.to_owned(),
        generation: String::new(),
        memory_scope: scope,
    }
}

fn memory_scope_of(namespace_id: &str, scope: i32) -> pb::Scope {
    pb::Scope {
        source: pb::Source::Memory as i32,
        namespace_ids: vec![namespace_id.to_owned()],
        memory_scope: scope,
        ..pb::Scope::default()
    }
}

fn memory_point(key: &str, version: u32, text: &str, vector: [f32; 4]) -> pb::Point {
    pb::Point {
        document_key: key.to_owned(),
        document_version: "1".to_owned(),
        chunk_id: format!("v{version}"),
        chunk_type: "memory".to_owned(),
        text: text.to_owned(),
        // A client that tries to claim an owner through the metadata.
        metadata_json: "{\"m\":{\"owner\":\"user:999\"},\"t\":{\"version\":1}}".to_owned(),
        vector: vector.to_vec(),
        ..pb::Point::default()
    }
}

impl Harness {
    async fn memory_upsert(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
        points: Vec<pb::Point>,
    ) -> Result<pb::UpsertResponse, tonic::Status> {
        self.upsert(token, 0, memory_namespace(namespace_id, scope), points)
            .await
    }

    async fn memory_search(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
    ) -> Result<Vec<pb::ScoredPoint>, tonic::Status> {
        self.search(token, memory_scope_of(namespace_id, scope), None)
            .await
    }

    async fn memory_read(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
        filter: Option<pb::Filter>,
    ) -> Result<Vec<pb::ScoredPoint>, tonic::Status> {
        let space = self.space();
        self.worker
            .read_memory(with_token(
                token,
                pb::ReadMemoryRequest {
                    scope: Some(memory_scope_of(namespace_id, scope)),
                    space: Some(space),
                    filter,
                    limit: 100,
                },
            ))
            .await
            .map(|response| response.into_inner().points)
    }

    async fn memory_count(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
    ) -> Result<u64, tonic::Status> {
        self.worker
            .count(with_token(
                token,
                pb::CountRequest {
                    scope: Some(memory_scope_of(namespace_id, scope)),
                    space: Some(self.space()),
                    filter: None,
                },
            ))
            .await
            .map(|response| response.into_inner().count)
    }

    async fn memory_delete(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
        selector: pb::delete_request::Selector,
    ) -> Result<(), tonic::Status> {
        let space = self.space();
        self.worker
            .delete(with_token(
                token,
                pb::DeleteRequest {
                    project_id: 0,
                    space: Some(space),
                    namespace: Some(memory_namespace(namespace_id, scope)),
                    selector: Some(selector),
                },
            ))
            .await
            .map(|_| ())
    }

    async fn memory_documents(
        &mut self,
        token: &str,
        scope: i32,
        namespace_id: &str,
    ) -> Result<usize, tonic::Status> {
        let space = self.space();
        let mut stream = self
            .worker
            .list_documents(with_token(
                token,
                pb::ListDocumentsRequest {
                    project_id: 0,
                    namespace: Some(memory_namespace(namespace_id, scope)),
                    space: Some(space),
                    max_documents: 0,
                },
            ))
            .await?
            .into_inner();
        let mut documents = 0;
        while let Some(page) = stream.message().await? {
            documents += page.documents.len();
        }
        Ok(documents)
    }

    /// Memory points of `project` owned by `owner` (`None`: any owner), read
    /// from Qdrant directly.
    async fn truth_memory(&self, project: i64, owner: Option<&str>) -> u64 {
        let mut must = vec![
            qdrant_client::qdrant::Condition::matches("project_id", project.to_string()),
            qdrant_client::qdrant::Condition::matches("source", "memory".to_owned()),
        ];
        if let Some(owner) = owner {
            must.push(qdrant_client::qdrant::Condition::matches(
                "owner",
                owner.to_owned(),
            ));
        }
        self.qdrant
            .count(
                qdrant_client::qdrant::CountPointsBuilder::new(format!(
                    "emb_{}_{DIMENSION}",
                    self.slug
                ))
                .filter(qdrant_client::qdrant::Filter::must(must))
                .exact(true),
            )
            .await
            .expect("count")
            .result
            .map_or(0, |result| result.count)
    }

    /// Alice and Bob each write a personal memory under the SAME key, and
    /// Alice writes a project one. Every point is one document of one chunk.
    async fn seed_memory(&mut self) {
        for (token, scope, text) in [
            (TOKEN_ALICE, PERSONAL, "alice private pasta"),
            (TOKEN_BOB, PERSONAL, "bob private pasta"),
            (TOKEN_ALICE, PROJECT, "team shared pasta"),
        ] {
            let written = self
                .memory_upsert(
                    token,
                    scope,
                    NAMESPACE_M,
                    vec![memory_point("k", 1, text, [1.0, 0.0, 0.0, 0.0])],
                )
                .await
                .expect("seed memory");
            assert_eq!(written.upserted, 1);
        }
    }
}

#[tokio::test]
async fn the_service_stamps_the_owner_from_the_token_and_gives_each_owner_its_own_point() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_memory().await;
    // Three points for one (project, namespace, key, chunk): the owner is part
    // of the point id, so Bob's write did not overwrite Alice's.
    assert_eq!(h.truth_memory(PROJECT_A, None).await, 3);
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 1);
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:12")).await, 1);
    assert_eq!(h.truth_memory(PROJECT_A, Some("project")).await, 1);
    // The owner the client tried to smuggle through metadata is only metadata.
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:999")).await, 0);
    // Each reads its own back.
    for (token, scope, text) in [
        (TOKEN_ALICE, PERSONAL, "alice private pasta"),
        (TOKEN_BOB, PERSONAL, "bob private pasta"),
        (TOKEN_BOB, PROJECT, "team shared pasta"),
    ] {
        let points = h
            .memory_search(token, scope, NAMESPACE_M)
            .await
            .expect("search");
        assert_eq!(points.len(), 1, "{token}");
        assert_eq!(points[0].text, text);
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One scenario, every RPC in turn.
async fn user_b_cannot_read_count_list_or_delete_user_as_personal_memories_even_with_the_namespace_id()
 {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    // Only Alice writes a personal memory; Bob knows the namespace id.
    h.memory_upsert(
        TOKEN_ALICE,
        PERSONAL,
        NAMESPACE_M,
        vec![memory_point(
            "secret",
            1,
            "alice diagnosis",
            [1.0, 0.0, 0.0, 0.0],
        )],
    )
    .await
    .expect("alice writes");
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 1);

    // Search, hybrid search, read, count and list: nothing, in either scope.
    for scope in [PERSONAL, PROJECT] {
        assert!(
            h.memory_search(TOKEN_BOB, scope, NAMESPACE_M)
                .await
                .expect("search")
                .is_empty(),
            "search {scope}"
        );
        assert!(
            h.memory_read(TOKEN_BOB, scope, NAMESPACE_M, None)
                .await
                .expect("read")
                .is_empty(),
            "read {scope}"
        );
        assert_eq!(
            h.memory_count(TOKEN_BOB, scope, NAMESPACE_M)
                .await
                .expect("count"),
            0,
            "count {scope}"
        );
        assert_eq!(
            h.memory_documents(TOKEN_BOB, scope, NAMESPACE_M)
                .await
                .expect("list"),
            0,
            "list {scope}"
        );
    }
    let hybrid = h
        .worker
        .hybrid_search(with_token(
            TOKEN_BOB,
            pb::HybridSearchRequest {
                scope: Some(memory_scope_of(NAMESPACE_M, PERSONAL)),
                space: Some(h.space()),
                vector: vec![1.0, 0.0, 0.0, 0.0],
                text: "alice diagnosis".to_owned(),
                limit: 10,
                ..pb::HybridSearchRequest::default()
            },
        ))
        .await
        .expect("hybrid")
        .into_inner();
    assert!(hybrid.points.is_empty());
    // A crafted filter on the document key or a catch-all `should` only
    // narrows within Bob's own (empty) scope.
    let crafted = pb::Filter {
        should: vec![pb::Condition {
            key: "document_key".to_owned(),
            r#match: Some(pb::condition::Match::Keyword("secret".to_owned())),
        }],
        ..pb::Filter::default()
    };
    assert!(
        h.memory_read(TOKEN_BOB, PERSONAL, NAMESPACE_M, Some(crafted))
            .await
            .expect("read")
            .is_empty()
    );

    // Every delete selector of Bob's removes nothing of Alice's.
    for selector in [
        pb::delete_request::Selector::WholeNamespace(pb::WholeNamespace {}),
        pb::delete_request::Selector::Documents(pb::DocumentKeys {
            document_keys: vec!["secret".to_owned()],
        }),
        pb::delete_request::Selector::Generations(pb::Generations {
            generations: vec!["g".to_owned()],
        }),
    ] {
        h.memory_delete(TOKEN_BOB, PERSONAL, NAMESPACE_M, selector.clone())
            .await
            .expect("delete");
        h.memory_delete(TOKEN_BOB, PROJECT, NAMESPACE_M, selector)
            .await
            .expect("delete");
    }
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 1);

    // Bob cannot rewrite Alice's metadata either.
    h.worker
        .set_memory_metadata(with_token(
            TOKEN_BOB,
            pb::SetMemoryMetadataRequest {
                namespace: Some(memory_namespace(NAMESPACE_M, PERSONAL)),
                space: None,
                document_key: "secret".to_owned(),
                chunk_id: "v1".to_owned(),
                metadata_json: "{\"m\":{\"tampered\":true}}".to_owned(),
            },
        ))
        .await
        .expect("a no-op for Bob");
    let alice = h
        .memory_read(TOKEN_ALICE, PERSONAL, NAMESPACE_M, None)
        .await
        .expect("alice reads");
    assert_eq!(alice.len(), 1);
    assert!(!alice[0].metadata_json.contains("tampered"));
    // The `owner` key is not a filter key at all.
    let by_owner = pb::Filter {
        must: vec![pb::Condition {
            key: "owner".to_owned(),
            r#match: Some(pb::condition::Match::Keyword("user:11".to_owned())),
        }],
        ..pb::Filter::default()
    };
    let refused = h
        .memory_read(TOKEN_BOB, PERSONAL, NAMESPACE_M, Some(by_owner))
        .await
        .expect_err("owner is not filterable");
    assert_eq!(refused.code(), Code::InvalidArgument);
}

#[tokio::test]
async fn project_memory_is_shared_inside_a_project_and_never_across_projects() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.memory_upsert(
        TOKEN_ALICE,
        PROJECT,
        NAMESPACE_M,
        vec![memory_point(
            "team",
            1,
            "team standup at nine",
            [1.0, 0.0, 0.0, 0.0],
        )],
    )
    .await
    .expect("alice writes");
    // Bob reads, and rewrites it (shared: one point, last writer wins per id
    // is not the question here, he can write his own key).
    let seen = h
        .memory_search(TOKEN_BOB, PROJECT, NAMESPACE_M)
        .await
        .expect("bob reads");
    assert_eq!(seen.len(), 1);
    h.memory_upsert(
        TOKEN_BOB,
        PROJECT,
        NAMESPACE_M,
        vec![memory_point(
            "retro",
            1,
            "retro on friday",
            [1.0, 0.0, 0.0, 0.0],
        )],
    )
    .await
    .expect("bob writes");
    assert_eq!(
        h.memory_search(TOKEN_ALICE, PROJECT, NAMESPACE_M)
            .await
            .expect("alice reads")
            .len(),
        2
    );
    // Another project, even for the same user id: nothing, in either scope.
    for scope in [PERSONAL, PROJECT] {
        assert!(
            h.memory_search(TOKEN_ALICE_IN_B, scope, NAMESPACE_M)
                .await
                .expect("search")
                .is_empty()
        );
    }
    assert_eq!(
        h.memory_count(TOKEN_ALICE_IN_B, PROJECT, NAMESPACE_M)
            .await
            .expect("count"),
        0
    );
    // And its own write there stays there.
    h.memory_upsert(
        TOKEN_ALICE_IN_B,
        PERSONAL,
        NAMESPACE_M,
        vec![memory_point("k", 1, "alice in b", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("write in B");
    assert_eq!(h.truth_memory(PROJECT_B, Some("user:11")).await, 1);
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 0);
}

#[tokio::test]
async fn personal_memory_is_refused_for_a_token_without_a_user_or_in_a_shared_conversation() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    for (token, reason) in [
        (TOKEN_NO_USER, "memory.personal_unavailable"),
        (TOKEN_ALICE_SHARED, "memory.personal_shared_conversation"),
    ] {
        let status = h
            .memory_upsert(
                token,
                PERSONAL,
                NAMESPACE_M,
                vec![memory_point("k", 1, "nope", [1.0, 0.0, 0.0, 0.0])],
            )
            .await
            .expect_err("personal upsert");
        assert_eq!(status.code(), Code::PermissionDenied, "{token}");
        assert!(
            status.message().starts_with(reason),
            "{token}: {}",
            status.message()
        );
        for status in [
            h.memory_search(token, PERSONAL, NAMESPACE_M)
                .await
                .expect_err("search"),
            h.memory_read(token, PERSONAL, NAMESPACE_M, None)
                .await
                .expect_err("read"),
            h.memory_count(token, PERSONAL, NAMESPACE_M)
                .await
                .expect_err("count"),
            h.memory_documents(token, PERSONAL, NAMESPACE_M)
                .await
                .expect_err("list"),
            h.memory_delete(
                token,
                PERSONAL,
                NAMESPACE_M,
                pb::delete_request::Selector::WholeNamespace(pb::WholeNamespace {}),
            )
            .await
            .expect_err("delete"),
        ] {
            assert_eq!(status.code(), Code::PermissionDenied, "{token}");
            assert!(status.message().starts_with(reason));
        }
        // The project's memory works for both.
        h.memory_upsert(
            token,
            PROJECT,
            NAMESPACE_M,
            vec![memory_point(token, 1, "shared fact", [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect("project write");
    }
    assert_eq!(h.truth_memory(PROJECT_A, Some("project")).await, 2);
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 0);
}

#[tokio::test]
async fn memory_scope_is_required_for_memory_and_refused_elsewhere_and_other_tokens_cannot_use_memory()
 {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    // Memory without a scope.
    let status = h
        .memory_upsert(
            TOKEN_ALICE,
            0,
            NAMESPACE_M,
            vec![memory_point("k", 1, "x", [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect_err("no scope");
    assert_eq!(status.code(), Code::InvalidArgument);
    assert_eq!(
        h.memory_search(TOKEN_ALICE, 0, NAMESPACE_M)
            .await
            .expect_err("no scope")
            .code(),
        Code::InvalidArgument
    );
    // A scope on a source that has none.
    let mut namespace = Harness::namespace(NAMESPACE_A);
    namespace.memory_scope = PERSONAL;
    let status = h
        .upsert(
            TOKEN_ALICE,
            0,
            namespace,
            vec![Harness::point("d", "t", [1.0, 0.0, 0.0, 0.0])],
        )
        .await
        .expect_err("scope on an index");
    assert_eq!(status.code(), Code::InvalidArgument);
    // A worker token without the memory source, and a callback token.
    for token in [TOKEN_A, TOKEN_A_WIKI] {
        let status = h
            .memory_upsert(
                token,
                PROJECT,
                NAMESPACE_M,
                vec![memory_point("k", 1, "x", [1.0, 0.0, 0.0, 0.0])],
            )
            .await
            .expect_err("no memory source");
        assert_eq!(status.code(), Code::PermissionDenied, "{token}");
    }
    // An administrator has no memory scope at all.
    for scope in [PERSONAL, PROJECT] {
        let status = h
            .admin
            .count(tonic::Request::new(pb::CountRequest {
                scope: Some(pb::Scope {
                    project_id: PROJECT_A,
                    ..memory_scope_of(NAMESPACE_M, scope)
                }),
                space: None,
                filter: None,
            }))
            .await
            .expect_err("admin count of memory");
        assert_eq!(status.code(), Code::PermissionDenied);
    }
}

#[tokio::test]
async fn memory_is_never_listed_as_an_index() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_memory().await;
    h.upsert(
        TOKEN_A,
        0,
        Harness::namespace(NAMESPACE_A),
        vec![Harness::point("d", "alpha", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("an index too");
    let suffix = format!("{}_{DIMENSION}", h.slug);
    for token in [TOKEN_ALICE, TOKEN_BOB, TOKEN_A] {
        let listed = h
            .worker
            .list_indexes(with_token(token, pb::ListIndexesRequest::default()))
            .await
            .expect("list")
            .into_inner()
            .indexes;
        assert!(
            listed
                .iter()
                .filter(|index| index.collection.ends_with(&suffix))
                .all(|index| index.source != pb::Source::Memory as i32
                    && index.namespace_id != NAMESPACE_M),
            "{token}: {listed:?}"
        );
        let status = h
            .worker
            .list_indexes(with_token(
                token,
                pb::ListIndexesRequest {
                    project_id: 0,
                    source: pb::Source::Memory as i32,
                },
            ))
            .await
            .expect_err("memory is not an index");
        assert_eq!(status.code(), Code::InvalidArgument, "{token}");
    }
    let admin = h
        .admin
        .list_indexes(tonic::Request::new(pb::ListIndexesRequest {
            project_id: PROJECT_A,
            source: 0,
        }))
        .await
        .expect("admin list")
        .into_inner()
        .indexes;
    assert!(
        admin
            .iter()
            .all(|index| index.source != pb::Source::Memory as i32)
    );
}

#[tokio::test]
async fn set_memory_metadata_changes_the_owners_point_and_keeps_its_vector() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_memory().await;
    h.worker
        .set_memory_metadata(with_token(
            TOKEN_ALICE,
            pb::SetMemoryMetadataRequest {
                namespace: Some(memory_namespace(NAMESPACE_M, PERSONAL)),
                space: Some(h.space()),
                document_key: "k".to_owned(),
                chunk_id: "v1".to_owned(),
                metadata_json: "{\"t\":{\"valid_to\":123}}".to_owned(),
            },
        ))
        .await
        .expect("alice sets");
    let alice = h
        .memory_read(TOKEN_ALICE, PERSONAL, NAMESPACE_M, None)
        .await
        .expect("read");
    assert_eq!(alice.len(), 1);
    assert!(
        alice[0].metadata_json.contains("123"),
        "{}",
        alice[0].metadata_json
    );
    // Bob's point of the same key, and the project's, are unchanged.
    for (token, scope) in [(TOKEN_BOB, PERSONAL), (TOKEN_BOB, PROJECT)] {
        let points = h
            .memory_read(token, scope, NAMESPACE_M, None)
            .await
            .expect("read");
        assert_eq!(points.len(), 1);
        assert!(!points[0].metadata_json.contains("123"), "{scope}");
    }
    // The vector is still there: a dense search still finds it.
    assert_eq!(
        h.memory_search(TOKEN_ALICE, PERSONAL, NAMESPACE_M)
            .await
            .expect("search")
            .len(),
        1
    );
    // A non-memory source, a non-object and a missing key are refused.
    let bad = |namespace: pb::Namespace, key: &str, metadata: &str| pb::SetMemoryMetadataRequest {
        namespace: Some(namespace),
        space: None,
        document_key: key.to_owned(),
        chunk_id: "v1".to_owned(),
        metadata_json: metadata.to_owned(),
    };
    for request in [
        bad(Harness::namespace(NAMESPACE_A), "k", "{}"),
        bad(memory_namespace(NAMESPACE_M, PERSONAL), "k", "[1]"),
        bad(memory_namespace(NAMESPACE_M, PERSONAL), "", "{}"),
    ] {
        assert_eq!(
            h.worker
                .set_memory_metadata(with_token(TOKEN_ALICE, request))
                .await
                .expect_err("refused")
                .code(),
            Code::InvalidArgument
        );
    }
}

#[tokio::test]
async fn only_an_administrator_deletes_a_users_personal_memories_or_a_namespace() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_memory().await;
    h.memory_upsert(
        TOKEN_ALICE,
        PERSONAL,
        NAMESPACE_M2,
        vec![memory_point("k2", 1, "alice second", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("second namespace");
    h.memory_upsert(
        TOKEN_ALICE_IN_B,
        PERSONAL,
        NAMESPACE_M,
        vec![memory_point("k", 1, "alice in b", [1.0, 0.0, 0.0, 0.0])],
    )
    .await
    .expect("project b");

    // A token (even Alice's own) cannot use the user-wide delete.
    for token in [TOKEN_ALICE, TOKEN_BOB] {
        let status = h
            .worker
            .delete_memory_user(with_token(
                token,
                pb::DeleteMemoryUserRequest {
                    project_id: 0,
                    user_id: ALICE,
                },
            ))
            .await
            .expect_err("a token cannot");
        assert_eq!(status.code(), Code::PermissionDenied);
        let status = h
            .worker
            .delete_memory_namespace(with_token(
                token,
                pb::DeleteMemoryNamespaceRequest {
                    project_id: 0,
                    namespace_id: NAMESPACE_M.to_owned(),
                },
            ))
            .await
            .expect_err("a token cannot");
        assert_eq!(status.code(), Code::PermissionDenied);
    }
    assert_eq!(h.truth_memory(PROJECT_A, None).await, 4);

    // The administrator removes Alice's personal points in project A only.
    h.admin
        .delete_memory_user(tonic::Request::new(pb::DeleteMemoryUserRequest {
            project_id: PROJECT_A,
            user_id: ALICE,
        }))
        .await
        .expect("delete user");
    assert_eq!(h.truth_memory(PROJECT_A, Some("user:11")).await, 0);
    assert_eq!(
        h.truth_memory(PROJECT_A, Some("user:12")).await,
        1,
        "Bob's stay"
    );
    assert_eq!(
        h.truth_memory(PROJECT_A, Some("project")).await,
        1,
        "the project's stay"
    );
    assert_eq!(
        h.truth_memory(PROJECT_B, Some("user:11")).await,
        1,
        "another project's stay"
    );
    // Not a positive user id.
    assert_eq!(
        h.admin
            .delete_memory_user(tonic::Request::new(pb::DeleteMemoryUserRequest {
                project_id: PROJECT_A,
                user_id: 0,
            }))
            .await
            .expect_err("invalid")
            .code(),
        Code::InvalidArgument
    );

    // The namespace delete removes every owner's points of that namespace.
    h.admin
        .delete_memory_namespace(tonic::Request::new(pb::DeleteMemoryNamespaceRequest {
            project_id: PROJECT_A,
            namespace_id: NAMESPACE_M.to_owned(),
        }))
        .await
        .expect("delete namespace");
    assert_eq!(h.truth_memory(PROJECT_A, None).await, 0);
    assert_eq!(
        h.truth_memory(PROJECT_B, None).await,
        1,
        "another project's stay"
    );
}

#[tokio::test]
async fn an_administrator_cannot_name_memory_through_the_ordinary_operations() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    h.seed_memory().await;
    for scope in [PERSONAL, PROJECT, 0] {
        let status = h
            .admin
            .delete(tonic::Request::new(pb::DeleteRequest {
                project_id: PROJECT_A,
                space: None,
                namespace: Some(memory_namespace(NAMESPACE_M, scope)),
                selector: Some(pb::delete_request::Selector::WholeNamespace(
                    pb::WholeNamespace {},
                )),
            }))
            .await
            .expect_err("admin delete of memory");
        assert!(
            matches!(
                status.code(),
                Code::PermissionDenied | Code::InvalidArgument
            ),
            "{scope}: {status:?}"
        );
    }
    assert_eq!(h.truth_memory(PROJECT_A, None).await, 3);
    // DropProject still removes everything of the project, memory included.
    h.admin
        .drop_project(tonic::Request::new(pb::DropProjectRequest {
            project_id: PROJECT_A,
        }))
        .await
        .expect("drop");
    assert_eq!(h.truth_memory(PROJECT_A, None).await, 0);
}

/// The temporal filters the memory tools send (`is_null` and ranges on
/// `metadata.t.*`, a nested `should`) mean what the tools assume on the real
/// store, and closing a version in place changes what they select.
#[tokio::test]
#[allow(clippy::too_many_lines)] // One scenario, each filter in turn.
async fn the_memory_tools_temporal_filters_work_on_the_real_store() {
    let _ = require_qdrant!();
    let mut h = harness().await.expect("harness");
    let version = |id: &str, from: i64, to: Option<i64>| {
        let mut point = memory_point("k", 1, &format!("version {id}"), [1.0, 0.0, 0.0, 0.0]);
        point.chunk_id = format!("v{id}");
        point.metadata_json =
            serde_json::json!({"m": {}, "t": {"valid_from": from, "valid_to": to, "id": id}})
                .to_string();
        point
    };
    for (id, from, to, generation) in [("a", 500, Some(1000), "g-a"), ("b", 1000, None, "g-b")] {
        let mut namespace = memory_namespace(NAMESPACE_M, PROJECT);
        namespace.generation = generation.to_owned();
        h.upsert(TOKEN_ALICE, 0, namespace, vec![version(id, from, to)])
            .await
            .expect("version");
    }
    let key = |value: &str| pb::Condition {
        key: "document_key".to_owned(),
        r#match: Some(pb::condition::Match::Keyword(value.to_owned())),
    };
    let is_null = pb::Condition {
        key: "metadata.t.valid_to".to_owned(),
        r#match: Some(pb::condition::Match::IsNull(pb::IsNull {})),
    };
    let range = |name: &str, gt: Option<f64>, lte: Option<f64>| pb::Condition {
        key: name.to_owned(),
        r#match: Some(pb::condition::Match::Range(pb::Range {
            gt,
            gte: None,
            lt: None,
            lte,
        })),
    };
    let ids = |points: &[pb::ScoredPoint]| {
        let mut ids: Vec<String> = points.iter().map(|p| p.chunk_id.clone()).collect();
        ids.sort();
        ids
    };
    let filter = |must: Vec<pb::Condition>| {
        Some(pb::Filter {
            must,
            ..pb::Filter::default()
        })
    };

    // Open versions: only "b".
    let open = h
        .memory_read(
            TOKEN_ALICE,
            PROJECT,
            NAMESPACE_M,
            filter(vec![key("k"), is_null.clone()]),
        )
        .await
        .expect("open");
    assert_eq!(ids(&open), ["vb"]);
    // As of 700: valid_from <= 700 AND (valid_to is null OR valid_to > 700): "a".
    let as_of = |at: f64| {
        filter(vec![
            key("k"),
            range("metadata.t.valid_from", None, Some(at)),
            pb::Condition {
                key: String::new(),
                r#match: Some(pb::condition::Match::Filter(pb::Filter {
                    should: vec![
                        is_null.clone(),
                        range("metadata.t.valid_to", Some(at), None),
                    ],
                    ..pb::Filter::default()
                })),
            },
        ])
    };
    assert_eq!(
        ids(&h
            .memory_read(TOKEN_ALICE, PROJECT, NAMESPACE_M, as_of(700.0))
            .await
            .expect("as of")),
        ["va"]
    );
    // At the boundary instant the newer version is the one valid.
    assert_eq!(
        ids(&h
            .memory_read(TOKEN_ALICE, PROJECT, NAMESPACE_M, as_of(1000.0))
            .await
            .expect("boundary")),
        ["vb"]
    );
    assert!(
        h.memory_read(TOKEN_ALICE, PROJECT, NAMESPACE_M, as_of(100.0))
            .await
            .expect("before")
            .is_empty()
    );
    // The same filters on a search.
    let hits = h
        .search(
            TOKEN_ALICE,
            memory_scope_of(NAMESPACE_M, PROJECT),
            as_of(700.0),
        )
        .await
        .expect("search");
    assert_eq!(ids(&hits), ["va"]);

    // Close "b" in place: it is no longer open, its vector is still searchable.
    h.worker
        .set_memory_metadata(with_token(
            TOKEN_ALICE,
            pb::SetMemoryMetadataRequest {
                namespace: Some(pb::Namespace {
                    generation: "g-b".to_owned(),
                    ..memory_namespace(NAMESPACE_M, PROJECT)
                }),
                space: Some(h.space()),
                document_key: "k".to_owned(),
                chunk_id: "vb".to_owned(),
                metadata_json: serde_json::json!({"m": {}, "t": {"valid_from": 1000, "valid_to": 2000, "id": "b"}}).to_string(),
            },
        ))
        .await
        .expect("close");
    assert!(
        h.memory_read(
            TOKEN_ALICE,
            PROJECT,
            NAMESPACE_M,
            filter(vec![key("k"), is_null])
        )
        .await
        .expect("none open")
        .is_empty()
    );
    let hits = h
        .memory_search(TOKEN_ALICE, PROJECT, NAMESPACE_M)
        .await
        .expect("search");
    assert_eq!(hits.len(), 2, "both versions are still points with vectors");
    // A version is deleted alone by its generation; the other stays.
    h.memory_delete(
        TOKEN_ALICE,
        PROJECT,
        NAMESPACE_M,
        pb::delete_request::Selector::Generations(pb::Generations {
            generations: vec!["g-a".to_owned()],
        }),
    )
    .await
    .expect("delete one version");
    let left = h
        .memory_read(TOKEN_ALICE, PROJECT, NAMESPACE_M, None)
        .await
        .expect("left");
    assert_eq!(ids(&left), ["vb"]);
}
