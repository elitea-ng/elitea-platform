//! Retrieval parity: the P0 retrieval fixtures
//! (`conformance/provider/fixtures/deepwiki/retrieval/sample-repo`) against
//! the native index. A port of `tests/storage/test_retrieval_parity.py`,
//! with one difference that makes it stronger: the corpus reaches the live
//! tables through the build space and the publish transaction (the path a
//! generation takes), not through direct upserts.
//!
//! | branch | assertion |
//! | --- | --- |
//! | dense | exact: same order (up to recorded ties), same L2 distances |
//! | bm25 | exact: same order, same scores; corpus statistics equal |
//! | fts | same match set; order within the recorded tie groups; legacy sign |
//! | fused | recomputed from the components; equal to the recording when the components agree and no dense tie reaches the top k |

mod storage_common;

use elitea_deepwiki_engine::storage::build::{BuildSpace, WikiRecord};
use elitea_deepwiki_engine::storage::search::{Fusion, Hit, Hybrid, IndexReader, rrf_fuse};
use serde_json::Value;
use sqlx::postgres::PgPool;
use storage_common as common;

/// Absolute tolerance for a score recorded at nine decimal places.
const TOLERANCE: f64 = 1e-6;
const WIKI: &str = "acme--notes-service--main";

/// A migrated database holding the fixture corpus, published.
async fn published(name: &str) -> Option<(PgPool, IndexReader)> {
    let pool = common::fresh_database(name).await?;
    let space = BuildSpace::new(pool.clone(), "parity");
    let mut build = space.begin(&common::key(WIKI)).await.expect("begin");
    build
        .stage_nodes(common::corpus())
        .await
        .expect("stage nodes");
    let embeddings = common::embeddings();
    build
        .stage_embeddings(embeddings.iter().map(|(id, v)| (id.as_str(), v.as_slice())))
        .await
        .expect("stage embeddings");
    let counts = build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    assert_eq!(counts.nodes, 20);
    assert_eq!(counts.embeddings, 20);
    Some((pool.clone(), IndexReader::new(pool, common::key(WIKI))))
}

fn ids(hits: &[Hit]) -> Vec<String> {
    hits.iter().map(|hit| hit.node_id.clone()).collect()
}

fn recorded(fixture: &Value, branch: &str, key: &str) -> (Vec<String>, Vec<f64>) {
    let rows = fixture["rankings"][branch]["results"]
        .as_array()
        .expect("results");
    (
        rows.iter()
            .map(|row| row["node_id"].as_str().expect("id").to_owned())
            .collect(),
        rows.iter()
            .map(|row| row[key].as_f64().expect("score"))
            .collect(),
    )
}

fn param(fixture: &Value, key: &str) -> usize {
    usize::try_from(fixture["parameters"][key].as_u64().expect("parameter")).expect("usize")
}

fn fparam(fixture: &Value, key: &str) -> f64 {
    fixture["parameters"][key].as_f64().expect("parameter")
}

#[tokio::test(flavor = "multi_thread")]
async fn dense_ranking_is_exact() {
    let Some((_pool, reader)) = published("parity_dense").await else {
        return;
    };
    for (slug, fixture) in common::queries() {
        let embedding = common::floats(&fixture["query_embedding"]);
        let hits = reader
            .search_dense(&embedding, param(&fixture, "vec_pool"))
            .await
            .expect("dense");
        let (expected_ids, expected) = recorded(&fixture, "dense", "vec_distance");
        let scores: Vec<f64> = hits
            .iter()
            .map(|h| h.scores.vec_distance.expect("distance"))
            .collect();
        common::assert_rankings_agree(
            &ids(&hits),
            &scores,
            &expected_ids,
            &expected,
            TOLERANCE,
            &format!("dense/{slug}"),
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn bm25_ranking_is_exact() {
    let Some((_pool, reader)) = published("parity_bm25").await else {
        return;
    };
    for (slug, fixture) in common::queries() {
        let (expected_ids, expected) = recorded(&fixture, "bm25", "bm25_score");
        let query = fixture["query"].as_str().expect("query");
        let hits = reader
            .search_bm25(query, expected_ids.len().max(1))
            .await
            .expect("bm25");
        let scores: Vec<f64> = hits
            .iter()
            .map(|h| h.scores.bm25_score.expect("score"))
            .collect();
        common::assert_rankings_agree(
            &ids(&hits),
            &scores,
            &expected_ids,
            &expected,
            TOLERANCE,
            &format!("bm25/{slug}"),
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn bm25_statistics_match_the_recorded_index() {
    let Some((pool, reader)) = published("parity_bm25_stats").await else {
        return;
    };
    let recorded = &common::load("index_stats.json")["stats"]["bm25"];
    let stats = reader.stats().await.expect("stats");
    let (_, branch) = stats
        .branches
        .iter()
        .find(|(name, _)| name == "bm25")
        .expect("the bm25 branch");
    assert_eq!(Some(branch.doc_count), recorded["doc_count"].as_i64());
    assert!((branch.avgdl - recorded["avgdl"].as_f64().expect("avgdl")).abs() < 1e-9);
    assert_eq!(Some(branch.k1), recorded["k1"].as_f64());
    assert_eq!(Some(branch.b), recorded["b"].as_f64());
    let terms: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM wiki_bm25_terms WHERE wiki_id = $1 AND branch = 'bm25'",
    )
    .bind(WIKI)
    .fetch_one(&pool)
    .await
    .expect("terms");
    assert_eq!(Some(terms), recorded["term_count"].as_i64());
}

#[tokio::test(flavor = "multi_thread")]
async fn fts_match_set_order_and_sign_are_preserved() {
    let Some((_pool, reader)) = published("parity_fts").await else {
        return;
    };
    for (slug, fixture) in common::queries() {
        let query = fixture["query"].as_str().expect("query");
        let hits = reader
            .search_fts(query, param(&fixture, "fts_pool"))
            .await
            .expect("fts");
        let (expected_ids, expected) = recorded(&fixture, "fts", "fts_rank");
        common::assert_ordering_agrees(
            &ids(&hits),
            &expected_ids,
            &expected,
            TOLERANCE,
            &format!("fts/{slug}"),
        );
        // The legacy sign and normalisation, which everything downstream
        // assumes: more negative is better, the list ordered by it.
        let ranks: Vec<f64> = hits
            .iter()
            .map(|h| h.scores.fts_rank.expect("rank"))
            .collect();
        assert!(
            ranks.windows(2).all(|w| w[0] <= w[1]),
            "fts/{slug}: not ordered"
        );
        for hit in &hits {
            let rank = hit.scores.fts_rank.expect("rank");
            let norm = hit.scores.score_norm.expect("norm");
            assert!(rank <= 0.0, "fts/{slug}: positive rank");
            assert!((norm - 1.0 / (1.0 + rank.exp())).abs() < 1e-12);
            assert!((0.0..=1.0).contains(&norm));
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn fused_ranking_follows_from_the_components_and_the_fixture() {
    let Some((_pool, reader)) = published("parity_fused").await else {
        return;
    };
    let mut compared_with_fixture = Vec::new();
    for (slug, fixture) in common::queries() {
        let query = fixture["query"].as_str().expect("query");
        let embedding = common::floats(&fixture["query_embedding"]);
        let params = Hybrid {
            limit: param(&fixture, "top_k"),
            fts_weight: fparam(&fixture, "fts_weight"),
            vec_weight: fparam(&fixture, "vec_weight"),
            fts_pool: param(&fixture, "fts_pool"),
            vec_pool: param(&fixture, "vec_pool"),
        };
        let fts = reader
            .search_fts(query, params.fts_pool)
            .await
            .expect("fts");
        let dense = reader
            .search_dense(&embedding, params.vec_pool)
            .await
            .expect("dense");
        let expected = rrf_fuse(
            &fts,
            &dense,
            &Fusion {
                fts_weight: params.fts_weight,
                vec_weight: params.vec_weight,
                rrf_constant: fparam(&fixture, "rrf_constant"),
                limit: params.limit,
            },
        );
        let actual = reader
            .search_hybrid(query, Some(&embedding), &params)
            .await
            .expect("hybrid");
        assert_eq!(
            ids(&actual),
            ids(&expected),
            "fused/{slug}: not the frozen RRF"
        );
        for (got, want) in actual.iter().zip(&expected) {
            let got = got.scores.combined_score.expect("combined");
            let want = want.scores.combined_score.expect("combined");
            assert!((got - want).abs() < 1e-12, "fused/{slug}");
        }

        // The end-to-end claim, on the queries where it is determined: the
        // FTS order equals the recording and no dense tie reaches the top k
        // (RRF consumes positions, and tied positions are arbitrary in both
        // engines).
        let (recorded_fts, _) = recorded(&fixture, "fts", "fts_rank");
        let (_, dense_scores) = recorded(&fixture, "dense", "vec_distance");
        let dense_tie_in_top_k = common::tie_groups(&dense_scores, TOLERANCE)
            .into_iter()
            .any(|(start, end)| end - start > 1 && start < params.limit);
        if ids(&fts) != recorded_fts || dense_tie_in_top_k {
            continue;
        }
        let (expected_ids, expected_scores) = recorded(&fixture, "fused", "combined_score");
        let scores: Vec<f64> = actual
            .iter()
            .map(|h| h.scores.combined_score.expect("combined"))
            .collect();
        common::assert_rankings_agree(
            &ids(&actual),
            &scores,
            &expected_ids,
            &expected_scores,
            1e-9,
            &format!("fused/{slug}"),
        );
        compared_with_fixture.push(slug);
    }
    eprintln!("fused rankings equal to the recording: {compared_with_fixture:?}");
    assert!(
        !compared_with_fixture.is_empty(),
        "no query compared the fused ranking with the recording"
    );
}

/// A property of weighted RRF the port inherits (`test_fused_ranking_is_
/// undetermined_when_a_component_ties`): some fixture queries have a dense
/// tie block inside the fused top k. Pinned so a corpus change that loses
/// the property is noticed.
#[tokio::test(flavor = "multi_thread")]
async fn some_fixture_queries_tie_in_the_dense_top_k() {
    let Some((_pool, _reader)) = published("parity_ties").await else {
        return;
    };
    let tied: Vec<String> = common::queries()
        .into_iter()
        .filter(|(_, fixture)| {
            let (_, scores) = recorded(fixture, "dense", "vec_distance");
            let top_k = param(fixture, "top_k");
            common::tie_groups(&scores, TOLERANCE)
                .into_iter()
                .any(|(start, end)| end - start > 1 && start < top_k)
        })
        .map(|(slug, _)| slug)
        .collect();
    eprintln!("queries whose fused order is undetermined by dense ties: {tied:?}");
    assert!(
        !tied.is_empty(),
        "no fixture query exercises a component tie any more"
    );
}

/// `test_fts_parity_report`: the drift of the one branch that cannot be
/// numerically exact, printed on every run and bounded.
#[tokio::test(flavor = "multi_thread")]
async fn fts_parity_report() {
    let Some((_pool, reader)) = published("parity_fts_report").await else {
        return;
    };
    let mut discriminating = 0;
    let mut inversions_total = 0;
    let mut sets_equal = true;
    eprintln!("FTS parity report — legacy SQLite FTS5 vs native PostgreSQL tsvector");
    eprintln!(
        "{:<26}{:>5}{:>4}{:>6}{:>11}{:>9}{:>12}",
        "query", "rows", "pg", "set", "spread", "discrim", "inversions"
    );
    for (slug, fixture) in common::queries() {
        let query = fixture["query"].as_str().expect("query");
        let (recorded_ids, recorded_scores) = recorded(&fixture, "fts", "fts_rank");
        let actual = ids(&reader
            .search_fts(query, param(&fixture, "fts_pool"))
            .await
            .expect("fts"));
        let spread = recorded_scores
            .iter()
            .copied()
            .fold(f64::NEG_INFINITY, f64::max)
            - recorded_scores
                .iter()
                .copied()
                .fold(f64::INFINITY, f64::min);
        let separated = recorded_ids.len() > 1 && spread > TOLERANCE;
        if separated {
            discriminating += 1;
        }
        let inversions = if separated {
            common::discordant_pairs(&actual, &recorded_ids)
        } else {
            0
        };
        inversions_total += inversions;
        let mut a = actual.clone();
        let mut r = recorded_ids.clone();
        a.sort();
        r.sort();
        let same = a == r;
        sets_equal &= same;
        eprintln!(
            "{slug:<26}{:>5}{:>4}{:>6}{:>11.6}{:>9}{:>12}",
            recorded_ids.len(),
            actual.len(),
            if same { "ok" } else { "DIFF" },
            if recorded_scores.is_empty() {
                0.0
            } else {
                spread
            },
            if separated { "yes" } else { "-" },
            inversions
        );
    }
    eprintln!("discriminating queries: {discriminating}   total inversions: {inversions_total}");
    assert!(sets_equal, "the FTS match set diverged");
    assert_eq!(inversions_total, 0, "documents FTS5 separated are inverted");
    assert!(
        discriminating >= 4,
        "only {discriminating} discriminating queries"
    );
}
