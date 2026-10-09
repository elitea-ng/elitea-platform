//! Per-activation progress coalescer for the Parallel and Map runners.
//!
//! Streamed model deltas from fan-out children would otherwise become one
//! frame (and one Main transaction) each. The coalescer buffers deltas per
//! child and emits ONE aggregated frame per activation every
//! [`FANOUT_PROGRESS_FLUSH_INTERVAL`] or at [`FANOUT_PROGRESS_FLUSH_BYTES`],
//! whichever comes first. Lifecycle frames are never delayed, and they flush
//! the buffer first so a member's frames stay in order.
//!
//! The type is a single-owner state machine: no locks, tasks or channels, and
//! the clock is injected so the owner can drive it from `select!` with
//! `sleep_until(next_flush_at())` and tests stay deterministic.

use std::fmt;
use std::time::Duration;

use thiserror::Error;
use tokio::time::Instant;

/// Longest a buffered delta waits before it is flushed.
pub const FANOUT_PROGRESS_FLUSH_INTERVAL: Duration = Duration::from_millis(250);
/// Largest total delta payload of one aggregated frame (and of the buffer).
pub const FANOUT_PROGRESS_FLUSH_BYTES: usize = 64 * 1024;
/// Largest member count (the Map item cap; Parallel allows 16).
pub const FANOUT_PROGRESS_MAX_MEMBERS: usize = super::fanout_budget::MAX_FANOUT_CHILDREN;

/// Typed coalescer failure with a stable machine code.
#[derive(Debug, Error, Clone, Copy, PartialEq, Eq)]
pub enum FanoutProgressError {
    #[error("fan-out progress member count {members} is outside the supported range")]
    InvalidMemberCount { members: usize },
    #[error("fan-out progress member {ordinal} is outside 0..{members}")]
    InvalidMember { ordinal: usize, members: usize },
}

impl FanoutProgressError {
    #[must_use]
    pub fn code(&self) -> &'static str {
        match self {
            Self::InvalidMemberCount { .. } => "graph.fanout.progress_invalid_member_count",
            Self::InvalidMember { .. } => "graph.fanout.progress_invalid_member",
        }
    }
}

/// Child lifecycle transitions that are forwarded immediately.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FanoutLifecycle {
    Started,
    Completed,
    Paused,
    Failed,
    Cancelled,
}

/// Frame handed to the owner for emission.
#[derive(Clone, PartialEq, Eq)]
pub enum FanoutProgressFrame {
    /// One entry per member with data, ordered by ordinal; each member's
    /// buffered deltas are concatenated losslessly.
    Progress { deltas: Vec<(usize, String)> },
    Lifecycle {
        ordinal: usize,
        event: FanoutLifecycle,
    },
}

impl fmt::Debug for FanoutProgressFrame {
    // Delta text is model output: print ordinals and byte counts only.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Progress { deltas } => {
                let sizes: Vec<(usize, usize)> = deltas
                    .iter()
                    .map(|(ordinal, text)| (*ordinal, text.len()))
                    .collect();
                f.debug_struct("Progress")
                    .field("member_bytes", &sizes)
                    .finish()
            }
            Self::Lifecycle { ordinal, event } => f
                .debug_struct("Lifecycle")
                .field("ordinal", ordinal)
                .field("event", event)
                .finish(),
        }
    }
}

pub struct FanoutProgressCoalescer {
    buffers: Vec<String>,
    buffered_bytes: usize,
    /// When the first byte of the current window was buffered.
    window_start: Option<Instant>,
}

impl fmt::Debug for FanoutProgressCoalescer {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let sizes: Vec<(usize, usize)> = self
            .buffers
            .iter()
            .enumerate()
            .filter(|(_, text)| !text.is_empty())
            .map(|(ordinal, text)| (ordinal, text.len()))
            .collect();
        f.debug_struct("FanoutProgressCoalescer")
            .field("members", &self.buffers.len())
            .field("buffered_bytes", &self.buffered_bytes)
            .field("member_bytes", &sizes)
            .field("window_open", &self.window_start.is_some())
            .finish()
    }
}

impl FanoutProgressCoalescer {
    pub fn new(members: usize) -> Result<Self, FanoutProgressError> {
        if members == 0 || members > FANOUT_PROGRESS_MAX_MEMBERS {
            return Err(FanoutProgressError::InvalidMemberCount { members });
        }
        Ok(Self {
            buffers: vec![String::new(); members],
            buffered_bytes: 0,
            window_start: None,
        })
    }

    /// Buffer a delta. Returns the frames that became due because the byte
    /// bound was reached (usually none). Oversized deltas are split only at
    /// char boundaries, so no byte is lost or duplicated.
    pub fn push_delta(
        &mut self,
        ordinal: usize,
        delta: &str,
        now: Instant,
    ) -> Result<Vec<FanoutProgressFrame>, FanoutProgressError> {
        self.check(ordinal)?;
        let mut frames = Vec::new();
        let mut rest = delta;
        while !rest.is_empty() {
            let space = FANOUT_PROGRESS_FLUSH_BYTES - self.buffered_bytes;
            let take = char_floor(rest, space);
            if take == 0 {
                // Less room than the next char needs: close the frame first.
                frames.extend(self.flush());
                continue;
            }
            let (head, tail) = rest.split_at(take);
            self.buffers[ordinal].push_str(head);
            self.buffered_bytes += take;
            self.window_start.get_or_insert(now);
            rest = tail;
            if self.buffered_bytes == FANOUT_PROGRESS_FLUSH_BYTES {
                frames.extend(self.flush());
            }
        }
        Ok(frames)
    }

    /// Emit a lifecycle frame immediately, preceded by any buffered deltas.
    pub fn lifecycle(
        &mut self,
        ordinal: usize,
        event: FanoutLifecycle,
    ) -> Result<Vec<FanoutProgressFrame>, FanoutProgressError> {
        self.check(ordinal)?;
        let mut frames: Vec<_> = self.flush().into_iter().collect();
        frames.push(FanoutProgressFrame::Lifecycle { ordinal, event });
        Ok(frames)
    }

    /// Instant at which buffered data must be flushed; `None` when empty.
    #[must_use]
    pub fn next_flush_at(&self) -> Option<Instant> {
        self.window_start
            .map(|start| start + FANOUT_PROGRESS_FLUSH_INTERVAL)
    }

    /// Flush when the window has expired.
    pub fn poll(&mut self, now: Instant) -> Option<FanoutProgressFrame> {
        match self.next_flush_at() {
            Some(due) if now >= due => self.flush(),
            _ => None,
        }
    }

    /// Flush the remainder (join or cancel).
    pub fn finish(&mut self) -> Option<FanoutProgressFrame> {
        self.flush()
    }

    fn check(&self, ordinal: usize) -> Result<(), FanoutProgressError> {
        if ordinal >= self.buffers.len() {
            return Err(FanoutProgressError::InvalidMember {
                ordinal,
                members: self.buffers.len(),
            });
        }
        Ok(())
    }

    fn flush(&mut self) -> Option<FanoutProgressFrame> {
        if self.buffered_bytes == 0 {
            return None;
        }
        let deltas = self
            .buffers
            .iter_mut()
            .enumerate()
            .filter(|(_, text)| !text.is_empty())
            .map(|(ordinal, text)| (ordinal, std::mem::take(text)))
            .collect();
        self.buffered_bytes = 0;
        self.window_start = None;
        Some(FanoutProgressFrame::Progress { deltas })
    }
}

/// Largest char boundary of `text` that is `<= max`.
fn char_floor(text: &str, max: usize) -> usize {
    let mut index = max.min(text.len());
    while !text.is_char_boundary(index) {
        index -= 1;
    }
    index
}

#[cfg(test)]
mod tests {
    use super::*;

    const MS: fn(u64) -> Duration = Duration::from_millis;

    fn progress_bytes(frame: &FanoutProgressFrame) -> usize {
        match frame {
            FanoutProgressFrame::Progress { deltas } => deltas.iter().map(|(_, t)| t.len()).sum(),
            FanoutProgressFrame::Lifecycle { .. } => 0,
        }
    }

    #[test]
    fn rate_budget_and_lossless_reassembly() {
        let start = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(8).expect("members");
        let mut expected = vec![String::new(); 8];
        let mut got = vec![String::new(); 8];
        let mut emitted: Vec<(u64, FanoutProgressFrame)> = Vec::new();
        let mut collect = |at: u64, frame: FanoutProgressFrame, got: &mut Vec<String>| {
            if let FanoutProgressFrame::Progress { deltas } = &frame {
                for (ordinal, text) in deltas {
                    got[*ordinal].push_str(text);
                }
            }
            emitted.push((at, frame));
        };
        // 8 members x 30/s x 10 s = 2400 deltas, one every ~4 ms round-robin.
        for step in 0..2400u64 {
            let at = step * 4;
            let now = start + MS(at);
            if let Some(frame) = coalescer.poll(now) {
                collect(at, frame, &mut got);
            }
            let ordinal = (step % 8) as usize;
            let delta = format!("m{ordinal}-{step:05}-tokens ");
            expected[ordinal].push_str(&delta);
            for frame in coalescer.push_delta(ordinal, &delta, now).expect("push") {
                collect(at, frame, &mut got);
            }
            if let Some(due) = coalescer.next_flush_at() {
                assert!(due > now, "a due flush must have been polled");
            }
        }
        if let Some(frame) = coalescer.finish() {
            collect(9_600, frame, &mut got);
        }
        assert!(emitted.len() <= 41, "got {} frames", emitted.len());
        for second in 0..10u64 {
            let count = emitted
                .iter()
                .filter(|(at, _)| *at / 1000 == second)
                .count();
            assert!(count <= 4, "second {second} had {count} frames");
        }
        assert_eq!(got, expected);
    }

    #[test]
    fn lifecycle_flushes_buffer_first_at_same_instant() {
        let now = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(2).expect("members");
        assert!(
            coalescer
                .push_delta(1, "hello", now)
                .expect("push")
                .is_empty()
        );
        let frames = coalescer
            .lifecycle(1, FanoutLifecycle::Completed)
            .expect("lifecycle");
        assert_eq!(
            frames,
            vec![
                FanoutProgressFrame::Progress {
                    deltas: vec![(1, "hello".to_owned())]
                },
                FanoutProgressFrame::Lifecycle {
                    ordinal: 1,
                    event: FanoutLifecycle::Completed
                },
            ]
        );
        assert_eq!(coalescer.next_flush_at(), None);
        // With nothing buffered only the lifecycle frame is returned.
        let frames = coalescer
            .lifecycle(0, FanoutLifecycle::Started)
            .expect("lifecycle");
        assert_eq!(frames.len(), 1);
    }

    #[test]
    fn byte_bound_at_limit_and_limit_plus_one() {
        let now = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(1).expect("members");
        let frames = coalescer
            .push_delta(0, &"a".repeat(FANOUT_PROGRESS_FLUSH_BYTES), now)
            .expect("push");
        assert_eq!(frames.len(), 1);
        assert_eq!(progress_bytes(&frames[0]), FANOUT_PROGRESS_FLUSH_BYTES);
        assert_eq!(coalescer.next_flush_at(), None);

        let frames = coalescer
            .push_delta(0, &"b".repeat(FANOUT_PROGRESS_FLUSH_BYTES + 1), now)
            .expect("push");
        assert_eq!(frames.len(), 1);
        assert_eq!(progress_bytes(&frames[0]), FANOUT_PROGRESS_FLUSH_BYTES);
        assert_eq!(coalescer.buffered_bytes, 1);
        assert_eq!(
            coalescer.next_flush_at(),
            Some(now + FANOUT_PROGRESS_FLUSH_INTERVAL)
        );
    }

    #[test]
    fn multibyte_split_stays_valid_and_byte_exact() {
        let now = Instant::now();
        for unit in ["é", "😀"] {
            let input = format!("x{}", unit.repeat(FANOUT_PROGRESS_FLUSH_BYTES));
            let mut coalescer = FanoutProgressCoalescer::new(1).expect("members");
            let mut frames = coalescer.push_delta(0, &input, now).expect("push");
            frames.extend(coalescer.finish());
            assert!(frames.len() >= 2);
            let mut joined = String::new();
            for frame in &frames {
                assert!(progress_bytes(frame) <= FANOUT_PROGRESS_FLUSH_BYTES);
                if let FanoutProgressFrame::Progress { deltas } = frame {
                    joined.push_str(&deltas[0].1);
                }
            }
            assert_eq!(joined, input);
        }
    }

    #[test]
    fn flush_deadline_is_window_start_plus_interval() {
        let now = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(2).expect("members");
        assert_eq!(coalescer.next_flush_at(), None);
        assert_eq!(coalescer.poll(now), None);
        coalescer.push_delta(0, "a", now).expect("push");
        coalescer.push_delta(1, "b", now + MS(100)).expect("push");
        let due = now + FANOUT_PROGRESS_FLUSH_INTERVAL;
        assert_eq!(coalescer.next_flush_at(), Some(due));
        assert_eq!(coalescer.poll(due - MS(1)), None);
        assert_eq!(
            coalescer.poll(due),
            Some(FanoutProgressFrame::Progress {
                deltas: vec![(0, "a".to_owned()), (1, "b".to_owned())]
            })
        );
        assert_eq!(coalescer.next_flush_at(), None);
        assert_eq!(coalescer.finish(), None);
    }

    #[test]
    fn invalid_members_are_typed_errors_without_state_change() {
        assert_eq!(
            FanoutProgressCoalescer::new(0).err().map(|e| e.code()),
            Some("graph.fanout.progress_invalid_member_count")
        );
        assert!(FanoutProgressCoalescer::new(FANOUT_PROGRESS_MAX_MEMBERS + 1).is_err());
        assert!(FanoutProgressCoalescer::new(FANOUT_PROGRESS_MAX_MEMBERS).is_ok());

        let now = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(2).expect("members");
        let err = coalescer
            .push_delta(2, "x", now)
            .expect_err("unknown member");
        assert_eq!(err.code(), "graph.fanout.progress_invalid_member");
        assert!(coalescer.lifecycle(2, FanoutLifecycle::Failed).is_err());
        assert_eq!(coalescer.buffered_bytes, 0);
        assert_eq!(coalescer.next_flush_at(), None);
    }

    #[test]
    fn debug_never_prints_delta_text() {
        let now = Instant::now();
        let mut coalescer = FanoutProgressCoalescer::new(1).expect("members");
        coalescer
            .push_delta(0, "secret-delta-marker", now)
            .expect("push");
        assert!(!format!("{coalescer:?}").contains("secret-delta-marker"));
        let frame = coalescer.finish().expect("frame");
        assert!(!format!("{frame:?}").contains("secret-delta-marker"));
    }
}
