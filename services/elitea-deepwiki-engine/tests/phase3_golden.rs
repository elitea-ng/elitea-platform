//! The Phase 3 clustering gate on the synthetic fixtures
//! (`tests/fixtures/phase3`, written by `parity/python_phase3_fixture.py`).
//!
//! With leidenalg's recorded memberships replayed, the Rust Phase 3 must
//! give Python's cluster columns and stats exactly: everything but the
//! Leiden calls is a port. With the vendored Leiden, the result must be
//! deterministic and inside the consolidation targets.

use elitea_deepwiki_engine::graph::clustering::dump::{self, Phase3Dump};
use elitea_deepwiki_engine::graph::clustering::sizing::{target_section_count, target_total_pages};
use elitea_deepwiki_engine::graph::clustering::{
    ClusterAssignment, LeidenPartitioner, Phase3Flags, ReplayPartitioner, run_phase3,
};
use std::path::PathBuf;

const SCENARIOS: [&str; 3] = ["default", "exclude_tests_legacy", "all_isolated"];

fn load(name: &str) -> Phase3Dump {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/phase3")
        .join(name);
    dump::load(&dir).unwrap()
}

fn flags_of(dump: &Phase3Dump) -> Phase3Flags {
    let flags = &dump.summary["flags"];
    Phase3Flags {
        exclude_tests: flags["exclude_tests"].as_bool().unwrap(),
        calibrated_weights: flags["weight_calibration_profile"] == "calibrated",
    }
}

fn sorted(mut rows: Vec<ClusterAssignment>) -> Vec<ClusterAssignment> {
    rows.sort_by(|a, b| a.node_id.cmp(&b.node_id));
    rows
}

#[test]
fn replayed_leiden_gives_python_assignments_and_stats_exactly() {
    for name in SCENARIOS {
        let dump = load(name);
        let mut replay = ReplayPartitioner::new(dump.calls.clone());
        let out = run_phase3(&dump.graph, &dump.hubs, flags_of(&dump), &mut replay).unwrap();
        assert_eq!(
            replay.replayed,
            dump.calls.len(),
            "{name}: every recorded call replayed"
        );
        let rows = sorted(out.assignments(&dump.graph));
        assert_eq!(rows.len(), dump.expected.len(), "{name}");
        for (got, want) in rows.iter().zip(&dump.expected) {
            assert_eq!(got, want, "{name}");
        }
        let stats = serde_json::to_value(&out.stats).unwrap();
        assert_eq!(stats, dump.summary["phase3_stats"], "{name}: stats");
    }
}

#[test]
fn vendored_leiden_is_deterministic_and_meets_the_targets() {
    for name in SCENARIOS {
        let dump = load(name);
        let flags = flags_of(&dump);
        let first = run_phase3(&dump.graph, &dump.hubs, flags, &mut LeidenPartitioner).unwrap();
        for _ in 0..3 {
            let again = run_phase3(&dump.graph, &dump.hubs, flags, &mut LeidenPartitioner).unwrap();
            assert_eq!(
                again.assignments(&dump.graph),
                first.assignments(&dump.graph),
                "{name}"
            );
        }
        let meta = &first.stats.algorithm_metadata;
        let sections = first.stats.macro_.cluster_count;
        let pages = first.stats.micro.total_pages;
        assert!(
            sections <= target_section_count(meta.file_nodes.unwrap()),
            "{name}: {sections} sections"
        );
        assert!(
            pages <= target_total_pages(first.stats.macro_.nodes_assigned),
            "{name}: {pages} pages"
        );
        // Every clustered node has a section and a page.
        for row in first.assignments(&dump.graph) {
            assert_eq!(
                row.macro_cluster.is_some(),
                row.micro_cluster.is_some(),
                "{name}: {row:?}"
            );
        }
    }
}
