// Tests for scripts/ci/stack-images.mjs, the build-once digest-map contract.

import assert from "node:assert/strict";
import { test } from "node:test";

import * as si from "./stack-images.mjs";

const WS = "/home/runner/work/elitea-platform/elitea-platform";
const DIGEST = "sha256:" + "a".repeat(64);

const config = (services) => ({ services });

function svc(image, dockerfile, target = null, { context = WS, args = null } = {}) {
  const build = { context, dockerfile };
  if (target) build.target = target;
  if (args) build.args = args;
  return { image, build };
}

const MAIN_E2E = svc("ghcr.io/elitea-ng/elitea-main:standalone", "services/elitea-main/Containerfile", "e2e");
const HOST = svc("ghcr.io/elitea-ng/elitea-subapp-host:standalone", "services/elitea-subapp-host/Containerfile");

function built(cfg, ...services) {
  const [entries, sets] = si.plan(cfg, WS, "ghcr.io/elitea-ng/ci-", "deadbeef", services);
  const meta = Object.fromEntries(entries.map((e) => [e.service, { "containerimage.digest": DIGEST }]));
  return [si.record(entries, meta), sets];
}

const raises = (fn, pattern) => assert.throws(fn, (error) => error instanceof si.StackImageError && pattern.test(error.message));

test("plan names ci images after the compose image", () => {
  const [, sets] = built(config({ "elitea-main": MAIN_E2E, "elitea-deepwiki": HOST }), "elitea-main", "elitea-deepwiki");
  assert.deepEqual(sets, [
    "elitea-main.tags=ghcr.io/elitea-ng/ci-elitea-main:deadbeef",
    "elitea-deepwiki.tags=ghcr.io/elitea-ng/ci-elitea-subapp-host:deadbeef",
  ]);
});

test("one build serves every tag of the same variant", () => {
  const [digests] = built(config({ "elitea-main": MAIN_E2E }), "elitea-main");
  // The e2e compose tags the same build under another org and tag, and
  // three standalone services share it: one pull, every local tag.
  const lane = config({
    "elitea-main": svc("ghcr.io/elitea-ng/elitea-main:e2e", "services/elitea-main/Containerfile", "e2e"),
    "elitea-migrate": MAIN_E2E,
    "elitea-agentstate-migrate": MAIN_E2E,
  });
  assert.deepEqual(si.resolve(lane, WS, digests), [
    ["ghcr.io/elitea-ng/elitea-main:e2e", `ghcr.io/elitea-ng/ci-elitea-main@${DIGEST}`],
    ["ghcr.io/elitea-ng/elitea-main:standalone", `ghcr.io/elitea-ng/ci-elitea-main@${DIGEST}`],
  ]);
});

test("a different variant is refused", () => {
  const [digests] = built(config({ "elitea-main": MAIN_E2E }), "elitea-main");
  for (const changed of [
    svc("x/elitea-main:final", "services/elitea-main/Containerfile", "final"),
    svc("x/elitea-main:e2e", "services/elitea-main/Containerfile", "e2e", { args: { VERSION: "1" } }),
    svc("x/elitea-main:e2e", "other/Containerfile", "e2e"),
  ]) {
    raises(() => si.resolve(config({ "elitea-main": changed }), WS, digests), /but this compose builds/);
  }
});

test("an image nobody built is named", () => {
  const [digests] = built(config({ "elitea-main": MAIN_E2E }), "elitea-main");
  raises(() => si.resolve(config({ "elitea-main": MAIN_E2E, "elitea-inventory": HOST }), WS, digests), /elitea-subapp-host was built/);
});

test("vacuous inputs fail", () => {
  raises(() => si.resolve(config({ postgres: { image: "postgres:18" } }), WS, {}), /vacuous/);
  raises(() => si.plan(config({ postgres: { image: "postgres:18" } }), WS, "p-", "t", ["postgres"]), /not a `build:` service/);
});

test("the same image twice in one plan is refused", () => {
  raises(() => si.plan(config({ a: MAIN_E2E, b: MAIN_E2E }), WS, "p-", "t", ["a", "b"]), /build it once/);
});

test("a build without a digest fails", () => {
  const [entries] = si.plan(config({ "elitea-main": MAIN_E2E }), WS, "p-", "t", ["elitea-main"]);
  raises(() => si.record(entries, { "elitea-main": {} }), /no pushed digest/);
});

test("merge refuses conflicting fragments", () => {
  const a = { x: { ref: "r", digest: DIGEST, build: {} } };
  const b = { x: { ref: "r", digest: "sha256:" + "b".repeat(64), build: {} } };
  assert.deepEqual(si.merge([["1", a], ["2", a]]), a);
  raises(() => si.merge([["1", a], ["2", b]]), /twice/);
});

// Not in the Python suite: the message a refused variant prints is Python's
// repr of both specs, and a context outside the workspace is named.
test("a refused variant prints both specs the way the Python version did", () => {
  // Through the file, as a lane reads it: the recorded spec's keys sorted.
  const digests = JSON.parse(si.dumps(built(config({ "elitea-main": MAIN_E2E }), "elitea-main")[0]));
  raises(
    () => si.resolve(config({ "elitea-main": svc("x/elitea-main:e2e", "other/Containerfile", "e2e") }), WS, digests),
    /built from \{'args': \{\}, 'context': '\.', 'dockerfile': 'services\/elitea-main\/Containerfile', 'target': 'e2e'\}, but this compose builds \{'context': '\.', 'dockerfile': 'other\/Containerfile', 'target': 'e2e', 'args': \{\}\}$/,
  );
  raises(() => si.buildSpec({ context: "/elsewhere/repo" }, WS), /is outside the workspace/);
  assert.equal(si.buildSpec({ context: `${WS}/deploy/` }, WS).context, "deploy");
});
