// node --test scripts/ci/check-image-users.test.mjs
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { checkRustRuntime, checkWeb } from "./check-image-users.mjs";

const read = (p) => readFileSync(new URL(`../../${p}`, import.meta.url), "utf8");

test("the repository's Containerfiles pass", () => {
  assert.deepEqual(checkWeb(read("apps/elitea-web/Containerfile")), []);
  assert.deepEqual(checkRustRuntime(read("services/elitea-code-runner/Containerfile")), []);
});

test("a web image with no USER, or that switches back to root, fails", () => {
  assert.equal(checkWeb("FROM nginx AS spa\nCOPY a b\n").length, 1);
  assert.equal(checkWeb("FROM nginx AS spa\nUSER 101\nUSER root\n").length, 1);
  assert.equal(checkWeb("FROM nginx AS spa\nUSER 101\nFROM spa\nUSER 0\n").length, 1);
  assert.deepEqual(checkWeb("FROM nginx AS spa\nUSER 101\nFROM spa\nCOPY a b\n"), []);
});

test("a rust-runtime stage without the chmod, or with it after USER, fails", () => {
  const fix = "RUN chmod -R go-w /usr/local/cargo && \\\n    find /usr/local/rustup -type d -perm /022 -exec chmod go-w {} +\n";
  assert.equal(checkRustRuntime("FROM rust AS rust-runtime\nUSER 10001:10001\n").length, 1);
  assert.equal(checkRustRuntime("FROM rust AS rust-runtime\nRUN chmod -R go-w /usr/local/cargo\nUSER 10001\n").length, 1);
  assert.equal(checkRustRuntime(`FROM rust AS rust-runtime\nUSER 10001\n${fix}`).length, 1);
  assert.deepEqual(checkRustRuntime(`FROM rust AS rust-runtime\n${fix}USER 10001:10001\n`), []);
});
