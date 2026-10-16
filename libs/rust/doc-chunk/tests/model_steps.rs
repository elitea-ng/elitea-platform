//! Structural goldens for the model-step chunkers (ADR-0030 I4), with a
//! fixture model: the answers a chat model or an embeddings model would give
//! are scripted, so what is pinned is the crate's own structure (windows,
//! chunk types, order, ids, metadata, boundaries), not a model's output.
//! Regenerate with `UPDATE_GOLDEN=1 cargo test -p elitea-doc-chunk`.

use elitea_doc_chunk::{
    Chunk, ChunkError, ChunkPlan, ChunkingConfig, ProposalPlan, StatisticalPlan, chunk,
    parse_chunk_analysis, plan, proposal_system_prompt, proposal_user_message,
};
use serde_json::{Value, json};
use std::fmt::Write as _;
use std::path::PathBuf;

fn config(value: &Value) -> ChunkingConfig {
    ChunkingConfig::from_value(value).unwrap_or_else(|e| panic!("{e}"))
}

fn proposal_plan(text: &str, config: &ChunkingConfig) -> ProposalPlan {
    match plan(text, "doc.txt", config).unwrap_or_else(|e| panic!("{e}")) {
        ChunkPlan::Proposal(plan) => plan,
        other => panic!("not a proposal plan: {other:?}"),
    }
}

fn statistical_plan(text: &str, config: &ChunkingConfig) -> StatisticalPlan {
    match plan(text, "doc.txt", config).unwrap_or_else(|e| panic!("{e}")) {
        ChunkPlan::Statistical(plan) => plan,
        other => panic!("not a statistical plan: {other:?}"),
    }
}

fn summary(chunks: &[Chunk]) -> String {
    let mut out = String::new();
    for c in chunks {
        let title = c
            .metadata
            .get("chunk_title")
            .and_then(Value::as_str)
            .unwrap_or("-");
        let tokens = c
            .metadata
            .get("chunk_token_count")
            .map_or_else(|| "-".to_owned(), ToString::to_string);
        let first: String = c
            .text
            .lines()
            .next()
            .unwrap_or("")
            .chars()
            .take(40)
            .collect();
        let _ = writeln!(
            out,
            "{} {} {} title={title} tokens={tokens} | {first}",
            c.chunk_id,
            c.chunk_type,
            c.metadata["method_name"].as_str().unwrap_or("?"),
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
        std::fs::write(&path, &actual).unwrap_or_else(|e| panic!("{e}"));
        return;
    }
    let expected =
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    assert_eq!(
        actual, expected,
        "{name} changed; UPDATE_GOLDEN=1 and read the diff"
    );
}

#[test]
fn the_proposal_plan_cuts_windows_and_numbers_entries_across_them() {
    // 2300 one-token words: windows of 1024, 1024 and 252 tokens.
    let text = vec!["word"; 2300].join(" ");
    let plan = proposal_plan(&text, &config(&json!({"chunking_tool": "proposal"})));
    assert_eq!(plan.windows().len(), 3);
    assert_eq!(plan.windows().concat(), text, "no overlap, nothing lost");
    // The fixture model: window 1 has two entries, the others one.
    let answers: Vec<_> = [
        r#"{"chunks":[
            {"chunk_title":"Intro","chunk_summary":"The start.","propositions":["a","b"]},
            {"chunk_title":"Setup","chunk_summary":"Getting ready.","propositions":["c"]}]}"#,
        "```json\n{\"chunks\":[{\"chunk_title\":\"Use\",\"chunk_summary\":\"Using it.\",\"propositions\":[\"d\"]}]}\n```",
        r#"{"chunks":[{"chunk_title":"End","chunk_summary":"The end.","propositions":[]}]}"#,
    ]
    .iter()
    .map(|reply| parse_chunk_analysis(reply).unwrap_or_else(|e| panic!("{e}")))
    .collect();
    let chunks = plan.chunks(&answers).unwrap_or_else(|e| panic!("{e}"));
    golden("proposal_three_windows", &chunks);
    // The last entry's blank proposal list is no chunk: 4 + 4 + 4 + 3.
    assert_eq!(chunks.len(), 15);
    assert!(plan.chunks(&answers[..2]).is_err(), "one answer per window");
}

#[test]
fn the_proposal_prompt_is_the_sdks_with_the_json_shape() {
    let prompt = proposal_system_prompt();
    assert!(prompt.contains("You are the steward of a group of chunks"));
    assert!(prompt.contains("\"chunk_title\""));
    assert_eq!(proposal_user_message("W"), "Content: W");
}

#[test]
fn the_statistical_plan_splits_and_groups_on_topic_boundaries() {
    // Nine 100-token splits, three topics of three; the fixture embedding
    // puts each topic on its own axis.
    let text = vec!["word"; 900].join(" ");
    let config = config(&json!({
        "chunking_tool": "statistical",
        "max_doc_size": 100, "dynamic_threshold": false,
        "min_split_tokens": 200, "max_split_tokens": 400
    }));
    let plan = statistical_plan(&text, &config);
    assert_eq!(plan.splits().len(), 9);
    let vectors: Vec<Vec<f32>> = (0..9)
        .map(|i| {
            let mut v = vec![0.0; 3];
            v[i / 3] = 1.0;
            v
        })
        .collect();
    let chunks = plan.chunks(&vectors).unwrap_or_else(|e| panic!("{e}"));
    golden("statistical_three_topics", &chunks);
    assert_eq!(chunks.len(), 3);
    assert!(matches!(
        plan.chunks(&vectors[..8]),
        Err(ChunkError::InvalidModelStep(_))
    ));
}

#[test]
fn chunk_alone_still_reports_the_model_chunkers_as_requiring_a_model() {
    for tool in ["statistical", "proposal"] {
        let result = chunk("text", "a.txt", &config(&json!({"chunking_tool": tool})));
        assert!(
            matches!(result, Err(ChunkError::RequiresModel { .. })),
            "{tool}"
        );
    }
    // `use_llm` is the image-file step (`plan_image`): on a text document it
    // changes nothing, so the plan is the ordinary chunks.
    let result = plan("text", "a.txt", &config(&json!({"use_llm": true})));
    assert!(matches!(result, Ok(ChunkPlan::Chunks(_))));
}

#[test]
fn model_chunker_settings_read_the_sdk_keys() {
    let config = config(&json!({
        "chunking_tool": "proposal", "max_doc_tokens": 64,
        ".md": {"score_threshold": "0.7", "dynamic_threshold": false}
    }));
    assert_eq!(
        config.params_for(".txt").model_steps.max_doc_tokens,
        Some(64)
    );
    let md = config.params_for(".md");
    assert_eq!(md.model_steps.max_doc_tokens, Some(64));
    assert_eq!(md.model_steps.score_threshold, Some(0.7));
    assert_eq!(md.model_steps.dynamic_threshold, Some(false));
    // A 64-token window makes more windows than the default.
    let text = vec!["word"; 200].join(" ");
    assert_eq!(proposal_plan(&text, &config).windows().len(), 4);
}
