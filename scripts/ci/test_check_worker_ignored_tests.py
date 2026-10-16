"""Scenarios for check_worker_ignored_tests.py: it must catch an unrun ignored test."""
from __future__ import annotations

import io
import sys
import tempfile
import textwrap
import unittest
from contextlib import redirect_stdout
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_worker_ignored_tests as guard  # noqa: E402

WORKFLOW = """
jobs:
  test:
    steps:
      - name: Plain run
        run: cargo test --locked --all-targets --all-features
      - name: Deadlines
        run: cargo test --locked --lib --all-features sandbox::docker_supervisor::deadline_tests -- --ignored
      - name: Postgres-only
        run: |
          cargo test --locked --lib --all-features -- --ignored --skip sandbox_supervisor_ \\
            state::postgres_checkpointer_tests:: \\
            agents::graph::
      - name: Exact
        run: cargo test --locked --lib --all-features -p worker one::exact -- --ignored --exact  # comment
"""

LIST = """\
     Running unittests src/lib.rs (target/debug/deps/worker-1)
sandbox::docker_supervisor::deadline_tests::a: test
state::postgres_checkpointer_tests::journal: test
state::postgres_checkpointer_tests::sandbox_supervisor_mtls_submission: test
agents::graph::wiring::b: test
one::exact: test
manual::probe: test
"""

ALLOW = """\
# comment
state::postgres_checkpointer_tests::sandbox_supervisor_  # runs in the Docker step of ci-rust.yml
manual::probe  # manual release-profile timing probe, see docs/x.md
"""


def with_step(step: str, workflow: str = WORKFLOW) -> str:
    """Append one step (YAML, indented as a list item) to the test job."""
    return workflow + textwrap.indent(textwrap.dedent(step).strip("\n") + "\n", "      ")


def run(workflow: str = WORKFLOW, listing: str = LIST, allow: str = ALLOW,
        sources: dict[str, str] | None = None) -> tuple[int, str]:
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        (root / "ci.yml").write_text(workflow)
        (root / "list.txt").write_text(listing)
        (root / "allow.txt").write_text(allow)
        arguments = ["--list", str(root / "list.txt"), "--workflow", str(root / "ci.yml"),
                     "--allowlist", str(root / "allow.txt")]
        if sources is not None:
            for relative, text in sources.items():
                path = root / "crate" / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)
            arguments += ["--sources", str(root / "crate")]
        out = io.StringIO()
        with redirect_stdout(out):
            code = guard.main(arguments)
        return code, out.getvalue()


UNRUN = "     Running unittests src/lib.rs (x)\nunrun::test: test\n"


class IgnoredTestGuard(unittest.TestCase):
    def test_every_ignored_test_is_run_or_allowlisted(self):
        code, out = run()
        self.assertEqual(code, 0, out)
        self.assertIn("6 ignored test(s): 4 run by 3 CI invocation(s), 2 allowlisted, "
                      "0 run by nothing", out)

    def test_a_new_unrun_ignored_test_fails(self):
        code, out = run(listing=LIST + "sandbox::ledger::new_owner_test: test\n")
        self.assertEqual(code, 1)
        self.assertIn("sandbox::ledger::new_owner_test is #[ignore] and no CI", out)

    def test_skip_excludes_a_test_the_filter_would_select(self):
        code, out = run(allow="manual::probe  # manual release-profile timing probe, docs/x.md\n")
        self.assertEqual(code, 1)
        self.assertIn("sandbox_supervisor_mtls_submission is #[ignore]", out)

    def test_exact_does_not_select_by_substring(self):
        code, out = run(listing=LIST + "one::exact_longer: test\n")
        self.assertEqual(code, 1)
        self.assertIn("one::exact_longer is #[ignore]", out)

    def test_an_invocation_without_ignored_selects_nothing(self):
        workflow = with_step("- run: cargo test --locked --lib manual::probe")
        code, out = run(workflow=workflow, allow=ALLOW.replace("manual::probe", "manual::other"))
        self.assertEqual(code, 1)
        self.assertIn("manual::probe is #[ignore]", out)

    def test_the_ledger_wrapper_counts_as_an_invocation(self):
        workflow = with_step("""
            - run: python3 scripts/runtime/test_sandbox_ledger.py --test-filter sandbox_supervisor_mtls --adapter-image
        """)
        allow = "manual::probe  # manual release-profile timing probe, docs/x.md\n"
        code, out = run(workflow=workflow, allow=allow)
        self.assertEqual(code, 0, out)

    def test_the_list_command_is_not_an_invocation(self):
        workflow = with_step("- run: cargo test --locked --lib --all-features -- --list --ignored > out.txt")
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1)
        self.assertIn("unrun::test is #[ignore]", out)

    # --- invocations that look like coverage but never run, or never fail -----

    def test_a_yaml_commented_out_step_is_not_credited(self):
        # The reviewer's case: the whole step commented out in the workflow.
        workflow = WORKFLOW + "      # - run: cargo test --lib unrun::test -- --ignored\n"
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("unrun::test is #[ignore]", out)

    def test_a_shell_comment_inside_run_is_not_credited(self):
        workflow = with_step("""
            - name: Disabled inside the script
              run: |
                echo skipped
                # cargo test --lib unrun::test -- --ignored
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("unrun::test is #[ignore]", out)

    def test_a_step_name_mentioning_the_command_is_not_credited(self):
        workflow = with_step("""
            - name: "cargo test --lib unrun::test -- --ignored"
              run: echo nothing
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)

    def test_an_if_false_step_is_not_credited(self):
        workflow = with_step("""
            - if: false
              run: cargo test --lib unrun::test -- --ignored
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("not credited (step runs only `if: False`)", out)

    def test_an_if_failure_step_is_not_credited(self):
        workflow = with_step("""
            - if: ${{ failure() }}
              run: cargo test --lib unrun::test -- --ignored
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)

    def test_an_unconditional_if_is_credited(self):
        workflow = with_step("""
            - if: ${{ !cancelled() }}
              run: cargo test --lib unrun::test -- --ignored
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 0, out)

    def test_continue_on_error_is_not_credited(self):
        workflow = with_step("""
            - continue-on-error: true
              run: cargo test --lib unrun::test -- --ignored
        """)
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("continue-on-error", out)

    def test_a_job_level_condition_is_not_credited(self):
        workflow = WORKFLOW + textwrap.indent(textwrap.dedent("""
            optional:
              if: github.event_name == 'schedule'
              steps:
                - run: cargo test --lib unrun::test -- --ignored
        """).lstrip("\n"), "  ")
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("job runs only", out)

    def test_or_true_is_not_credited(self):
        workflow = with_step("- run: cargo test --lib unrun::test -- --ignored || true")
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)
        self.assertIn("its failure is tolerated", out)

    def test_or_true_on_the_ledger_wrapper_is_not_credited(self):
        workflow = with_step(
            "- run: python3 scripts/runtime/test_sandbox_ledger.py --test-filter unrun || echo ok")
        code, out = run(workflow=workflow, listing=LIST + "unrun::test: test\n")
        self.assertEqual(code, 1, out)

    # --- target selection --------------------------------------------------------

    def test_lib_does_not_credit_an_integration_test(self):
        listing = LIST + "     Running tests/contract.rs (x)\nunrun::test: test\n"
        workflow = with_step("- run: cargo test --lib unrun::test -- --ignored")
        code, out = run(workflow=workflow, listing=listing)
        self.assertEqual(code, 1, out)
        self.assertIn("unrun::test [test:contract] is #[ignore]", out)

    def test_test_name_credits_only_that_integration_test(self):
        listing = LIST + ("     Running tests/contract.rs (x)\nunrun::test: test\n"
                          "     Running tests/other.rs (x)\nunrun::test: test\n")
        workflow = with_step("- run: cargo test --test contract unrun::test -- --ignored")
        code, out = run(workflow=workflow, listing=listing)
        self.assertEqual(code, 1, out)
        self.assertIn("unrun::test [test:other] is #[ignore]", out)
        self.assertNotIn("[test:contract] is #[ignore]", out)

    def test_no_selector_credits_every_target(self):
        listing = LIST + "     Running tests/contract.rs (x)\nunrun::test: test\n"
        workflow = with_step("- run: cargo test unrun::test -- --ignored")
        code, out = run(workflow=workflow, listing=listing)
        self.assertEqual(code, 0, out)

    # --- allowlist and list integrity --------------------------------------------

    def test_a_stale_allowlist_entry_fails(self):
        code, out = run(allow=ALLOW + "gone::test  # used to run manually somewhere else\n")
        self.assertEqual(code, 1)
        self.assertIn("allowlist entry gone::test matches no ignored test", out)

    def test_an_allowlist_entry_needs_a_reason(self):
        code, out = run(allow=ALLOW + "manual::probe  # todo\n")
        self.assertEqual(code, 1)
        self.assertIn("the reason must say where the test runs", out)

    def test_an_empty_list_cannot_pass_against_real_sources(self):
        sources = {"src/a.rs": '#[test]\n#[ignore = "x"]\nfn a() {}\n    #[ignore]\nfn b() {}\n'}
        code, out = run(listing="", allow="", sources=sources)
        self.assertEqual(code, 1)
        self.assertIn("the sources carry 2 #[ignore] attribute(s) but the test list names 0", out)

    def test_source_count_matching_the_list_passes(self):
        body = "".join('#[test]\n#[ignore = "x"]\nfn t() {}\n' for _ in range(6))
        code, out = run(sources={"src/a.rs": body})
        self.assertEqual(code, 0, out)

    def test_no_invocation_at_all_fails(self):
        code, out = run(workflow="jobs:\n  t:\n    steps:\n      - run: cargo test --locked\n",
                        listing="", allow="")
        self.assertEqual(code, 1)
        self.assertIn("no `cargo test ... -- --ignored` invocation found", out)

    def test_two_cargo_test_names_are_refused(self):
        workflow = with_step("""
            - run: |
                cargo test --lib a:: b:: -- --ignored
        """)
        with self.assertRaises(SystemExit) as raised:
            run(workflow=workflow)
        self.assertIn("cargo takes one test name before `--`", str(raised.exception))

    def test_a_file_that_is_not_a_workflow_is_refused(self):
        with self.assertRaises(SystemExit) as raised:
            run(workflow="run: cargo test --lib x -- --ignored\n")
        self.assertIn("not a workflow", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
