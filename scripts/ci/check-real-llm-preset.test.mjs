// Tests for scripts/ci/check-real-llm-preset.mjs.

import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";

import { check } from "./check-real-llm-preset.mjs";

const compose = (ref) => `  llm-real:\n    image: ${ref}\n    profiles: ["real-llm"]\n`;
const ini = (ref) => `; base-image: ${ref}\nversion = 1\n`;

test("a copy taken from the pinned image passes", () => {
  assert.deepEqual(check(compose("ghcr.io/elitea-ng/elitea-e2e-llm:v0.1.0@sha256:aa"), ini("ghcr.io/elitea-ng/elitea-e2e-llm:v0.1.0@sha256:aa")), []);
});

test("an image bump without the copy fails", () => {
  const problems = check(compose("ghcr.io/elitea-ng/elitea-e2e-llm:v0.2.0@sha256:bb"), ini("ghcr.io/elitea-ng/elitea-e2e-llm:v0.1.0@sha256:aa"));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /v0\.2\.0/);
});

test("a missing record or reference fails rather than passes", () => {
  assert.equal(check(compose("ghcr.io/elitea-ng/elitea-e2e-llm:v0.1.0@sha256:aa"), "version = 1\n").length, 1);
  assert.equal(check("services: {}\n", ini("x")).length, 1);
});

test("the checked-in files agree", () => {
  assert.deepEqual(
    check(fs.readFileSync("deploy/docker-compose.standalone-full.yml", "utf8"), fs.readFileSync("deploy/real-llm/models.ini", "utf8")),
    [],
  );
});
