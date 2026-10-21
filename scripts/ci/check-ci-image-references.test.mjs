// Scenarios for check-ci-image-references.mjs (new with the port: the Python
// version had none). The fixture refs are assembled, so this file holds no
// reference of its own.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { ALLOWED, CACHE_ONLY, scan } from "./check-ci-image-references.mjs";

const ORG = "ghcr.io/elitea-ng/";
const IMAGE = ORG + "ci-" + "elitea-main:abc";
const CACHE = ORG + "ci-" + "cache-elitea-main:linux-amd64";

function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "ci-refs-"));
  for (const [name, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), text);
  }
  return [root, Object.keys(files)];
}

test("a CI image named by a shipped file is a violation with its line", () => {
  const [root, files] = tree({ "deploy/helm/values.yaml": `a: 1\nimage: ${IMAGE}\n` });
  assert.deepEqual(scan(root, files), [[`deploy/helm/values.yaml:2: image: ${IMAGE}`], 0]);
});

test("the allowed files may name both, the cache-only files only the cache", () => {
  const allowed = Object.keys(ALLOWED)[0];
  const cacheOnly = Object.keys(CACHE_ONLY)[0];
  const [root, files] = tree({
    [allowed]: `${IMAGE}\n${CACHE}\n`,
    [cacheOnly]: `cache-from: ${CACHE}\nboth: ${CACHE} ${IMAGE}\n`,
  });
  const [violations, hits] = scan(root, files);
  assert.equal(hits, 3);
  assert.deepEqual(violations, [`${cacheOnly}:2: both: ${CACHE} ${IMAGE}`]);
});

test("a file that is not UTF-8 is skipped, as the Python read skipped it", () => {
  const [root, files] = tree({ "blob.bin": "" });
  fs.writeFileSync(path.join(root, "blob.bin"), Buffer.from([0xff, 0xfe, ...Buffer.from(IMAGE)]));
  assert.deepEqual(scan(root, files), [[], 0]);
});
