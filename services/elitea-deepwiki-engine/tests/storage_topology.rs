//! Phase 2's PostgreSQL index ([`PgTopologyStore`]) over the P0 retrieval
//! corpus staged in a build: the contract of every `TopologyStore` method
//! (which rows match, the fields, the order), the dense search that ranks
//! over ALL vectors before the prefix filter (sqlite-vec's order), the
//! staged edge replacement, and the stop.
//!
//! Needs `DEEPWIKI_TEST_DSN` (see `storage_common`).

mod storage_common;

use elitea_deepwiki_engine::graph::EdgeRow;
use elitea_deepwiki_engine::graph::topology::replay::standin_embedding;
use elitea_deepwiki_engine::graph::topology::{SearchHit, TopologyStore};
use elitea_deepwiki_engine::runner::StopSignal;
use elitea_deepwiki_engine::storage::build::BuildSpace;
use elitea_deepwiki_engine::storage::text::score_norm;
use elitea_deepwiki_engine::storage::topology::{PgTopologyStore, STOPPED};
use serde_json::json;
use tokio::runtime::Handle;

fn edge(source: &str, target: &str, rel_type: &str, weight: f64) -> EdgeRow {
    EdgeRow {
        source_id: source.to_owned(),
        target_id: target.to_owned(),
        rel_type: rel_type.to_owned(),
        edge_class: "structural".to_owned(),
        analysis_level: "comprehensive".to_owned(),
        weight,
        raw_similarity: None,
        source_file: String::new(),
        target_file: String::new(),
        language: "python".to_owned(),
        annotations: "{}".to_owned(),
        created_by: String::new(),
    }
}

fn ids(hits: &[SearchHit]) -> Vec<&str> {
    hits.iter().map(|h| h.node_id.as_str()).collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[allow(clippy::too_many_lines, clippy::float_cmp)] // exact ties are the order rule
async fn the_build_space_answers_phase_2() {
    let Some(pool) = storage_common::fresh_database("topology_store").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "topology-test");
    let mut build = space.begin("acme--notes--main").await.unwrap();
    let corpus = storage_common::corpus();
    let vectors: Vec<(String, Vec<f64>)> = corpus
        .iter()
        .filter(|n| !n.source_text.trim().is_empty())
        .map(|n| (n.node_id.clone(), standin_embedding(&n.source_text)))
        .collect();
    build.stage_nodes(corpus.clone()).await.unwrap();
    build
        .stage_embeddings(vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())))
        .await
        .unwrap();
    let stop = StopSignal::default();
    let store = PgTopologyStore::new(build, Handle::current(), stop.clone());
    let probe = vectors[0].1.clone();

    let (store, checks) = tokio::task::spawn_blocking(move || {
        let mut store = store;
        let mut checks = Vec::new();
        assert_eq!(store.node_count().unwrap(), 20);

        // Rows in the asked order; an unknown id is None.
        let rows = store
            .get_nodes(&["notes/store.py::NoteStore", "missing", "README.md::module"])
            .unwrap();
        assert_eq!(rows[0].as_ref().unwrap().symbol_name, "NoteStore");
        assert_eq!(rows[0].as_ref().unwrap().rel_path, "notes/store.py");
        assert!(rows[1].is_none());
        assert_eq!(rows[2].as_ref().unwrap().language, "markdown");

        // Lexical: matches only, best (most negative) first, score_norm the
        // logistic of the rank, ties by node id.
        let hits = store.search_lexical("note", None, 30).unwrap();
        assert!(!hits.is_empty());
        for pair in hits.windows(2) {
            let (a, b) = (pair[0].fts_rank.unwrap(), pair[1].fts_rank.unwrap());
            assert!(a < b || (a == b && pair[0].node_id < pair[1].node_id));
        }
        for hit in &hits {
            assert_eq!(hit.score_norm, Some(score_norm(hit.fts_rank.unwrap())));
            assert!(hit.vec_distance.is_none());
        }
        checks.push(ids(&hits).len());
        let limited = store.search_lexical("note", None, 2).unwrap();
        assert_eq!(ids(&limited), ids(&hits)[..2]);
        // The prefix is a directory: only rows under `notes/`.
        let scoped = store.search_lexical("note", Some("notes"), 30).unwrap();
        assert!(!scoped.is_empty());
        assert!(scoped.iter().all(|h| h.rel_path.starts_with("notes/")));
        // A symbol name with punctuation is folded, not parsed.
        assert!(
            !store
                .search_lexical("NoteStore.save_note", None, 5)
                .unwrap()
                .is_empty()
        );
        assert!(store.search_lexical("  ", None, 5).unwrap().is_empty());
        assert!(store.search_lexical("::", None, 5).unwrap().is_empty());

        // Phrase counts.
        assert!(store.count_phrase_matches("save note").unwrap() >= 1);
        assert_eq!(store.count_phrase_matches("").unwrap(), 0);
        assert_eq!(store.count_phrase_matches("zebra unicorn").unwrap(), 0);

        // Vectors back as staged (float4).
        let stored = store
            .get_embeddings(&["missing", "notes/store.py::NoteStore"])
            .unwrap();
        assert!(stored[0].is_none());
        let stored = stored[1].as_ref().unwrap();
        let staged = &standin_embedding(
            &corpus
                .iter()
                .find(|n| n.node_id == "notes/store.py::NoteStore")
                .unwrap()
                .source_text,
        );
        assert_eq!(stored.len(), staged.len());
        assert!(stored.iter().zip(staged).all(|(a, b)| (a - b).abs() < 1e-6));

        // Dense: the k nearest of ALL vectors, THEN the prefix.
        let global = store.search_dense(&probe, 6, None).unwrap();
        assert_eq!(global.len(), 6);
        assert_eq!(global[0].vec_distance.map(|d| d < 1e-6), Some(true));
        for pair in global.windows(2) {
            assert!(pair[0].vec_distance <= pair[1].vec_distance);
        }
        let scoped = store.search_dense(&probe, 6, Some("notes")).unwrap();
        let expected: Vec<&str> = global
            .iter()
            .filter(|h| h.rel_path.starts_with("notes/"))
            .map(|h| h.node_id.as_str())
            .collect();
        assert_eq!(ids(&scoped), expected);
        assert!(scoped.len() <= 6);

        // Edges: the rows offered are counted, the stage holds them
        // collapsed onto the primary key.
        let mut rows = vec![
            edge(
                "api.py::handle_search",
                "notes/search.py::rank_notes",
                "calls",
                0.5,
            ),
            edge(
                "api.py::handle_search",
                "notes/search.py::rank_notes",
                "calls",
                0.7,
            ),
            edge(
                "api.py::module",
                "notes/store.py::NoteStore",
                "imports",
                1.0,
            ),
        ]
        .into_iter();
        assert_eq!(store.replace_edges(&mut rows).unwrap(), 3);
        store.set_hubs(&["api.py::module"]).unwrap();
        store.set_meta("phase2_completed", &json!(true)).unwrap();
        (store, checks)
    })
    .await
    .unwrap();
    assert!(checks[0] > 0);

    let build_id = {
        let parts_store = store;
        // A stop fails the next call with the stop text.
        stop.request();
        let (store, failed) = tokio::task::spawn_blocking(move || {
            let mut store = parts_store;
            let failed = store.node_count().err();
            (store, failed)
        })
        .await
        .unwrap();
        assert_eq!(failed.map(|e| e.0), Some(STOPPED.to_owned()));
        let parts = store.into_parts();
        assert_eq!(parts.hubs, ["api.py::module"]);
        assert_eq!(parts.meta, [("phase2_completed".to_owned(), json!(true))]);
        let id = parts.build.build_id().to_owned();
        parts.build.abandon().await.unwrap();
        id
    };
    let edges: i64 =
        sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.wiki_edges WHERE build_id = $1")
            .bind(&build_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(edges, 0, "the abandoned build took its edges along");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn replaced_edges_are_collapsed_and_clusters_written() {
    let Some(pool) = storage_common::fresh_database("topology_writes").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "topology-test");
    let mut build = space.begin("acme--notes--main").await.unwrap();
    build.stage_nodes(storage_common::corpus()).await.unwrap();
    let build_id = build.build_id().to_owned();
    let store = PgTopologyStore::new(build, Handle::current(), StopSignal::default());
    let store = tokio::task::spawn_blocking(move || {
        let mut store = store;
        let mut rows = vec![
            edge(
                "api.py::handle_search",
                "notes/search.py::rank_notes",
                "calls",
                0.5,
            ),
            edge(
                "api.py::handle_search",
                "notes/search.py::rank_notes",
                "calls",
                0.7,
            ),
            edge(
                "api.py::module",
                "notes/store.py::NoteStore",
                "imports",
                0.0,
            ),
        ]
        .into_iter();
        assert_eq!(store.replace_edges(&mut rows).unwrap(), 3);
        store
    })
    .await
    .unwrap();
    let staged: Vec<(String, f32)> = sqlx::query_as(
        "SELECT rel_type, weight FROM deepwiki_build.wiki_edges WHERE build_id = $1 ORDER BY rel_type",
    )
    .bind(&build_id)
    .fetch_all(&pool)
    .await
    .unwrap();
    // The last weight of the parallel pair; a zero weight is 1.0.
    assert_eq!(
        staged,
        [
            ("calls".to_owned(), 0.7_f32),
            ("imports".to_owned(), 1.0_f32)
        ]
    );

    let mut build = store.into_parts().build;
    let written = build
        .set_clusters(&[
            ("api.py::module".to_owned(), Some(2), Some(5)),
            ("README.md::module".to_owned(), Some(0), None),
        ])
        .await
        .unwrap();
    assert_eq!(written, 2);
    let clusters: Vec<(String, Option<i32>, Option<i32>)> = sqlx::query_as(
        "SELECT node_id, macro_cluster, micro_cluster FROM deepwiki_build.wiki_nodes \
         WHERE build_id = $1 AND macro_cluster IS NOT NULL ORDER BY node_id",
    )
    .bind(&build_id)
    .fetch_all(&pool)
    .await
    .unwrap();
    assert_eq!(
        clusters,
        [
            ("README.md::module".to_owned(), Some(0), None),
            ("api.py::module".to_owned(), Some(2), Some(5)),
        ]
    );
    build.abandon().await.unwrap();
}
