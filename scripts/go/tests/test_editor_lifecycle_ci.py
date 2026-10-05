"""Test the CI gate without starting PostgreSQL or reading credentials."""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/go/editor-lifecycle-postgres.sh"
spec = importlib.util.spec_from_file_location(
    "editor_gate", ROOT / "scripts/go/editor-lifecycle-gate.py"
)
GATE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(GATE)


def passed_events():
    return [
        {"Package": GATE.PACKAGE, "Test": name, "Action": "pass"}
        for name in (*GATE.SCENARIOS, *GATE.PARENTS)
    ] + [{"Package": GATE.PACKAGE, "Action": "pass"}]


def encoded(events):
    return (json.dumps(event) for event in events)


class EditorLifecycleGateTests(unittest.TestCase):
    def test_requires_existing_nine_and_four_empty_stop_results(self):
        result = GATE.check_events(encoded(passed_events()))
        self.assertEqual(result["scenarios"], 13)
        self.assertEqual(result["scenario_groups"], {"lifecycle": 9, "empty_stop": 4})
        self.assertEqual(set(GATE.EMPTY_STOP_SCENARIOS), {
            "TestEditorEmptyStopPostgres/empty_cancelled_receipt_survives_fresh_history_and_replay",
            "TestEditorEmptyStopPostgres/actor_project_and_generation_fences_remain",
            "TestEditorEmptyStopPostgres/ordinary_empty_stop_keeps_deletion_and_durable_replay",
            "TestEditorEmptyStopPostgres/successful_terminal_without_pause_cannot_be_stopped",
        })

    def test_old_nine_scenario_receipt_does_not_satisfy_empty_stop_acceptance(self):
        old_receipt = [event for event in passed_events()
                       if not event.get("Test", "").startswith("TestEditorEmptyStopPostgres")]
        with self.assertRaises(ValueError):
            GATE.check_events(encoded(old_receipt))

    def test_each_empty_stop_case_and_parent_must_pass_exactly_once(self):
        for name in (*GATE.EMPTY_STOP_SCENARIOS, "TestEditorEmptyStopPostgres"):
            complete = passed_events()
            missing = [event for event in complete if event.get("Test") != name]
            duplicate = complete + [{"Package": GATE.PACKAGE, "Test": name, "Action": "pass"}]
            for events in (missing, duplicate):
                with self.subTest(name=name, events=len(events)), self.assertRaises(ValueError):
                    GATE.check_events(encoded(events))

    def test_each_empty_stop_skip_or_failure_refuses_acceptance(self):
        for name in GATE.EMPTY_STOP_SCENARIOS:
            for action in ("skip", "fail"):
                events = passed_events() + [{"Package": GATE.PACKAGE, "Test": name, "Action": action}]
                with self.subTest(name=name, action=action), self.assertRaises(ValueError):
                    GATE.check_events(encoded(events))

    def test_empty_or_missing_or_duplicate_scenarios_fail(self):
        for events in ([], passed_events()[1:], passed_events() + [passed_events()[0]]):
            with self.subTest(events=len(events)):
                with self.assertRaises(ValueError):
                    GATE.check_events(encoded(events))

    def test_any_failure_or_skip_fails_even_when_all_required_cases_pass(self):
        for action in ("fail", "skip"):
            events = passed_events() + [
                {"Package": GATE.PACKAGE, "Test": "TestEditorLifecycleExtra", "Action": action}
            ]
            with self.subTest(action=action), self.assertRaises(ValueError):
                GATE.check_events(encoded(events))

    def test_foreign_or_malformed_events_fail(self):
        for lines in (["not-json"], encoded([{"Package": "another/package", "Action": "pass"}])):
            with self.assertRaises(ValueError):
                GATE.check_events(lines)

    def test_missing_package_completion_fails(self):
        with self.assertRaises(ValueError):
            GATE.check_events(encoded(passed_events()[:-1]))

    def test_workflow_owns_exact_disposable_service_and_required_gate(self):
        workflow = (ROOT / ".github/workflows/ci-go.yml").read_text()
        job = workflow.split("  editor-lifecycle:\n", 1)[1].split("  integration:\n", 1)[0]
        for text in (
            "image: postgres:18", "POSTGRES_DB: elitea_it_editor_lifecycle_20261002",
            "127.0.0.1:15444:5432", "timeout-minutes: 10", 'ELITEA_REQUIRE_EDITOR_POSTGRES_TEST: "true"',
            "@127.0.0.1:15444/elitea_it_editor_lifecycle_20261002?sslmode=disable",
            "bash scripts/go/editor-lifecycle-postgres.sh",
        ):
            self.assertIn(text, job)
        self.assertNotIn('ELITEA_TEST_DATABASE_URL:', job)
        ledger = (ROOT / "scripts/go/declared-skips.txt").read_text()
        for name in ("TestEditorLifecyclePostgres", "TestEditorLifecyclePostgresTraceReceiptIdentity", "TestEditorLifecyclePostgresTracePagination"):
            self.assertIn(f"{GATE.PACKAGE}\t{name}\tThe editor-lifecycle job", ledger)


class EditorLifecycleRunnerTests(unittest.TestCase):
    def run_script(self, events=None, status=0, configured=True):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fake_go = directory / "go"
            fake_go.write_text(
                f"#!{sys.executable}\n"
                "import json,os,sys\n"
                "from pathlib import Path\n"
                "record={'args':sys.argv[1:],'required':os.environ.get('ELITEA_REQUIRE_EDITOR_POSTGRES_TEST'),'gomaxprocs':os.environ.get('GOMAXPROCS'),'service_vars_present':any(key in os.environ for key in ('ELITEA_TEST_DATABASE_URL','ELITEA_TEST_USE_SERVICE_DATABASE_URL','DATABASE_URL'))}\n"
                "Path(os.environ['EDITOR_GATE_TEST_RECORD']).write_text(json.dumps(record))\n"
                "for event in json.loads(os.environ['EDITOR_GATE_TEST_EVENTS']): print(json.dumps(event))\n"
                "sys.exit(int(os.environ['EDITOR_GATE_TEST_STATUS']))\n"
            )
            fake_go.chmod(0o700)
            environment = dict(os.environ)
            environment.pop("ELITEA_EDITOR_TEST_DATABASE_URL", None)
            environment.update(
                PATH=str(directory) + os.pathsep + environment.get("PATH", ""),
                ELITEA_EDITOR_TEST_ARTIFACT_DIR=str(directory / "artifacts"),
                EDITOR_GATE_TEST_RECORD=str(directory / "record.json"),
                EDITOR_GATE_TEST_EVENTS=json.dumps(passed_events() if events is None else events),
                EDITOR_GATE_TEST_STATUS=str(status),
                ELITEA_TEST_DATABASE_URL="must-not-reach-editor-process",
                ELITEA_TEST_USE_SERVICE_DATABASE_URL="must-not-reach-editor-process",
                DATABASE_URL="must-not-reach-editor-process",
                ELITEA_REQUIRE_EDITOR_POSTGRES_TEST="false",
                GOMAXPROCS="99",
            )
            if configured:
                environment["ELITEA_EDITOR_TEST_DATABASE_URL"] = (
                    "postgres://ci-fixture@127.0.0.1:15444/elitea_it_editor_lifecycle_20261002?sslmode=disable"
                )
            result = subprocess.run(["bash", str(SCRIPT)], env=environment, text=True, capture_output=True, timeout=10)
            record_path = directory / "record.json"
            record = json.loads(record_path.read_text()) if record_path.exists() else None
            return result, record

    def test_runner_clears_product_sources_and_bounds_actual_go_command(self):
        result, record = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["scenarios"], 13)
        self.assertFalse(record["service_vars_present"])
        self.assertEqual(record["required"], "true")
        self.assertEqual(record["gomaxprocs"], "2")
        self.assertEqual(record["args"], ["test", "-p", "2", "-race", "-json", "-count=1", "-timeout=5m", "./internal/infra/db/repos", "-run", "^(TestEditorLifecycle.*|TestEditorEmptyStopPostgres)$"])

    def test_missing_explicit_url_fails_before_invoking_go(self):
        result, record = self.run_script(configured=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(record)

    def test_failed_go_command_is_not_masked_by_json_gate(self):
        result, _ = self.run_script(status=3)
        self.assertEqual(result.returncode, 3)

    def test_successful_go_with_skip_or_empty_or_missing_stop_result_still_fails(self):
        old_receipt = [event for event in passed_events()
                       if not event.get("Test", "").startswith("TestEditorEmptyStopPostgres")]
        for events in ([], old_receipt, passed_events() + [{"Package": GATE.PACKAGE, "Action": "skip", "Test": "TestEditorLifecycleExtra"}]):
            with self.subTest(events=len(events)):
                result, _ = self.run_script(events=events)
                self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
