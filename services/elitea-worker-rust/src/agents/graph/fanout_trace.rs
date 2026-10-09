//! Fan-out spans and lifecycle logs with safe fields only.
//!
//! Every field is a static label, a count, an ordinal or the author-defined
//! node id. The types here accept nothing else, so prompts, item data, tool
//! arguments, URLs, credentials, thread ids and checkpoint payloads cannot reach
//! a span or a log line through them.

use tracing::Span;

#[derive(Clone, Copy)]
pub(crate) enum FanoutKind {
    Parallel,
    Map,
}

impl FanoutKind {
    const fn label(self) -> &'static str {
        match self {
            Self::Parallel => "parallel",
            Self::Map => "map",
        }
    }
}

/// How a child starts in this visit.
#[derive(Clone, Copy)]
pub(crate) enum ChildStart {
    /// No checkpoint yet: the child runs from its frozen input.
    Fresh,
    /// A terminal or paused receipt replays without running the child.
    Restored,
    /// A paused child continues from its checkpoint with a decision.
    Resumed,
}

impl ChildStart {
    const fn label(self) -> &'static str {
        match self {
            Self::Fresh => "fresh",
            Self::Restored => "restored",
            Self::Resumed => "resumed",
        }
    }
}

#[derive(Clone, Copy)]
pub(crate) enum ChildOutcome {
    Completed,
    Paused,
    Blocked,
    Failed,
    Cancelled,
    LeaseLost,
}

impl ChildOutcome {
    const fn label(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Paused => "paused",
            Self::Blocked => "blocked",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
            Self::LeaseLost => "lease_lost",
        }
    }
}

#[derive(Clone, Copy)]
pub(crate) enum ActivationOutcome {
    Joined,
    Paused,
    Blocked,
    Failed,
    Cancelled,
    LeaseLost,
}

impl ActivationOutcome {
    const fn label(self) -> &'static str {
        match self {
            Self::Joined => "joined",
            Self::Paused => "paused",
            Self::Blocked => "blocked",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
            Self::LeaseLost => "lease_lost",
        }
    }
}

pub(crate) fn activation_span(
    kind: FanoutKind,
    node: &str,
    step: usize,
    children: Option<usize>,
    concurrency: usize,
    resumed: bool,
) -> Span {
    let span = tracing::info_span!(
        "graph.fanout.activation",
        kind = kind.label(),
        node,
        step,
        children = tracing::field::Empty,
        concurrency,
        resumed,
        outcome = tracing::field::Empty,
    );
    if let Some(children) = children {
        span.record("children", children);
    }
    span
}

/// Record the child count once a plan that sizes it at run time is frozen.
pub(crate) fn activation_children(span: &Span, children: usize) {
    span.record("children", children);
}

pub(crate) fn activation_started(span: &Span) {
    tracing::info!(parent: span, "Fan-out started");
}

pub(crate) fn activation_finished(span: &Span, outcome: ActivationOutcome) {
    let label = outcome.label();
    span.record("outcome", label);
    match outcome {
        ActivationOutcome::Joined => {
            tracing::info!(parent: span, outcome = label, "Fan-out joined");
        }
        ActivationOutcome::Paused => {
            tracing::info!(parent: span, outcome = label, "Fan-out paused for review");
        }
        ActivationOutcome::Cancelled | ActivationOutcome::LeaseLost => {
            tracing::info!(parent: span, outcome = label, "Fan-out cancelled");
        }
        ActivationOutcome::Blocked | ActivationOutcome::Failed => {
            tracing::warn!(parent: span, outcome = label, "Fan-out failed");
        }
    }
}

pub(crate) fn child_span(kind: FanoutKind, node: &str, ordinal: usize) -> Span {
    tracing::info_span!(
        "graph.fanout.child",
        kind = kind.label(),
        node,
        ordinal,
        start = tracing::field::Empty,
        outcome = tracing::field::Empty,
    )
}

pub(crate) fn child_admitted(span: &Span, start: ChildStart) {
    let label = start.label();
    span.record("start", label);
    match start {
        ChildStart::Fresh => tracing::info!(parent: span, start = label, "Fan-out child admitted"),
        ChildStart::Restored | ChildStart::Resumed => {
            tracing::info!(parent: span, start = label, "Fan-out child resumed from checkpoint");
        }
    }
}

pub(crate) fn child_finished(span: &Span, outcome: ChildOutcome) {
    span.record("outcome", outcome.label());
    let label = outcome.label();
    match outcome {
        ChildOutcome::Completed => {
            tracing::info!(parent: span, outcome = label, "Fan-out child completed");
        }
        ChildOutcome::Paused => {
            tracing::info!(parent: span, outcome = label, "Fan-out child paused");
        }
        ChildOutcome::Cancelled | ChildOutcome::LeaseLost => {
            tracing::info!(parent: span, outcome = label, "Fan-out child cancelled");
        }
        ChildOutcome::Blocked | ChildOutcome::Failed => {
            tracing::warn!(parent: span, outcome = label, "Fan-out child failed");
        }
    }
}
