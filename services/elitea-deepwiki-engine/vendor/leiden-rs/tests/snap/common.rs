use std::collections::HashMap;
use std::io::Read;
use std::path::PathBuf;

use flate2::read::GzDecoder;
pub use leiden_rs::{GraphData, GraphDataBuilder, Leiden, LeidenConfig, Partition};

pub struct SnapDataset {
    pub filename: &'static str,
    pub url: &'static str,
    pub node_count: usize,
    pub directed: bool,
}

impl SnapDataset {
    fn data_dir() -> PathBuf {
        let manifest_dir = std::env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR not set");
        PathBuf::from(manifest_dir).join("data").join(".cache")
    }

    fn local_path(&self) -> PathBuf {
        Self::data_dir().join(self.filename.trim_end_matches(".gz"))
    }

    pub fn fetch(&self) -> Result<PathBuf, String> {
        let local = self.local_path();
        if local.exists() {
            return Ok(local);
        }

        let dir = Self::data_dir();
        std::fs::create_dir_all(&dir)
            .map_err(|e| format!("Failed to create {}: {e}", dir.display()))?;

        println!("Downloading {}...", self.url);
        let response = ureq::get(self.url)
            .call()
            .map_err(|e| format!("Failed to download {}: {e}", self.url))?;

        let mut compressed = Vec::new();
        response
            .into_body()
            .into_reader()
            .read_to_end(&mut compressed)
            .map_err(|e| format!("Failed to read response body: {e}"))?;

        let mut decoder = GzDecoder::new(compressed.as_slice());
        let mut text = String::new();
        decoder
            .read_to_string(&mut text)
            .map_err(|e| format!("Failed to decompress {}: {e}", self.filename))?;

        let tmp_path = local.with_extension("tmp");
        std::fs::write(&tmp_path, &text)
            .map_err(|e| format!("Failed to write {}: {e}", tmp_path.display()))?;
        std::fs::rename(&tmp_path, &local).map_err(|e| {
            format!(
                "Failed to rename {} -> {}: {e}",
                tmp_path.display(),
                local.display()
            )
        })?;

        println!("Saved {} ({} bytes)", local.display(), text.len());
        Ok(local)
    }

    pub fn load(&self) -> Result<GraphData, String> {
        let path = self.fetch()?;
        let text = std::fs::read_to_string(&path)
            .map_err(|e| format!("Failed to read {}: {e}", path.display()))?;
        parse_snap_edgelist(&text, self.directed)
    }
}

// SNAP format: # comment lines, then "src\tdst" per edge.
// Node IDs are non-contiguous integers, remapped to 0-indexed.
fn parse_snap_edgelist(text: &str, directed: bool) -> Result<GraphData, String> {
    let mut node_map: HashMap<usize, usize> = HashMap::new();
    let mut next_id: usize = 0;
    let mut edges: Vec<(usize, usize)> = Vec::new();

    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() < 2 {
            continue;
        }
        let src: usize = match parts[0].parse() {
            Ok(v) => v,
            Err(_) => continue,
        };
        let dst: usize = match parts[1].parse() {
            Ok(v) => v,
            Err(_) => continue,
        };

        let src_idx = *node_map.entry(src).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });
        let dst_idx = *node_map.entry(dst).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });

        edges.push((src_idx, dst_idx));
    }

    let node_count = node_map.len();
    if node_count == 0 {
        return Err("Empty dataset: no edges found".to_string());
    }

    let mut builder = GraphDataBuilder::new(node_count);

    if directed {
        builder = builder.directed();
        for (src, dst) in &edges {
            builder
                .add_edge(*src, *dst, 1.0)
                .map_err(|e| format!("Failed to add edge ({src}, {dst}): {e}"))?;
        }
    } else {
        let mut seen = std::collections::HashSet::new();
        for (src, dst) in &edges {
            let key = (*src).min(*dst) * node_count + (*src).max(*dst);
            if seen.insert(key) && *src != *dst {
                builder
                    .add_edge(*src, *dst, 1.0)
                    .map_err(|e| format!("Failed to add edge ({src}, {dst}): {e}"))?;
            }
        }
    }

    builder
        .build()
        .map_err(|e| format!("Failed to build GraphData: {e}"))
}

pub fn run_leiden(data: &GraphData, seed: u64) -> Partition {
    Leiden::new(LeidenConfig {
        seed: Some(seed),
        ..Default::default()
    })
    .run(data)
    .unwrap()
    .partition
}

pub const CA_GRQC: SnapDataset = SnapDataset {
    filename: "ca-GrQc.txt.gz",
    url: "https://snap.stanford.edu/data/ca-GrQc.txt.gz",
    node_count: 5242,
    directed: false,
};

pub const CA_HEPTH: SnapDataset = SnapDataset {
    filename: "ca-HepTh.txt.gz",
    url: "https://snap.stanford.edu/data/ca-HepTh.txt.gz",
    node_count: 9877,
    directed: false,
};

pub const CA_HEPPH: SnapDataset = SnapDataset {
    filename: "ca-HepPh.txt.gz",
    url: "https://snap.stanford.edu/data/ca-HepPh.txt.gz",
    node_count: 12008,
    directed: false,
};

pub const EMAIL_ENRON: SnapDataset = SnapDataset {
    filename: "email-Enron.txt.gz",
    url: "https://snap.stanford.edu/data/email-Enron.txt.gz",
    node_count: 36692,
    directed: false,
};
