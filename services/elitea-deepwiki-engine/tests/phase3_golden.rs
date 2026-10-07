//! The Phase 3 clustering gate on the synthetic fixtures
//! (`tests/fixtures/phase3`, written by `parity/python_phase3_fixture.py`).
//!
//! With leidenalg's recorded memberships replayed, the Rust Phase 3 must
//! give Python's cluster columns and stats exactly: everything but the
//! Leiden calls is a port. With the vendored Leiden, the result must be
//! deterministic, inside the consolidation targets, and equal to the
//! committed digest ([`VENDORED_LEIDEN_DIGESTS`]).

use elitea_deepwiki_engine::graph::clustering::dump::{self, Phase3Dump};
use elitea_deepwiki_engine::graph::clustering::sizing::{target_section_count, target_total_pages};
use elitea_deepwiki_engine::graph::clustering::{
    ClusterAssignment, LeidenPartitioner, Phase3Flags, ReplayPartitioner, run_phase3,
};
use elitea_deepwiki_engine::graph::topology::digest::sha256;
use std::fmt::Write as _;
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

/// SHA-256 of the vendored-Leiden cluster columns per scenario: one
/// compact JSON line per node, in graph order (see [`assignment_digest`]).
///
/// The determinism test compares runs of ONE build, so a dependency bump
/// that changes the partitions (`rand` behind leiden-rs, its own code, the
/// toolchain's float code) would pass it unnoticed. These digests fail it.
///
/// To update them DELIBERATELY: make the change, run
/// `cargo test --test phase3_golden vendored_leiden_output_matches_the_committed_digests`,
/// check the new partitions (the Phase 3 quality gate,
/// `parity/compare_phase3.py`, and a look at the sections of a real
/// repository), then copy the digests the failure prints here, in the same
/// commit as the change and with the reason in its message.
const VENDORED_LEIDEN_DIGESTS: [(&str, &str); 3] = [
    (
        "default",
        "d8e938141027855320b0b9b64553706a058d309e7265aea023e829675c959142",
    ),
    (
        "exclude_tests_legacy",
        "8a7b0346edbfca398587f05924dd12bab94626730220e1178a239ba177b27488",
    ),
    (
        "all_isolated",
        "4a385f042e065574aa9fc0ce98274cb3bae441ab90f4f7e3e21bfafe8bb5a961",
    ),
];

/// The digest of a scenario's cluster columns.
fn assignment_digest(rows: &[ClusterAssignment]) -> String {
    let mut text = String::new();
    for row in rows {
        text.push_str(&serde_json::to_string(row).unwrap());
        text.push('\n');
    }
    let mut hex = String::new();
    for byte in sha256(text.as_bytes()) {
        write!(hex, "{byte:02x}").unwrap();
    }
    hex
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

#[test]
fn vendored_leiden_output_matches_the_committed_digests() {
    let mut mismatches = Vec::new();
    for (name, want) in VENDORED_LEIDEN_DIGESTS {
        let dump = load(name);
        let out = run_phase3(
            &dump.graph,
            &dump.hubs,
            flags_of(&dump),
            &mut LeidenPartitioner,
        )
        .unwrap();
        let got = assignment_digest(&out.assignments(&dump.graph));
        if got != want {
            mismatches.push(format!("    (\"{name}\", \"{got}\"),"));
        }
    }
    assert!(
        mismatches.is_empty(),
        "the vendored Leiden partitions changed; if that is deliberate, \
         read VENDORED_LEIDEN_DIGESTS and set:\n{}",
        mismatches.join("\n")
    );
}
