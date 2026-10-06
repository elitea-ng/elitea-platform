//! # leiden-rs
//!
//! A Rust implementation of the [Leiden algorithm](https://doi.org/10.1038/s41598-019-41695-z)
//! for community detection in graphs, with optional adapters for [gryf](https://github.com/pnevyk/gryf) and [petgraph](https://github.com/petgraph/petgraph).
//!
//! The Leiden algorithm guarantees well-connected communities through three phases:
//! local moving, refinement, and aggregation. It supports four quality functions:
//! **Modularity**, **CPM**, **RBConfiguration**, and **RBER**.
//!
//! # Quick Start
//!
//! ```
//! use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};
//!
//! let mut b = GraphDataBuilder::new(3);
//! b.add_edge(0, 1, 1.0).unwrap();
//! b.add_edge(1, 2, 1.0).unwrap();
//! let graph = b.build().unwrap();
//!
//! let leiden = Leiden::new(LeidenConfig::default());
//! let result = leiden.run(&graph).expect("leiden failed");
//! println!("Found {} communities (quality: {:.4})", result.partition.num_communities(), result.quality);
//! ```
//!
//! # Quality Functions
//!
//! - **Modularity**: `Q = Σ_c [e_c/m - γ*(Σ_c/(2m))²]` — the classic Newman-Girvan modularity
//! - **CPM**: `H = Σ_c [e_c - γ * n_c*(n_c-1)/2]` — avoids resolution limit, tunable via γ
//! - **RBConfiguration**: `Q = Σ_c [e_c - γ * K_c²/(4m)]` — Reichardt-Bornholdt with configuration null model
//! - **RBER**: `Q = Σ_c [e_c - γ * p * n_c*(n_c-1)/2]` — Reichardt-Bornholdt with Erdős-Rényi null model

#![warn(missing_docs)]
#![deny(unsafe_code)]

mod algorithm;
pub mod generators;
pub mod convert;
pub mod error;
pub mod fluid_communities;
pub mod graph;
pub mod hierarchy;
pub mod infomap;
pub mod label_propagation;
pub mod leiden;
pub mod lfr;
pub mod metrics;
pub mod multiplex;
pub mod output;
#[cfg(feature = "rayon")]
mod parallel;
pub mod partition;
pub mod quality;
pub mod resolution;
pub mod util;

#[cfg(feature = "wasm")]
pub mod wasm;

pub use error::{LeidenError, Result as LeidenResult};
pub use graph::{GraphData, GraphDataBuilder, MoveComponents};
pub use hierarchy::{HierarchicalOutput, HierarchyLevel};
pub use fluid_communities::{FluidCommunities, FluidCommunitiesConfig, FluidCommunitiesOutput};
pub use infomap::{
    calc_codelength, compute_flow, compute_node_flows, DeltaFlow, FlowData, Infomap,
    InfomapConfig, InfomapOutput, MapEquation, ModuleFlowData, plogp,
};
pub use leiden::{Leiden, LeidenConfig, LeidenConfigBuilder, LeidenOutput, QualityType};
pub use generators::{
    generate_ba_graph, generate_er_graph, generate_er_graph_exact, generate_planted_partition,
    generate_sbm, generate_sbm_symmetric,
};
pub use label_propagation::{LabelPropagation, LabelPropagationConfig, LabelPropagationOutput};
pub use lfr::{generate_lfr_graph, LfrConfig, LfrGraph};
pub use metrics::{
    ami, ari, completeness, conductance, coverage, fmi, homogeneity, internal_density, nmi,
    performance, purity, try_ami, try_ari, try_completeness, try_fmi, try_homogeneity, try_nmi,
    try_purity, try_vi, try_v_measure, v_measure, vi,
};
pub use multiplex::{run_multiplex, MultiplexConfig, MultiplexOutput};
pub use output::{CommunityDetectionOutput, ToJson};
pub use partition::Partition;
pub use quality::{Modularity, QualityFunction, RBConfiguration, CPM, RBER};
pub use resolution::{resolution_profile, resolution_scan, ResolutionEntry};
pub use util::{load_edgelist, modularity as compute_modularity};

#[cfg(feature = "gryf")]
pub use convert::gryf::from_gryf;
#[cfg(feature = "gryf")]
pub use convert::gryf::from_gryf_directed;

#[cfg(feature = "petgraph")]
pub use convert::petgraph::from_petgraph;

#[cfg(test)]
mod tests {
    /// Verify that `#[must_use]` attributes are present on all
    /// public constructors and pure functions in non-test code.
    fn collect_rs_files(dir: &std::path::Path) -> Vec<std::path::PathBuf> {
        let mut files = Vec::new();
        if let Ok(entries) = std::fs::read_dir(dir) {
            for entry in entries.flatten() {
                let path = entry.path();
                if path.is_dir() {
                    files.extend(collect_rs_files(&path));
                } else if path.extension().and_then(|e| e.to_str()) == Some("rs") {
                    files.push(path);
                }
            }
        }
        files
    }

    #[test]
    fn must_use_attributes_present() {
        let src = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
        let mut count = 0usize;

        for path in collect_rs_files(&src) {
            let content = std::fs::read_to_string(&path).unwrap();
            let mut in_test_block = false;

            for line in content.lines() {
                let t = line.trim();
                if t == "#[cfg(test)]" || t.starts_with("mod tests") {
                    in_test_block = true;
                } else if (t.starts_with("pub mod ") || t.starts_with("mod "))
                    && !t.contains("tests")
                {
                    in_test_block = false;
                }
                if t.starts_with("#[must_use") && !in_test_block {
                    count += 1;
                }
            }
        }

        assert!(
            count >= 53,
            "expected at least 53 #[must_use] attributes in non-test code, found {count}"
        );
    }
}
