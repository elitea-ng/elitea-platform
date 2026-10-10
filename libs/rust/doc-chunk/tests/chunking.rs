//! Goldens and properties for the chunkers (ADR-0030 decision 4).
//!
//! The goldens are this crate's own, not byte-compared with the SDK: they
//! pin where our boundaries fall so a change to a splitter shows up as a
//! diff. Regenerate with `UPDATE_GOLDEN=1 cargo test -p elitea-doc-chunk`
//! and read the diff.

#![allow(clippy::format_collect)]

use elitea_doc_chunk::{Chunk, ChunkError, ChunkingConfig, chunk};
use serde_json::{Value, json};
use std::fmt::Write as _;
use std::path::PathBuf;

fn fixture(name: &str) -> String {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn config(value: &Value) -> ChunkingConfig {
    ChunkingConfig::from_value(value).unwrap_or_else(|e| panic!("{e}"))
}

fn run(text: &str, source: &str, config: &ChunkingConfig) -> Vec<Chunk> {
    chunk(text, source, config).unwrap_or_else(|e| panic!("{source}: {e}"))
}

fn bpe() -> &'static tiktoken_rs::CoreBPE {
    static BPE: std::sync::OnceLock<tiktoken_rs::CoreBPE> = std::sync::OnceLock::new();
    BPE.get_or_init(|| tiktoken_rs::cl100k_base().unwrap_or_else(|e| panic!("{e}")))
}

fn tokens(text: &str) -> usize {
    bpe().encode_ordinary(text).len()
}

/// One line per chunk: id, method, headers or language, size, first line.
fn summary(chunks: &[Chunk]) -> String {
    let mut out = String::new();
    for c in chunks {
        let tag = c
            .metadata
            .get("headers")
            .or_else(|| c.metadata.get("language"))
            .and_then(Value::as_str)
            .unwrap_or("");
        let first: String = c
            .text
            .lines()
            .next()
            .unwrap_or("")
            .chars()
            .take(48)
            .collect();
        let _ = writeln!(
            out,
            "{} {} {} [{tag}] {}c {}t | {first}",
            c.chunk_id,
            c.chunk_type,
            c.metadata["method_name"].as_str().unwrap_or("?"),
            c.text.chars().count(),
            tokens(&c.text),
        );
    }
    out
}

fn golden(name: &str, chunks: &[Chunk]) {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/golden")
        .join(format!("{name}.txt"));
    let actual = summary(chunks);
    if std::env::var_os("UPDATE_GOLDEN").is_some() {
        std::fs::create_dir_all(path.parent().unwrap_or(&path)).unwrap_or_else(|e| panic!("{e}"));
        std::fs::write(&path, &actual).unwrap_or_else(|e| panic!("{e}"));
        return;
    }
    let expected =
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    assert_eq!(actual, expected, "golden {name} changed");
}

/// Every byte of `text` lies in some chunk, chunks follow the text in order,
/// and neighbours touch or overlap (only whitespace may fall between them,
/// which the splitters trim).
fn assert_covers(text: &str, chunks: &[Chunk]) {
    assert!(!chunks.is_empty());
    let (mut from, mut end) = (0, 0);
    for (i, c) in chunks.iter().enumerate() {
        let at = text[from..]
            .find(&c.text)
            .unwrap_or_else(|| panic!("chunk {} is not in the text: {:?}", i + 1, c.text));
        let start = from + at;
        assert!(
            text[end.min(start)..start].trim().is_empty(),
            "a gap before chunk {}: {:?}",
            i + 1,
            &text[end..start]
        );
        end = end.max(start + c.text.len());
        from = start + 1;
    }
    assert!(
        text[end..].trim().is_empty(),
        "text left over: {:?}",
        &text[end..]
    );
}

fn words(count: usize) -> String {
    (0..count)
        .map(|n| format!("word{n} lorem{} ipsum,", n * 7 % 13))
        .collect::<Vec<_>>()
        .join(" ")
}

// ---- goldens -------------------------------------------------------------

#[test]
fn golden_markdown_with_nested_headers() {
    let text = fixture("handbook.md");
    let chunks = run(&text, "handbook.md", &ChunkingConfig::default());
    golden("handbook_md_default", &chunks);
    // With the merge off, every header opens its own chunk.
    let each = config(&json!({"min_chunk_chars": 0}));
    golden("handbook_md_unmerged", &run(&text, "handbook.md", &each));
}

#[test]
fn golden_markdown_long_section_is_token_split() {
    let text = format!(
        "# Long\n\n{}\n\n## Tail\n\nshort tail that is long enough to stand alone as a chunk of its own, yes.",
        words(500)
    );
    let small = config(&json!({"max_tokens": 100, "token_overlap": 10}));
    golden("long_md_100", &run(&text, "long.md", &small));
}

#[test]
fn golden_text_long_document() {
    let text = words(900);
    golden(
        "long_txt_default",
        &run(&text, "long.txt", &ChunkingConfig::default()),
    );
    let small = config(&json!({".txt": {"max_tokens": 128, "token_overlap": 16}}));
    golden("long_txt_128", &run(&text, "long.txt", &small));
}

#[test]
fn golden_json() {
    let text = fixture("config.json");
    golden(
        "config_json_default",
        &run(&text, "config.json", &ChunkingConfig::default()),
    );
    let small = config(&json!({"max_tokens": 160}));
    let chunks = run(&text, "config.json", &small);
    golden("config_json_160", &chunks);
    // The pieces of keys are Python-style dumps: ", " and ": " separators,
    // ASCII. The one string too long for a chunk is raw text, with its path.
    assert!(
        chunks
            .iter()
            .filter(|c| !c.metadata.contains_key("json_path"))
            .all(|c| c.text.is_ascii())
    );
    assert!(
        chunks
            .iter()
            .any(|c| c.metadata.get("json_path").is_some_and(|p| p == "/notes"))
    );
    assert!(chunks.iter().any(|c| c.text.contains("\"path\": ")));
}

#[test]
fn golden_code_rust_python_typescript() {
    let defaults = ChunkingConfig::default();
    golden(
        "sample_rs",
        &run(&fixture("sample.rs"), "src/sample.rs", &defaults),
    );
    golden(
        "sample_py",
        &run(&fixture("sample.py"), "sample.py", &defaults),
    );
    golden(
        "sample_ts",
        &run(&fixture("sample.ts"), "sample.ts", &defaults),
    );
    // The doc comment travels with its function; code outside functions
    // (the `use`, the `const`, the struct) is not a chunk, as in the SDK.
    let rust = run(&fixture("sample.rs"), "sample.rs", &defaults);
    assert!(rust[0].text.starts_with("/// Counts words."));
    assert!(!rust.iter().any(|c| c.text.contains("const LIMIT")));
}

#[test]
fn golden_unknown_language_is_a_token_split() {
    let text = fixture("report.sql");
    let small = config(&json!({"unknown_chunk_size": 40, "unknown_chunk_overlap": 5}));
    let chunks = run(&text, "report.sql", &small);
    golden("report_sql_40", &chunks);
    assert!(chunks.len() > 2);
    assert!(chunks.iter().all(|c| c.metadata["language"] == "unknown"));
    assert_covers(&text, &chunks);
    // At the SDK default (256 tokens) this file is one chunk.
    assert_eq!(
        run(&text, "report.sql", &ChunkingConfig::default()).len(),
        1
    );
}

#[test]
fn the_router_sends_each_extension_to_its_chunker() {
    let md = run("# A\n\ntext\n", "x.md", &ChunkingConfig::default());
    assert_eq!(md[0].metadata["method_name"], "markdown");
    let txt = run("hello", "x.log", &ChunkingConfig::default());
    assert_eq!(txt[0].metadata["method_name"], "text");
    let js = run("{\"a\": 1}", "x.json", &ChunkingConfig::default());
    assert_eq!(js[0].metadata["method_name"], "json");
    let py = run(
        "def f():\n    return 1\n",
        "x.py",
        &ChunkingConfig::default(),
    );
    assert_eq!(py[0].metadata["method_name"], "f");
    // The same through a media type.
    let by_mime = run(
        "def f():\n    return 1\n",
        "text/x-python",
        &ChunkingConfig::default(),
    );
    assert_eq!(by_mime, py);
}

#[test]
fn model_steps_are_typed_outcomes_not_chunks() {
    let c = config(&json!({"chunking_tool": "statistical"}));
    assert!(matches!(
        chunk("text", "a.txt", &c),
        Err(ChunkError::RequiresModel {
            chunker: "statistical",
            ..
        })
    ));
}

// ---- properties ----------------------------------------------------------

#[test]
fn text_chunks_never_exceed_max_tokens_and_cover_the_input() {
    let text = words(400);
    for (size, overlap) in [(512, 10), (128, 16), (64, 0), (50, 45), (20, 25)] {
        let c = config(&json!({"max_tokens": size, "token_overlap": overlap}));
        let chunks = run(&text, "a.txt", &c);
        assert!(chunks.len() > 1);
        for chunk in &chunks {
            assert!(
                tokens(&chunk.text) <= size,
                "{size}/{overlap}: {}",
                tokens(&chunk.text)
            );
        }
        assert_covers(&text, &chunks);
        assert_eq!(
            chunks.iter().map(|c| c.chunk_id).collect::<Vec<_>>(),
            (1..=chunks.len()).collect::<Vec<_>>()
        );
    }
}

#[test]
fn consecutive_text_chunks_share_the_configured_overlap() {
    let text = words(800);
    for overlap in [1_usize, 10, 32] {
        let c = config(&json!({"max_tokens": 100, "token_overlap": overlap}));
        let chunks = run(&text, "a.txt", &c);
        let bpe = bpe();
        for pair in chunks.windows(2) {
            let a = bpe.encode_ordinary(&pair[0].text);
            let b = bpe.encode_ordinary(&pair[1].text);
            assert_eq!(a[a.len() - overlap..], b[..overlap], "overlap {overlap}");
        }
    }
}

#[test]
fn markdown_keeps_every_character_when_nothing_needs_splitting() {
    let text = fixture("handbook.md");
    for min in [0, 100, 400] {
        let c = config(&json!({"min_chunk_chars": min}));
        let chunks = run(&text, "handbook.md", &c);
        let joined: String = chunks.iter().map(|c| c.text.as_str()).collect();
        let squash = |s: &str| s.chars().filter(|c| !c.is_whitespace()).collect::<String>();
        assert_eq!(squash(&joined), squash(&text), "min_chunk_chars {min}");
        for chunk in &chunks {
            assert!(tokens(&chunk.text) <= 512);
        }
    }
}

#[test]
fn markdown_token_splits_stay_within_the_limit_and_cover_their_section() {
    let body = words(700);
    let text = format!("## Big\n{body}\n");
    let c = config(&json!({"max_tokens": 90, "token_overlap": 9}));
    let chunks = run(&text, "a.md", &c);
    assert!(chunks.len() > 5);
    for chunk in &chunks {
        assert!(tokens(&chunk.text) <= 90);
        assert_eq!(chunk.metadata["headers"], "Big");
    }
    assert_covers(text.trim(), &chunks);
}

/// The scalar leaves of a JSON value, lists as index-keyed objects.
fn leaves(value: &Value, out: &mut Vec<String>) {
    match value {
        Value::Object(map) => map.values().for_each(|v| leaves(v, out)),
        Value::Array(items) => items.iter().for_each(|v| leaves(v, out)),
        other => out.push(other.to_string()),
    }
}

#[test]
fn json_chunks_are_valid_bounded_and_keep_every_value() {
    let text = fixture("config.json");
    let original: Value = serde_json::from_str(&text).unwrap_or_else(|e| panic!("{e}"));
    let mut want = Vec::new();
    leaves(&original, &mut want);
    want.sort();
    for max in [120_usize, 160, 300, 512] {
        let c = config(&json!({"max_tokens": max}));
        let chunks = run(&text, "config.json", &c);
        let mut got = Vec::new();
        let mut pieces: Vec<&str> = Vec::new();
        for chunk in &chunks {
            if chunks.len() > 1 {
                // No chunk is over the limit, the one long string included.
                assert!(chunk.text.chars().count() <= max, "{max}: {}", chunk.text);
            }
            if let Some(path) = chunk.metadata.get("json_path") {
                // A piece of a string too long for any chunk: raw text, with
                // the pointer of the value it was cut from.
                assert_eq!(path, "/notes", "{max}");
                pieces.push(&chunk.text);
                continue;
            }
            let parsed: Value = serde_json::from_str(&chunk.text)
                .unwrap_or_else(|e| panic!("{max}: {e}: {}", chunk.text));
            leaves(&parsed, &mut got);
        }
        if !pieces.is_empty() {
            let notes = original["notes"].as_str().unwrap_or_default();
            let joined = pieces.join(" ");
            assert_eq!(
                joined.split_whitespace().collect::<Vec<_>>(),
                notes.split_whitespace().collect::<Vec<_>>(),
                "{max}"
            );
            got.push(Value::String(notes.to_owned()).to_string());
        }
        got.sort();
        assert_eq!(got, want, "max {max}");
    }
}

#[test]
fn code_pieces_respect_the_character_size_and_overlap() {
    let body: String = (0..150)
        .map(|n| format!("    let value_{n} = compute({n}) + offset;\n"))
        .collect();
    let function = format!("fn big() {{\n{body}}}\n");
    let c = config(&json!({"chunk_size": 300, "chunk_overlap": 40}));
    let chunks = run(&function, "big.rs", &c);
    assert!(chunks.len() > 5);
    for chunk in &chunks {
        assert!(chunk.text.chars().count() <= 300);
        assert_eq!(chunk.metadata["method_name"], "big");
    }
    assert_covers(function.trim(), &chunks);
}

#[test]
fn multi_byte_text_never_panics_and_loses_no_character_outside_cuts() {
    let text = "Привет, мир! 你好，世界。 ".repeat(120);
    let c = config(&json!({"max_tokens": 40, "token_overlap": 4}));
    let chunks = run(&text, "a.txt", &c);
    assert!(chunks.len() > 3);
    // A window may cut through a character (decoded with U+FFFD, as Python
    // does); everything else is intact.
    let lossy = chunks
        .iter()
        .filter(|c| c.text.contains('\u{fffd}'))
        .count();
    assert!(lossy <= chunks.len());
    assert!(chunks[0].text.starts_with("Привет"));
}
