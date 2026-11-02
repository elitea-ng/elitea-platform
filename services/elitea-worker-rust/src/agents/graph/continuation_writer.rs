//! FU-265 / FU-269: the paused execution a pipeline continuation re-enters.
//!
//! A continuation (a decided pause, a text answer, an MCP authorization or a
//! static tool selection) re-enters the root checkpoint row its paused
//! execution wrote. Main reads that writer in the admission transaction that
//! authorized the decision, stores it on the admitted job and serves it on the
//! claim receipt (`ClaimReceiptV1.paused_writer`), never in the command. The
//! Worker accepts that row only for that continuation and only for an exact
//! writer match: on the first attempt and on a reclaim, where it lets the
//! continuation re-run from the paused row.

use serde_json::Value;

use crate::protocol::elitea::runtime::v1::PausedWriterV1;

/// The execution and generation that wrote the paused root row.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PausedWriter {
    execution_id: String,
    generation: u64,
}

/// A claim field Main must never have sent.
#[derive(Debug, Eq, PartialEq)]
pub(crate) struct PausedWriterError;

impl PausedWriter {
    /// The claim's paused writer: `None` when Main named none (an older Main,
    /// or a continuation without one settled pause), an error when it is
    /// malformed or names the claimed execution itself.
    pub(crate) fn from_claim(
        field: Option<&PausedWriterV1>,
        own_execution_id: &str,
    ) -> Result<Option<Self>, PausedWriterError> {
        let Some(field) = field else {
            return Ok(None);
        };
        if field.generation == 0
            || !valid_execution_id(&field.execution_id)
            || field.execution_id == own_execution_id
        {
            return Err(PausedWriterError);
        }
        Ok(Some(Self {
            execution_id: field.execution_id.clone(),
            generation: field.generation,
        }))
    }

    /// Whether `writer` (a root checkpoint's `elitea.pipeline.execution.v1`
    /// metadata) is exactly this execution and generation.
    pub(crate) fn is_writer(&self, writer: Option<&Value>) -> bool {
        writer == Some(&serde_json::json!([self.execution_id, self.generation]))
    }

    pub(crate) fn execution_id(&self) -> &str {
        &self.execution_id
    }

    pub(crate) const fn generation(&self) -> u64 {
        self.generation
    }
}

/// FU-269 (E6): the root session a re-running continuation's resolvers read,
/// ending at the pause it answers. Read-only (ADK's `Session` has no writer):
/// the re-run writes through the session service to the stored session, after
/// the suffix it already holds; nothing is deleted or rewritten.
pub(crate) struct PauseView {
    inner: Box<dyn adk_rust::session::Session>,
    len: usize,
}

impl PauseView {
    /// `inner` truncated to its first `len` events (`len` ≤ its length).
    pub(crate) fn new(inner: Box<dyn adk_rust::session::Session>, len: usize) -> Self {
        let len = len.min(inner.events().len());
        Self { inner, len }
    }
}

impl adk_rust::session::Session for PauseView {
    fn id(&self) -> &str {
        self.inner.id()
    }

    fn app_name(&self) -> &str {
        self.inner.app_name()
    }

    fn user_id(&self) -> &str {
        self.inner.user_id()
    }

    fn state(&self) -> &dyn adk_rust::session::State {
        self.inner.state()
    }

    fn events(&self) -> &dyn adk_rust::session::Events {
        self
    }

    fn last_update_time(&self) -> chrono::DateTime<chrono::Utc> {
        self.inner.last_update_time()
    }
}

impl adk_rust::session::Events for PauseView {
    fn all(&self) -> Vec<adk_rust::Event> {
        let mut events = self.inner.events().all();
        events.truncate(self.len);
        events
    }

    fn len(&self) -> usize {
        self.len
    }

    fn at(&self, index: usize) -> Option<&adk_rust::Event> {
        (index < self.len)
            .then(|| self.inner.events().at(index))
            .flatten()
    }
}

/// 32 lowercase hex characters (the execution id schema pattern).
fn valid_execution_id(value: &str) -> bool {
    value.len() == 32
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost::Message as _;
    use serde_json::json;

    const OWN: &str = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b";

    fn field(execution_id: &str, generation: u64) -> PausedWriterV1 {
        PausedWriterV1 {
            execution_id: execution_id.to_owned(),
            generation,
        }
    }

    /// The typed refusals map to the registered `RECOVERY_FRONTIER_UNSUPPORTED`
    /// terminal (`assembly_failure`), never Internal.
    #[test]
    fn continuation_refusals_are_recovery_frontier_causes() {
        for cause in [
            "pipeline.recovery.root_writer_mismatch",
            "pipeline.recovery.continuation_pause_unbound",
        ] {
            assert!(
                crate::agents::session::is_recovery_frontier_cause(cause),
                "{cause}"
            );
        }
    }

    #[test]
    fn the_claim_field_is_optional_strict_and_matches_only_its_writer() {
        assert_eq!(PausedWriter::from_claim(None, OWN), Ok(None));
        let id = "0123456789abcdef0123456789abcdef";
        let writer = PausedWriter::from_claim(Some(&field(id, 2)), OWN)
            .expect("valid")
            .expect("present");
        assert!(writer.is_writer(Some(&json!([id, 2]))));
        assert!(!writer.is_writer(Some(&json!([id, 1]))));
        assert!(!writer.is_writer(Some(&json!(["fedcba9876543210fedcba9876543210", 2]))));
        assert!(!writer.is_writer(None));
        for bad in [
            field(id, 0),
            field(OWN, 2),
            field("0123456789ABCDEF0123456789ABCDEF", 1),
            field("short", 1),
            field("", 1),
        ] {
            assert_eq!(
                PausedWriter::from_claim(Some(&bad), OWN),
                Err(PausedWriterError),
                "{bad:?}"
            );
        }
    }

    /// An older Worker decodes a receipt carrying the field and ignores it:
    /// field 19 is unknown to it, and every field it knows reads the same.
    #[test]
    fn an_older_worker_ignores_the_paused_writer_on_its_claim_receipt() {
        use crate::protocol::elitea::runtime::v1::{ClaimDispositionV1, ClaimReceiptV1};

        /// `ClaimReceiptV1` as a Worker from before field 19 knows it (the
        /// fields this test sets).
        #[derive(Clone, PartialEq, prost::Message)]
        struct ReceiptBeforeFieldNineteen {
            #[prost(enumeration = "ClaimDispositionV1", tag = "1")]
            disposition: i32,
            #[prost(string, tag = "9")]
            claim_id: String,
            #[prost(bool, tag = "18")]
            toolkit_typed_refusals: bool,
        }
        let current = ClaimReceiptV1 {
            disposition: ClaimDispositionV1::Accepted as i32,
            claim_id: "claim-1".to_owned(),
            toolkit_typed_refusals: true,
            paused_writer: Some(field("0123456789abcdef0123456789abcdef", 3)),
            ..ClaimReceiptV1::default()
        };
        let older = ReceiptBeforeFieldNineteen::decode(current.encode_to_vec().as_slice())
            .expect("an older Worker decodes the new receipt");
        assert_eq!(
            older,
            ReceiptBeforeFieldNineteen {
                disposition: ClaimDispositionV1::Accepted as i32,
                claim_id: "claim-1".to_owned(),
                toolkit_typed_refusals: true,
            }
        );
        // And a receipt from an older Main reads as "no writer".
        let from_older_main = ClaimReceiptV1::decode(older.encode_to_vec().as_slice())
            .expect("the new Worker decodes an older receipt");
        assert_eq!(from_older_main.paused_writer, None);
    }

    /// A stored session: three events, the second the pause.
    struct Stored(Vec<adk_rust::Event>);

    impl adk_rust::session::Session for Stored {
        fn id(&self) -> &'static str {
            "session-1"
        }
        fn app_name(&self) -> &'static str {
            "elitea-agent-v1"
        }
        fn user_id(&self) -> &'static str {
            "user-1"
        }
        fn state(&self) -> &dyn adk_rust::session::State {
            self
        }
        fn events(&self) -> &dyn adk_rust::session::Events {
            self
        }
        fn last_update_time(&self) -> chrono::DateTime<chrono::Utc> {
            chrono::DateTime::UNIX_EPOCH
        }
    }

    impl adk_rust::session::State for Stored {
        fn get(&self, _key: &str) -> Option<Value> {
            None
        }
        fn set(&mut self, _key: String, _value: Value) {}
        fn all(&self) -> std::collections::HashMap<String, Value> {
            std::collections::HashMap::new()
        }
    }

    impl adk_rust::session::Events for Stored {
        fn all(&self) -> Vec<adk_rust::Event> {
            self.0.clone()
        }
        fn len(&self) -> usize {
            self.0.len()
        }
        fn at(&self, index: usize) -> Option<&adk_rust::Event> {
            self.0.get(index)
        }
    }

    /// E6: the resolvers read the session ending at the pause; the session it
    /// wraps keeps every event (the view writes nothing).
    #[test]
    fn the_pause_view_ends_at_the_pause_and_hides_nothing_it_does_not_own() {
        use adk_rust::session::Session as _;
        let events = vec![
            adk_rust::Event::new("root-0"),
            adk_rust::Event::new("pause"),
            adk_rust::Event::new("continuing"),
        ];
        let ids = events
            .iter()
            .map(|event| event.id.clone())
            .collect::<Vec<_>>();
        let view = PauseView::new(Box::new(Stored(events)), 2);
        assert_eq!(view.events().len(), 2);
        assert_eq!(
            view.events()
                .all()
                .iter()
                .map(|event| event.id.clone())
                .collect::<Vec<_>>(),
            ids[..2]
        );
        assert_eq!(
            view.events().at(1).map(|event| event.id.clone()),
            Some(ids[1].clone())
        );
        assert!(view.events().at(2).is_none());
        assert_eq!(view.id(), "session-1");
        // The wrapped session is untouched, and a view longer than it is capped.
        assert_eq!(view.inner.events().len(), 3);
        let capped = PauseView::new(Box::new(Stored(Vec::new())), 5);
        assert_eq!(capped.events().len(), 0);
    }
}
