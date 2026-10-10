//! Deleting graphs (issue #1244, ADR-0031 decision 7, phase C0).
//!
//! Before it nothing deleted a graph: `store::delete` had no caller but
//! the tests, so an Inventory toolkit's entities, descriptions, citations,
//! embeddings and document ACLs outlived the toolkit and its project.
//!
//! * [`delete_graph`] deletes one `(project, toolkit)` graph. It takes the
//!   graph's **ingestion lease** first, the session advisory lock a run, an
//!   import, a source removal and a type normalisation hold for their whole
//!   work, and REFUSES ([`GraphDeletion::Busy`]) while another holder has
//!   it. Then one transaction, serialised against every writer by the
//!   graph's write lock, removes the graph's rows from every table. So a
//!   delete never interleaves with a write, and a graph is whole or absent,
//!   never half.
//! * [`delete_project`] deletes every graph of a project, one at a time (a
//!   project can hold many, and one transaction would hold all their
//!   locks). A graph whose lease is held is skipped and reported; the call is
//!   idempotent, so it is repeated after the run ends.
//! * [`orphans`] lists the graphs whose project (or toolkit) is not in the
//!   lists of existing ids the CALLER supplies: this database does not hold
//!   the platform's projects.

use super::{GraphKey, Result, lock, sources};
use sqlx::postgres::PgPool;
use std::collections::HashSet;

/// The rows a delete removed (the `graphs` row is not counted: `existed`
/// says whether there was one).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Removed {
    pub entities: i64,
    pub relations: i64,
    pub sources: i64,
    pub documents: i64,
}

impl Removed {
    fn add(&mut self, other: Self) {
        self.entities += other.entities;
        self.relations += other.relations;
        self.sources += other.sources;
        self.documents += other.documents;
    }
}

/// What deleting one graph did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GraphDeletion {
    /// Nothing is left of the graph. `existed` is false when there was no
    /// graph (there may still have been a source status row, which went).
    Deleted { existed: bool, removed: Removed },
    /// An ingestion, import or other write holds the graph; nothing was
    /// deleted.
    Busy,
}

/// The delete in one transaction: the write lock, the counts, then the
/// rows. Callers that must not race a run hold the lease ([`delete_graph`]).
pub(super) async fn remove(pool: &PgPool, key: GraphKey) -> Result<(bool, Removed)> {
    let mut transaction = pool.begin().await?;
    lock(&mut transaction, key).await?;
    let (graphs, entities, relations, sources, documents): (i64, i64, i64, i64, i64) =
        sqlx::query_as(
            "SELECT
                 (SELECT count(*) FROM inventory_graph.graphs
                   WHERE project_id = $1 AND application_id = $2),
                 (SELECT count(*) FROM inventory_graph.entities
                   WHERE project_id = $1 AND application_id = $2),
                 (SELECT count(*) FROM inventory_graph.relations
                   WHERE project_id = $1 AND application_id = $2),
                 (SELECT count(*) FROM inventory_graph.sources
                   WHERE project_id = $1 AND application_id = $2),
                 (SELECT count(*) FROM inventory_graph.documents
                   WHERE project_id = $1 AND application_id = $2)",
        )
        .bind(key.project_id)
        .bind(key.application_id)
        .fetch_one(&mut *transaction)
        .await?;
    // The graph row takes its entities with it, and their relations
    // (ON DELETE CASCADE); sources and documents reference no graph, as a
    // first ingestion's status exists before its graph does.
    for table in [
        "inventory_graph.graphs",
        "inventory_graph.sources",
        "inventory_graph.documents",
    ] {
        sqlx::query(&format!(
            "DELETE FROM {table} WHERE project_id = $1 AND application_id = $2"
        ))
        .bind(key.project_id)
        .bind(key.application_id)
        .execute(&mut *transaction)
        .await?;
    }
    transaction.commit().await?;
    Ok((
        graphs > 0,
        Removed {
            entities,
            relations,
            sources,
            documents,
        },
    ))
}

/// Delete the graph `key` and every row that belongs to it, unless an
/// ingestion (or an import, source removal or normalisation) holds it.
///
/// # Errors
///
/// [`super::StoreError::Database`].
pub async fn delete_graph(pool: &PgPool, key: GraphKey) -> Result<GraphDeletion> {
    let Some(_lease) = sources::lease(pool, key).await? else {
        return Ok(GraphDeletion::Busy);
    };
    let (existed, removed) = remove(pool, key).await?;
    Ok(GraphDeletion::Deleted { existed, removed })
}

/// What [`delete_project`] did.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProjectDeletion {
    /// The toolkits whose graph state was deleted, in id order.
    pub deleted: Vec<i64>,
    /// The toolkits skipped because a write holds them.
    pub busy: Vec<i64>,
    /// What the deleted graphs held, together.
    pub removed: Removed,
}

/// The toolkits that have any graph state in `project_id`.
///
/// # Errors
///
/// [`super::StoreError::Database`].
pub async fn project_toolkits(pool: &PgPool, project_id: i64) -> Result<Vec<i64>> {
    Ok(sqlx::query_scalar(
        "SELECT application_id FROM (
             SELECT application_id FROM inventory_graph.graphs WHERE project_id = $1
             UNION SELECT application_id FROM inventory_graph.sources WHERE project_id = $1
             UNION SELECT application_id FROM inventory_graph.documents WHERE project_id = $1
         ) toolkits ORDER BY application_id",
    )
    .bind(project_id)
    .fetch_all(pool)
    .await?)
}

/// Delete every graph of `project_id` (and only that project's), one
/// transaction per graph. A graph a write holds is skipped and named in
/// [`ProjectDeletion::busy`]; call again once it ends.
///
/// # Errors
///
/// [`super::StoreError::Database`]. Graphs deleted before the failure stay
/// deleted; the call is idempotent.
pub async fn delete_project(pool: &PgPool, project_id: i64) -> Result<ProjectDeletion> {
    let mut done = ProjectDeletion {
        deleted: Vec::new(),
        busy: Vec::new(),
        removed: Removed::default(),
    };
    for application_id in project_toolkits(pool, project_id).await? {
        let key = GraphKey {
            project_id,
            application_id,
        };
        match delete_graph(pool, key).await? {
            GraphDeletion::Deleted { removed, .. } => {
                done.deleted.push(application_id);
                done.removed.add(removed);
            }
            GraphDeletion::Busy => done.busy.push(application_id),
        }
    }
    Ok(done)
}

/// A stored graph whose project or toolkit no longer exists.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OrphanGraph {
    pub key: GraphKey,
    /// `project` (the project is not in the list) or `toolkit` (the project
    /// is, the toolkit is not).
    pub reason: &'static str,
    pub entities: i64,
    /// When the graph was last written, as text; empty for state without a
    /// graph row (a failed first ingestion's status).
    pub updated_at: String,
}

/// Every graph key with any state in the database, with its entity count.
async fn stored_keys(pool: &PgPool) -> Result<Vec<(GraphKey, i64, String)>> {
    let rows: Vec<(i64, i64, i64, String)> = sqlx::query_as(
        "SELECT k.project_id, k.application_id,
                (SELECT count(*) FROM inventory_graph.entities e
                  WHERE e.project_id = k.project_id AND e.application_id = k.application_id),
                coalesce((SELECT g.updated_at::text FROM inventory_graph.graphs g
                           WHERE g.project_id = k.project_id
                             AND g.application_id = k.application_id), '')
           FROM (
               SELECT project_id, application_id FROM inventory_graph.graphs
               UNION SELECT project_id, application_id FROM inventory_graph.sources
               UNION SELECT project_id, application_id FROM inventory_graph.documents
           ) k
          ORDER BY k.project_id, k.application_id",
    )
    .fetch_all(pool)
    .await?;
    Ok(rows
        .into_iter()
        .map(|(project_id, application_id, entities, updated)| {
            (
                GraphKey {
                    project_id,
                    application_id,
                },
                entities,
                updated,
            )
        })
        .collect())
}

/// The stored graphs whose project is not in `projects`, or, when
/// `toolkits` is given, whose `(project, toolkit)` pair is not in it. With
/// no toolkit list a graph of a live project is never reported: the engine
/// cannot tell that its toolkit is gone.
///
/// # Errors
///
/// [`super::StoreError::Database`].
#[allow(clippy::implicit_hasher)] // the caller's plain sets; no other hasher is used
pub async fn orphans(
    pool: &PgPool,
    projects: &HashSet<i64>,
    toolkits: Option<&HashSet<(i64, i64)>>,
) -> Result<Vec<OrphanGraph>> {
    Ok(stored_keys(pool)
        .await?
        .into_iter()
        .filter_map(|(key, entities, updated_at)| {
            let reason = if !projects.contains(&key.project_id) {
                "project"
            } else if toolkits
                .is_some_and(|known| !known.contains(&(key.project_id, key.application_id)))
            {
                "toolkit"
            } else {
                return None;
            };
            Some(OrphanGraph {
                key,
                reason,
                entities,
                updated_at,
            })
        })
        .collect())
}
