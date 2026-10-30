//! FU-265: the paused execution a pipeline continuation re-enters.
//!
//! A continuation (a decided pause, a text answer or an MCP authorization)
//! re-enters the root checkpoint row its paused execution wrote. Main names
//! that writer, read inside the admission transaction that authorized the
//! decision, in a key of its own beside the continuation's decision envelope
//! (a Worker without it still reads the envelope). The Worker accepts that row
//! only for that continuation and only for an exact writer match: on the first
//! attempt when the marker is present, and on a reclaim, where it is required.

use serde::Deserialize;
use serde_json::Value;

/// The top-level `meta` key of the marker.
pub(crate) const PAUSED_WRITER_META_KEY: &str = "elitea.continuation.paused_writer.v1";

/// The execution and generation that wrote the paused root row.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PausedWriter {
    execution_id: String,
    generation: u64,
}

/// A marker that is present but malformed.
#[derive(Debug, Eq, PartialEq)]
pub(crate) struct PausedWriterError;

impl PausedWriter {
    /// The marker in `meta`: `None` when absent, an error when malformed.
    pub(crate) fn from_meta<'a>(
        mut lookup: impl FnMut(&str) -> Option<&'a Value>,
    ) -> Result<Option<Self>, PausedWriterError> {
        let Some(marker) = lookup(PAUSED_WRITER_META_KEY) else {
            return Ok(None);
        };
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Raw {
            execution_id: String,
            generation: u64,
        }
        let raw: Raw = serde_json::from_value(marker.clone()).map_err(|_| PausedWriterError)?;
        if raw.generation == 0 || !valid_execution_id(&raw.execution_id) {
            return Err(PausedWriterError);
        }
        Ok(Some(Self {
            execution_id: raw.execution_id,
            generation: raw.generation,
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
    use serde_json::json;
    use std::collections::BTreeMap;

    fn parse(meta: &BTreeMap<String, Value>) -> Result<Option<PausedWriter>, PausedWriterError> {
        PausedWriter::from_meta(|key| meta.get(key))
    }

    #[test]
    fn the_marker_is_optional_strict_and_matches_only_its_writer() {
        assert_eq!(parse(&BTreeMap::new()), Ok(None));
        let id = "0123456789abcdef0123456789abcdef";
        let meta = BTreeMap::from([(
            PAUSED_WRITER_META_KEY.to_owned(),
            json!({"execution_id": id, "generation": 2}),
        )]);
        let writer = parse(&meta).expect("valid").expect("present");
        assert!(writer.is_writer(Some(&json!([id, 2]))));
        assert!(!writer.is_writer(Some(&json!([id, 1]))));
        assert!(!writer.is_writer(Some(&json!(["fedcba9876543210fedcba9876543210", 2]))));
        assert!(!writer.is_writer(None));
        for bad in [
            json!({"execution_id": id, "generation": 0}),
            json!({"execution_id": id, "generation": -1}),
            json!({"execution_id": id, "generation": "2"}),
            json!({"execution_id": "0123456789ABCDEF0123456789ABCDEF", "generation": 1}),
            json!({"execution_id": "short", "generation": 1}),
            json!({"execution_id": id}),
            json!({"execution_id": id, "generation": 1, "extra": true}),
            json!([id, 1]),
            json!(null),
        ] {
            let meta = BTreeMap::from([(PAUSED_WRITER_META_KEY.to_owned(), bad.clone())]);
            assert_eq!(parse(&meta), Err(PausedWriterError), "{bad}");
        }
    }
}
