use std::error::Error;
use std::io::{self, BufRead, Write};
use std::path::PathBuf;

use clap::Parser;
use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig, QualityType};
use rustc_hash::FxHashMap;

#[derive(Parser)]
#[command(name = "leiden-cli")]
#[command(about = "Community detection using the Leiden algorithm")]
#[command(version)]
struct Cli {
    /// Input edge list file (stdin if not specified)
    input: Option<PathBuf>,

    /// Resolution parameter γ
    #[arg(short, long, default_value = "1.0")]
    resolution: f64,

    /// Max iterations
    #[arg(short, long, default_value_t = 100)]
    iterations: usize,

    /// Random seed for reproducibility
    #[arg(short, long)]
    seed: Option<u64>,

    /// Quality function: modularity, cpm, rbconfiguration, or rber
    #[arg(long, default_value = "modularity")]
    quality: String,

    /// Convergence threshold (stop when quality improvement < epsilon)
    #[arg(long, default_value = "1e-10")]
    epsilon: f64,

    /// Maximum nodes per community (0 = unlimited)
    #[arg(long, default_value_t = 0)]
    max_comm_size: usize,

    /// Whether the graph is directed
    #[arg(long, default_value_t = false)]
    directed: bool,

    /// Minimum edge slots for parallel local moving (default: 2000)
    #[arg(long)]
    parallel_local_moving_threshold: Option<usize>,

    /// Minimum edge slots for parallel aggregation (default: 10000)
    #[arg(long)]
    parallel_aggregation_threshold: Option<usize>,

    /// Output file (stdout if not specified)
    #[arg(short, long)]
    output: Option<PathBuf>,
}

fn main() -> Result<(), Box<dyn Error>> {
    let cli = Cli::parse();

    let quality = match cli.quality.as_str() {
        "modularity" => QualityType::Modularity,
        "cpm" => QualityType::CPM,
        "rbconfiguration" | "rbconfig" => QualityType::RBConfiguration,
        "rber" => QualityType::RBER,
        other => {
            eprintln!("Error: unknown quality function '{other}' (use 'modularity', 'cpm', 'rbconfiguration', or 'rber')");
            std::process::exit(1);
        }
    };

    let reader: Box<dyn BufRead> = match &cli.input {
        Some(path) => Box::new(io::BufReader::new(
            std::fs::File::open(path).unwrap_or_else(|e| {
                eprintln!("Error: cannot open input file '{}': {e}", path.display());
                std::process::exit(1);
            }),
        )),
        None => Box::new(io::BufReader::new(io::stdin())),
    };

    let mut edges: Vec<(usize, usize, f64)> = Vec::new();

    for line in reader.lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("Error reading input: {e}");
                std::process::exit(1);
            }
        };
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            continue;
        }
        let parts: Vec<&str> = trimmed.split_whitespace().collect();
        if parts.len() < 2 {
            eprintln!("Skipping malformed line: {trimmed}");
            continue;
        }
        let u: usize = match parts[0].parse() {
            Ok(v) => v,
            Err(_) => {
                eprintln!("Skipping line with non-integer node: {trimmed}");
                continue;
            }
        };
        let v: usize = match parts[1].parse() {
            Ok(v) => v,
            Err(_) => {
                eprintln!("Skipping line with non-integer node: {trimmed}");
                continue;
            }
        };
        let w: f64 = parts
            .get(2)
            .map(|s| s.parse().unwrap_or(1.0))
            .unwrap_or(1.0);
        edges.push((u, v, w));
    }

    if edges.is_empty() {
        eprintln!("Error: no edges found in input");
        std::process::exit(1);
    }

    let mut id_map: FxHashMap<usize, usize> = FxHashMap::default();
    let mut next_id = 0usize;
    for &(u, v, _) in &edges {
        id_map.entry(u).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });
        id_map.entry(v).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });
    }

    let n = next_id;
    let mut builder = GraphDataBuilder::new(n);
    if cli.directed {
        builder = builder.directed();
    }
    for &(u, v, w) in &edges {
        builder.add_edge(id_map[&u], id_map[&v], w)?;
    }
    let data = builder.build()?;

    let mut rev_map: Vec<usize> = vec![0; n];
    for (&orig, &remapped) in &id_map {
        rev_map[remapped] = orig;
    }

    eprintln!("Loaded graph: {n} nodes, {} edges", edges.len());

    let config = LeidenConfig::builder()
        .max_iterations(cli.iterations)
        .resolution(cli.resolution)
        .maybe_seed(cli.seed)
        .quality(quality)
        .epsilon(cli.epsilon)
        .max_comm_size(cli.max_comm_size)
        .maybe_parallel_local_moving_threshold(cli.parallel_local_moving_threshold)
        .maybe_parallel_aggregation_threshold(cli.parallel_aggregation_threshold)
        .build();
    let leiden = Leiden::new(config);
    let result = leiden.run(&data).unwrap_or_else(|e| {
        eprintln!("Error: {e}");
        std::process::exit(1);
    });

    eprintln!(
        "Found {} communities (quality: {:.4})",
        result.partition.num_communities(),
        result.quality
    );

    let mut writer: Box<dyn Write> = match &cli.output {
        Some(path) => Box::new(std::fs::File::create(path).unwrap_or_else(|e| {
            eprintln!("Error: cannot create output file '{}': {e}", path.display());
            std::process::exit(1);
        })),
        None => Box::new(io::stdout()),
    };

    let _ = writeln!(writer, "# node_id community_id");
    for (node, orig_id) in rev_map.into_iter().enumerate() {
        let _ = writeln!(
            writer,
            "{} {}",
            orig_id,
            result.partition.community_of(node)
        );
    }

    Ok(())
}
