"""Scenarios for check_workflow_cache_writes.py.

Each negative case is a step shape that DID, or would, write the Actions cache
from a pull request. Each positive case is the shape the repository uses now.
The suite builds a throwaway tree with enough filler workflows to clear the
checker's floors, so a case fails or passes on its own step alone.
"""

from __future__ import annotations

import io
import pathlib
import sys
import tempfile
import textwrap
import unittest
from contextlib import redirect_stdout

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_workflow_cache_writes as checker  # noqa: E402

PR_AND_PUSH = """\
on:
  pull_request:
  push:
    branches: [main]
"""

MAIN_PUSH = "github.ref == 'refs/heads/main' && github.event_name == 'push'"


def workflow(steps: str, on: str = PR_AND_PUSH) -> str:
    return on + "jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n" + textwrap.indent(textwrap.dedent(steps), "      ")


class CacheWriteChecks(unittest.TestCase):
    def check(self, steps: str, on: str = PR_AND_PUSH, hcl: str = "", action: str | None = None):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            wf = root / ".github" / "workflows"
            wf.mkdir(parents=True)
            for i in range(checker.MINIMUM_WORKFLOWS):
                (wf / f"filler{i}.yml").write_text(workflow("- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n"))
            (wf / "subject.yml").write_text(workflow(steps, on))
            if hcl:
                (root / "deploy" / "ci").mkdir(parents=True)
                (root / "deploy" / "ci" / "x.hcl").write_text(hcl)
            if action is not None:
                (root / ".github" / "actions" / "build-stack-images").mkdir(parents=True)
                (root / ".github" / "actions" / "build-stack-images" / "action.yml").write_text(action)
            out = io.StringIO()
            with redirect_stdout(out):
                code = checker.main(["--root", str(root)])
            return code, out.getvalue()

    def check_raw(self, subject: str, extra: dict | None = None):
        """Like check(), with the whole subject workflow given verbatim."""
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            wf = root / ".github" / "workflows"
            wf.mkdir(parents=True)
            for i in range(checker.MINIMUM_WORKFLOWS):
                (wf / f"filler{i}.yml").write_text(workflow("- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n"))
            (wf / "subject.yml").write_text(subject)
            for fname, text in (extra or {}).items():
                (wf / fname).write_text(text)
            out = io.StringIO()
            with redirect_stdout(out):
                code = checker.main(["--root", str(root)])
            return code, out.getvalue()

    def assertWrites(self, steps, needle, **kw):
        code, out = self.check(steps, **kw)
        self.assertEqual(code, 1, out)
        self.assertIn(needle, out)

    def assertClean(self, steps, **kw):
        code, out = self.check(steps, **kw)
        self.assertEqual(code, 0, out)

    # ── setup-go ──────────────────────────────────────────────────────────
    def test_setup_go_default_cache_is_refused(self):
        self.assertWrites("- uses: actions/setup-go@v7\n  with: {go-version-file: go.work, cache-dependency-path: go.sum}\n", "cache: false")

    def test_setup_go_cache_true_is_refused(self):
        self.assertWrites("- uses: actions/setup-go@v7\n  with: {go-version: '1.26', cache: true}\n", "cache: false")

    def test_setup_go_cache_false_passes(self):
        self.assertClean("- uses: actions/setup-go@v7\n  with: {go-version: '1.26', cache: false}\n")

    # ── actions/cache ─────────────────────────────────────────────────────
    def test_full_actions_cache_without_condition_is_refused(self):
        self.assertWrites("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", "actions/cache saves the cache")

    def test_actions_cache_gated_on_ref_only_is_refused(self):
        # A schedule or dispatch on main is not a push to main.
        self.assertWrites("- uses: actions/cache@v4\n  if: github.ref == 'refs/heads/main'\n  with: {path: x, key: k}\n", "actions/cache saves the cache")

    def test_actions_cache_save_without_condition_is_refused(self):
        self.assertWrites("- uses: actions/cache/save@v4\n  with: {path: x, key: k}\n", "actions/cache/save saves the cache")

    def test_actions_cache_gated_on_main_push_passes(self):
        self.assertClean(f"- uses: actions/cache@v4\n  if: {MAIN_PUSH}\n  with: {{path: x, key: k}}\n")

    def test_actions_cache_in_release_only_workflow_passes(self):
        self.assertClean("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", on="on:\n  push:\n    branches: [main, next]\n")

    def test_actions_cache_in_push_to_any_branch_is_refused(self):
        self.assertWrites("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", "actions/cache saves", on="on:\n  push:\n")

    # ── BuildKit cache-to ─────────────────────────────────────────────────
    def test_cache_to_min_on_pull_request_is_refused(self):
        # The ci-image-scan shape this change replaced.
        self.assertWrites(
            "- uses: docker/build-push-action@v7\n  with:\n    cache-to: type=gha,mode=${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'max' || 'min' }},scope=s\n",
            "cache-to must be empty",
        )

    def test_cache_to_literal_is_refused(self):
        self.assertWrites("- uses: docker/build-push-action@v7\n  with: {cache-to: 'type=gha,mode=max,scope=s'}\n", "cache-to must be empty")

    def test_cache_to_gated_passes(self):
        self.assertClean(
            "- uses: docker/build-push-action@v7\n  with:\n    cache-to: ${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'type=gha,mode=max,scope=s' || '' }}\n"
        )

    def test_bake_set_cache_to_mode_variable_is_refused(self):
        # The helm-install-smoke LAYER_MODE shape this change replaced.
        self.assertWrites(
            "- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n    set: |\n      a.cache-to=type=gha,mode=${{ env.LAYER_MODE }},scope=a\n",
            "writes the layer cache from every run",
        )

    def test_bake_of_docker_bake_hcl_without_override_is_refused(self):
        self.assertWrites("- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n", "keeps its mode=max cache-to")

    def test_bake_set_gated_passes(self):
        self.assertClean(
            "- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n    set: |\n"
            "      a.cache-to=${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'type=gha,mode=max,scope=a' || '' }}\n"
        )

    def test_buildx_bake_run_without_clear_is_refused(self):
        self.assertWrites("- run: docker buildx bake --load elitea-x\n", "add `--set '*.cache-to='`")

    def test_buildx_bake_run_with_clear_passes(self):
        self.assertClean("- run: docker buildx bake --set '*.cache-to=' --load elitea-x\n")

    def test_buildx_bake_print_passes(self):
        self.assertClean("- run: docker buildx bake --print > /dev/null\n")

    def test_cache_to_in_release_only_workflow_passes(self):
        self.assertClean("- uses: docker/build-push-action@v7\n  with: {cache-to: 'type=gha,mode=max,scope=s'}\n", on="on:\n  push:\n    branches: [main]\n")

    def test_cache_to_in_ci_hcl_is_refused(self):
        self.assertWrites("- run: 'true'\n", "deploy/ci HCL", hcl='target "a" {\n  cache-to = ["type=gha,mode=min,scope=a"]\n}\n')

    # ── Rust, golangci-lint, node, python, trivy ──────────────────────────
    def test_rust_cache_without_save_if_is_refused(self):
        self.assertWrites("- uses: Swatinem/rust-cache@v2\n  with: {workspaces: x}\n", "save-if")

    def test_rust_cache_with_save_if_passes(self):
        self.assertClean("- uses: Swatinem/rust-cache@v2\n  with:\n    save-if: ${{ github.ref == 'refs/heads/main' }}\n")

    def test_rust_cache_with_save_if_false_passes(self):
        self.assertClean("- uses: Swatinem/rust-cache@v2\n  with: {save-if: false}\n")

    def test_golangci_lint_default_cache_is_refused(self):
        self.assertWrites("- uses: golangci/golangci-lint-action@v9\n  with: {version: latest}\n", "golangci-lint-action saves")

    def test_golangci_lint_skip_save_gated_passes(self):
        self.assertClean(f"- uses: golangci/golangci-lint-action@v9\n  with:\n    skip-save-cache: ${{{{ !({MAIN_PUSH}) }}}}\n")

    def test_setup_node_cache_npm_is_refused(self):
        self.assertWrites("- uses: actions/setup-node@v7\n  with: {node-version: '26', cache: npm, package-manager-cache: false}\n", "setup-node `cache:`")

    def test_setup_node_automatic_cache_is_refused(self):
        self.assertWrites("- uses: actions/setup-node@v7\n  with: {node-version: '26'}\n", "package-manager-cache: false")

    def test_setup_node_without_cache_passes(self):
        self.assertClean("- uses: actions/setup-node@v7\n  with: {node-version: '26', package-manager-cache: false}\n")

    def test_setup_python_cache_pip_is_refused(self):
        self.assertWrites("- uses: actions/setup-python@v7\n  with: {python-version: '3.12', cache: pip}\n", "setup-python `cache:`")

    def test_trivy_default_cache_is_refused(self):
        self.assertWrites("- uses: aquasecurity/trivy-action@v0.36.0\n  with: {image-ref: x}\n", "trivy-action caches")

    def test_trivy_cache_false_passes(self):
        self.assertClean("- uses: aquasecurity/trivy-action@v0.36.0\n  with: {image-ref: x, cache: 'false'}\n")

    # ── build-stack-images ────────────────────────────────────────────────
    def test_stack_images_cache_write_true_is_refused(self):
        self.assertWrites("- uses: ./.github/actions/build-stack-images\n  with: {cache-write: 'true'}\n", "cache-write")

    def test_stack_images_default_ref_only_is_refused(self):
        action = "inputs:\n  cache-write:\n    default: ${{ github.ref == 'refs/heads/main' }}\nruns:\n  using: composite\n  steps: []\n"
        self.assertWrites("- run: 'true'\n", "`cache-write` default", action=action)

    def test_stack_images_default_main_push_passes(self):
        action = f"inputs:\n  cache-write:\n    default: ${{{{ {MAIN_PUSH} }}}}\nruns:\n  using: composite\n  steps: []\n"
        self.assertClean("- uses: ./.github/actions/build-stack-images\n  with: {cache-write: 'false'}\n", action=action)

    # ── the floors ────────────────────────────────────────────────────────
    def test_stack_images_two_mount_writers_for_one_suffix_are_refused(self):
        two = PR_AND_PUSH + textwrap.dedent("""\
            jobs:
              a:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust, mount-cache-writer: "true"}
              b:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust, mount-cache-writer: "true"}
            """)
        code, out = self.check_raw(two)
        self.assertEqual(code, 1, out)
        self.assertIn("exactly one job in the repository", out)

    def test_stack_images_one_mount_writer_per_suffix_passes(self):
        ok = PR_AND_PUSH + textwrap.dedent("""\
            jobs:
              a:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust, mount-cache-writer: "true"}
              b:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: e2e, mount-cache-writer: "true"}
              c:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust}
            """)
        code, out = self.check_raw(ok)
        self.assertEqual(code, 0, out)

    def test_stack_images_mount_writers_in_two_workflows_for_one_suffix_are_refused(self):
        # The cache key is repository-wide, so a second writer in ANOTHER file races too.
        one = PR_AND_PUSH + textwrap.dedent("""\
            jobs:
              a:
                runs-on: ubuntu-latest
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust, mount-cache-writer: "true"}
            """)
        code, out = self.check_raw(one, extra={"other.yml": one})
        self.assertEqual(code, 1, out)
        self.assertIn("exactly one job in the repository", out)
        self.assertIn("other.yml", out)

    def test_stack_images_mount_writer_on_a_matrix_job_is_refused(self):
        matrix = PR_AND_PUSH + textwrap.dedent("""\
            jobs:
              a:
                runs-on: ubuntu-latest
                strategy:
                  matrix: {leg: [1, 2]}
                steps:
                  - uses: ./.github/actions/build-stack-images
                    with: {mount-cache-suffix: rust, mount-cache-writer: "true"}
            """)
        code, out = self.check_raw(matrix)
        self.assertEqual(code, 1, out)
        self.assertIn("matrix job", out)

    def test_stack_images_mount_writer_expression_is_refused(self):
        self.assertWrites("- uses: ./.github/actions/build-stack-images\n  with: {mount-cache-writer: '${{ strategy.job-index == 0 }}'}\n", "literal")

    def test_an_empty_tree_is_refused_not_passed(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = io.StringIO()
            with redirect_stdout(out):
                code = checker.main(["--root", tmp])
            self.assertEqual(code, 1)
            self.assertIn("this check read nothing", out.getvalue())

    def test_the_repository_itself_is_clean(self):
        out = io.StringIO()
        with redirect_stdout(out):
            code = checker.main([])
        self.assertEqual(code, 0, out.getvalue())


if __name__ == "__main__":
    unittest.main()
