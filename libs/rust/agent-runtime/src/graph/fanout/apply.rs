//! The per-interrupt apply rule (contract §7, design §5.3), as a pure function.
//!
//! The child's own latest checkpoint is the only proof that a decision was
//! applied. Main holds the decision; the parent holds nothing. Applying is a
//! no-op once the child has moved past the pause, so duplicate fetches,
//! restore fetches and retries are all safe.

/// One card a paused child published.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CardIdentity {
    /// Worker-minted, opaque to Main (contract §3.1).
    pub interrupt_key: String,
    /// The child-local interrupt id (contract §3.2).
    pub interrupt_id: String,
    /// The child checkpoint the pause was saved at.
    pub child_paused_checkpoint_id: String,
}

/// What the child's latest checkpoint says, read at apply time.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ChildCheckpointView {
    /// No checkpoint, or one mid-run (process died while it ran).
    Running { checkpoint_id: Option<String> },
    /// Paused at `checkpoint_id` with these interrupt ids.
    Paused {
        checkpoint_id: String,
        interrupt_ids: Vec<String>,
    },
    /// Terminal (completed or failed) at `checkpoint_id`.
    Terminal { checkpoint_id: String },
}

/// The typed apply outcome (2a-2 task 7).
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ApplyOutcome {
    /// The proof holds: resume the child with this decision.
    Apply,
    /// The child already moved past this pause (applied before a crash):
    /// ACK `applied` with the child's current checkpoint, resume nothing.
    AlreadyApplied { child_checkpoint_id: String },
    /// The card can no longer apply: ACK `stale`, resume nothing.
    Stale,
    /// The child is running (a multi-card Agent child between pauses): keep
    /// the decision until the child pauses again or finishes.
    Held,
}

/// Classify one decision for `card` against the child's latest checkpoint.
///
/// `card` is `None` when the decision names a key this activation never
/// published (another occurrence, a superseded card): always stale.
#[must_use]
pub fn classify(card: Option<&CardIdentity>, latest: &ChildCheckpointView) -> ApplyOutcome {
    let Some(card) = card else {
        return ApplyOutcome::Stale;
    };
    match latest {
        ChildCheckpointView::Running { .. } => ApplyOutcome::Held,
        ChildCheckpointView::Paused {
            checkpoint_id,
            interrupt_ids,
        } if *checkpoint_id == card.child_paused_checkpoint_id => {
            if interrupt_ids.contains(&card.interrupt_id) {
                ApplyOutcome::Apply
            } else {
                // Same checkpoint, different pause: not this card's pause.
                ApplyOutcome::Stale
            }
        }
        // Paused again later, or finished: this pause was already answered.
        ChildCheckpointView::Paused { checkpoint_id, .. }
        | ChildCheckpointView::Terminal { checkpoint_id } => ApplyOutcome::AlreadyApplied {
            child_checkpoint_id: checkpoint_id.clone(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn card() -> CardIdentity {
        CardIdentity {
            interrupt_key: "ik-1".to_owned(),
            interrupt_id: "card-1".to_owned(),
            child_paused_checkpoint_id: "cp-pause".to_owned(),
        }
    }

    fn paused(checkpoint: &str, ids: &[&str]) -> ChildCheckpointView {
        ChildCheckpointView::Paused {
            checkpoint_id: checkpoint.to_owned(),
            interrupt_ids: ids.iter().map(|id| (*id).to_owned()).collect(),
        }
    }

    #[test]
    fn the_exact_pause_applies() {
        assert_eq!(
            classify(Some(&card()), &paused("cp-pause", &["card-1", "card-2"])),
            ApplyOutcome::Apply
        );
    }

    #[test]
    fn a_child_past_the_pause_acks_applied_with_its_current_checkpoint() {
        assert_eq!(
            classify(Some(&card()), &paused("cp-later", &["card-9"])),
            ApplyOutcome::AlreadyApplied {
                child_checkpoint_id: "cp-later".to_owned()
            }
        );
        assert_eq!(
            classify(
                Some(&card()),
                &ChildCheckpointView::Terminal {
                    checkpoint_id: "cp-end".to_owned()
                }
            ),
            ApplyOutcome::AlreadyApplied {
                child_checkpoint_id: "cp-end".to_owned()
            }
        );
    }

    #[test]
    fn a_wrong_pause_or_a_foreign_card_is_stale() {
        assert_eq!(
            classify(Some(&card()), &paused("cp-pause", &["card-2"])),
            ApplyOutcome::Stale
        );
        assert_eq!(
            classify(None, &paused("cp-pause", &["card-1"])),
            ApplyOutcome::Stale
        );
    }

    #[test]
    fn a_running_child_holds_the_decision() {
        assert_eq!(
            classify(
                Some(&card()),
                &ChildCheckpointView::Running {
                    checkpoint_id: Some("cp-mid".to_owned())
                }
            ),
            ApplyOutcome::Held
        );
    }
}
