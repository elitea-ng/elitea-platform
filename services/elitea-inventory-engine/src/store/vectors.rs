//! Similarity ranking in PostgreSQL (pgvector, migration 0004): a graph's
//! entities by cosine similarity to a query vector, best first.
//!
//! Only the ranking is here. Which ranked entities a caller sees (the ACL
//! view), the type, layer and file filters, the rounding and the answer's
//! wording stay with retrieval (`retrieval::semantic`).

use super::{GraphKey, Result};
use sqlx::postgres::PgPool;

/// A ranking (the shared core's type, ADR-0029 decision 7).
pub use elitea_inventory_core::store::Ranking;

/// The entities of `key` whose vector is at least `min_score` similar to
/// `vector`, best first (ties by entity id, so the order is stable).
///
/// A query vector of zero length or zero norm ranks nothing, as numpy's
/// division guard did; so does an entity vector of zero norm.
///
/// # Errors
///
/// [`super::StoreError::Database`] for a failed query.
pub async fn rank(pool: &PgPool, key: GraphKey, vector: &[f64], min_score: f64) -> Result<Ranking> {
    if vector.iter().all(|value| *value == 0.0) {
        return Ok(Ok(Vec::new()));
    }
    let width = i32::try_from(vector.len()).unwrap_or(i32::MAX);
    let other: Option<i32> = sqlx::query_scalar(
        "SELECT vector_dims(embedding_vector)
           FROM inventory_graph.entities
          WHERE project_id = $1 AND application_id = $2
            AND embedding_vector IS NOT NULL
            AND vector_dims(embedding_vector) <> $3
          LIMIT 1",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(width)
    .fetch_optional(pool)
    .await?;
    if let Some(other) = other {
        return Ok(Err(format!(
            "shapes ({width},) and ({other},) not aligned: {width} (dim 0) != {other} (dim 0)"
        )));
    }
    let rows: Vec<(String, f64)> = sqlx::query_as(
        "SELECT entity_id, score FROM (
             SELECT entity_id,
                    1 - (embedding_vector <=> $3::float8[]::vector) AS score
               FROM inventory_graph.entities
              WHERE project_id = $1 AND application_id = $2
                AND embedding_vector IS NOT NULL
                AND vector_norm(embedding_vector) > 0
         ) ranked
          WHERE score >= $4
          ORDER BY score DESC, entity_id",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(vector)
    .bind(min_score)
    .fetch_all(pool)
    .await?;
    Ok(Ok(rows))
}
