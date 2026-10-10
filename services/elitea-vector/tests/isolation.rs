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
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use elitea_vector::auth::{
    Authenticator, CachePolicy, CachingIntrospector, GrpcIntrospector, IntrospectError,
    Introspector, TokenGrant, unix_now,
};
use elitea_vector::pb;
use elitea_vector::pb::vector_service_client::VectorServiceClient;
use elitea_vector::pb::vector_service_server::VectorServiceServer;
use elitea_vector::service::VectorService;
use elitea_vector::store::{CollectionSettings, Store};
use qdrant_client::Qdrant;
use rcgen::{BasicConstraints, CertificateParams, IsCa, Issuer, KeyPair, KeyUsagePurpose};
use tokio_stream::wrappers::TcpListenerStream;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity, Server, ServerTlsConfig};
use tonic::{Code, Request};

const PROJECT_A: i64 = 101;
const PROJECT_B: i64 = 202;
const TOKEN_A: &str = "token-a";
const TOKEN_B: &str = "token-b";
const TOKEN_EXPIRED: &str = "token-expired";
const TOKEN_SHORT: &str = "token-short";
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

fn grant(project_id: i64, expires_at_unix: i64) -> TokenGrant {
    TokenGrant {
        project_id,
        principal: format!("user:{project_id}"),
        kind: pb::TokenKind::EngineCallback,
        expires_at_unix,
    }
}

fn oracle() -> Arc<dyn Introspector> {
    let now = unix_now();
    let tokens = Tokens(HashMap::from([
        (TOKEN_A.to_owned(), grant(PROJECT_A, now + 3600)),
        (TOKEN_B.to_owned(), grant(PROJECT_B, now + 3600)),
        (TOKEN_EXPIRED.to_owned(), grant(PROJECT_A, now - 1)),
        (TOKEN_SHORT.to_owned(), grant(PROJECT_A, now + 3)),
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
    let store = Arc::new(Store::new(
        Qdrant::from_url(url)
            .skip_compatibility_check()
            .build()
            .expect("qdrant client"),
        CollectionSettings::default(),
    ));
    let auth = Authenticator::new(introspector, HashSet::from(["dns:elitea-main".to_owned()]));
    let service = VectorService::new(auth, store);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let tls = ServerTlsConfig::new()
        .identity(Identity::from_pem(&pki.server.0, &pki.server.1))
        .client_ca_root(Certificate::from_pem(&pki.ca_pem));
    tokio::spawn(
        Server::builder()
            .tls_config(tls)
            .expect("server tls")
            .add_service(VectorServiceServer::new(service))
            .serve_with_incoming(TcpListenerStream::new(listener)),
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
    let url = qdrant_url()?;
    let serial = SERIAL.lock().await;
    let pki = pki();
    let address = serve(&url, oracle(), &pki).await;
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
        let messages = vec![
            pb::UpsertRequest {
                message: Some(pb::upsert_request::Message::Header(pb::UpsertHeader {
                    project_id,
                    space: Some(self.space()),
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
            generation: String::new(),
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
    let read = |generation: &str| pb::Scope {
        generation: generation.to_owned(),
        ..Harness::scope(&[NAMESPACE_A])
    };
    let g1 = h.search(TOKEN_A, read("g1"), None).await.expect("g1");
    assert_eq!(g1.len(), 1);
    assert_eq!(g1[0].text, "g1");
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
