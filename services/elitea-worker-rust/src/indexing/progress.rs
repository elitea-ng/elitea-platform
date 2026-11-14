//! What an index run says about itself while it runs (#2041).
//!
//! The pipeline reports a few typed notes: a stage began, what the plan
//! decided, how far the writing is. It knows nothing of frames, sequences or
//! the output plane; the execution layer turns a note into a browser-visible
//! `NodeEvent` and decides how many of them are worth sending. A run without a
//! sink (every test, every caller that does not stream) behaves exactly as it
//! did before.

/// A stage of the run a person can follow.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProgressStage {
    /// Reading what the store holds, listing the source and diffing the two.
    Plan,
    /// Fetching, chunking, embedding and writing the documents.
    Write,
}

/// One thing the run reports.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProgressNote {
    Started(ProgressStage),
    /// The plan: documents to write, unchanged documents, documents to delete.
    Planned {
        to_write: u64,
        unchanged: u64,
        vanished: u64,
    },
    /// Documents written so far, of the `total` the plan wrote, and the chunks
    /// in them. Sent after every document; the sink decides which to publish.
    Documents {
        processed: u64,
        total: u64,
        chunks: u64,
    },
    /// The documents are written (the vanished ones are deleted after this).
    Written {
        documents: u64,
        chunks: u64,
    },
}

/// Why a sink asks the run to stop: Main rejected a frame because the
/// execution was stopped or its deadline passed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProgressStop {
    Cancelled,
    DeadlineExceeded,
}

/// What a sink answers a note with.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProgressSignal {
    Continue,
    Stop(ProgressStop),
}

/// Receives a run's notes. Reporting never blocks or fails the run: a sink that
/// cannot publish keeps the failure to itself and answers `Continue`. Only a
/// Stop or deadline decided by Main is passed back, at the next note.
pub(crate) trait ProgressSink: Send + Sync {
    fn note(&self, note: ProgressNote) -> ProgressSignal;
}
