import assert from "node:assert/strict";
import test from "node:test";

import { loadYaml } from "./ci-compat.mjs";
import {
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
  assert.deepEqual(featureSet(run.args), { all: false, set: "" });
  assert.deepEqual(featureSet(["--features=x", "-Fy", "--", "-F", "z"]), { all: false, set: "x,y" });
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
