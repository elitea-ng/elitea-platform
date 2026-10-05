#!/usr/bin/env python3
"""Require actual editor acceptance events, without skipped or missing scenarios."""
import json
import sys
from collections import Counter
from pathlib import Path

PACKAGE = "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
LIFECYCLE_SCENARIOS = (
    "TestEditorLifecyclePostgres/atomic_scope_immutable_identity_and_saved_version",
    "TestEditorLifecyclePostgres/participant_failure_rolls_back_atomic_initial_scope",
    "TestEditorLifecyclePostgres/normal_list_excludes_persisted_test_scope",
    "TestEditorLifecyclePostgres/competing_exact_admission_is_one_atomic_effect",
    "TestEditorLifecyclePostgres/wrong_saved_version_admission_rolls_back_all_effects",
    "TestEditorLifecyclePostgres/durable_history_pages_57_real_admissions_without_read_effects",
    "TestEditorLifecyclePostgres/paused_restore_retains_original_control_and_denies_foreign_actor",
    "TestEditorLifecyclePostgresTraceReceiptIdentity",
    "TestEditorLifecyclePostgresTracePagination",
)
EMPTY_STOP_SCENARIOS = (
    "TestEditorEmptyStopPostgres/empty_cancelled_receipt_survives_fresh_history_and_replay",
    "TestEditorEmptyStopPostgres/actor_project_and_generation_fences_remain",
    "TestEditorEmptyStopPostgres/ordinary_empty_stop_keeps_deletion_and_durable_replay",
    "TestEditorEmptyStopPostgres/successful_terminal_without_pause_cannot_be_stopped",
)
SCENARIOS = (*LIFECYCLE_SCENARIOS, *EMPTY_STOP_SCENARIOS)
PARENTS = ("TestEditorLifecyclePostgres", "TestEditorEmptyStopPostgres")


def check_events(lines):
    """Return a measured summary. Reject missing, failed, repeated or skipped tests."""
    passed = Counter()
    package_passes = 0
    events = 0
    for line in lines:
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict) or event.get("Package") != PACKAGE:
            raise ValueError("unexpected editor acceptance event")
        events += 1
        action = event.get("Action")
        if action in ("skip", "fail"):
            raise ValueError("editor acceptance failed or skipped")
        if action == "pass":
            if event.get("Test"):
                passed[event["Test"]] += 1
            else:
                package_passes += 1
    expected = (*SCENARIOS, *PARENTS)
    if not events or package_passes != 1 or any(passed[name] != 1 for name in expected):
        raise ValueError("editor acceptance did not pass every required scenario exactly once")
    return {
        "scenarios": len(SCENARIOS),
        "scenario_groups": {
            "lifecycle": len(LIFECYCLE_SCENARIOS),
            "empty_stop": len(EMPTY_STOP_SCENARIOS),
        },
        "failed": 0,
        "skipped": 0,
        "package": PACKAGE,
    }


def main():
    if len(sys.argv) != 2:
        raise ValueError("expected one editor acceptance JSON log")
    with Path(sys.argv[1]).open(encoding="utf-8") as source:
        result = check_events(source)
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, TypeError, KeyError) as error:
        print(f"editor acceptance gate: {error}", file=sys.stderr)
        raise SystemExit(1)
