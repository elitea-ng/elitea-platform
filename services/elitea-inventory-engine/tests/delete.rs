//! Deleting graphs (issue #1244, ADR-0031 decision 7, phase C0): a graph's
//! rows go from every table and only that graph's, a delete never
//! interleaves with a writer, a project's delete covers every toolkit of
//! the project and no other, the `platform` engine tools do it, and the
//! orphans query finds what the platform no longer has.
//!
//! Needs `INVENTORY_TEST_DSN` (see `graph_store.rs`).

#![allow(clippy::too_many_lines)]

mod common;

use elitea_engine_core::stream::{Context, Line, StopSignal};
use elitea_inventory_core::store::{Completion, DocumentState, GraphStore as _, RunCounts};
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::native::NativeRunner;
use elitea_inventory_engine::store::delete::{self, GraphDeletion, Removed};
use elitea_inventory_engine::store::{self, GraphKey, PgGraphStore};
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use std::collections::{BTreeMap, HashMap, HashSet};

fn key(project: i64, toolkit: i64) -> GraphKey {
    GraphKey::new(project, toolkit).expect("key")
}

fn graph(prefix: &str) -> Graph {
    let mut graph = Graph::new();
    for name in ["a", "b", "c"] {
        let id = format!("{prefix}-{name}");
        graph.add_entity(&id, name, "class", None, None);
        graph.set_embedding(&id, &[1.0, 0.0, 0.5, 0.25]);
    }
    graph.add_relation(&format!("{prefix}-a"), &format!("{prefix}-b"), "uses", None);
    graph.add_relation(
        &format!("{prefix}-b"),
        &format!("{prefix}-c"),
        "calls",
        None,
    );
    graph
}

/// A graph with a source, document versions and a restricted document, as
/// an ingestion commits it.
async fn commit(store: &PgGraphStore, key: GraphKey, graph: &Graph) {
    use elitea_content_source::Acl;
    use elitea_content_source::acl::{Principal, PrincipalKind};
    use elitea_inventory_core::store::SourceStatus;
    store
        .start(
            key,
            &SourceStatus {
                toolkit_id: "5".to_owned(),
                toolkit_name: "repo".to_owned(),
                toolkit_type: "github".to_owned(),
                branch: Some("main".to_owned()),
            },
        )
        .await
        .expect("start");
    let documents = BTreeMap::from([
        (
            "src/a.py".to_owned(),
            DocumentState {
                version: "v1".to_owned(),
                mime: "text/x-python".to_owned(),
                acl: Acl::Project,
            },
        ),
        (
            "docs/secret.md".to_owned(),
            DocumentState {
                version: "v1".to_owned(),
                mime: "text/markdown".to_owned(),
                acl: Acl::Restricted {
                    principals: vec![Principal {
                        kind: PrincipalKind::User,
                        id: "7".to_owned(),
                    }],
                },
            },
        ),
    ]);
    store
        .complete(
            key,
            graph,
            &Completion {
                toolkit_id: "5",
                source_name: "repo",
                documents: &documents,
                counts: RunCounts::default(),
                commit_sha: Some("abc"),
            },
        )
        .await
        .expect("complete");
}

/// Rows per table for one key: graphs, entities, relations, sources,
/// documents.
async fn rows(pool: &PgPool, key: GraphKey) -> [i64; 5] {
    let mut counts = [0; 5];
    for (slot, table) in
        counts
            .iter_mut()
            .zip(["graphs", "entities", "relations", "sources", "documents"])
    {
        *slot = sqlx::query_scalar(&format!(
            "SELECT count(*) FROM inventory_graph.{table}
              WHERE project_id = $1 AND application_id = $2"
        ))
        .bind(key.project_id)
        .bind(key.application_id)
        .fetch_one(pool)
        .await
        .expect("count");
    }
    counts
}

#[tokio::test]
async fn a_delete_removes_every_table_for_that_graph_only() {
    let Some(pool) = common::database("delete_tables").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    let (doomed, kept, other_project) = (key(1, 10), key(1, 11), key(2, 10));
    for (key, prefix) in [(doomed, "d"), (kept, "k"), (other_project, "o")] {
        commit(&graphs, key, &graph(prefix)).await;
    }
    assert_eq!(rows(&pool, doomed).await, [1, 3, 2, 1, 2]);

    let outcome = delete::delete_graph(&pool, doomed).await.expect("delete");
    assert_eq!(
        outcome,
        GraphDeletion::Deleted {
            existed: true,
            removed: Removed {
                entities: 3,
                relations: 2,
                sources: 1,
                documents: 2,
            },
        }
    );
    assert_eq!(rows(&pool, doomed).await, [0; 5], "every table is empty");
    // The same toolkit id in another project, and another toolkit of the
    // project, are whole.
    assert_eq!(rows(&pool, kept).await, [1, 3, 2, 1, 2]);
    assert_eq!(rows(&pool, other_project).await, [1, 3, 2, 1, 2]);
    // Deleting it again is not an error and says there was nothing.
    assert_eq!(
        delete::delete_graph(&pool, doomed).await.expect("delete"),
        GraphDeletion::Deleted {
            existed: false,
            removed: Removed::default()
        }
    );
}

#[tokio::test]
async fn a_failed_first_ingestions_status_goes_too() {
    let Some(pool) = common::database("delete_status_only").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    let key = key(3, 30);
    // A status row and no graph: the first ingestion failed.
    graphs
        .start(
            key,
            &elitea_inventory_core::store::SourceStatus {
                toolkit_id: "5".to_owned(),
                ..Default::default()
            },
        )
        .await
        .expect("start");
    assert_eq!(rows(&pool, key).await, [0, 0, 0, 1, 0]);
    let GraphDeletion::Deleted { existed, removed } =
        delete::delete_graph(&pool, key).await.expect("delete")
    else {
        panic!("nothing holds it");
    };
    assert!(!existed, "there was no graph");
    assert_eq!(removed.sources, 1);
    assert_eq!(rows(&pool, key).await, [0; 5]);
}

/// A running ingestion (the lease) makes the delete refuse; nothing moves.
/// Once the run ends, the delete goes through.
#[tokio::test]
async fn a_delete_during_an_ingestion_is_refused_and_leaves_the_graph_whole() {
    let Some(pool) = common::database("delete_busy").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    let key = key(1, 10);
    commit(&graphs, key, &graph("g")).await;
    let run = graphs.lease(key).await.expect("lease").expect("free");
    assert_eq!(
        delete::delete_graph(&pool, key).await.expect("delete"),
        GraphDeletion::Busy
    );
    assert_eq!(rows(&pool, key).await, [1, 3, 2, 1, 2], "whole, not half");
    drop(run);
    assert!(matches!(
        delete::delete_graph(&pool, key).await.expect("delete"),
        GraphDeletion::Deleted { existed: true, .. }
    ));
    assert_eq!(rows(&pool, key).await, [0; 5]);
}

/// A writer that does not hold the lease (a plain save) and a delete race
/// to the graph's write lock: whichever wins, the graph ends whole or
/// absent in EVERY table, never with entities and no graph row.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_delete_racing_a_save_never_leaves_half_a_graph() {
    let Some(pool) = common::database("delete_race").await else {
        return;
    };
    let key = key(1, 10);
    let full = graph("r");
    for round in 0..20 {
        store::save(&pool, key, &full).await.expect("seed");
        let saver = {
            let (pool, graph) = (pool.clone(), full.clone());
            tokio::spawn(async move { store::save(&pool, key, &graph).await })
        };
        let deleter = {
            let pool = pool.clone();
            tokio::spawn(async move { delete::delete_graph(&pool, key).await })
        };
        saver.await.expect("join").expect("save");
        deleter.await.expect("join").expect("delete");
        let counts = rows(&pool, key).await;
        assert!(
            counts == [0; 5] || (counts[0] == 1 && counts[1] == 3 && counts[2] == 2),
            "round {round}: {counts:?} is neither whole nor absent"
        );
    }
}

#[tokio::test]
async fn a_project_delete_covers_every_toolkit_of_the_project_and_no_other() {
    let Some(pool) = common::database("delete_project").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    for toolkit in [1, 2, 3] {
        commit(&graphs, key(5, toolkit), &graph(&format!("p{toolkit}"))).await;
    }
    // A toolkit with a failed first ingestion: status only.
    graphs
        .start(
            key(5, 4),
            &elitea_inventory_core::store::SourceStatus {
                toolkit_id: "9".to_owned(),
                ..Default::default()
            },
        )
        .await
        .expect("start");
    commit(&graphs, key(6, 1), &graph("other")).await;

    // Toolkit 2 is being ingested: it is skipped and named, the rest go.
    let run = graphs.lease(key(5, 2)).await.expect("lease").expect("free");
    let done = delete::delete_project(&pool, 5).await.expect("delete");
    assert_eq!(done.deleted, vec![1, 3, 4]);
    assert_eq!(done.busy, vec![2]);
    assert_eq!(rows(&pool, key(5, 2)).await, [1, 3, 2, 1, 2], "untouched");
    assert_eq!(rows(&pool, key(6, 1)).await, [1, 3, 2, 1, 2]);
    drop(run);

    // Repeated once the run ended, it finishes the job.
    let done = delete::delete_project(&pool, 5).await.expect("delete");
    assert_eq!(done.deleted, vec![2]);
    assert!(done.busy.is_empty());
    assert_eq!(
        done.removed,
        Removed {
            entities: 3,
            relations: 2,
            sources: 1,
            documents: 2,
        }
    );
    for toolkit in 1..=4 {
        assert_eq!(
            rows(&pool, key(5, toolkit)).await,
            [0; 5],
            "toolkit {toolkit}"
        );
    }
    assert_eq!(
        rows(&pool, key(6, 1)).await,
        [1, 3, 2, 1, 2],
        "another project"
    );
    // Nothing left to delete is a success.
    let done = delete::delete_project(&pool, 5).await.expect("delete");
    assert!(done.deleted.is_empty() && done.busy.is_empty());
}

#[tokio::test]
async fn orphans_are_the_graphs_the_platform_no_longer_has() {
    let Some(pool) = common::database("delete_orphans").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    for (project, toolkit) in [(1, 10), (1, 11), (2, 20), (3, 30)] {
        commit(
            &graphs,
            key(project, toolkit),
            &graph(&format!("{project}x{toolkit}")),
        )
        .await;
    }
    let projects: HashSet<i64> = [1, 2].into();
    // Without a toolkit list only graphs of deleted projects are orphans.
    let found = delete::orphans(&pool, &projects, None)
        .await
        .expect("orphans");
    assert_eq!(
        found
            .iter()
            .map(|o| (o.key, o.reason, o.entities))
            .collect::<Vec<_>>(),
        vec![(key(3, 30), "project", 3)]
    );
    // With one, a graph of a live project whose toolkit is gone is too.
    let toolkits: HashSet<(i64, i64)> = [(1, 10), (2, 20)].into();
    let found = delete::orphans(&pool, &projects, Some(&toolkits))
        .await
        .expect("orphans");
    assert_eq!(
        found.iter().map(|o| (o.key, o.reason)).collect::<Vec<_>>(),
        vec![(key(1, 11), "toolkit"), (key(3, 30), "project")]
    );
    // Listing deletes nothing.
    assert_eq!(rows(&pool, key(3, 30)).await, [1, 3, 2, 1, 2]);
}

fn runner(database: &str) -> NativeRunner {
    let env: HashMap<&str, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url(database),
        ),
    ]
    .into_iter()
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    NativeRunner::new(settings).expect("runner")
}

fn context() -> (Context, tokio::sync::mpsc::UnboundedReceiver<Line>) {
    let (sender, receiver) = tokio::sync::mpsc::unbounded_channel();
    (Context::new(sender, StopSignal::default()), receiver)
}

async fn call(runner: &NativeRunner, tool: &str, arguments: Value) -> Result<Value, String> {
    let (context, _lines) = context();
    runner
        .run(tool, arguments.as_object().expect("an object"), &context)
        .await
        .map_err(|e| e.message)
}

/// The `platform` tools: what the host sends the engine on a toolkit
/// deletion and on a project deletion.
#[tokio::test]
async fn the_platform_tools_delete_a_graph_and_a_projects_graphs() {
    let Some(pool) = common::database("delete_tools").await else {
        return;
    };
    let runner = runner("delete_tools");
    let graphs = PgGraphStore::new(pool.clone());
    for (project, toolkit) in [(7, 70), (7, 71), (8, 80)] {
        commit(
            &graphs,
            key(project, toolkit),
            &graph(&format!("{project}x{toolkit}")),
        )
        .await;
    }

    let answer = call(
        &runner,
        "delete_graph",
        json!({"family": "platform", "project_id": 7, "application_id": "70", "params": {}}),
    )
    .await
    .expect("deletes");
    assert_eq!(answer["success"], json!(true));
    assert_eq!(
        answer["result"],
        json!(
            "Deleted the Inventory graph of toolkit 70 in project 7: 3 entities, 2 relations, 1 sources, 2 documents."
        )
    );
    assert_eq!(rows(&pool, key(7, 70)).await, [0; 5]);
    assert_eq!(rows(&pool, key(7, 71)).await, [1, 3, 2, 1, 2]);

    // Again: nothing to delete, and it says so (as JSON when asked).
    let answer = call(
        &runner,
        "delete_graph",
        json!({"family": "platform", "project_id": 7, "application_id": 70,
               "params": {"output_format": "json"}}),
    )
    .await
    .expect("deletes");
    let report: Value =
        serde_json::from_str(answer["result"].as_str().expect("text")).expect("json");
    assert_eq!(report["deleted"], json!(false));
    assert_eq!(report["entities"], json!(0));

    // A running ingestion refuses it in band.
    let run = graphs
        .lease(key(7, 71))
        .await
        .expect("lease")
        .expect("free");
    let refused = call(
        &runner,
        "delete_graph",
        json!({"family": "platform", "project_id": 7, "application_id": 71}),
    )
    .await
    .expect_err("refused");
    assert!(refused.contains("ingestion"), "{refused}");
    assert_eq!(rows(&pool, key(7, 71)).await, [1, 3, 2, 1, 2]);
    // ... as does a project delete, which still deletes what it can.
    let refused = call(
        &runner,
        "delete_project_graphs",
        json!({"family": "platform", "project_id": "7"}),
    )
    .await
    .expect_err("refused");
    assert!(refused.contains("toolkit(s) 71"), "{refused}");
    drop(run);
    let answer = call(
        &runner,
        "delete_project_graphs",
        json!({"family": "platform", "project_id": 7}),
    )
    .await
    .expect("deletes");
    assert_eq!(
        answer["result"],
        json!(
            "Deleted the Inventory graphs of 1 toolkit(s) of project 7: 3 entities, 2 relations, 1 sources, 2 documents."
        )
    );
    assert_eq!(rows(&pool, key(7, 71)).await, [0; 5]);
    assert_eq!(
        rows(&pool, key(8, 80)).await,
        [1, 3, 2, 1, 2],
        "another project"
    );

    // A call that names no graph names no project.
    for (tool, arguments) in [
        (
            "delete_graph",
            json!({"family": "platform", "project_id": 7}),
        ),
        ("delete_project_graphs", json!({"family": "platform"})),
        (
            "delete_project_graphs",
            json!({"family": "platform", "project_id": 0}),
        ),
    ] {
        let refused = call(&runner, tool, arguments.clone()).await;
        assert!(refused.is_err(), "{tool} {arguments}: {refused:?}");
    }
    assert_eq!(rows(&pool, key(8, 80)).await, [1, 3, 2, 1, 2]);
}

/// `orphans` on the binary: a dry run lists and deletes nothing, `--delete`
/// removes exactly the listed graphs, and an empty list is refused.
#[tokio::test]
async fn the_orphans_command_is_a_dry_run_unless_asked() {
    use std::io::Write as _;
    use std::process::{Command, Stdio};
    let Some(pool) = common::database("delete_cli").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    for (project, toolkit) in [(1, 10), (3, 30), (3, 31)] {
        let prefix = format!("{project}x{toolkit}");
        commit(&graphs, key(project, toolkit), &graph(&prefix)).await;
    }
    let orphans = |list: &str, extra: &[&str]| {
        let mut child = Command::new(env!("CARGO_BIN_EXE_elitea-inventory-engine"))
            .args(["orphans", "--existing-projects", "-"])
            .args(extra)
            .env(
                "ELITEA_INVENTORY_DATABASE_URL",
                common::database_url("delete_cli"),
            )
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("spawn");
        child
            .stdin
            .take()
            .expect("stdin")
            .write_all(list.as_bytes())
            .expect("write");
        let output = child.wait_with_output().expect("run");
        (
            output.status.success(),
            String::from_utf8_lossy(&output.stdout).into_owned(),
            String::from_utf8_lossy(&output.stderr).into_owned(),
        )
    };

    let (ok, out, err) = orphans("1\n", &[]);
    assert!(ok, "{err}");
    assert!(out.contains("3\t30\tproject\t3\t"), "{out}");
    assert!(out.contains("3\t31\tproject\t3\t"), "{out}");
    assert!(!out.contains("1\t10\t"), "{out}");
    assert!(out.contains("dry run: 2 orphaned graph(s)"), "{out}");
    assert_eq!(
        rows(&pool, key(3, 30)).await,
        [1, 3, 2, 1, 2],
        "a dry run deletes nothing"
    );

    let (ok, _, err) = orphans("", &[]);
    assert!(
        !ok && err.contains("refusing to call every graph an orphan"),
        "{err}"
    );
    assert_eq!(rows(&pool, key(3, 30)).await, [1, 3, 2, 1, 2]);

    // A deletion needs the time the list was taken.
    let (ok, _, err) = orphans("1\n", &["--delete"]);
    assert!(!ok && err.contains("--delete needs --listed-at"), "{err}");
    assert_eq!(rows(&pool, key(3, 30)).await, [1, 3, 2, 1, 2]);

    let listed_at = now(&pool).await;
    let (ok, out, err) = orphans("1\n", &["--delete", "--listed-at", &listed_at]);
    assert!(ok, "{err}");
    assert!(
        out.contains("deleted project 3 toolkit 30: 3 entities"),
        "{out}"
    );
    assert_eq!(rows(&pool, key(3, 30)).await, [0; 5]);
    assert_eq!(rows(&pool, key(3, 31)).await, [0; 5]);
    assert_eq!(
        rows(&pool, key(1, 10)).await,
        [1, 3, 2, 1, 2],
        "a live project stays"
    );
    let (ok, out, _) = orphans("1\n", &[]);
    assert!(ok && out.contains("no orphaned Inventory graph"), "{out}");
}

/// The database's clock as an RFC 3339 time, `offset` seconds from now.
async fn clock(pool: &PgPool, offset: i32) -> String {
    sqlx::query_scalar(
        "SELECT to_char((clock_timestamp() + make_interval(secs => $1)) AT TIME ZONE 'UTC',
                        'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')",
    )
    .bind(f64::from(offset))
    .fetch_one(pool)
    .await
    .expect("clock")
}

async fn now(pool: &PgPool) -> String {
    clock(pool, 0).await
}

fn orphans_command(database: &str, list: &str, extra: &[&str]) -> (Option<i32>, String, String) {
    use std::io::Write as _;
    use std::process::{Command, Stdio};
    let mut child = Command::new(env!("CARGO_BIN_EXE_elitea-inventory-engine"))
        .args(["orphans", "--existing-projects", "-"])
        .args(extra)
        .env(
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url(database),
        )
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn");
    child
        .stdin
        .take()
        .expect("stdin")
        .write_all(list.as_bytes())
        .expect("write");
    let output = child.wait_with_output().expect("run");
    (
        output.status.code(),
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

/// A graph written after the list was taken is not an orphan, however the
/// list reads: the deletion skips it, in the transaction under the graph's
/// lock, and removes the older one.
#[tokio::test]
async fn a_deletion_by_a_list_skips_what_was_written_after_the_list() {
    let Some(pool) = common::database("delete_listed_at").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    commit(&graphs, key(3, 30), &graph("old")).await;
    let before = now(&pool).await;
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    commit(&graphs, key(3, 31), &graph("new")).await;

    // The guarded delete itself.
    assert!(matches!(
        delete::delete_graph_guarded(&pool, key(3, 31), Some(&before))
            .await
            .expect("guarded"),
        delete::GuardedDeletion::Newer(reason) if reason.contains("after")
    ));
    assert_eq!(
        rows(&pool, key(3, 31)).await,
        [1, 3, 2, 1, 2],
        "a graph written after the list stays"
    );
    let after = clock(&pool, 1).await;
    assert!(
        matches!(
            delete::delete_graph_guarded(&pool, key(3, 30), Some(&before))
                .await
                .expect("guarded"),
            delete::GuardedDeletion::Deleted { existed: true, .. }
        ),
        "a graph untouched since the list goes"
    );
    assert!(matches!(
        delete::delete_graph_guarded(&pool, key(3, 31), Some(&after))
            .await
            .expect("guarded"),
        delete::GuardedDeletion::Deleted { existed: true, .. }
    ));
    assert_eq!(rows(&pool, key(3, 31)).await, [0; 5]);
}

/// A source ingested after the list guards a graph whose own row is old.
#[tokio::test]
async fn a_source_ingested_after_the_list_guards_its_graph() {
    let Some(pool) = common::database("delete_listed_source").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    commit(&graphs, key(4, 40), &graph("g")).await;
    sqlx::query(
        "UPDATE inventory_graph.graphs SET updated_at = now() - interval '1 hour'
          WHERE project_id = 4 AND application_id = 40",
    )
    .execute(&pool)
    .await
    .expect("age the graph");
    let listed = clock(&pool, -600).await;
    // The graph row is older than the list; the source status is not.
    let reason = delete::activity_since(&pool, key(4, 40), &listed)
        .await
        .expect("activity");
    assert!(
        reason.is_some_and(|r| r.contains("source")),
        "the source row should guard the graph"
    );
    assert!(matches!(
        delete::delete_graph_guarded(&pool, key(4, 40), Some(&listed))
            .await
            .expect("guarded"),
        delete::GuardedDeletion::Newer(_)
    ));
    assert_eq!(rows(&pool, key(4, 40)).await, [1, 3, 2, 1, 2]);
}

#[tokio::test]
async fn the_orphans_command_refuses_a_stale_or_future_list_and_skips_newer_graphs() {
    let Some(pool) = common::database("delete_cli_listed").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    commit(&graphs, key(1, 10), &graph("live")).await;
    commit(&graphs, key(3, 30), &graph("old")).await;
    let listed_at = now(&pool).await;
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    commit(&graphs, key(3, 31), &graph("new")).await;
    let database = "delete_cli_listed";

    // Stale: an hour old, refused unless allowed; nothing deleted.
    let hour_old = clock(&pool, -3600).await;
    let (code, _, err) = orphans_command(database, "1\n", &["--delete", "--listed-at", &hour_old]);
    assert_eq!(code, Some(1), "{err}");
    assert!(err.contains("--allow-stale-list"), "{err}");
    assert_eq!(rows(&pool, key(3, 30)).await, [1, 3, 2, 1, 2]);
    // The future is always refused.
    let future = clock(&pool, 3600).await;
    let (code, _, err) = orphans_command(
        database,
        "1\n",
        &["--delete", "--listed-at", &future, "--allow-stale-list"],
    );
    assert_eq!(code, Some(1), "{err}");
    assert!(err.contains("in the future"), "{err}");
    // The override only applies to a deletion, and --listed-at must be a time.
    let (code, _, err) = orphans_command(database, "1\n", &["--allow-stale-list"]);
    assert_eq!(code, Some(2), "{err}");
    let (code, _, err) =
        orphans_command(database, "1\n", &["--delete", "--listed-at", "yesterday"]);
    assert_eq!(code, Some(2), "{err}");

    // A dry run with the time lists the older graph and skips the newer.
    let (code, out, err) = orphans_command(database, "1\n", &["--listed-at", &listed_at]);
    assert_eq!(code, Some(0), "{err}");
    assert!(out.contains("3\t30\tproject\t3\t"), "{out}");
    assert!(
        out.contains("3\t31\tproject") && out.contains("SKIPPED"),
        "{out}"
    );
    assert!(
        out.contains("dry run: 1 orphaned graph(s) would be deleted, 1 skipped"),
        "{out}"
    );

    // The deletion removes the older one and leaves the newer.
    let (code, out, err) =
        orphans_command(database, "1\n", &["--delete", "--listed-at", &listed_at]);
    assert_eq!(code, Some(0), "{err}");
    assert!(out.contains("deleted project 3 toolkit 30"), "{out}");
    assert_eq!(rows(&pool, key(3, 30)).await, [0; 5]);
    assert_eq!(
        rows(&pool, key(3, 31)).await,
        [1, 3, 2, 1, 2],
        "newer than the list"
    );
    assert_eq!(
        rows(&pool, key(1, 10)).await,
        [1, 3, 2, 1, 2],
        "a live project"
    );

    // An hour-old list goes with --allow-stale-list; the graph is then older
    // than the list's time only if it really was: here it is not, so skipped.
    let (code, out, err) = orphans_command(
        database,
        "1\n",
        &["--delete", "--listed-at", &hour_old, "--allow-stale-list"],
    );
    assert_eq!(code, Some(0), "{err}");
    assert!(
        out.contains("SKIPPED") && !out.contains("deleted project"),
        "{out}"
    );
    assert_eq!(rows(&pool, key(3, 31)).await, [1, 3, 2, 1, 2]);
}

/// A graph whose deletion fails makes the command exit non-zero; the others
/// still go.
#[tokio::test]
async fn the_orphans_command_exits_non_zero_when_a_deletion_fails() {
    let Some(pool) = common::database("delete_cli_failure").await else {
        return;
    };
    let graphs = PgGraphStore::new(pool.clone());
    commit(&graphs, key(3, 30), &graph("a")).await;
    commit(&graphs, key(3, 31), &graph("b")).await;
    sqlx::query(
        "CREATE FUNCTION inventory_graph.refuse_31() RETURNS trigger LANGUAGE plpgsql AS
         $$ BEGIN IF OLD.application_id = 31 THEN RAISE EXCEPTION 'refused by the test'; END IF; RETURN OLD; END $$",
    )
    .execute(&pool)
    .await
    .expect("function");
    sqlx::query(
        "CREATE TRIGGER refuse_31 BEFORE DELETE ON inventory_graph.graphs
         FOR EACH ROW EXECUTE FUNCTION inventory_graph.refuse_31()",
    )
    .execute(&pool)
    .await
    .expect("trigger");
    let listed_at = clock(&pool, 1).await;
    let (code, out, err) = orphans_command(
        "delete_cli_failure",
        "1\n",
        &["--delete", "--listed-at", &listed_at],
    );
    assert_eq!(code, Some(1), "{out} {err}");
    assert!(
        err.contains("toolkit 31") && err.contains("refused by the test"),
        "{err}"
    );
    assert_eq!(
        rows(&pool, key(3, 30)).await,
        [0; 5],
        "the other graph still went"
    );
    assert_eq!(
        rows(&pool, key(3, 31)).await,
        [1, 3, 2, 1, 2],
        "the failed delete rolled back"
    );
}
