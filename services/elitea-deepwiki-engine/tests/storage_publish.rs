//! The build space and the publish transaction against PostgreSQL:
//! atomicity under concurrent readers, the refusals, the graph mapping, the
//! `wikis` row and the read surface (`UnifiedDb`).

mod storage_common;

use elitea_deepwiki_engine::graph::{CodeGraph, EdgeData, NodeData};
use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::adapter::{Scope, UnifiedDb};
use elitea_deepwiki_engine::storage::build::{BuildSpace, WikiRecord};
use elitea_deepwiki_engine::storage::rows::IndexNode;
use elitea_deepwiki_engine::storage::search::Hybrid;
use serde_json::json;
use sqlx::Row;
use sqlx::postgres::PgPool;
use std::borrow::Cow;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use storage_common as common;

const DIM: usize = 8;

/// One version of an index: `count` nodes named `{tag}{i}`, a chain of
/// edges, one vector each. Every node's text holds the word `common`.
fn version(tag: &str, count: usize) -> (Vec<IndexNode>, Vec<(String, Vec<f64>)>) {
    let nodes = (0..count)
        .map(|i| IndexNode {
            node_id: format!("{tag}/{i:05}.py::f"),
            rel_path: format!("{tag}/{i:05}.py"),
            file_name: format!("{i:05}.py"),
            language: "python".into(),
            start_line: 1,
            end_line: 3,
            symbol_name: format!("{tag}{i}"),
            symbol_type: "function".into(),
            source_text: format!("def {tag}{i}():\n    return common_{tag} + common\n"),
            ..IndexNode::default()
        })
        .collect::<Vec<_>>();
    let vectors = nodes
        .iter()
        .enumerate()
        .map(|(i, node)| {
            let base = if tag == "alpha" { 1.0 } else { -1.0 };
            let vector = (0..DIM)
                .map(|d| base + f64::from(u32::try_from(i * DIM + d).expect("small")) / 1e4)
                .collect();
            (node.node_id.clone(), vector)
        })
        .collect();
    (nodes, vectors)
}

async fn publish_version(space: &BuildSpace, wiki: &str, tag: &str, count: usize) {
    let (nodes, vectors) = version(tag, count);
    let edges: Vec<_> = nodes
        .windows(2)
        .map(|pair| elitea_deepwiki_engine::storage::rows::IndexEdge {
            source_id: pair[0].node_id.clone(),
            target_id: pair[1].node_id.clone(),
            rel_type: "calls".into(),
            edge_class: Some("structural".into()),
            weight: 1.0,
        })
        .collect();
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    build.stage_nodes(nodes).await.expect("nodes");
    build.stage_edges(&edges).await.expect("edges");
    build
        .stage_embeddings(
            common::TEST_MODEL,
            vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())),
        )
        .await
        .expect("embeddings");
    build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
}

/// Everything a reader can count about one wiki, in ONE statement (one
/// snapshot under READ COMMITTED). `vectors` is 1 when the wiki names an
/// active vector generation (the vectors are in the dense store).
async fn observe(pool: &PgPool, wiki: &str) -> (i64, i64, i64, i64, i64, i64) {
    let row = sqlx::query(
        "SELECT \
           (SELECT count(*) FROM wiki_nodes WHERE wiki_id = $1) AS nodes, \
           (SELECT count(*) FROM wiki_edges WHERE wiki_id = $1) AS edges, \
           (SELECT count(*) FROM wikis WHERE wiki_id = $1 AND active_generation IS NOT NULL) AS vectors, \
           (SELECT count(*) FROM wiki_bm25_docs WHERE wiki_id = $1 AND branch = 'fts') AS fts_docs, \
           (SELECT coalesce(max(doc_count), -1)::bigint FROM wiki_bm25_meta \
              WHERE wiki_id = $1 AND branch = 'fts') AS fts, \
           (SELECT count(*) FROM wiki_nodes WHERE wiki_id = $1 AND symbol_name LIKE 'alpha%') AS alpha",
    )
    .bind(wiki)
    .fetch_one(pool)
    .await
    .expect("observe");
    (
        row.get("nodes"),
        row.get("edges"),
        row.get("vectors"),
        row.get("fts_docs"),
        row.get("fts"),
        row.get("alpha"),
    )
}

/// The ADR-0026 phase 3 proof: "a reader never sees a partial index during
/// publish". Readers poll without pause while the wiki is republished,
/// alternating between two versions of different sizes; every observation
/// must be exactly one whole version.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[allow(clippy::too_many_lines)] // the readers, the publishes, the checks
async fn readers_never_see_a_partial_index() {
    let Some(pool) = common::fresh_database("publish_atomic").await else {
        return;
    };
    let wiki = "acme--atomic--main";
    let (space, store) = common::dense_space(&pool, "publisher");
    let (alpha, beta) = (3_000, 5_000);
    publish_version(&space, wiki, "alpha", alpha).await;
    let full = |count: usize, alpha_nodes: usize| {
        let n = i64::try_from(count).expect("small");
        (
            n,
            n - 1,
            1,
            n,
            n,
            i64::try_from(alpha_nodes).expect("small"),
        )
    };
    let expected = [full(alpha, alpha), full(beta, 0)];
    assert_eq!(observe(&pool, wiki).await, expected[0]);

    let stop = Arc::new(AtomicBool::new(false));
    let mut readers = Vec::new();
    for reader in 0..3 {
        let pool = pool.clone();
        let stop = Arc::clone(&stop);
        let dense = common::dense(&store);
        readers.push(tokio::spawn(async move {
            let db = UnifiedDb::new(pool.clone(), common::key(wiki)).with_dense(dense);
            let mut seen = [0_usize; 2];
            while !stop.load(Ordering::Relaxed) {
                if reader == 0 {
                    // A multi-statement read: one snapshot per search.
                    let rows = db
                        .search_hybrid(
                            "common",
                            Some(&[1.0; DIM]),
                            &Scope::default(),
                            &Hybrid::default(),
                        )
                        .await
                        .expect("search");
                    assert_eq!(rows.len(), 20, "a search saw an empty or partial index");
                    let alphas = rows
                        .iter()
                        .filter(|r| r.node.symbol_name.starts_with("alpha"))
                        .count();
                    assert!(
                        alphas == 0 || alphas == rows.len(),
                        "a search mixed two versions"
                    );
                    seen[usize::from(alphas == 0)] += 1;
                } else {
                    let observed = observe(&pool, wiki).await;
                    let which = expected
                        .iter()
                        .position(|e| *e == observed)
                        .unwrap_or_else(|| panic!("a partial index was visible: {observed:?}"));
                    seen[which] += 1;
                }
            }
            seen
        }));
    }
    let publishes = 6;
    let started = std::time::Instant::now();
    for round in 0..publishes {
        if round % 2 == 0 {
            publish_version(&space, wiki, "beta", beta).await;
        } else {
            publish_version(&space, wiki, "alpha", alpha).await;
        }
    }
    let elapsed = started.elapsed();
    stop.store(true, Ordering::Relaxed);
    let mut total = [0_usize; 2];
    for reader in readers {
        let seen = reader.await.expect("reader");
        total[0] += seen[0];
        total[1] += seen[1];
    }
    eprintln!(
        "{publishes} publishes in {:.2}s; readers saw whole version A {} times, whole version B {} times, never a partial one",
        elapsed.as_secs_f64(),
        total[0],
        total[1]
    );
    assert!(
        total[0] > 0 && total[1] > 0,
        "readers did not observe both versions: {total:?}"
    );
    // Every build was removed by its publish.
    let builds: i64 = sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.builds")
        .fetch_one(&pool)
        .await
        .expect("builds");
    let staged: i64 = sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.wiki_nodes")
        .fetch_one(&pool)
        .await
        .expect("staged");
    assert_eq!((builds, staged), (0, 0));
    // Each publish deleted the generations before it: the dense store holds
    // the last version's vectors only (the last round published alpha).
    let generations = store.generations(&common::key(wiki));
    assert_eq!(generations.len(), 1, "{generations:?}");
    assert_eq!(generations[0].1, alpha);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_failed_publish_leaves_the_live_index_untouched() {
    let Some(pool) = common::fresh_database("publish_refusals").await else {
        return;
    };
    let wiki = "acme--refuse--main";
    let (space, store) = common::dense_space(&pool, "publisher");
    publish_version(&space, wiki, "alpha", 50).await;
    let before = observe(&pool, wiki).await;
    let live_generations = store.generations(&common::key(wiki));

    // An empty build is never published over a possibly good one.
    let mut empty = space.begin(&common::key(wiki)).await.expect("begin");
    let refused = empty.publish(&WikiRecord::default()).await;
    assert!(
        matches!(&refused, Err(StorageError::Publish(m)) if m.contains("staged no nodes")),
        "{refused:?}"
    );
    assert_eq!(observe(&pool, wiki).await, before);

    // A value PostgreSQL cannot store is refused before anything is
    // written, naming the row: a non-finite edge weight.
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    let bad = elitea_deepwiki_engine::storage::rows::IndexEdge {
        source_id: "bad.py::f".into(),
        target_id: "bad.py::g".into(),
        rel_type: "calls".into(),
        edge_class: None,
        weight: f64::NAN,
    };
    let refused = build.stage_edges(&[bad]).await;
    assert!(
        matches!(&refused, Err(StorageError::Publish(m)) if m.contains("bad.py::f") && m.contains("non-finite")),
        "{refused:?}"
    );
    // An embedding for a node the build does not have is refused, and so
    // is another dimension than the build's first vectors.
    let (beta_nodes, beta_vectors) = version("beta", 5);
    build
        .stage_nodes(beta_nodes)
        .await
        .expect("stage after a refused COPY");
    let orphan = build
        .stage_embeddings(common::TEST_MODEL, [("not-a-node", [1.0; DIM].as_slice())])
        .await;
    assert!(
        matches!(&orphan, Err(StorageError::Publish(m)) if m.contains("not-a-node")),
        "{orphan:?}"
    );
    build
        .stage_embeddings(
            common::TEST_MODEL,
            [(beta_vectors[0].0.as_str(), beta_vectors[0].1.as_slice())],
        )
        .await
        .expect("a vector");
    let other = build
        .stage_embeddings(
            common::TEST_MODEL,
            [(beta_vectors[1].0.as_str(), [1.0; 3].as_slice())],
        )
        .await;
    assert!(
        matches!(&other, Err(StorageError::Publish(m)) if m.contains("dimensions")),
        "{other:?}"
    );
    // The build wrote one vector under its own generation, which no reader
    // names; abandoning it deletes that generation.
    assert_eq!(store.generations(&common::key(wiki)).len(), 2);
    build.abandon().await.expect("abandon");
    assert_eq!(observe(&pool, wiki).await, before);
    assert_eq!(store.generations(&common::key(wiki)), live_generations);

    // elitea-vector refusing the build's vectors fails the staging; the
    // live index and its vectors stay.
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    build
        .stage_nodes(version("beta", 5).0)
        .await
        .expect("nodes");
    store.set_unavailable(true);
    let refused = build
        .stage_embeddings(
            common::TEST_MODEL,
            beta_vectors
                .iter()
                .map(|(id, v)| (id.as_str(), v.as_slice())),
        )
        .await;
    assert!(
        matches!(&refused, Err(StorageError::Vector(m)) if m.contains("elitea-vector")),
        "{refused:?}"
    );
    store.set_unavailable(false);
    build.abandon().await.expect("abandon");
    assert_eq!(observe(&pool, wiki).await, before);
    assert_eq!(store.generations(&common::key(wiki)), live_generations);
}

/// The publish flips `wikis.active_generation` to the build's generation in
/// the same transaction as the rows, and only after the commit deletes the
/// namespace's other generations (an abandoned build's included). A publish
/// that fails keeps the previous generation active.
#[tokio::test(flavor = "multi_thread")]
async fn the_publish_flips_the_active_generation_and_cleans_the_old_ones() {
    let Some(pool) = common::fresh_database("publish_generations").await else {
        return;
    };
    let wiki = "acme--generations--main";
    let key = common::key(wiki);
    let (space, store) = common::dense_space(&pool, "publisher");
    let active = |pool: PgPool| async move {
        sqlx::query_as::<_, (Option<String>, Option<String>, Option<i32>)>(
            "SELECT active_generation, embedding_model, embedding_dim FROM wikis WHERE wiki_id = $1",
        )
        .bind(wiki)
        .fetch_one(&pool)
        .await
        .expect("wikis row")
    };

    publish_version(&space, wiki, "alpha", 10).await;
    let (first, model, dim) = active(pool.clone()).await;
    let first = first.expect("an active generation");
    assert_eq!(model.as_deref(), Some(common::TEST_MODEL));
    assert_eq!(dim, Some(i32::try_from(DIM).expect("small")));
    assert_eq!(store.generations(&key), vec![(first.clone(), 10)]);

    // A build that wrote its vectors and then failed its publish: the old
    // generation stays active, the new one is never searched.
    let (nodes, vectors) = version("beta", 4);
    let mut failing = space.begin(&key).await.expect("begin");
    let failing_id = failing.build_id().to_owned();
    failing.stage_nodes(nodes).await.expect("nodes");
    failing
        .stage_embeddings(
            common::TEST_MODEL,
            vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())),
        )
        .await
        .expect("embeddings");
    let refused = failing
        .publish(&WikiRecord {
            embedding_model: Some("another-model".into()),
            ..WikiRecord::default()
        })
        .await;
    assert!(
        matches!(refused, Err(StorageError::Publish(_))),
        "{refused:?}"
    );
    assert_eq!(
        active(pool.clone()).await.0.as_deref(),
        Some(first.as_str())
    );
    // Still held (it could retry): its generation is in flight.
    assert_eq!(store.generations(&key).len(), 2);
    let db = UnifiedDb::new(pool.clone(), key.clone()).with_dense(common::dense(&store));
    let hits = db
        .reader()
        .search_dense(&vectors[0].1, 50)
        .await
        .expect("dense");
    assert!(
        hits.iter().all(|h| h.node_id.starts_with("alpha/")),
        "a reader searched a generation that is not active: {hits:?}"
    );

    // The next publish activates its own generation and deletes the one it
    // replaced; the build still in flight keeps its vectors.
    publish_version(&space, wiki, "beta", 6).await;
    let second = active(pool.clone()).await.0.expect("active");
    assert_ne!(second, first);
    assert_ne!(second, failing_id);
    let mut expected = vec![(second.clone(), 6), (failing_id.clone(), 4)];
    expected.sort();
    assert_eq!(store.generations(&key), expected);
    assert!(store.calls().contains(&"delete"));
    // Abandoned, it deletes its own.
    failing.abandon().await.expect("abandon");
    assert_eq!(store.generations(&key), vec![(second, 6)]);
}

async fn stale_generations(pool: &PgPool, wiki: &str) -> Vec<String> {
    sqlx::query_scalar("SELECT stale_generations FROM wikis WHERE wiki_id = $1")
        .bind(wiki)
        .fetch_one(pool)
        .await
        .expect("wikis row")
}

/// Two builds of one wiki in flight at once (only their publishes are
/// serialised): B publishes and sweeps, then A publishes and sweeps. B's
/// sweep must not delete A's vectors (A's generation is not stale), A's
/// dense search answers, and the stale list ends empty. A build the sweep
/// removes without a bearer is recorded stale, and a failed sweep keeps the
/// list for the next publish.
#[tokio::test(flavor = "multi_thread")]
async fn a_concurrent_build_keeps_its_vectors_through_another_publish() {
    let Some(pool) = common::fresh_database("publish_concurrent").await else {
        return;
    };
    let wiki = "acme--concurrent--main";
    let key = common::key(wiki);
    let (space, store) = common::dense_space(&pool, "publisher");
    publish_version(&space, wiki, "alpha", 5).await;

    let stage = |tag: &'static str, count: usize| {
        let space = space.clone();
        let key = key.clone();
        async move {
            let (nodes, vectors) = version(tag, count);
            let mut build = space.begin(&key).await.expect("begin");
            build.stage_nodes(nodes).await.expect("nodes");
            build
                .stage_embeddings(
                    common::TEST_MODEL,
                    vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())),
                )
                .await
                .expect("embeddings");
            build
        }
    };
    let mut a = stage("alpha", 7).await;
    let mut b = stage("beta", 3).await;
    let a_id = a.build_id().to_owned();
    b.publish(&WikiRecord::default())
        .await
        .expect("B publishes");
    assert_eq!(
        store
            .vector(&key, &a_id, "alpha/00000.py::f")
            .map(|v| v.len()),
        Some(DIM),
        "B's sweep deleted the vectors of A, which is still in flight"
    );
    assert!(stale_generations(&pool, wiki).await.is_empty());
    a.publish(&WikiRecord::default())
        .await
        .expect("A publishes");
    assert_eq!(store.generations(&key), vec![(a_id.clone(), 7)]);
    assert!(stale_generations(&pool, wiki).await.is_empty());
    let db = UnifiedDb::new(pool.clone(), key.clone()).with_dense(common::dense(&store));
    let hits = db
        .reader()
        .search_dense(&[1.0; DIM], 5)
        .await
        .expect("dense");
    assert_eq!(hits.len(), 5, "{hits:?}");

    // A build removed by the sweep (no bearer) is recorded stale.
    let c = stage("beta", 2).await;
    let c_id = c.build_id().to_owned();
    drop(c);
    space.sweep(std::time::Duration::ZERO).await.expect("sweep");
    assert_eq!(stale_generations(&pool, wiki).await, vec![c_id.clone()]);
    assert!(store.generations(&key).iter().any(|(g, _)| *g == c_id));
    // A failed sweep keeps the list for the next publish.
    let mut d = stage("alpha", 4).await;
    let d_id = d.build_id().to_owned();
    store.fail_deletes(true);
    d.publish(&WikiRecord::default())
        .await
        .expect("D publishes");
    let mut listed = stale_generations(&pool, wiki).await;
    listed.sort();
    let mut expected = vec![c_id.clone(), a_id.clone()];
    expected.sort();
    assert_eq!(listed, expected);
    store.fail_deletes(false);
    publish_version(&space, wiki, "beta", 3).await;
    assert!(stale_generations(&pool, wiki).await.is_empty());
    let left: Vec<String> = store
        .generations(&key)
        .into_iter()
        .map(|(g, _)| g)
        .collect();
    assert_eq!(left.len(), 1);
    assert!(!left.contains(&d_id) && !left.contains(&c_id) && !left.contains(&a_id));
}

/// When elitea-vector cannot be reached the hybrid search answers with the
/// lexical branch alone, and says so in the log; it does not fail.
#[tokio::test(flavor = "multi_thread")]
async fn an_unreachable_vector_store_degrades_to_lexical() {
    let Some(pool) = common::fresh_database("publish_degrade").await else {
        return;
    };
    let wiki = "acme--degrade--main";
    let (space, store) = common::dense_space(&pool, "publisher");
    publish_version(&space, wiki, "alpha", 30).await;
    let db = UnifiedDb::new(pool.clone(), common::key(wiki)).with_dense(common::dense(&store));
    assert!(db.vec_available().await.expect("vec"));
    let fused = db
        .search_hybrid(
            "common",
            Some(&[1.0; DIM]),
            &Scope::default(),
            &Hybrid::default(),
        )
        .await
        .expect("search");
    // Every dense hit is also a lexical hit here, so the rows keep the FTS
    // fields; the dense branch shows in the fused score (more than the FTS
    // weight alone can give).
    let lexical_only_best = 0.4 / 61.0;
    assert!(store.calls().contains(&"search"));
    assert!(fused[0].scores.combined_score.expect("score") > lexical_only_best);

    store.set_unavailable(true);
    let lexical = db
        .search_hybrid(
            "common",
            Some(&[1.0; DIM]),
            &Scope::default(),
            &Hybrid::default(),
        )
        .await
        .expect("a search without elitea-vector still answers");
    assert_eq!(lexical.len(), 20);
    assert!(lexical.iter().all(|r| r.scores.vec_distance.is_none()));
    assert!(
        lexical
            .iter()
            .all(|r| r.scores.combined_score.expect("score") <= lexical_only_best)
    );
    assert!(lexical.iter().all(|r| r.scores.fts_rank.is_some()));
    // Without a dense store at all, no dense branch either.
    let plain = UnifiedDb::new(pool, common::key(wiki));
    assert!(!plain.vec_available().await.expect("vec"));
}

fn node(path: &str, name: &str, kind: &'static str, text: &str) -> NodeData {
    NodeData {
        rel_path: path.into(),
        file_name: path.rsplit('/').next().unwrap_or(path).into(),
        language: "python".into(),
        start_line: 1,
        end_line: 4,
        symbol_name: name.into(),
        symbol_type: Cow::Borrowed(kind),
        source_text: text.into(),
        ..NodeData::default()
    }
}

fn edge(rel: &'static str, class: &'static str, weight: f64) -> EdgeData {
    EdgeData {
        rel_type: Cow::Borrowed(rel),
        edge_class: Cow::Borrowed(class),
        weight,
        ..EdgeData::default()
    }
}

const GRAPH_WIKI: &str = "acme--graph--main";

/// A four-node graph: a class, a function, a test, a document; parallel
/// edges on one `(source, target, rel_type)`, and a zero weight.
fn small_graph() -> CodeGraph {
    let mut graph = CodeGraph::new();
    graph.add_node(
        "src/app.py::App",
        node("src/app.py", "App", "class", "class App:\n    pass\n"),
    );
    graph.add_node(
        "src/app.py::run",
        node(
            "src/app.py",
            "run",
            "function",
            "def run(app):\n    return app.serve()\n",
        ),
    );
    graph.add_node(
        "tests/test_app.py::test_run",
        node(
            "tests/test_app.py",
            "test_run",
            "function",
            "def test_run():\n    run(App())\n",
        ),
    );
    graph.add_node(
        "docs/guide.md::module",
        node("docs/guide.md", "guide", "markdown_document", "# Guide\n"),
    );
    graph.add_edge(
        "src/app.py::run",
        "src/app.py::App",
        edge("calls", "structural", 1.0),
    );
    // Parallel edges on one (source, target, rel_type): the first position,
    // the last edge_class and weight survive.
    graph.add_edge(
        "tests/test_app.py::test_run",
        "src/app.py::run",
        edge("calls", "structural", 1.0),
    );
    graph.add_edge(
        "tests/test_app.py::test_run",
        "src/app.py::run",
        edge("calls", "semantic", 0.25),
    );
    graph.add_edge(
        "tests/test_app.py::test_run",
        "src/app.py::App",
        edge("references", "structural", 0.0),
    );
    graph
}

/// [`small_graph`] staged and published into a fresh database.
async fn published_graph(name: &str) -> Option<(PgPool, BuildSpace, CodeGraph, UnifiedDb)> {
    let pool = common::fresh_database(name).await?;
    let wiki = GRAPH_WIKI;
    let graph = small_graph();
    let space = BuildSpace::new(pool.clone(), "graph");
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    let staged = build.stage_graph(&graph).await.expect("stage");
    assert_eq!((staged.nodes, staged.edges), (4, 3));
    let record = WikiRecord::from_result(&json!({
        "wiki_id": wiki,
        "canonical_repo_identifier": "acme/graph:main:abcdef0",
        "branch": "main",
        "commit_hash": "abcdef0",
        "wiki_title": "Graph",
    }));
    let counts = build.publish(&record).await.expect("publish");
    assert_eq!((counts.nodes, counts.edges, counts.embeddings), (4, 3, 0));
    let db = UnifiedDb::new(pool.clone(), common::key(wiki));
    Some((pool, space, graph, db))
}

/// `publish.py`'s mapping: flags from the graph's rules, parallel edges
/// collapsed with the last value, weight 0 → 1.0, `metadata` `{}`.
#[tokio::test(flavor = "multi_thread")]
async fn a_code_graph_publishes_with_the_python_mapping() {
    let Some((_pool, _space, _graph, db)) = published_graph("publish_graph").await else {
        return;
    };
    assert!(!db.vec_available().await.expect("vec"));
    let test_node = db
        .get_node("tests/test_app.py::test_run")
        .await
        .expect("get")
        .expect("exists");
    assert!(test_node.is_test, "is_test comes from the path rule");
    let class_node = db
        .get_node("src/app.py::App")
        .await
        .expect("get")
        .expect("exists");
    assert!(class_node.is_architectural);
    let doc = db
        .get_node("docs/guide.md::module")
        .await
        .expect("get")
        .expect("exists");
    assert!(doc.is_doc && doc.is_architectural);
    assert_eq!(db.get_node("missing").await.expect("get"), None);

    let out = db
        .get_edges_from("tests/test_app.py::test_run", &[])
        .await
        .expect("edges");
    let mut shapes: Vec<(String, String, Option<String>, f64)> = out
        .iter()
        .map(|e| {
            (
                e.target_id.clone(),
                e.rel_type.clone(),
                e.edge_class.clone(),
                e.weight,
            )
        })
        .collect();
    shapes.sort_by(|a, b| a.0.cmp(&b.0));
    assert_eq!(
        shapes,
        [
            (
                "src/app.py::App".to_owned(),
                "references".to_owned(),
                Some("structural".to_owned()),
                1.0
            ),
            (
                "src/app.py::run".to_owned(),
                "calls".to_owned(),
                Some("semantic".to_owned()),
                0.25
            ),
        ]
    );
    let only_calls = db
        .get_edges_from("tests/test_app.py::test_run", &["calls"])
        .await
        .expect("edges");
    assert_eq!(only_calls.len(), 1);
    assert_eq!(only_calls[0].to_legacy()["metadata"], json!({}));
    let into_app = db.get_edges_to("src/app.py::App").await.expect("edges");
    assert_eq!(into_app.len(), 2);
}

/// The adapter's legacy row shapes and the scoped search.
#[tokio::test(flavor = "multi_thread")]
async fn the_read_surface_has_the_legacy_shapes() {
    let Some((_pool, _space, _graph, db)) = published_graph("publish_surface").await else {
        return;
    };
    // The legacy row shape of a fused hit: node keys, then score keys.
    let rows = db
        .search_hybrid("run", None, &Scope::default(), &Hybrid::default())
        .await
        .expect("search");
    assert_ne!(rows, []);
    let legacy = rows[0].to_legacy();
    assert_eq!(legacy["is_hub"], json!(0));
    assert!(legacy.contains_key("combined_score") && legacy.contains_key("fts_rank"));
    // The path filter keeps the prefix's subtree; `_` in it is literal.
    let scoped = db
        .search_hybrid(
            "run",
            None,
            &Scope {
                path_prefix: Some("tests/".into()),
                cluster_id: None,
            },
            &Hybrid::default(),
        )
        .await
        .expect("search");
    assert!(scoped.iter().all(|r| r.node.rel_path.starts_with("tests/")));
    assert_ne!(scoped, []);
    let none = db
        .search_hybrid(
            "run",
            None,
            &Scope {
                path_prefix: Some("src_".into()),
                cluster_id: None,
            },
            &Hybrid::default(),
        )
        .await
        .expect("search");
    assert_eq!(none, []);
}

/// The `wikis` row: `registry_from_result`'s fields, kept by a republish
/// that does not supply them.
#[tokio::test(flavor = "multi_thread")]
async fn the_wikis_row_follows_the_generation_result() {
    let Some((pool, space, graph, db)) = published_graph("publish_wikis").await else {
        return;
    };
    let wiki = GRAPH_WIKI;
    // The `wikis` row: registry_from_result's fields.
    assert_eq!(
        db.get_meta("repo").await.expect("meta").as_deref(),
        Some("acme/graph")
    );
    assert_eq!(
        db.get_meta("commit_hash").await.expect("meta").as_deref(),
        Some("abcdef0")
    );
    assert_eq!(db.get_meta("analysis_key").await.expect("meta"), None);
    assert_eq!(db.get_meta("no_such_key").await.expect("meta"), None);
    let display: String = sqlx::query_scalar("SELECT display_name FROM wikis WHERE wiki_id = $1")
        .bind(wiki)
        .fetch_one(&pool)
        .await
        .expect("wikis");
    assert_eq!(display, "Graph");

    // A republish with a partial record keeps the stored fields.
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    build.stage_graph(&graph).await.expect("stage");
    build
        .publish(&WikiRecord {
            commit_hash: Some("1234567".into()),
            ..WikiRecord::default()
        })
        .await
        .expect("publish");
    assert_eq!(
        db.get_meta("repo").await.expect("meta").as_deref(),
        Some("acme/graph")
    );
    assert_eq!(
        db.get_meta("commit_hash").await.expect("meta").as_deref(),
        Some("1234567")
    );
    assert_eq!(db.node_count().await.expect("count"), 4);
    assert_eq!(db.edge_count().await.expect("count"), 3);
}

/// Dense vectors are stored as `f32` (pgvector's `float4` too) under the
/// build's generation, and a search of the same vector finds it at
/// distance 0.
#[tokio::test(flavor = "multi_thread")]
async fn vectors_round_trip_as_float4() {
    let Some(pool) = common::fresh_database("publish_vectors").await else {
        return;
    };
    let wiki = "acme--vectors--main";
    let (space, store) = common::dense_space(&pool, "vectors");
    let (nodes, _) = version("alpha", 1);
    let vector = [0.1_f64, -2.5, 1e-7, 0.333_333_333_333_333_3];
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    let generation = build.generation().to_owned();
    build.stage_nodes(nodes.clone()).await.expect("nodes");
    build
        .stage_embeddings(
            common::TEST_MODEL,
            [(nodes[0].node_id.as_str(), vector.as_slice())],
        )
        .await
        .expect("embeddings");
    build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    let db = UnifiedDb::new(pool, common::key(wiki)).with_dense(common::dense(&store));
    assert!(db.vec_available().await.expect("vec"));
    let stored = store
        .vector(&common::key(wiki), &generation, &nodes[0].node_id)
        .expect("exists");
    #[allow(clippy::cast_possible_truncation)]
    let expected: Vec<f32> = vector.iter().map(|v| *v as f32).collect();
    assert_eq!(stored, expected);
    let hits = db.reader().search_dense(&vector, 5).await.expect("dense");
    assert_eq!(hits.len(), 1);
    assert!(hits[0].scores.vec_distance.expect("distance").abs() < 1e-6);
    let stats = db.reader().stats().await.expect("stats");
    assert_eq!(stats.vector_count, 1);
}

/// A whitespace-free run longer than a B-tree key allows (a minified line)
/// still publishes: PostgreSQL's parser drops a word over 2046 bytes from
/// the `fts` tsvector, so it gets no posting, and the node keeps the
/// lexemes it does have. The elitea-platform corpus has a 108 kB one.
#[tokio::test(flavor = "multi_thread")]
async fn a_huge_token_gets_no_posting_and_does_not_fail_the_publish() {
    let Some(pool) = common::fresh_database("publish_huge_token").await else {
        return;
    };
    let wiki = "acme--huge--main";
    let space = BuildSpace::new(pool.clone(), "huge");
    let mut build = space.begin(&common::key(wiki)).await.expect("begin");
    let node = IndexNode {
        node_id: "min.js::module".into(),
        symbol_name: "min".into(),
        source_text: format!("var a = {};", "x".repeat(100_000)),
        ..IndexNode::default()
    };
    // A NUL, which PostgreSQL text cannot hold, is stored as U+FFFD.
    let binary = IndexNode {
        node_id: "blob.pdf::module".into(),
        source_text: "%PDF\0x".into(),
        ..IndexNode::default()
    };
    build.stage_nodes([node, binary]).await.expect("stage");
    build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    let stored: String =
        sqlx::query_scalar("SELECT source_text FROM wiki_nodes WHERE node_id = 'blob.pdf::module'")
            .fetch_one(&pool)
            .await
            .expect("node");
    assert_eq!(stored, "%PDF\u{fffd}x");
    let (length, longest): (i32, i32) = sqlx::query_as(
        "SELECT (SELECT length FROM wiki_bm25_docs WHERE wiki_id = $1 AND branch = 'fts' \
                    AND node_id = 'min.js::module'), \
                (SELECT coalesce(max(octet_length(term)), 0)::integer FROM wiki_bm25_terms \
                  WHERE wiki_id = $1 AND branch = 'fts')",
    )
    .bind(wiki)
    .fetch_one(&pool)
    .await
    .expect("stats");
    // "min", "var" and the other short words still index; the 100 kB token
    // contributes no lexeme.
    assert!(length >= 2, "{length}");
    assert!(longest < 1024, "{longest}");
}
