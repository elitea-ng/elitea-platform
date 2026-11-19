#!/usr/bin/env node
// The real-model preset override stays in step with the image it overrides.
//
// deploy/real-llm/models.ini is a COPY of the preset baked into the pinned
// elitea-e2e-llm image, with one key changed (the chat model's context). A
// copy of someone else's file drifts silently: bump the image to a release
// that renames a GGUF file or a section and the mounted copy points at
// nothing, and the router refuses to start. So the copy records the image
// reference it was taken from on a `; base-image:` line, and this gate fails
// when the compose file's `llm-real` image reference is anything else.
//
// To bump the image: take the new image's /models/models.ini, re-apply the
// override (see the header of deploy/real-llm/models.ini), and update the
// `; base-image:` line in the same change.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const IMAGE_RE = /^\s*image:\s*(ghcr\.io\/elitea-ng\/elitea-e2e-llm:\S+)\s*$/m;
const BASE_RE = /^;\s*base-image:\s*(\S+)\s*$/m;

/** Returns the list of problems (empty when the copy matches the pinned image). */
export function check(composeText, iniText) {
  const image = IMAGE_RE.exec(composeText)?.[1];
  const base = BASE_RE.exec(iniText)?.[1];
  if (image === undefined) return ["no elitea-e2e-llm image reference found in the compose file"];
  if (base === undefined) return ["deploy/real-llm/models.ini has no `; base-image:` line"];
  if (image !== base) {
    return [
      `the llm-real image is ${image} but deploy/real-llm/models.ini was copied from ${base}: ` +
        "re-take the preset from the new image, re-apply the override, and update the `; base-image:` line",
    ];
  }
  return [];
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const problems = check(
    fs.readFileSync(path.join(root, "deploy/docker-compose.standalone-full.yml"), "utf8"),
    fs.readFileSync(path.join(root, "deploy/real-llm/models.ini"), "utf8"),
  );
  for (const problem of problems) console.error(`::error::${problem}`);
  process.exit(problems.length === 0 ? 0 : 1);
}
