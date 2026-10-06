//! The page gate without Python: `tests/fixtures/wiki/golden` is a small
//! repository (Python, C++, a Pylon API, Markdown, YAML, a file only on
//! disk) and the Python engine's run of the page phase over it
//! (`parity/python_pages_dump.py --mermaid-pages`, the structure planner's
//! output plus three hand-written pages: one over the split cap, one
//! documentation page whose targets are only on disk or outside the
//! checkout, one with no targets at all).
//!
//! The Rust page phase runs over the same index rows, against the
//! in-process stub model, and must send byte-identical prompts and
//! produce byte-identical pages and artifacts (time- and uuid-derived
//! names normalised).

use elitea_deepwiki_engine::errors::EngineError;
use elitea_deepwiki_engine::runner::StopSignal;
use elitea_deepwiki_engine::wiki::context::PylonPlugin;
use elitea_deepwiki_engine::wiki::expansion::ExpansionFlags;
use elitea_deepwiki_engine::wiki::pages::{PageGenerator, PageSettings, generate_pages};
use elitea_deepwiki_engine::wiki::parity::{self, StubModel};
use elitea_deepwiki_engine::wiki::retrieve::{CONTEXT_TOKEN_BUDGET, RetrievalContext};
use elitea_deepwiki_engine::wiki::search::{PageSearch, ReplaySearch};
use serde_json::{Map, Value};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::thread::ThreadId;

fn golden() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/wiki/golden/ref")
}

fn jsonl(name: &str) -> Vec<Value> {
    std::fs::read_to_string(golden().join(name))
        .unwrap()
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| serde_json::from_str(l).unwrap())
        .collect()
}

fn json(name: &str) -> Value {
    serde_json::from_str(&std::fs::read_to_string(golden().join(name)).unwrap()).unwrap()
}

fn page_of(request: &Value) -> String {
    let prompt = request["messages"].as_array().unwrap().last().unwrap()["content"]
        .as_str()
        .unwrap();
    prompt
        .lines()
        .find_map(|l| l.strip_prefix("- Page: "))
        .unwrap()
        .to_owned()
}

/// Replace the time- and uuid-derived parts of a name or text.
fn normalise(text: &str, version: &str, stamp: &str) -> String {
    text.replace(version, "VERSION").replace(stamp, "STAMP")
}

fn stamp_of(name: &str) -> String {
    name.split("wiki_structure_")
        .nth(1)
        .unwrap()
        .trim_end_matches(".json")
        .to_owned()
}

/// The artifacts by normalised name: the manifest's time- and uuid-derived
/// fields nulled, its page list sorted (Python listed files in directory
/// order, a recorded deliberate difference).
fn normalised_artifacts(result: &Value) -> Map<String, Value> {
    let version = result["wiki_version_id"].as_str().unwrap().to_owned();
    let structure = result["artifacts"]
        .as_array()
        .unwrap()
        .iter()
        .find(|a| a["name"].as_str().unwrap().contains("wiki_structure_"))
        .unwrap()["name"]
        .as_str()
        .unwrap()
        .to_owned();
    let stamp = stamp_of(&structure);
    result["artifacts"]
        .as_array()
        .unwrap()
        .iter()
        .map(|a| {
            let name = normalise(a["name"].as_str().unwrap(), &version, &stamp);
            let mut data = a["data"].as_str().unwrap().to_owned();
            if name.contains("wiki_manifest_") {
                let mut manifest: Map<String, Value> = serde_json::from_str(&data).unwrap();
                for key in [
                    "created_at",
                    "analysis_cache_key",
                    "wiki_version_id",
                    "analysis_key",
                ] {
                    manifest.insert(key.into(), Value::Null);
                }
                let mut pages: Vec<String> =
                    serde_json::from_value(manifest["pages"].clone()).unwrap();
                pages.sort();
                manifest.insert("pages".into(), serde_json::json!(pages));
                data = Value::Object(manifest).to_string();
            }
            (name, Value::String(data))
        })
        .collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_rust_page_phase_equals_the_python_reference() {
    let dump = parity::load_dump(&golden()).unwrap();
    let model = StubModel::start(dump.page_answer).await.unwrap();
    let (outcome, _) = parity::run_dump(dump, &model).await.unwrap();

    // Prompts, paired by page (four drafts run at a time).
    let mut python = jsonl("requests.jsonl");
    let mut rust = model.requests();
    python.sort_by_key(page_of);
    rust.sort_by_key(page_of);
    assert_eq!(python.len(), rust.len());
    for (want, got) in python.iter().zip(&rust) {
        assert_eq!(
            want["messages"],
            got["messages"],
            "prompt of {}",
            page_of(want)
        );
        assert_eq!(want["model"], got["model"]);
        assert_eq!(want["temperature"], got["temperature"]);
        assert_eq!(want["stream"], got["stream"]);
        assert_eq!(want["max_completion_tokens"], got["max_completion_tokens"]);
    }

    // Pages and the structure after the split.
    let pages = json("pages.json");
    assert_eq!(
        pages["structure_after_split"],
        outcome.structure.to_value().unwrap()
    );
    let got_pages: Vec<Value> = outcome
        .pages
        .pages
        .iter()
        .map(|p| serde_json::json!({"page_id": p.page_id, "title": p.title, "content": p.content, "status": p.status.as_str()}))
        .collect();
    assert_eq!(pages["pages"], Value::Array(got_pages));
    assert!(outcome.pages.errors.is_empty());

    // Artifacts.
    let python_result = json("result.json");
    let rust_result = Value::Object(outcome.composed.result.clone());
    let want = normalised_artifacts(&python_result);
    let got = normalised_artifacts(&rust_result);
    assert_eq!(want.keys().collect::<Vec<_>>().len(), got.len());
    for (name, data) in &want {
        assert_eq!(Some(data), got.get(name), "artifact {name}");
    }

    // Top-level fields (the timing in the message normalised).
    for key in [
        "success",
        "errors",
        "failed_pages",
        "repository_context",
        "commit_hash",
        "branch",
        "provider_type",
        "wiki_id",
        "wiki_title",
        "wiki_description",
    ] {
        assert_eq!(python_result[key], rust_result[key], "{key}");
    }
    let keys = |v: &Value| v.as_object().unwrap().keys().cloned().collect::<Vec<_>>();
    assert_eq!(keys(&python_result), keys(&rust_result));
    let message = |v: &Value| {
        let text = v["result"].as_str().unwrap();
        let at = text.find("Execution Time: ").unwrap();
        let end = text[at..].find('s').unwrap() + at;
        format!("{}{}", &text[..at], &text[end..])
    };
    assert_eq!(message(&python_result), message(&rust_result));
}

/// A `PageSearch` that records the thread of every call.
struct ThreadProbe {
    inner: ReplaySearch,
    threads: Mutex<Vec<ThreadId>>,
}

impl PageSearch for ThreadProbe {
    fn search(
        &self,
        term: &str,
        cluster_id: Option<i64>,
        limit: usize,
    ) -> Result<Vec<String>, EngineError> {
        self.threads
            .lock()
            .unwrap()
            .push(std::thread::current().id());
        self.inner.search(term, cluster_id, limit)
    }

    fn symbol(&self, name: &str, macro_id: Option<i64>) -> Result<Vec<String>, EngineError> {
        self.threads
            .lock()
            .unwrap()
            .push(std::thread::current().id());
        self.inner.symbol(name, macro_id)
    }
}

async fn probe_generator(
    stop: StopSignal,
) -> (Arc<PageGenerator<ThreadProbe>>, parity::Dump, StubModel) {
    let dump = parity::load_dump(&golden()).unwrap();
    let model = StubModel::start(dump.page_answer).await.unwrap();
    let generator = Arc::new(PageGenerator {
        retrieval: RetrievalContext {
            index: Arc::new(dump.index.clone()),
            search: Arc::new(ThreadProbe {
                inner: dump.search.clone(),
                threads: Mutex::new(Vec::new()),
            }),
            repo_root: dump.repo_root.clone(),
            flags: ExpansionFlags::from_env().unwrap(),
            budget: CONTEXT_TOKEN_BUDGET,
            pylon: PylonPlugin::new(dump.repo_root.clone()),
        },
        chat: parity::stub_chat(&model.base_url).unwrap(),
        settings: PageSettings::new(dump.identity.repository.clone()),
        stop,
        thinking: None,
    });
    (generator, dump, model)
}

/// Retrieval is CPU work: on a single-threaded runtime it must run off the
/// runtime's thread (the blocking pool), or four pages in flight would
/// stall the socket and every model stream.
#[tokio::test(flavor = "current_thread")]
async fn page_retrieval_runs_off_the_runtime_thread() {
    let (generator, dump, _model) = probe_generator(StopSignal::default()).await;
    let runtime_thread = std::thread::current().id();
    let pages = generate_pages(Arc::clone(&generator), &dump.structure, &dump.repo_context)
        .await
        .unwrap();
    assert!(!pages.pages.is_empty());
    let threads = generator.retrieval.search.threads.lock().unwrap().clone();
    assert!(!threads.is_empty(), "the golden pages search");
    assert!(threads.iter().all(|t| *t != runtime_thread));
}

/// A stop before the fan-out starts no page and sends no model request.
#[tokio::test(flavor = "current_thread")]
async fn a_stop_starts_no_page() {
    let stop = StopSignal::default();
    stop.request();
    let (generator, dump, model) = probe_generator(stop).await;
    let result = generate_pages(generator, &dump.structure, &dump.repo_context).await;
    assert_eq!(
        result.map_err(|e| e.message),
        Err("Invocation cancelled".to_owned())
    );
    assert!(model.requests().is_empty());
}
