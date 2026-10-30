//! Run caps (ADR-0041 decision 3, #1498): the most toolkit tool calls,
//! model turns and records read one agent run may use.
//!
//! One [`RunCapsMeter`] is shared by every path of a run: the root agent,
//! nested agents and pipeline nodes. The first cap a run reaches is latched;
//! from then on every charge of any kind is refused with that cap, so the run
//! ends with `cap_reached` and the cap's name instead of being silently
//! truncated. A value of 0 is no cap.
//!
//! What is counted:
//!
//! * **Tool calls** — every call of a toolkit tool (MCP tools included) that
//!   the run's [`crate::toolkits::ToolAdmissionPolicy`] admits. Platform tools
//!   that are not toolkit tools (planner, attachments, an agent called as a
//!   tool) are not counted; the toolkit calls such an agent makes are.
//! * **Model turns** — every model call of the run's agents. Summary and
//!   compaction calls are not counted.
//! * **Records read** — the items a toolkit tool declares it returned, through
//!   the explicit hook [`report_records`]. A tool that does not call the hook
//!   counts 0. The tools that declare a count today:
//!
//!   | Toolkit type | Tool | Counted items |
//!   | --- | --- | --- |
//!   | `jira` | `search_using_jql` | issues returned |
//!   | `github` | `search_issues` | issues and pull requests returned |
//!   | `sql` | `execute_sql` | rows returned |
//!   | `confluence` | `search_pages`, `search_by_title` | pages returned |
//!   | every index toolkit | `search_index` | documents returned |
//!
//! Counters are per execution: a continuation after a pause is a new
//! execution and starts at zero.

use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicU8, AtomicU32, AtomicU64, Ordering};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};

/// The worker's default tool call cap when the command carries no caps.
pub const DEFAULT_MAX_TOOL_CALLS: u32 = 200;

/// One run cap.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum RunCap {
    MaxToolCalls,
    MaxModelTurns,
    MaxRecordsRead,
}

impl RunCap {
    /// The cap's name, as the platform configuration and the run's outcome
    /// spell it.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::MaxToolCalls => "max_tool_calls",
            Self::MaxModelTurns => "max_model_turns",
            Self::MaxRecordsRead => "max_records_read",
        }
    }

    /// The error code of a refusal at this cap.
    #[must_use]
    pub const fn error_code(self) -> &'static str {
        match self {
            Self::MaxToolCalls => "run_caps.max_tool_calls",
            Self::MaxModelTurns => "run_caps.max_model_turns",
            Self::MaxRecordsRead => "run_caps.max_records_read",
        }
    }

    /// The cap whose refusal carries `code`.
    #[must_use]
    pub fn from_error_code(code: &str) -> Option<Self> {
        [
            Self::MaxToolCalls,
            Self::MaxModelTurns,
            Self::MaxRecordsRead,
        ]
        .into_iter()
        .find(|cap| cap.error_code() == code)
    }

    const fn tag(self) -> u8 {
        match self {
            Self::MaxToolCalls => 1,
            Self::MaxModelTurns => 2,
            Self::MaxRecordsRead => 3,
        }
    }

    const fn from_tag(tag: u8) -> Option<Self> {
        match tag {
            1 => Some(Self::MaxToolCalls),
            2 => Some(Self::MaxModelTurns),
            3 => Some(Self::MaxRecordsRead),
            _ => None,
        }
    }
}

/// A run's limits; 0 is no cap.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct RunCapLimits {
    pub max_tool_calls: u32,
    pub max_model_turns: u32,
    pub max_records_read: u64,
}

impl RunCapLimits {
    /// The worker's defaults: [`DEFAULT_MAX_TOOL_CALLS`] tool calls, no
    /// other cap.
    #[must_use]
    pub const fn worker_default() -> Self {
        Self {
            max_tool_calls: DEFAULT_MAX_TOOL_CALLS,
            max_model_turns: 0,
            max_records_read: 0,
        }
    }

    /// No cap at all.
    #[must_use]
    pub const fn unlimited() -> Self {
        Self {
            max_tool_calls: 0,
            max_model_turns: 0,
            max_records_read: 0,
        }
    }
}

/// One run's counters against its limits.
#[derive(Debug)]
pub struct RunCapsMeter {
    limits: RunCapLimits,
    tool_calls: AtomicU32,
    model_turns: AtomicU32,
    records_read: AtomicU64,
    tripped: AtomicU8,
}

impl RunCapsMeter {
    /// A meter with no cap (hosts without run caps, such as the desktop).
    #[must_use]
    pub fn unlimited() -> Arc<Self> {
        Self::new(RunCapLimits::unlimited())
    }

    #[must_use]
    pub fn new(limits: RunCapLimits) -> Arc<Self> {
        Arc::new(Self {
            limits,
            tool_calls: AtomicU32::new(0),
            model_turns: AtomicU32::new(0),
            records_read: AtomicU64::new(0),
            tripped: AtomicU8::new(0),
        })
    }

    #[must_use]
    pub const fn limits(&self) -> RunCapLimits {
        self.limits
    }

    /// The first cap the run reached, if any.
    #[must_use]
    pub fn tripped(&self) -> Option<RunCap> {
        RunCap::from_tag(self.tripped.load(Ordering::Acquire))
    }

    #[must_use]
    pub fn tool_calls(&self) -> u32 {
        self.tool_calls.load(Ordering::Acquire)
    }

    #[must_use]
    pub fn model_turns(&self) -> u32 {
        self.model_turns.load(Ordering::Acquire)
    }

    #[must_use]
    pub fn records_read(&self) -> u64 {
        self.records_read.load(Ordering::Acquire)
    }

    /// The used count and the limit of `cap`.
    #[must_use]
    pub fn usage(&self, cap: RunCap) -> (u64, u64) {
        match cap {
            RunCap::MaxToolCalls => (
                u64::from(self.tool_calls()),
                u64::from(self.limits.max_tool_calls),
            ),
            RunCap::MaxModelTurns => (
                u64::from(self.model_turns()),
                u64::from(self.limits.max_model_turns),
            ),
            RunCap::MaxRecordsRead => (self.records_read(), self.limits.max_records_read),
        }
    }

    /// Count one tool call. The call past the cap is refused and trips it.
    ///
    /// # Errors
    ///
    /// The tripped cap: this one, or the one the run reached before.
    pub fn charge_tool_call(&self) -> Result<(), RunCap> {
        self.ensure_open()?;
        let max = self.limits.max_tool_calls;
        let admitted = self
            .tool_calls
            .try_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                (max == 0 || used < max).then(|| used.saturating_add(1))
            })
            .is_ok();
        if admitted {
            Ok(())
        } else {
            Err(self.trip(RunCap::MaxToolCalls))
        }
    }

    /// Count one model turn. The turn past the cap is refused and trips it.
    ///
    /// # Errors
    ///
    /// The tripped cap: this one, or the one the run reached before.
    pub fn charge_model_turn(&self) -> Result<(), RunCap> {
        self.ensure_open()?;
        let max = self.limits.max_model_turns;
        let admitted = self
            .model_turns
            .try_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                (max == 0 || used < max).then(|| used.saturating_add(1))
            })
            .is_ok();
        if admitted {
            Ok(())
        } else {
            Err(self.trip(RunCap::MaxModelTurns))
        }
    }

    /// Count `count` records a tool returned. A total past the cap is
    /// refused and trips it; the records are counted either way.
    ///
    /// # Errors
    ///
    /// The tripped cap: this one, or the one the run reached before.
    pub fn charge_records(&self, count: u64) -> Result<(), RunCap> {
        self.ensure_open()?;
        let total = self
            .records_read
            .fetch_add(count, Ordering::AcqRel)
            .saturating_add(count);
        let max = self.limits.max_records_read;
        if max != 0 && total > max {
            return Err(self.trip(RunCap::MaxRecordsRead));
        }
        Ok(())
    }

    fn ensure_open(&self) -> Result<(), RunCap> {
        self.tripped().map_or(Ok(()), Err)
    }

    /// Latch `cap` unless another cap was latched first; answer the latched
    /// cap.
    fn trip(&self, cap: RunCap) -> RunCap {
        match self
            .tripped
            .compare_exchange(0, cap.tag(), Ordering::AcqRel, Ordering::Acquire)
        {
            Ok(_) => cap,
            Err(previous) => RunCap::from_tag(previous).unwrap_or(cap),
        }
    }
}

/// The refusal of a charge at `cap`: the call does not run and the run
/// ends with `cap_reached`.
#[must_use]
pub fn cap_error(cap: RunCap) -> AdkError {
    AdkError::new(
        ErrorComponent::Guardrail,
        ErrorCategory::Forbidden,
        cap.error_code(),
        "the run reached a run cap an administrator set; the call does not run",
    )
    .with_retry(RetryHint {
        should_retry: false,
        retry_after_ms: None,
        max_attempts: Some(1),
    })
}

tokio::task_local! {
    static RECORDS: std::cell::Cell<u64>;
}

/// The explicit record-count hook: a toolkit tool that returns records
/// (search results, rows, issues) calls this with the number of items it
/// returns. Outside a [`with_record_scope`] it does nothing.
pub fn report_records(count: u64) {
    let _ = RECORDS.try_with(|records| records.set(records.get().saturating_add(count)));
}

/// Run `future` and answer its output with the records it reported. A
/// nested scope reports to itself only.
pub async fn with_record_scope<F: Future>(future: F) -> (F::Output, u64) {
    RECORDS
        .scope(std::cell::Cell::new(0), async move {
            let output = future.await;
            (output, RECORDS.with(std::cell::Cell::get))
        })
        .await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_and_codes_round_trip() {
        for cap in [
            RunCap::MaxToolCalls,
            RunCap::MaxModelTurns,
            RunCap::MaxRecordsRead,
        ] {
            assert_eq!(RunCap::from_error_code(cap.error_code()), Some(cap));
            assert_eq!(cap.error_code(), format!("run_caps.{}", cap.as_str()));
        }
        assert_eq!(RunCap::from_error_code("run_caps.other"), None);
    }

    #[test]
    fn the_worker_default_caps_tool_calls_at_200() {
        let limits = RunCapLimits::worker_default();
        assert_eq!(limits.max_tool_calls, 200);
        assert_eq!(limits.max_model_turns, 0);
        assert_eq!(limits.max_records_read, 0);
    }

    #[test]
    fn the_call_past_the_cap_is_refused_and_latches() {
        let meter = RunCapsMeter::new(RunCapLimits {
            max_tool_calls: 2,
            max_model_turns: 0,
            max_records_read: 0,
        });
        assert_eq!(meter.charge_tool_call(), Ok(()));
        assert_eq!(meter.charge_tool_call(), Ok(()));
        assert_eq!(meter.charge_tool_call(), Err(RunCap::MaxToolCalls));
        assert_eq!(meter.tripped(), Some(RunCap::MaxToolCalls));
        assert_eq!(meter.tool_calls(), 2);
        // Every charge of any kind is refused with the latched cap.
        assert_eq!(meter.charge_model_turn(), Err(RunCap::MaxToolCalls));
        assert_eq!(meter.charge_records(0), Err(RunCap::MaxToolCalls));
        assert_eq!(meter.usage(RunCap::MaxToolCalls), (2, 2));
    }

    #[test]
    fn model_turns_trip_their_own_cap() {
        let meter = RunCapsMeter::new(RunCapLimits {
            max_tool_calls: 0,
            max_model_turns: 1,
            max_records_read: 0,
        });
        assert_eq!(meter.charge_model_turn(), Ok(()));
        assert_eq!(meter.charge_model_turn(), Err(RunCap::MaxModelTurns));
        assert_eq!(meter.charge_tool_call(), Err(RunCap::MaxModelTurns));
    }

    #[test]
    fn records_trip_when_the_total_passes_the_cap() {
        let meter = RunCapsMeter::new(RunCapLimits {
            max_tool_calls: 0,
            max_model_turns: 0,
            max_records_read: 10,
        });
        assert_eq!(meter.charge_records(4), Ok(()));
        assert_eq!(meter.charge_records(6), Ok(()));
        assert_eq!(meter.charge_records(1), Err(RunCap::MaxRecordsRead));
        assert_eq!(meter.records_read(), 11);
        assert_eq!(meter.tripped(), Some(RunCap::MaxRecordsRead));
    }

    #[test]
    fn zero_is_no_cap() {
        let meter = RunCapsMeter::unlimited();
        for _ in 0..1_000 {
            assert_eq!(meter.charge_tool_call(), Ok(()));
            assert_eq!(meter.charge_model_turn(), Ok(()));
        }
        assert_eq!(meter.charge_records(u64::MAX), Ok(()));
        assert_eq!(meter.tripped(), None);
    }

    #[test]
    fn the_cap_error_names_the_cap_and_never_retries() {
        let error = cap_error(RunCap::MaxRecordsRead);
        assert_eq!(error.code, "run_caps.max_records_read");
        assert!(!error.is_retryable());
    }

    #[tokio::test]
    async fn a_record_scope_collects_what_the_tool_reports() {
        // Outside a scope the hook does nothing.
        report_records(5);
        let ((), counted) = with_record_scope(async {
            report_records(3);
            report_records(4);
        })
        .await;
        assert_eq!(counted, 7);
        let ((), outer) = with_record_scope(async {
            report_records(1);
            let ((), inner) = with_record_scope(async { report_records(10) }).await;
            assert_eq!(inner, 10);
        })
        .await;
        assert_eq!(outer, 1);
        let ((), none) = with_record_scope(async {}).await;
        assert_eq!(none, 0);
    }
}
