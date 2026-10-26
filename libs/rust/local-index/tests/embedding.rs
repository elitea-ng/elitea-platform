//! The background embedder over a real index, with a fake embedder: off
//! by default, embeds every distinct chunk text once, keeps vectors across
//! refreshes, holds for turns and power (unless forced), drops the model
//! when idle and the vectors when turned off.

use elitea_local_embed::{EmbedError, Embedder};
use elitea_local_index::embed_caps::{self, EmbedCaps};
use elitea_local_index::embedding::{
    AlwaysRun, Conditions, EmbedConfig, EmbedState, EmbedStatus, EmbedderSource, Embedding, SETTING,
};
use elitea_local_index::service::{IndexService, NoEvents};
use elitea_local_index::sqlite_store::VectorSet;
use std::collections::VecDeque;
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
    /// Run once, at the start of the next embedding call.
    hook: Mutex<Option<Box<dyn FnOnce() + Send>>>,
    /// What the next calls answer instead of vectors, one per call.
    script: Mutex<VecDeque<EmbedError>>,
    /// Calls made, failed ones included.
    calls: AtomicUsize,
}

/// The vector the fake makes from `text`.
fn fake_vector(text: &str) -> Vec<f32> {
    let mut v = vec![0.0_f32; DIM];
    for (i, b) in text.bytes().enumerate() {
        v[i % DIM] += f32::from(b);
    }
    elitea_local_embed::vector::normalize(&mut v);
    v
}

impl Embedder for Fake {
    fn stamp(&self) -> String {
        "local:fake".into()
    }

    fn dimension(&self) -> usize {
        DIM
    }

    fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError> {
        let hook = self.hook.lock().unwrap().take();
        if let Some(hook) = hook {
            hook();
        }
        self.calls.fetch_add(1, Ordering::SeqCst);
        if let Some(error) = self.script.lock().unwrap().pop_front() {
            return Err(error);
        }
        self.embedded.fetch_add(texts.len(), Ordering::SeqCst);
        self.texts
            .lock()
            .unwrap()
            .extend(texts.iter().map(|text| (*text).to_owned()));
        Ok(texts.iter().map(|text| fake_vector(text)).collect())
    }

    fn embed_query(&self, text: &str) -> Result<Vec<f32>, EmbedError> {
        Ok(self.embed_documents(&[text])?.remove(0))
    }
}

struct Source(Arc<Fake>);

impl EmbedderSource for Source {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        // `remote:fake` stands for the deployment's route: paid per token.
        let origin = match choice {
            "local:fake" => "local",
            "remote:fake" => "remote",
            _ => return None,
        };
        Some(VectorSet {
            origin: origin.into(),
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
    assert_ne!(semantic.hits, []);
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

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_chunk_id_reused_mid_pass_never_gets_another_texts_vector() {
    let s = setup().await;
    // The last file written holds the highest chunk ids.
    write(
        s.folder.path(),
        "z/x.py",
        "def original_x():\n    return 'old text'\n",
    );
    s.service.refresh(false).await.unwrap();
    let store = s.service.store();
    let max_of = |path: &str| store.chunks_of(path).unwrap().iter().map(|c| c.id).max();
    let x_ids: Vec<i64> = store
        .chunks_of("z/x.py")
        .unwrap()
        .iter()
        .map(|c| c.id)
        .collect();
    let others = (0..12)
        .filter_map(|n| max_of(&format!("src/m{n}.py")))
        .max()
        .unwrap();
    assert!(
        x_ids.iter().all(|id| *id > others),
        "{x_ids:?} after {others}"
    );
    store.set_setting(SETTING, "local:fake").unwrap();
    // While the first batch embeds (the pass planned already), a build
    // rewrites z/x.py: its first new chunk (another text) takes the freed
    // id, and its old text lives on in the chunk after it.
    let (folder, service) = (s.folder.path().to_owned(), s.service.clone());
    let runtime = tokio::runtime::Handle::current();
    *s.fake.hook.lock().unwrap() = Some(Box::new(move || {
        std::fs::write(
            folder.join("z/x.py"),
            "def rewritten_x():\n    return 'new text!'\n\n\ndef original_x():\n    return 'old text'\n",
        )
        .unwrap();
        runtime.block_on(service.refresh(false)).unwrap();
    }));
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    std::thread::sleep(Duration::from_millis(50));
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let (with, all) = store.vector_coverage(STAMP).unwrap();
        if with == all
            && embedding
                .status("ws")
                .is_some_and(|st| st.state == EmbedState::Done)
        {
            break;
        }
        assert!(Instant::now() < deadline, "{with}/{all}");
        std::thread::sleep(Duration::from_millis(20));
    }
    let now = store.chunks_of("z/x.py").unwrap();
    assert!(
        now.iter()
            .any(|c| x_ids.contains(&c.id) && c.text.contains("rewritten_x")),
        "an old id now holds another text: {now:?} {x_ids:?}"
    );
    assert!(now.iter().any(|c| c.text.contains("original_x")), "{now:?}");
    // Every chunk's vector is the one its own text makes.
    let vectors: std::collections::HashMap<i64, Vec<u8>> =
        store.chunk_vectors(STAMP).unwrap().into_iter().collect();
    for path in ["z/x.py", "src/m0.py"] {
        for chunk in store.chunks_of(path).unwrap() {
            assert_eq!(
                vectors[&chunk.id],
                elitea_local_embed::vector::quantize(&fake_vector(&chunk.text)),
                "{path}: {:?}",
                chunk.text
            );
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_build_reloads_the_vector_matrix_at_once() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    let before = s.service.dense().expect("vectors exist");
    let gone: Vec<i64> = s
        .service
        .store()
        .chunks_of("src/m11.py")
        .unwrap()
        .iter()
        .map(|c| c.id)
        .collect();
    let near = |dense: &dyn elitea_local_index::search::Dense| -> Vec<i64> {
        dense
            .nearest("class Model11:", 100, None)
            .unwrap()
            .into_iter()
            .map(|(id, _)| id)
            .collect()
    };
    assert!(near(before.as_ref()).iter().any(|id| gone.contains(id)));
    // A build drops m11's chunks: well within the 30 s a matrix is reused
    // while vectors are added, the next search loads the chunks now.
    std::fs::remove_file(s.folder.path().join("src/m11.py")).unwrap();
    s.service.refresh(false).await.unwrap();
    let after = s.service.dense().expect("vectors exist");
    let ids = near(after.as_ref());
    assert_ne!(ids, [] as [i64; 0]);
    assert!(ids.iter().all(|id| !gone.contains(id)), "{ids:?} {gone:?}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_done_workspace_is_not_scanned_again_until_something_changes() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    // Changed behind the embedder's back: several polls go by and the
    // workspace (done) is not looked at, so its status stays.
    s.service.store().set_setting(SETTING, "off").unwrap();
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        embedding.status("ws").map(|st| st.state),
        Some(EmbedState::Done)
    );
    let (kept, _) = s.service.store().vector_coverage(STAMP).unwrap();
    assert!(kept > 0, "not pruned without a look");
    // Told (a setting change): looked at, turned off, vectors dropped.
    embedding.wake_workspace("ws");
    let deadline = Instant::now() + Duration::from_secs(5);
    while embedding.status("ws").is_some() {
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(20));
    }
    assert_eq!(s.service.store().vector_coverage(STAMP).unwrap().0, 0);
}

/// A workspace on the metered embedder with `caps`, started and woken.
fn metered(s: &Setup, caps: EmbedCaps) -> Embedding {
    s.service
        .store()
        .set_setting(SETTING, "remote:fake")
        .unwrap();
    caps.save(s.service.store()).unwrap();
    let embedding = start(s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    embedding
}

fn big() -> u64 {
    10_000_000
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_estimate_is_what_a_run_counts() {
    let s = setup().await;
    let store = s.service.store();
    let (texts, tokens) = store.missing_text_stats(None).unwrap();
    let missing = store.chunks_missing_vectors("none").unwrap();
    let ids: Vec<i64> = missing.iter().map(|(id, _)| *id).collect();
    let by_id = store.chunks_by_id(&ids).unwrap();
    assert_eq!(texts, missing.len() as u64);
    assert_eq!(
        tokens,
        by_id
            .iter()
            .map(|chunk| embed_caps::estimate_tokens(&chunk.text))
            .sum::<u64>()
    );
    assert!(tokens > texts, "more than a token a text");
    // With a stamp, only what has no vector.
    assert_eq!(
        store.missing_text_stats(Some("none")).unwrap(),
        (texts, tokens)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_run_cap_stops_a_metered_run_cleanly_and_a_later_run_continues() {
    let s = setup().await;
    let embedding = metered(
        &s,
        EmbedCaps {
            run: 40,
            day: big(),
        },
    );
    let capped = wait(&embedding, |st| st.state == EmbedState::Capped);
    assert_eq!(capped.cap, Some("run"));
    assert!(capped.tokens > 0 && capped.tokens <= 40, "{capped:?}");
    assert!(capped.done > 0 && capped.done < capped.total);
    let first = s.service.store().embedding_runs().unwrap();
    assert_eq!(first.len(), 1);
    assert_eq!(first[0].status, "capped");
    assert_eq!(first[0].tokens, capped.tokens);
    assert_eq!(
        s.service.store().embed_tokens_today().unwrap(),
        capped.tokens
    );
    // The run cap holds until the person continues: not the clock, not a
    // build's wake, not the sweep.
    let calls = s.fake.calls.load(Ordering::SeqCst);
    embedding.wake_workspace("ws");
    embedding.wake();
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        s.fake.calls.load(Ordering::SeqCst),
        calls,
        "a run cap restarted on its own"
    );
    assert_eq!(s.service.store().embedding_runs().unwrap().len(), 1);
    // The person raises the cap: the rest is embedded, and what was
    // embedded is not embedded twice.
    EmbedCaps {
        run: big(),
        day: big(),
    }
    .save(s.service.store())
    .unwrap();
    embedding.caps_changed("ws");
    let done = wait(&embedding, |st| st.state == EmbedState::Done);
    assert_eq!(s.fake.embedded.load(Ordering::SeqCst) as u64, done.total);
    let runs = s.service.store().embedding_runs().unwrap();
    assert_eq!(runs.last().unwrap().status, "embedded");
    assert_eq!(runs.iter().map(|run| run.tokens).sum::<u64>(), {
        let (_, all) = s.service.store().missing_text_stats(None).unwrap();
        all
    });
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_day_cap_holds_every_run_of_the_day_even_a_forced_one() {
    let s = setup().await;
    let embedding = metered(
        &s,
        EmbedCaps {
            run: big(),
            day: 40,
        },
    );
    let capped = wait(&embedding, |st| st.state == EmbedState::Capped);
    assert_eq!(capped.cap, Some("day"));
    let embedded = s.fake.embedded.load(Ordering::SeqCst);
    // "Embed now" does not get around the day: no new run, no new call.
    embedding.force("ws");
    std::thread::sleep(Duration::from_millis(400));
    assert_eq!(s.fake.embedded.load(Ordering::SeqCst), embedded);
    assert_eq!(s.service.store().embedding_runs().unwrap().len(), 1);
    assert_eq!(embedding.status("ws").unwrap().state, EmbedState::Capped);
    // Raising the cap lets it go on.
    EmbedCaps {
        run: big(),
        day: big(),
    }
    .save(s.service.store())
    .unwrap();
    embedding.force("ws");
    wait(&embedding, |st| st.state == EmbedState::Done);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_402_stops_the_run_and_nothing_is_sent_until_the_person_asks_again() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "remote:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    // The first batch goes through; the hook of the second call queues the
    // refusal it then meets.
    let fake = s.fake.clone();
    *s.fake.hook.lock().unwrap() = Some(Box::new(move || {
        let queue = fake.clone();
        *fake.hook.lock().unwrap() = Some(Box::new(move || {
            queue
                .script
                .lock()
                .unwrap()
                .push_back(EmbedError::BudgetExhausted(
                    "model_gateway.project_budget_exhausted (HTTP 402)".into(),
                ));
        }));
    }));
    embedding.add(&s.service);
    let stopped = wait(&embedding, |st| st.state == EmbedState::BudgetExhausted);
    assert!(stopped.error.as_deref().unwrap().contains("402"));
    assert!(
        stopped.done > 0 && stopped.done < stopped.total,
        "{stopped:?}"
    );
    let runs = s.service.store().embedding_runs().unwrap();
    assert_eq!(runs.len(), 1);
    assert_eq!(runs[0].status, "budget_exhausted");
    assert!(runs[0].tokens > 0);
    // Not retried by the clock, by a build, a cap change, or the sweep.
    let calls = s.fake.calls.load(Ordering::SeqCst);
    embedding.wake();
    embedding.wake_workspace("ws");
    embedding.caps_changed("ws");
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(s.fake.calls.load(Ordering::SeqCst), calls, "retried a 402");
    assert_eq!(
        embedding.status("ws").unwrap().state,
        EmbedState::BudgetExhausted
    );
    // "Embed now" tries again, and the budget is back.
    embedding.force("ws");
    wait(&embedding, |st| st.state == EmbedState::Done);
}

/// The fake answers 402 at its second call (the first batch goes through).
fn refuse_the_second_call(fake: &Arc<Fake>) {
    let outer = fake.clone();
    *fake.hook.lock().unwrap() = Some(Box::new(move || {
        let queue = outer.clone();
        *outer.hook.lock().unwrap() = Some(Box::new(move || {
            queue
                .script
                .lock()
                .unwrap()
                .push_back(EmbedError::BudgetExhausted("HTTP 402".into()));
        }));
    }));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_402_ends_a_forced_run_too_and_it_is_not_sent_again() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "remote:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    refuse_the_second_call(&s.fake);
    // "Embed now" before the run: the run is forced when the 402 comes.
    embedding.force("ws");
    embedding.add(&s.service);
    let stopped = wait(&embedding, |st| st.state == EmbedState::BudgetExhausted);
    assert!(!stopped.forced);
    let calls = s.fake.calls.load(Ordering::SeqCst);
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        s.fake.calls.load(Ordering::SeqCst),
        calls,
        "a forced run looped on 402"
    );
    assert_eq!(s.service.store().embedding_runs().unwrap().len(), 1);
    assert_eq!(
        embedding.status("ws").unwrap().state,
        EmbedState::BudgetExhausted
    );
    // Choosing the embedder anew asks again.
    embedding.ask_again("ws");
    wait(&embedding, |st| st.state == EmbedState::Done);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_build_does_not_cut_a_retry_after_short() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "remote:fake")
        .unwrap();
    s.fake
        .script
        .lock()
        .unwrap()
        .push_back(EmbedError::RateLimited {
            retry_after: Duration::from_secs(60),
        });
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Error);
    let calls = s.fake.calls.load(Ordering::SeqCst);
    embedding.wake_workspace("ws");
    embedding.wake();
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        s.fake.calls.load(Ordering::SeqCst),
        calls,
        "the Retry-After was cut short"
    );
    // The person's "Embed now" does.
    embedding.force("ws");
    wait(&embedding, |st| st.state == EmbedState::Done);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_text_larger_than_the_cap_is_skipped_and_holds_nothing_back() {
    let s = setup().await;
    // One long line: the chunker cuts it at 1 024 characters (256 tokens),
    // one piece larger than the cap below, the rest smaller.
    let long = "x".repeat(1400);
    write(
        s.folder.path(),
        "src/long.py",
        &format!("BLOB = \"{long}\"\n"),
    );
    s.service.refresh(false).await.unwrap();
    let over = s.service.store().chunk_hashes_over(200).unwrap();
    assert_eq!(over.len(), 1, "one piece is over the cap");
    let embedding = metered(
        &s,
        EmbedCaps {
            run: 200,
            day: big(),
        },
    );
    // The first run stops at its cap with progress; the next one (the
    // person's) embeds the rest, past the piece no run could hold.
    wait(&embedding, |st| st.state == EmbedState::Capped);
    embedding.force("ws");
    let done = wait(&embedding, |st| st.state == EmbedState::Done);
    assert_eq!(done.total, done.done + 1, "{done:?}");
    let piece = "x".repeat(1000);
    assert!(
        s.fake
            .texts
            .lock()
            .unwrap()
            .iter()
            .all(|text| !text.contains(&piece)),
        "the oversized text was sent"
    );
    assert_eq!(s.fake.embedded.load(Ordering::SeqCst) as u64, done.done);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_rate_limit_waits_the_retry_after_and_then_goes_on() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "remote:fake")
        .unwrap();
    s.fake
        .script
        .lock()
        .unwrap()
        .push_back(EmbedError::RateLimited {
            retry_after: Duration::from_millis(600),
        });
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    let started = Instant::now();
    embedding.add(&s.service);
    let limited = wait(&embedding, |st| st.state == EmbedState::Error);
    assert!(limited.error.unwrap().contains("rate limited"));
    wait(&embedding, |st| st.state == EmbedState::Done);
    assert!(
        started.elapsed() >= Duration::from_millis(600),
        "waited the Retry-After, not the shorter poll"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_on_device_run_is_recorded_but_never_counts_against_the_day() {
    let s = setup().await;
    s.service
        .store()
        .set_setting(SETTING, "local:fake")
        .unwrap();
    let embedding = start(&s, Arc::new(AlwaysRun), fast());
    embedding.add(&s.service);
    wait(&embedding, |st| st.state == EmbedState::Done);
    let runs = s.service.store().embedding_runs().unwrap();
    assert!(
        runs.iter()
            .all(|run| run.status == "embedded" && run.tokens > 0)
    );
    assert_eq!(s.service.store().embed_tokens_today().unwrap(), 0);
}
