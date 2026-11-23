import assert from "node:assert/strict";
import test from "node:test";

import { loadYaml } from "./ci-compat.mjs";
import {
  alwaysOn,
  benchFeatures,
  canonical,
  cargoRuns,
  check,
  featureSet,
  helperRuns,
  imageFeatureSets,
  legRuns,
  workerRuns,
} from "./check-worker-feature-sets.mjs";

const W = "services/elitea-worker-rust";
const IMAGE = new Set(["", "sandbox-supervisor"]);

const CONTAINERFILE = `
# Both steps run the same \`cargo auditable build --frozen --release\` after a fetch
RUN --mount=type=cache,target=/usr/local/cargo/registry \\
    flock /lock cargo fetch --locked && \\
    CARGO_TARGET_DIR=/build-target cargo auditable build --frozen --release && \\
    cp /build-target/release/elitea-worker-rust /usr/local/bin/elitea-worker-rust
RUN CARGO_TARGET_DIR=/cargo-target cargo auditable build --frozen --release \\
      --features sandbox-supervisor --bin elitea-sandbox-supervisor && \\
    cp a b
`;

function runsOf(yaml) {
  return workerRuns(loadYaml(yaml), "wf.yml");
}

/** A workflow that lints and tests both sets, plus `extra` steps in a worker job. */
function workflow(extra = "") {
  return `
jobs:
  lint:
    strategy:
      matrix:
        include:
          - { name: shared, dir: libs/rust, features: --all-features }
          - { name: worker, dir: ${W}, features: "" }
          - { name: supervisor, dir: ${W}, features: --features sandbox-supervisor }
    defaults:
      run:
        working-directory: \${{ matrix.dir }}
    steps:
      - name: Clippy
        env:
          FEATURES: \${{ matrix.features }}
        run: cargo clippy --locked --all-targets \${FEATURES} -- -D warnings
      - name: libs only
        if: matrix.dir == 'libs/rust'
        run: cargo clippy --all-features -p x
  test:
    defaults:
      run:
        working-directory: ${W}
    steps:
      - run: cargo test --locked --all-targets --features sandbox-supervisor
      - run: cargo test --locked --lib
${extra}`;
}

test("the Containerfile's two builds are the image sets, comments ignored", () => {
  const { sets, errors } = imageFeatureSets(CONTAINERFILE);
  assert.deepEqual([...sets].sort(), ["", "sandbox-supervisor"]);
  assert.deepEqual(errors, []);
});

test("an image build with --all-features is refused", () => {
  const { errors } = imageFeatureSets("RUN cargo auditable build --release --all-features\n");
  assert.equal(errors.length, 1);
});

test("a workflow that lints and tests both image sets passes", () => {
  const runs = runsOf(workflow());
  assert.equal(runs.length, 4);
  assert.deepEqual(check(IMAGE, runs), []);
});

test("--all-features on the worker is refused, wherever the directory comes from", () => {
  const extra = `
      - working-directory: .
        run: |
          out="$(cd ${W} && cargo test --locked --lib --all-features --no-run)"
      - working-directory: \${{ github.workspace }}
        run: cargo clippy --manifest-path ${W}/Cargo.toml --all-features
`;
  const errors = check(IMAGE, runsOf(workflow(extra)));
  assert.equal(errors.length, 2, errors.join("\n"));
  assert.ok(errors.every((e) => e.includes("--all-features")));
});

test("a feature set no image builds is refused (a bench feature)", () => {
  const extra = `
      - run: cargo test --lib --features sandbox-supervisor,graph-shaping-bench
`;
  const errors = check(IMAGE, runsOf(workflow(extra)));
  assert.equal(errors.length, 1);
  assert.match(errors[0], /graph-shaping-bench/);
});

test("an image set nobody lints or tests is reported", () => {
  const yaml = `
jobs:
  only:
    defaults:
      run:
        working-directory: ${W}
    steps:
      - run: cargo clippy --features sandbox-supervisor
      - run: cargo test --features sandbox-supervisor
`;
  const errors = check(IMAGE, runsOf(yaml));
  assert.deepEqual(errors.length, 2);
  assert.ok(errors.some((e) => e.includes("cargo clippy") && e.includes("default set")));
  assert.ok(errors.some((e) => e.includes("cargo test") && e.includes("default set")));
});

test("a matrix leg the step's if excludes is not read, other workspaces are not the worker", () => {
  const runs = runsOf(workflow());
  assert.ok(runs.every((r) => !r.where.includes("libs only")));
  assert.ok(runs.every((r) => !r.where.includes("[shared]")));
});

test("legRuns reads ==, != and a bare key, and gives up on ||", () => {
  assert.equal(legRuns("matrix.dir == 'a'", { dir: "a" }), true);
  assert.equal(legRuns("matrix.dir == 'a'", { dir: "b" }), false);
  assert.equal(legRuns("${{ matrix.dir != 'a' }}", { dir: "a" }), false);
  assert.equal(legRuns("matrix.doc", {}), false);
  assert.equal(legRuns("matrix.doc", { doc: "--" }), true);
  assert.equal(legRuns("matrix.dir == 'a' || always()", { dir: "b" }), true);
});

test("cd is scoped to its subshell, and env prefixes and rustup run are seen through", () => {
  const runs = cargoRuns(
    `(cd ${W} && cargo build --all-features)\ncargo test\nFOO=1 rustup run 1.99.1 cargo clippy -F a,b`,
    ".",
  );
  assert.deepEqual(
    runs.map((r) => [r.dir, r.subcommand]),
    [
      [W, "build"],
      [".", "test"],
      [".", "clippy"],
    ],
  );
});

test("arguments after -- are the binary's, not cargo's", () => {
  const [run] = cargoRuns("cargo test --lib -- --all-features --ignored", W);
  assert.equal(featureSet(run.args).all, false);
  assert.equal(featureSet(run.args).set, "");
  assert.equal(featureSet(["--features=x", "-Fy", "--", "-F", "z"]).set, "x,y");
});

test("the Python helper's argv list is read", () => {
  const bad = helperRuns('subprocess.run(["cargo", "test", "--locked", "--all-features", "--lib"])', "h.py");
  assert.equal(bad.length, 1);
  assert.equal(bad[0].all, true);
  const good = helperRuns('["cargo", "test", "--features", "sandbox-supervisor", "--lib"]', "h.py");
  assert.equal(good[0].set, "sandbox-supervisor");
});

test("feature lists are compared as sets", () => {
  assert.equal(canonical(["b,a", "a"]), "a,b");
  assert.equal(canonical([]), "");
});

test("an argument it cannot expand, or --no-default-features, is refused", () => {
  const extra = `
      - run: cargo clippy --features "\${{ inputs.features }}"
      - run: cargo test --lib --no-default-features
`;
  const errors = check(IMAGE, runsOf(workflow(extra)));
  assert.equal(errors.length, 2, errors.join("\n"));
  assert.ok(errors.some((e) => e.includes("cannot expand")));
  assert.ok(errors.some((e) => e.includes("--no-default-features")));
});

test("bench features pass only on a --benches lint, and never count as an image set's lint", () => {
  const BENCH = benchFeatures(`
[[bench]]
name = "graph_shaping"
required-features = ["graph-shaping-bench"]

[dev-dependencies]
x = { features = ["not-a-bench"] }
`);
  assert.deepEqual([...BENCH], ["graph-shaping-bench"]);
  const yaml = `
jobs:
  only:
    defaults:
      run:
        working-directory: ${W}
    steps:
      - run: cargo clippy --benches --features graph-shaping-bench
      - run: cargo clippy --all-targets --features graph-shaping-bench
      - run: cargo clippy --features sandbox-supervisor
      - run: cargo test --features sandbox-supervisor
      - run: cargo test --lib
`;
  const errors = check(IMAGE, runsOf(yaml), BENCH);
  assert.equal(errors.length, 2, errors.join("\n"));
  assert.ok(errors.some((e) => e.includes("which no image builds")));
  assert.ok(errors.some((e) => e.includes("cargo clippy") && e.includes("default set")));
});

test("a step an event, failure() or continue-on-error can switch off is no coverage", () => {
  assert.equal(alwaysOn("${{ !cancelled() }}"), true);
  assert.equal(alwaysOn("matrix.benches && always()"), true);
  assert.equal(alwaysOn("github.event_name == 'schedule'"), false);
  assert.equal(alwaysOn("failure()"), false);
  const yaml = `
jobs:
  only:
    defaults:
      run:
        working-directory: ${W}
    steps:
      - run: cargo clippy
        if: github.event_name == 'schedule'
      - run: cargo clippy --features sandbox-supervisor
        continue-on-error: true
      - run: cargo test --features sandbox-supervisor
      - run: cargo test --lib
`;
  const errors = check(IMAGE, runsOf(yaml));
  assert.equal(errors.length, 2, errors.join("\n"));
  assert.ok(errors.every((e) => e.startsWith("no workflow runs cargo clippy")));
});

test("a command after a shell keyword is read, a redirection's target is not an argument", () => {
  const runs = cargoRuns('if [ "$X" = -- ]; then\n  cargo doc --no-deps\nelse\n  cargo doc -F a > "$OUT"\nfi', W);
  assert.deepEqual(
    runs.map((r) => [r.subcommand, featureSet(r.args).set, featureSet(r.args).unresolved]),
    [
      ["doc", "", false],
      ["doc", "a", false],
    ],
  );
});
