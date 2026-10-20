//! The background embedder over a real index, with a fake embedder: off
//! by default, embeds every distinct chunk text once, keeps vectors across
//! refreshes, holds for turns and power (unless forced), drops the model
//! when idle and the vectors when turned off.

use elitea_local_embed::{EmbedError, Embedder};
use elitea_local_index::embedding::{
    AlwaysRun, Conditions, EmbedConfig, EmbedState, EmbedStatus, EmbedderSource, Embedding, SETTING,
};
use elitea_local_index::service::{IndexService, NoEvents};
use elitea_local_index::sqlite_store::VectorSet;
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

const DIM: usize = 8;

/// Vectors from the bytes of the text; counts what it embedded.
#[derive(Default)]
struct Fake {
    embedded: AtomicUsize,
    loads: AtomicUsize,
    /// Every text embedded, in order.
    texts: Mutex<Vec<String>>,
}

impl Embedder for Fake {
    fn stamp(&self) -> String {
        "local:fake".into()
    }

    fn dimension(&self) -> usize {
        DIM
    }

    fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError> {
        self.embedded.fetch_add(texts.len(), Ordering::SeqCst);
        self.texts
            .lock()
            .unwrap()
            .extend(texts.iter().map(|text| (*text).to_owned()));
        Ok(texts
            .iter()
            .map(|text| {
                let mut v = vec![0.0_f32; DIM];
                for (i, b) in text.bytes().enumerate() {
                    v[i % DIM] += f32::from(b);
                }
                elitea_local_embed::vector::normalize(&mut v);
                v
            })
            .collect())
    }

    fn embed_query(&self, text: &str) -> Result<Vec<f32>, EmbedError> {
        Ok(self.embed_documents(&[text])?.remove(0))
    }
}

struct Source(Arc<Fake>);

impl EmbedderSource for Source {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        (choice == "local:fake").then(|| VectorSet {
            origin: "local".into(),
            model: "fake".into(),
            dim: DIM,
        })
    }

    fn load(&self, _choice: &str, _workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        self.0.loads.fetch_add(1, Ordering::SeqCst);
        Ok(self.0.clone())
    }
}

struct OnBattery(AtomicBool);

impl Conditions for OnBattery {
    fn hold(&self) -> Option<&'static str> {
        self.0.load(Ordering::SeqCst).then_some("on_battery")
    }
}

fn write(root: &Path, path: &str, text: &str) {
    let path = root.join(path);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

struct Setup {
    folder: tempfile::TempDir,
    _data: tempfile::TempDir,
    service: Arc<IndexService>,
    fake: Arc<Fake>,
    statuses: Arc<Mutex<Vec<EmbedStatus>>>,
}

async fn setup() -> Setup {
    let folder = tempfile::tempdir().unwrap();
    for n in 0..12 {
        write(
            folder.path(),
            &format!("src/m{n}.py"),
            &format!("class Model{n}:\n    def run_{n}(self):\n        return {n}\n"),
        );
    }
    // Same text twice: one vector.
    write(folder.path(), "a/same.py", "def same():\n    return 1\n");
    write(folder.path(), "b/same.py", "def same():\n    return 1\n");
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    Setup {
        folder,
        _data: data,
        service,
        fake: Arc::new(Fake::default()),
        statuses: Arc::new(Mutex::new(Vec::new())),
    }
}

fn start(s: &Setup, conditions: Arc<dyn Conditions>, config: EmbedConfig) -> Embedding {
    let seen = s.statuses.clone();
    Embedding::start(
        Arc::new(Source(s.fake.clone())),
        conditions,
        config,
        Arc::new(move |_, status| seen.lock().unwrap().push(status.clone())),
    )
    .unwrap()
}

fn fast() -> EmbedConfig {
    EmbedConfig {
        batch: 4,
        idle_unload: Duration::from_millis(300),
        poll: Duration::from_millis(100),
    }
}

fn wait(embedding: &Embedding, done: impl Fn(&EmbedStatus) -> bool) -> EmbedStatus {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Some(status) = embedding.status("ws")
            && done(&status)
        {
            return status;
        }
        assert!(Instant::now() < deadline, "{:?}", embedding.status("ws"));
        std::thread::sleep(Duration::from_millis(20));
    }
}

const STAMP: &str = "local:fake:8";

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn off_by_default_then_every_distinct_text_once_and_kept_across_refreshes() {
    let s = setup().await;
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(
        s.fake.loads.load(Ordering::SeqCst),
        0,
        "off: nothing loaded"
    );
    assert!(embedding.status("ws").is_none());

    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    embedding.wake();
    let status = wait(&embedding, |st| st.state == EmbedState::Done);
    let (with, all) = s.service.store().vector_coverage(STAMP).unwrap();
    assert_eq!(with, all, "every chunk has a vector");
    assert_eq!(status.done, status.total);
    let embedded = s.fake.embedded.load(Ordering::SeqCst) as u64;
    assert_eq!(embedded, status.total, "each distinct text once");
    assert!(
        all > status.total,
        "the duplicate shares one: {all} > {}",
        status.total
    );
    assert!(
        s.statuses
            .lock()
            .unwrap()
            .iter()
            .any(|st| st.state == EmbedState::Running)
    );

    // One file changes: only its chunk is embedded again.
    write(s.folder.path(), "src/m3.py", "class Model3:\n    pass\n");
    s.service.refresh(false).await.unwrap();
    embedding.wake();
    std::thread::sleep(Duration::from_millis(100));
    let after = wait(&embedding, |st| st.state == EmbedState::Done);
    let (with, all) = s.service.store().vector_coverage(STAMP).unwrap();
    assert_eq!(with, all);
    assert_eq!(
        s.fake.embedded.load(Ordering::SeqCst) as u64,
        embedded + 1,
        "only the changed chunk"
    );
    // The old text's vector is pruned on the pass.
    let sets = s.service.store().vector_sets().unwrap();
    assert_eq!(sets.len(), 1);
    assert_eq!(sets[0].1, after.total, "the old text's vector is gone");
    assert!(
        after.total < status.total + 1,
        "{} vs {}",
        after.total,
        status.total
    );

    // Idle: the model is dropped.
    std::thread::sleep(Duration::from_millis(700));
    assert!(!embedding.model_loaded());

    // Turned off: the vectors go.
    s.service.store().set_setting(SETTING, "off").unwrap();
    embedding.wake();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !s.service.store().vector_sets().unwrap().is_empty() {
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(20));
    }
    assert_eq!(s.service.store().vector_coverage(STAMP).unwrap().0, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_turn_pauses_it_and_battery_holds_it_unless_forced() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let battery = Arc::new(OnBattery(AtomicBool::new(true)));
    let embedding = start(&s, battery.clone(), fast());
    let turn = embedding.turn();
    embedding.add(&s.service);
    let paused = wait(&embedding, |st| st.state == EmbedState::Paused);
    assert_eq!(paused.held, Some("turn"));
    assert_eq!(s.fake.embedded.load(Ordering::SeqCst), 0);
    drop(turn);
    let held = wait(&embedding, |st| st.held == Some("on_battery"));
    assert_eq!(held.state, EmbedState::Paused);
    assert_eq!(s.fake.embedded.load(Ordering::SeqCst), 0);
    embedding.force("ws");
    let done = wait(&embedding, |st| st.state == EmbedState::Done);
    assert!(done.total > 0);
    // Forced once: later work waits for power again.
    write(s.folder.path(), "src/new.py", "def new():\n    pass\n");
    s.service.refresh(false).await.unwrap();
    embedding.wake();
    wait(&embedding, |st| st.held == Some("on_battery"));
    battery.0.store(false, Ordering::SeqCst);
    embedding.wake();
    wait(&embedding, |st| {
        st.state == EmbedState::Done && st.total > done.total
    });
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_unknown_choice_keeps_no_vectors_and_a_dropped_service_is_forgotten() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    // The policy no longer allows it (the source refuses it): dropped.
    s.service.store().set_setting(SETTING, "remote").unwrap();
    embedding.wake();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !s.service.store().vector_sets().unwrap().is_empty() {
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(embedding.status("ws").is_none());
    s.service.close();
    drop(s.service);
    embedding.wake();
    std::thread::sleep(Duration::from_millis(200));
    drop(embedding); // stops and joins
}

fn request(query: &str, mode: &str) -> elitea_local_index::search::Request {
    let args = serde_json::json!({"query": query, "mode": mode});
    elitea_local_index::search::Request::from_args(args.as_object().unwrap()).unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn vectors_join_the_search_once_they_exist() {
    let s = setup().await;
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    assert!(s.service.dense().is_none(), "off: no dense side");
    let (answer, _) = s
        .service
        .search(&request("run model", "semantic"), None)
        .unwrap();
    assert!(answer.note.is_some(), "semantic without vectors says so");

    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    embedding.wake();
    wait(&embedding, |st| st.state == EmbedState::Done);
    let dense = s.service.dense().expect("vectors exist");
    let (semantic, _) = s
        .service
        .search(&request("class Model7:", "semantic"), Some(dense.as_ref()))
        .unwrap();
    assert!(!semantic.hits.is_empty());
    assert!(
        semantic
            .hits
            .iter()
            .all(|hit| hit.matched_by == ["semantic"]),
        "{:?}",
        semantic.hits
    );
    let (hybrid, _) = s
        .service
        .search(&request("Model7 run_7", "auto"), Some(dense.as_ref()))
        .unwrap();
    assert_eq!(hybrid.hits[0].path, "src/m7.py", "{:?}", hybrid.hits);
    assert!(
        hybrid
            .hits
            .iter()
            .any(|hit| hit.matched_by.contains(&"semantic"))
    );
    let (lexical, _) = s
        .service
        .search(&request("Model7 run_7", "lexical"), Some(dense.as_ref()))
        .unwrap();
    assert!(
        lexical
            .hits
            .iter()
            .all(|hit| !hit.matched_by.contains(&"semantic"))
    );
    // Turned off: no dense side any more.
    s.service.store().set_setting(SETTING, "off").unwrap();
    assert!(s.service.dense().is_none());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_files_a_turn_worked_on_are_embedded_first() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    // Asked before the pass is planned: m11 then m7 go first, in that order.
    embedding.prioritise("ws", ["src/m11.py".to_owned(), "src/m7.py".to_owned()]);
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    let texts = s.fake.texts.lock().unwrap().clone();
    let of = |n: &str| texts.iter().filter(|text| text.contains(n)).count();
    assert_eq!((of("Model11"), of("Model7")), (1, 1), "{texts:?}");
    let at = |n: &str| texts.iter().position(|text| text.contains(n)).unwrap();
    let rest = texts
        .iter()
        .position(|text| !text.contains("11") && !text.contains('7'))
        .unwrap();
    assert!(
        at("Model11") < at("Model7") && at("Model7") < rest,
        "{texts:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_restarted_pass_embeds_only_what_is_missing_and_reports_an_eta() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    {
        let embedding = start(&s, Arc::new(AlwaysRun), fast());
        embedding.add(&s.service);
        wait(&embedding, |st| st.state == EmbedState::Done);
    }
    let statuses = s.statuses.lock().unwrap().clone();
    assert!(
        statuses
            .iter()
            .any(|st| st.state == EmbedState::Running && st.done > 0 && st.eta_seconds.is_some()),
        "{statuses:?}"
    );
    assert!(
        statuses
            .iter()
            .all(|st| st.state != EmbedState::Done || st.eta_seconds.is_none())
    );
    let first = s.fake.embedded.load(Ordering::SeqCst);
    // Between runs (the app was closed) one file changed: the next start
    // embeds that file's new texts and nothing else.
    write(
        s.folder.path(),
        "src/m3.py",
        "class Changed3:\n    def run_changed(self):\n        return 33\n",
    );
    s.service.refresh(false).await.unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    let texts = s.fake.texts.lock().unwrap().clone();
    let again = &texts[first..];
    assert!(
        !again.is_empty()
            && again
                .iter()
                .all(|text| text.contains("hanged") || text.contains("33")),
        "{again:?}"
    );
}
