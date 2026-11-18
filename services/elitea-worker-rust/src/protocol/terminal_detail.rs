//! The failed program's output a Code node failure ends its run with (#2227).
//!
//! A pipeline Code node whose program failed ends the run with the registered
//! `PipelineCodeFailed` sentence. The tail of the program's stderr (a compile
//! error, an exception, a panic) rides after that sentence, so the pipeline
//! author sees why. The failure travels as a `Copy` kind through the run's
//! terminal machinery, so the text waits here, keyed by the run (execution id
//! and generation), from the moment the node knows it until the terminal frame
//! is built. It is bounded twice: per text, and in how many runs wait at once.
//! The slot is in memory, so a restart between the node failure and the
//! terminal would lose it. The node that fails also keeps the text durably
//! (`DispatchJournal::record_failure_detail`, #2234), and the terminal path
//! calls [`hydrate`] before it builds the frame: an empty slot is refilled from
//! that copy. Either way the detail is a courtesy, never a reason to fail a run
//! differently: whatever cannot be read leaves the registered sentence alone.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex, OnceLock};

use super::elitea::runtime::v1::ExecutionIdentityV1;

/// What follows the registered sentence, before the program's output. Main's
/// `CodeFailureDetailSeparator` is the same text: Main admits nothing else
/// after the sentence.
pub const SEPARATOR: &str = "\n\nProgram output:\n";

/// The most characters of program output a failure carries. Main's
/// `MaxCodeFailureDetailRunes`, the sandbox client's bound and the model-facing
/// tool's bound are the same number.
pub const MAX_DETAIL_CHARS: usize = 1_500;

/// How many runs may wait for their terminal at once; the oldest is dropped.
const MAX_WAITING_RUNS: usize = 256;

type Waiting = VecDeque<((String, u64), String)>;

fn registry() -> &'static Mutex<Waiting> {
    static REGISTRY: OnceLock<Mutex<Waiting>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(VecDeque::new()))
}

/// Where a run's failure output is kept across a worker restart. The worker's
/// dispatch journal is the production one.
#[async_trait::async_trait]
pub(crate) trait DurableDetail: Send + Sync {
    /// The output kept for this run, or `None` (nothing kept, or unreadable).
    async fn recall(&self, identity: &ExecutionIdentityV1) -> Option<String>;
}

fn durable() -> &'static OnceLock<Arc<dyn DurableDetail>> {
    static DURABLE: OnceLock<Arc<dyn DurableDetail>> = OnceLock::new();
    &DURABLE
}

/// Name the durable copy [`hydrate`] reads. The first one installed stays: a
/// worker has one agent-state database.
pub(crate) fn install_durable(source: Arc<dyn DurableDetail>) {
    let _already_installed = durable().set(source);
}

/// Refill this run's slot from its durable copy when the slot is empty (a
/// restart lost it). A slot that holds output is never replaced.
pub(crate) async fn hydrate(identity: &ExecutionIdentityV1) {
    if let Some(source) = durable().get() {
        hydrate_from(source.as_ref(), identity).await;
    }
}

async fn hydrate_from(source: &dyn DurableDetail, identity: &ExecutionIdentityV1) {
    if noted(&identity.execution_id, identity.generation).is_some() {
        return;
    }
    if let Some(detail) = source.recall(identity).await {
        note(&identity.execution_id, identity.generation, Some(&detail));
    }
}

/// The program's output as bounded text, whatever it was: control characters
/// other than a newline or a tab removed, the last [`MAX_DETAIL_CHARS`]
/// characters kept, and `None` when nothing is left.
#[must_use]
pub fn bounded(raw: &str) -> Option<String> {
    let clean: String = raw
        .chars()
        .filter(|c| !c.is_control() || matches!(c, '\n' | '\t'))
        .collect();
    let clean = clean.trim();
    let count = clean.chars().count();
    if count == 0 {
        return None;
    }
    Some(
        clean
            .chars()
            .skip(count.saturating_sub(MAX_DETAIL_CHARS))
            .collect(),
    )
}

/// Remember `detail` for this run's terminal, replacing what was remembered.
/// `None` (or blank output) leaves what was remembered alone: the slot is the
/// run's, not a node's, so one node failing without output must not wipe the
/// output a node running beside it left.
pub fn note(execution_id: &str, generation: u64, detail: Option<&str>) {
    if let Ok(mut waiting) = registry().lock() {
        remember(
            &mut waiting,
            MAX_WAITING_RUNS,
            execution_id,
            generation,
            detail,
        );
    }
}

fn remember(
    waiting: &mut Waiting,
    most: usize,
    execution_id: &str,
    generation: u64,
    detail: Option<&str>,
) {
    let Some(detail) = detail.and_then(bounded) else {
        return;
    };
    waiting.retain(|(key, _)| key.0 != execution_id || key.1 != generation);
    while waiting.len() >= most {
        waiting.pop_front();
    }
    waiting.push_back(((execution_id.to_owned(), generation), detail));
}

/// What this run's failed Code node left, if anything.
#[must_use]
pub fn noted(execution_id: &str, generation: u64) -> Option<String> {
    let waiting = registry().lock().ok()?;
    waiting
        .iter()
        .find(|(key, _)| key.0 == execution_id && key.1 == generation)
        .map(|(_, detail)| detail.clone())
}

/// The registered sentence with the program's output after it.
#[must_use]
pub fn with_detail(sentence: &str, detail: &str) -> String {
    format!("{sentence}{SEPARATOR}{detail}")
}

/// The program's output when `message` is `sentence` followed by it, as Main
/// admits it: the exact separator, then non-empty bounded text.
#[must_use]
pub fn detail_of<'a>(message: &'a str, sentence: &str) -> Option<&'a str> {
    let detail = message.strip_prefix(sentence)?.strip_prefix(SEPARATOR)?;
    (!detail.trim().is_empty()
        && detail.chars().count() <= MAX_DETAIL_CHARS
        && !detail
            .chars()
            .any(|c| c.is_control() && !matches!(c, '\n' | '\t')))
    .then_some(detail)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn output_is_cleaned_and_keeps_its_tail() {
        assert_eq!(
            bounded("  \u{1b}[31mError: boom\u{0}\n").as_deref(),
            Some("[31mError: boom")
        );
        assert_eq!(bounded(" \n\t "), None);
        let long = format!("{}THE-ERROR", "x".repeat(5_000));
        let kept = bounded(&long).unwrap();
        assert_eq!(kept.chars().count(), MAX_DETAIL_CHARS);
        assert!(kept.ends_with("THE-ERROR"));
    }

    #[test]
    fn a_run_remembers_its_own_output_and_a_later_note_replaces_it() {
        note("run-a", 1, Some("first"));
        note("run-b", 1, Some("other run"));
        assert_eq!(noted("run-a", 1).as_deref(), Some("first"));
        assert_eq!(noted("run-a", 2), None, "another generation is another run");
        note("run-a", 1, Some("second"));
        assert_eq!(noted("run-a", 1).as_deref(), Some("second"));
        // A failure without output (or with blank output) leaves the run's
        // remembered output alone: the slot is the run's, not a node's.
        note("run-a", 1, None);
        note("run-a", 1, Some(" \n"));
        assert_eq!(noted("run-a", 1).as_deref(), Some("second"));
        assert_eq!(noted("run-b", 1).as_deref(), Some("other run"));
        assert_eq!(noted("run-c", 1), None, "no output was ever noted");
    }

    struct Kept(Option<&'static str>);
    #[async_trait::async_trait]
    impl DurableDetail for Kept {
        async fn recall(&self, _: &ExecutionIdentityV1) -> Option<String> {
            self.0.map(str::to_owned)
        }
    }
    fn identity(execution_id: &str, generation: u64) -> ExecutionIdentityV1 {
        ExecutionIdentityV1 {
            execution_id: execution_id.to_owned(),
            generation,
            ..ExecutionIdentityV1::default()
        }
    }

    /// #2234: a restart empties the slot; the durable copy refills it, and a
    /// slot that holds output is never replaced by an older copy.
    #[tokio::test]
    async fn an_empty_slot_is_refilled_from_the_durable_copy_and_a_full_one_is_not() {
        hydrate_from(&Kept(None), &identity("hydrate-none", 1)).await;
        assert_eq!(noted("hydrate-none", 1), None, "nothing was kept");
        hydrate_from(
            &Kept(Some("kept before the restart")),
            &identity("hydrate-a", 1),
        )
        .await;
        assert_eq!(
            noted("hydrate-a", 1).as_deref(),
            Some("kept before the restart")
        );
        assert_eq!(
            noted("hydrate-a", 2),
            None,
            "another generation is another run"
        );
        note("hydrate-b", 1, Some("in memory"));
        hydrate_from(&Kept(Some("older copy")), &identity("hydrate-b", 1)).await;
        assert_eq!(noted("hydrate-b", 1).as_deref(), Some("in memory"));
    }

    #[test]
    fn the_oldest_waiting_run_is_dropped_at_the_bound() {
        let mut waiting = Waiting::new();
        for generation in 1..=5 {
            remember(&mut waiting, 3, "run", generation, Some("detail"));
        }
        let kept: Vec<u64> = waiting.iter().map(|(key, _)| key.1).collect();
        assert_eq!(kept, [3, 4, 5]);
    }

    #[test]
    fn only_the_exact_sentence_and_separator_carry_output() {
        let sentence = "The pipeline stopped at a Code node.";
        let message = with_detail(sentence, "Error: boom");
        assert_eq!(detail_of(&message, sentence), Some("Error: boom"));
        assert_eq!(detail_of(sentence, sentence), None);
        assert_eq!(
            detail_of(&format!("{sentence} Error: boom"), sentence),
            None
        );
        assert_eq!(detail_of(&with_detail(sentence, " \n"), sentence), None);
        assert_eq!(
            detail_of(&with_detail(sentence, "bad\u{1b}"), sentence),
            None
        );
        assert_eq!(
            detail_of(
                &with_detail(sentence, &"x".repeat(MAX_DETAIL_CHARS + 1)),
                sentence
            ),
            None
        );
        assert_eq!(detail_of(&message, "another sentence"), None);
    }
}
