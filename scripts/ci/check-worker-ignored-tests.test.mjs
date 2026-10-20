// Scenarios for check-worker-ignored-tests.mjs: it must catch an unrun ignored test.
//
// Ported one for one from test_check_worker_ignored_tests.py; ci-rust.yml
// counts the `test(` lines below and requires node:test to run that many.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { SystemExit, main } from "./check-worker-ignored-tests.mjs";

const WORKFLOW = `
jobs:
  test:
    defaults:
      run:
        working-directory: services/elitea-worker-rust
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
        run: cargo test --locked --lib --all-features -p elitea-worker-rust one::exact -- --ignored --exact  # comment
`;

const LIST = `     Running unittests src/lib.rs (target/debug/deps/worker-1)
sandbox::docker_supervisor::deadline_tests::a: test
state::postgres_checkpointer_tests::journal: test
state::postgres_checkpointer_tests::sandbox_supervisor_mtls_submission: test
agents::graph::wiring::b: test
one::exact: test
manual::probe: test
`;

const ALLOW = `# comment
state::postgres_checkpointer_tests::sandbox_supervisor_  # runs in the Docker step of ci-rust.yml
manual::probe  # manual release-profile timing probe, see docs/x.md
`;

/** textwrap.dedent: remove the common leading whitespace of the non-blank lines. */
function dedent(text) {
  const lines = text.split("\n").map((line) => (line.trim() === "" ? "" : line));
  const margins = lines.filter((line) => line !== "").map((line) => /^[ \t]*/.exec(line)[0]);
  let margin = margins.length ? margins[0] : "";
  for (const m of margins) {
    while (!m.startsWith(margin)) margin = margin.slice(0, -1);
  }
  return lines.map((line) => line.slice(margin.length)).join("\n");
}

/** textwrap.indent: prefix every line that is not whitespace only. */
function indent(text, prefix) {
  return text.split(/(?<=\n)/).map((line) => (line.trim() ? prefix + line : line)).join("");
}

const stripNewlines = (text) => text.replace(/^\n+|\n+$/g, "");

/** Append one step (YAML, indented as a list item) to the test job. */
function withStep(step, workflow = WORKFLOW) {
  return workflow + indent(stripNewlines(dedent(step)) + "\n", "      ");
}

function run({ workflow = WORKFLOW, listing = LIST, allow = ALLOW, sources = null } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "worker-ignored-"));
  try {
    fs.writeFileSync(path.join(root, "ci.yml"), workflow);
    fs.writeFileSync(path.join(root, "list.txt"), listing);
    fs.writeFileSync(path.join(root, "allow.txt"), allow);
    const args = ["--list", path.join(root, "list.txt"), "--workflow", path.join(root, "ci.yml"),
      "--allowlist", path.join(root, "allow.txt")];
    if (sources !== null) {
      for (const [relative, text] of Object.entries(sources)) {
        const file = path.join(root, "crate", relative);
        fs.mkdirSync(path.dirname(file), { recursive: true });
        fs.writeFileSync(file, text);
      }
      args.push("--sources", path.join(root, "crate"));
    }
    const out = [];
    const code = main(args, (line) => out.push(`${line}\n`));
    return [code, out.join("")];
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

const UNRUN_LIST = LIST + "unrun::test: test\n";

const raisesSystemExit = (fn, needle) =>
  assert.throws(fn, (error) => error instanceof SystemExit && error.message.includes(needle));

test("every ignored test is run or allowlisted", () => {
  const [code, out] = run();
  assert.equal(code, 0, out);
  assert.ok(out.includes("6 ignored test(s): 4 run by 3 CI invocation(s), 2 allowlisted, 0 run by nothing"), out);
});

test("a new unrun ignored test fails", () => {
  const [code, out] = run({ listing: LIST + "sandbox::ledger::new_owner_test: test\n" });
  assert.equal(code, 1);
  assert.ok(out.includes("sandbox::ledger::new_owner_test is #[ignore] and no CI"), out);
});

test("skip excludes a test the filter would select", () => {
  const [code, out] = run({ allow: "manual::probe  # manual release-profile timing probe, docs/x.md\n" });
  assert.equal(code, 1);
  assert.ok(out.includes("sandbox_supervisor_mtls_submission is #[ignore]"), out);
});

test("exact does not select by substring", () => {
  const [code, out] = run({ listing: LIST + "one::exact_longer: test\n" });
  assert.equal(code, 1);
  assert.ok(out.includes("one::exact_longer is #[ignore]"), out);
});

test("an invocation without --ignored selects nothing", () => {
  const workflow = withStep("- run: cargo test --locked --lib manual::probe");
  const [code, out] = run({ workflow, allow: ALLOW.split("manual::probe").join("manual::other") });
  assert.equal(code, 1);
  assert.ok(out.includes("manual::probe is #[ignore]"), out);
});

test("the ledger wrapper counts as an invocation", () => {
  const workflow = withStep(`
            - run: python3 scripts/runtime/test_sandbox_ledger.py --test-filter sandbox_supervisor_mtls --adapter-image
        `);
  const allow = "manual::probe  # manual release-profile timing probe, docs/x.md\n";
  const [code, out] = run({ workflow, allow });
  assert.equal(code, 0, out);
});

test("the list command is not an invocation", () => {
  const workflow = withStep("- run: cargo test --locked --lib --all-features -- --list --ignored > out.txt");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1);
  assert.ok(out.includes("unrun::test is #[ignore]"), out);
});

// --- invocations that look like coverage but never run, or never fail -----

test("a YAML-commented-out step is not credited", () => {
  // The reviewer's case: the whole step commented out in the workflow.
  const workflow = WORKFLOW + "      # - run: cargo test --lib unrun::test -- --ignored\n";
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("unrun::test is #[ignore]"), out);
});

test("a shell comment inside run is not credited", () => {
  const workflow = withStep(`
            - name: Disabled inside the script
              run: |
                echo skipped
                # cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("unrun::test is #[ignore]"), out);
});

test("a step name mentioning the command is not credited", () => {
  const workflow = withStep(`
            - name: "cargo test --lib unrun::test -- --ignored"
              run: echo nothing
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
});

test("an if: false step is not credited", () => {
  const workflow = withStep(`
            - if: false
              run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("not credited (step runs only `if: False`)"), out);
});

test("an if: failure() step is not credited", () => {
  const workflow = withStep(`
            - if: \${{ failure() }}
              run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
});

test("an unconditional if is credited", () => {
  const workflow = withStep(`
            - if: \${{ !cancelled() }}
              run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 0, out);
});

test("continue-on-error is not credited", () => {
  const workflow = withStep(`
            - continue-on-error: true
              run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("continue-on-error"), out);
});

test("a job-level condition is not credited", () => {
  const workflow = WORKFLOW + indent(dedent(`
            optional:
              if: github.event_name == 'schedule'
              steps:
                - run: cargo test --lib unrun::test -- --ignored
        `).replace(/^\n+/, ""), "  ");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("job runs only"), out);
});

test("|| true is not credited", () => {
  const workflow = withStep("- run: cargo test --lib unrun::test -- --ignored || true");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("its failure is tolerated"), out);
});

test("|| true on the ledger wrapper is not credited", () => {
  const workflow = withStep("- run: python3 scripts/runtime/test_sandbox_ledger.py --test-filter unrun || echo ok");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
});

test("--no-run is not credited", () => {
  const workflow = withStep("- run: cargo test --lib --no-run unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("`--no-run` builds the tests and runs none"), out);
});

test("echo of a cargo command is not credited", () => {
  const workflow = withStep("- run: echo cargo test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("`echo` is the command, not cargo"), out);
});

test("cargo after a separator is its own command", () => {
  const workflow = withStep(`
            - working-directory: .
              run: cd services/elitea-worker-rust && cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 0, out);
});

test("environment assignments before cargo are allowed", () => {
  const workflow = withStep("- run: FOO=1 BAR=x cargo test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 0, out);
});

test("another package is not credited", () => {
  const workflow = withStep("- run: cargo test -p elitea-agent-runtime --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("`-p elitea-agent-runtime` runs another crate's tests"), out);
});

test("the --package= form is read", () => {
  const workflow = withStep("- run: cargo test --package=other --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("`-p other`"), out);
});

test("--skip= is a skip, not a filter", () => {
  // Read as a filter, `--skip=unrun` would credit unrun::test; it skips it.
  const workflow = withStep("- run: cargo test --lib -- --ignored --skip=unrun unrun::test");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("unrun::test is #[ignore]"), out);
});

test("a matrix expression gets a clear message", () => {
  const workflow = withStep("- run: cargo test --lib ${{ matrix.filter }} -- --ignored");
  const [code, out] = run({ workflow });
  assert.equal(code, 0, out);
  assert.ok(out.includes("a GitHub expression (e.g. `${{ matrix.* }}`) chooses its filters"), out);
});

// --- where cargo runs ---------------------------------------------------------

test("a run in another job's directory is not credited", () => {
  const workflow = WORKFLOW + indent(dedent(`
            libs:
              defaults:
                run:
                  working-directory: libs/rust
              steps:
                - run: cargo test --lib unrun::test -- --ignored
        `).replace(/^\n+/, ""), "  ");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("cargo runs in `libs/rust`, not services/elitea-worker-rust"), out);
});

test("a step working-directory overrides the job", () => {
  const workflow = withStep(`
            - working-directory: services/elitea-code-runner
              run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("cargo runs in `services/elitea-code-runner`"), out);
});

test("a cd away from the worker is followed", () => {
  const workflow = withStep("- run: cd ../elitea-vector && cargo test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("cargo runs in `services/elitea-vector`"), out);
});

test("a cd on an earlier line persists", () => {
  const workflow = withStep(`
            - run: |
                cd ..
                cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("cargo runs in `services`"), out);
});

test("a cd to a variable cannot be resolved", () => {
  const workflow = withStep('- run: cd "$CRATE" && cargo test --lib unrun::test -- --ignored');
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("a directory this check cannot resolve"), out);
});

test("env -C moves only that command", () => {
  const workflow = withStep("- run: env -C ../elitea-vector cargo test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("cargo runs in `services/elitea-vector`"), out);
});

test("the workflow default directory is used", () => {
  const workflow = dedent(`
            defaults:
              run:
                working-directory: services/elitea-worker-rust
            jobs:
              t:
                steps:
                  - run: cargo test --lib unrun::test -- --ignored
        `);
  const [code, out] = run({ workflow, listing: "     Running unittests src/lib.rs (x)\nunrun::test: test\n", allow: "" });
  assert.equal(code, 0, out);
});

// --- spellings of -p, refused selectors, background runs -------------------------

test("attached -p forms are read", () => {
  for (const spelling of ["-pelitea-agent-runtime", "-p=elitea-agent-runtime"]) {
    const workflow = withStep(`- run: cargo test ${spelling} --lib unrun::test -- --ignored`);
    const [code, out] = run({ workflow, listing: UNRUN_LIST });
    assert.equal(code, 1, `${spelling}: ${out}`);
    assert.ok(out.includes("`-p elitea-agent-runtime` runs another crate's tests"), out);
  }
  const workflow = withStep("- run: cargo test -pelitea-worker-rust --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 0, out);
});

test("selectors this check does not model are refused", () => {
  for (const [selector, why] of [
    ["--manifest-path ../x/Cargo.toml", "builds the crate a manifest names"],
    ["--doc", "runs doctests only"],
    ["--bench b", "runs a benchmark target"],
    ["--benches", "runs benchmark targets"],
    ["--example e", "runs an example target"],
    ["--examples", "runs example targets"],
  ]) {
    const workflow = withStep(`- run: cargo test ${selector} unrun::test -- --ignored`);
    const [code, out] = run({ workflow, listing: UNRUN_LIST });
    assert.equal(code, 1, `${selector}: ${out}`);
    assert.ok(out.includes(why), out);
  }
});

test("a backgrounded run is not credited", () => {
  const workflow = withStep("- run: cargo test --lib unrun::test -- --ignored &");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 1, out);
  assert.ok(out.includes("its failure is tolerated"), out);
});

// --- valid spellings that must be credited ---------------------------------------

test("redirections are not a background run", () => {
  for (const redirect of ["2>&1 | tee log", "> log 2>&1", "&> log", ">&2", "|& tee log"]) {
    const workflow = withStep(`- run: cargo test --lib unrun::test -- --ignored ${redirect}`);
    const [code, out] = run({ workflow, listing: UNRUN_LIST });
    assert.equal(code, 0, `${redirect}: ${out}`);
  }
});

test("a rustup toolchain override is still cargo test", () => {
  const workflow = withStep("- run: cargo +1.97.1 test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing: UNRUN_LIST });
  assert.equal(code, 0, out);
});

test("env, time and timeout prefixes run cargo", () => {
  for (const prefix of ["env X=1", "env -u Y X=1", "time", "timeout 600", "nice -n 5"]) {
    const workflow = withStep(`- run: ${prefix} cargo test --lib unrun::test -- --ignored`);
    const [code, out] = run({ workflow, listing: UNRUN_LIST });
    assert.equal(code, 0, `${prefix}: ${out}`);
  }
});

// --- target selection --------------------------------------------------------

test("--lib does not credit an integration test", () => {
  const listing = LIST + "     Running tests/contract.rs (x)\nunrun::test: test\n";
  const workflow = withStep("- run: cargo test --lib unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing });
  assert.equal(code, 1, out);
  assert.ok(out.includes("unrun::test [test:contract] is #[ignore]"), out);
});

test("--test <name> credits only that integration test", () => {
  const listing = LIST + "     Running tests/contract.rs (x)\nunrun::test: test\n" +
    "     Running tests/other.rs (x)\nunrun::test: test\n";
  const workflow = withStep("- run: cargo test --test contract unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing });
  assert.equal(code, 1, out);
  assert.ok(out.includes("unrun::test [test:other] is #[ignore]"), out);
  assert.ok(!out.includes("[test:contract] is #[ignore]"), out);
});

test("no selector credits every target", () => {
  const listing = LIST + "     Running tests/contract.rs (x)\nunrun::test: test\n";
  const workflow = withStep("- run: cargo test unrun::test -- --ignored");
  const [code, out] = run({ workflow, listing });
  assert.equal(code, 0, out);
});

// --- allowlist and list integrity --------------------------------------------

test("a stale allowlist entry fails", () => {
  const [code, out] = run({ allow: ALLOW + "gone::test  # used to run manually somewhere else\n" });
  assert.equal(code, 1);
  assert.ok(out.includes("allowlist entry gone::test matches no ignored test"), out);
});

test("an allowlist entry needs a reason", () => {
  const [code, out] = run({ allow: ALLOW + "manual::probe  # todo\n" });
  assert.equal(code, 1);
  assert.ok(out.includes("the reason must say where the test runs"), out);
});

test("an empty list cannot pass against real sources", () => {
  const sources = { "src/a.rs": '#[test]\n#[ignore = "x"]\nfn a() {}\n    #[ignore]\nfn b() {}\n' };
  const [code, out] = run({ listing: "", allow: "", sources });
  assert.equal(code, 1);
  assert.ok(out.includes("the sources carry 2 #[ignore] attribute(s) but the test list names 0"), out);
});

test("a source count matching the list passes", () => {
  const body = '#[test]\n#[ignore = "x"]\nfn t() {}\n'.repeat(6);
  const [code, out] = run({ sources: { "src/a.rs": body } });
  assert.equal(code, 0, out);
});

test("no invocation at all fails", () => {
  const [code, out] = run({ workflow: "jobs:\n  t:\n    steps:\n      - run: cargo test --locked\n", listing: "", allow: "" });
  assert.equal(code, 1);
  assert.ok(out.includes("no `cargo test ... -- --ignored` invocation found"), out);
});

test("two cargo test names are refused", () => {
  const workflow = withStep(`
            - run: |
                cargo test --lib a:: b:: -- --ignored
        `);
  raisesSystemExit(() => run({ workflow }), "cargo takes one test name before `--`");
});

test("a file that is not a workflow is refused", () => {
  raisesSystemExit(() => run({ workflow: "run: cargo test --lib x -- --ignored\n" }), "not a workflow");
});
