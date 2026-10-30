//! The command's run caps (ADR-0041 decision 3, #1498).
//!
//! Main puts `WorkerCommandV1.run_caps` (field 69) on a signed agent command
//! once an administrator saved the platform configuration section
//! `run_caps` with a per-tier cap: the root agent's tier caps, lowered by its
//! author's settings. Absent is the worker's defaults
//! ([`RunCapLimits::worker_default`]: 200 tool calls, no other cap). 0 is no
//! cap. One meter serves every path of the execution; a continuation is a
//! new execution and starts at zero.

use std::sync::Arc;

use elitea_agent_runtime::run_caps::{RunCapLimits, RunCapsMeter};

use super::elitea::runtime::v1::WorkerCommandV1;

/// The command's limits.
#[must_use]
pub fn limits_from_command(command: &WorkerCommandV1) -> RunCapLimits {
    command
        .run_caps
        .as_ref()
        .map_or_else(RunCapLimits::worker_default, |caps| RunCapLimits {
            max_tool_calls: caps.max_tool_calls,
            max_model_turns: caps.max_model_turns,
            max_records_read: caps.max_records_read,
        })
}

/// A fresh meter for one execution of the command.
#[must_use]
pub fn meter_from_command(command: &WorkerCommandV1) -> Arc<RunCapsMeter> {
    RunCapsMeter::new(limits_from_command(command))
}

#[cfg(test)]
mod tests {
    use super::super::elitea::runtime::v1::WorkerRunCapsV1;
    use super::*;

    #[test]
    fn absent_is_the_worker_default() {
        let limits = limits_from_command(&WorkerCommandV1::default());
        assert_eq!(limits, RunCapLimits::worker_default());
        assert_eq!(limits.max_tool_calls, 200);
    }

    #[test]
    fn present_caps_are_taken_as_given() {
        let command = WorkerCommandV1 {
            run_caps: Some(WorkerRunCapsV1 {
                max_tool_calls: 0,
                max_model_turns: 40,
                max_records_read: 5_000,
            }),
            ..WorkerCommandV1::default()
        };
        assert_eq!(
            limits_from_command(&command),
            RunCapLimits {
                max_tool_calls: 0,
                max_model_turns: 40,
                max_records_read: 5_000,
            }
        );
        let meter = meter_from_command(&command);
        assert_eq!(meter.limits().max_model_turns, 40);
        assert_eq!(meter.tripped(), None);
    }
}
