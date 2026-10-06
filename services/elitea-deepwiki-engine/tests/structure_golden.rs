//! Golden tests of repository analysis and structure planning against
//! fixtures the Python engine wrote (`parity/python_structure_fixture.py`):
//! no Python, no network. A scripted model answers in the recorded order
//! and checks every request's messages against Python's.

use elitea_deepwiki_engine::errors::{EngineError, ErrorType};
use elitea_deepwiki_engine::graph::EdgeRow;
use elitea_deepwiki_engine::graph::discover;
use elitea_deepwiki_engine::llm::{ChatMessage, ChatRequest};
use elitea_deepwiki_engine::structure::analysis::{self, AnalysisSummary, RepositoryAnalysis};
use elitea_deepwiki_engine::structure::cluster::{ClusterPlanner, ClusterSettings};
use elitea_deepwiki_engine::structure::index::{IndexNode, PlannerIndex};
use elitea_deepwiki_engine::structure::model::ChatModel;
use elitea_deepwiki_engine::structure::{self, PlannerChoice, StructureSettings};
use serde_json::Value;
use std::path::PathBuf;
use std::sync::Mutex;

fn fixture(name: &str) -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/structure")
        .join(name);
    serde_json::from_str(&std::fs::read_to_string(&path).expect("fixture")).expect("json")
}

fn message_pairs(messages: &[ChatMessage]) -> Value {
    Value::Array(
        messages
            .iter()
            .map(|m| match m {
                ChatMessage::System(text) => serde_json::json!(["system", text]),
                ChatMessage::User(text) => serde_json::json!(["user", text]),
                other => panic!("unexpected message {other:?}"),
            })
            .collect(),
    )
}

/// Answers from the recording, in order; records each mismatch.
struct ScriptedModel {
    calls: Vec<Value>,
    next: Mutex<usize>,
    mismatches: Mutex<Vec<String>>,
}

impl ScriptedModel {
    fn new(calls: Vec<Value>) -> Self {
        Self {
            calls,
            next: Mutex::new(0),
            mismatches: Mutex::new(Vec::new()),
        }
    }

    fn finish(&self) {
        let used = *self.next.lock().expect("lock");
        let mismatches = self.mismatches.lock().expect("lock");
        assert!(mismatches.is_empty(), "{}", mismatches.join("\n"));
        assert_eq!(used, self.calls.len(), "every recorded call is made");
    }
}

impl ChatModel for ScriptedModel {
    async fn complete(&self, request: &ChatRequest) -> Result<String, EngineError> {
        let index = {
            let mut next = self.next.lock().expect("lock");
            *next += 1;
            *next - 1
        };
        let Some(call) = self.calls.get(index) else {
            self.mismatches
                .lock()
                .expect("lock")
                .push(format!("call {index} was not recorded"));
            return Err(EngineError::new(ErrorType::Runtime, "no answer"));
        };
        let got = message_pairs(&request.messages);
        if got != call["messages"] {
            let detail = (0..2)
                .find_map(|i| {
                    let (a, b) = (got[i][1].as_str()?, call["messages"][i][1].as_str()?);
                    let at = a
                        .chars()
                        .zip(b.chars())
                        .position(|(x, y)| x != y)
                        .unwrap_or(a.len().min(b.len()));
                    (a != b).then(|| {
                        format!(
                            "message {i} at char {at}: rust {:?} / python {:?}",
                            a.chars()
                                .skip(at.saturating_sub(40))
                                .take(100)
                                .collect::<String>(),
                            b.chars()
                                .skip(at.saturating_sub(40))
                                .take(100)
                                .collect::<String>()
                        )
                    })
                })
                .unwrap_or_default();
            self.mismatches
                .lock()
                .expect("lock")
                .push(format!("call {index}: messages differ {detail}"));
        }
        match call["answer"].as_str() {
            Some(answer) => Ok(answer.to_owned()),
            None => Err(EngineError::new(ErrorType::Runtime, "scripted failure")),
        }
    }
}

fn index_of(cluster: &Value) -> PlannerIndex {
    let nodes = cluster["rows"]
        .as_array()
        .expect("rows")
        .iter()
        .map(|row| IndexNode {
            node_id: row["node_id"].as_str().expect("id").to_owned(),
            rel_path: row["rel_path"].as_str().expect("rel").to_owned(),
            symbol_name: row["symbol_name"].as_str().expect("name").to_owned(),
            symbol_type: row["symbol_type"].as_str().expect("type").to_owned(),
            signature: row["signature"].as_str().expect("sig").to_owned(),
            docstring: row["docstring"].as_str().expect("doc").to_owned(),
            source_text: row["source_text"].as_str().map(str::to_owned),
            is_architectural: row["is_architectural"] == 1,
            is_doc: row["is_doc"] == 1,
            is_test: row["is_test"] == 1,
            macro_cluster: row["macro_cluster"].as_i64(),
            micro_cluster: row["micro_cluster"].as_i64(),
        })
        .collect();
    let edges: Vec<EdgeRow> = cluster["edges"]
        .as_array()
        .expect("edges")
        .iter()
        .map(|edge| EdgeRow {
            source_id: edge[0].as_str().expect("source").to_owned(),
            target_id: edge[1].as_str().expect("target").to_owned(),
            rel_type: "calls".to_owned(),
            edge_class: "structural".to_owned(),
            analysis_level: "comprehensive".to_owned(),
            // NULL reads as 1.0 (`weight or 1.0`).
            weight: edge[2].as_f64().unwrap_or(1.0),
            raw_similarity: None,
            source_file: String::new(),
            target_file: String::new(),
            language: String::new(),
            annotations: String::new(),
            created_by: "ast".to_owned(),
        })
        .collect();
    PlannerIndex::new(
        nodes,
        &edges,
        cluster["repo_identifier"].as_str().map(str::to_owned),
    )
}

#[tokio::test]
async fn cluster_planner_matches_python_on_every_branch() {
    let cluster = fixture("cluster.json");
    let index = index_of(&cluster);
    for case in cluster["cases"].as_array().expect("cases") {
        let exclude_tests = case["exclude_tests"].as_bool().expect("flag");
        let model = ScriptedModel::new(case["calls"].as_array().expect("calls").clone());
        let settings = ClusterSettings {
            exclude_tests,
            ..ClusterSettings::default()
        };
        let spec = ClusterPlanner::new(&index, settings)
            .plan_structure(&model)
            .await
            .expect("plans");
        model.finish();
        assert_eq!(
            spec.to_value(),
            case["structure"],
            "exclude_tests={exclude_tests}"
        );
    }
}

#[tokio::test]
async fn analysis_and_classic_planner_match_python() {
    let golden = fixture("analysis.json");
    let repo = std::fs::canonicalize(
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/structure/repo"),
    )
    .expect("repo");
    let discovery = discover::discover_files(repo.to_str().expect("utf-8"));
    let call = &golden["analysis"];
    let model = ScriptedModel::new(vec![call.clone()]);
    let repository =
        analysis::analyze_repository(&model, &repo, &discovery, "acme/notes", "main", false)
            .await
            .expect("analysis");
    model.finish();
    assert_eq!(
        repository.repository_tree,
        golden["repository_tree"].as_str().expect("tree")
    );
    assert_eq!(
        repository.readme_content,
        golden["readme_content"].as_str().expect("readme")
    );

    for case in golden["classic"].as_array().expect("cases") {
        let name = case["name"].as_str().expect("name");
        let model = ScriptedModel::new(vec![serde_json::json!({
            "messages": case["messages"],
            "answer": case["answer"],
        })]);
        let result = structure::plan_wiki_structure(
            &model,
            PlannerChoice::Auto,
            &repository,
            None,
            &StructureSettings::default(),
        )
        .await;
        model.finish();
        if case["error"].as_bool().expect("error") {
            assert!(result.is_err(), "{name}: Python failed the node");
        } else {
            assert_eq!(result.expect(name).to_value(), case["structure"], "{name}");
        }
    }
}

#[tokio::test]
async fn deepagents_is_refused_and_an_empty_analysis_uses_the_summary() {
    let model = ScriptedModel::new(Vec::new());
    let repository = RepositoryAnalysis {
        repository_context: String::new(),
        repository_tree: "Total files: 1".to_owned(),
        readme_content: analysis::NO_README.to_owned(),
        summary: AnalysisSummary::new(&["a/b.py".to_owned()], analysis::NO_README),
        files: vec!["a/b.py".to_owned()],
    };
    let refused = structure::plan_wiki_structure(
        &model,
        PlannerChoice::DeepAgents,
        &repository,
        None,
        &StructureSettings::default(),
    )
    .await;
    assert_eq!(
        refused.map_err(|e| e.message),
        Err(structure::DEEPAGENTS_UNSUPPORTED.to_owned())
    );
    let messages = structure::classic_messages(&repository).expect("messages");
    let ChatMessage::User(user) = &messages[1] else {
        panic!("user message");
    };
    assert!(user.contains(
        "- Analysis: total_files=1 programming_languages=['Python'] primary_language='Python' has_readme=False"
    ));
    assert!(user.contains("key_directories=['a'] main_modules=['a.b']"));
    // `str(RepositoryAnalysis)` from Python for the same input.
    let summary = AnalysisSummary::new(
        &["x/y/z.cpp".to_owned(), "README.md".to_owned()],
        "Hello \"w\"\n\u{7}",
    );
    assert_eq!(
        summary.to_python_str(),
        "total_files=2 programming_languages=['C++'] primary_language='C++' has_readme=True \
         readme_content='Hello \"w\"\\n\\x07' total_symbols=0 file_types={'md': 1, 'cpp': 1} \
         project_complexity='simple' key_directories=['x', 'x/y'] main_modules=[] \
         documentation_files=['README.md'] configuration_files=[] test_files=[]"
    );
}
