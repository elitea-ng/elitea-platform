#!/usr/bin/env node
// The images that once ran as root, or left a writable directory on PATH,
// keep their fix (#2954). Static, from the Containerfiles: image-matrix.yml
// runs it on every PR in well under a second, with no image built. The built
// images are checked too, where they are built (ci-build-once.yml).
//
//   * apps/elitea-web/Containerfile: the `spa` stage ends on a non-root USER,
//     and the stages built FROM it never switch back to root.
//   * services/elitea-code-runner/Containerfile: the `rust-runtime` stage
//     removes group and world write from /usr/local/cargo and
//     /usr/local/rustup before it switches to its non-root USER.
//
// Usage: node scripts/ci/check-image-users.mjs [repo root]
import { readFileSync } from "node:fs";
import path from "node:path";

const root = process.argv[2] ?? process.cwd();

/** Stages of a Containerfile: [{ name, from, lines }], line continuations joined. */
export function stages(text) {
  const joined = text.replace(/\\\r?\n/g, " ");
  const out = [];
  for (const raw of joined.split(/\r?\n/)) {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) continue;
    const from = /^FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?/i.exec(line);
    if (from) {
      out.push({ name: from[2] ?? "", from: from[1], lines: [] });
      continue;
    }
    if (out.length > 0) out.at(-1).lines.push(line);
  }
  return out;
}

const isRoot = (user) => ["", "root", "0"].includes(user.split(":")[0]);
const users = (stage) => stage.lines.filter((l) => /^USER\s/i.test(l)).map((l) => l.replace(/^USER\s+/i, "").trim());

/** Problems with the web image's user. */
export function checkWeb(text) {
  const all = stages(text);
  const spa = all.find((s) => s.name === "spa");
  if (!spa) return ["apps/elitea-web/Containerfile has no `spa` stage"];
  const problems = [];
  const spaUsers = users(spa);
  if (spaUsers.length === 0 || isRoot(spaUsers.at(-1))) {
    problems.push("apps/elitea-web/Containerfile: the `spa` stage must end on a non-root USER (nginx runs unprivileged)");
  }
  // Every stage built on spa (the release image) inherits its USER; none may reset it.
  const derived = new Set(["spa"]);
  for (const stage of all) {
    if (!derived.has(stage.from)) continue;
    if (stage.name) derived.add(stage.name);
    if (users(stage).some(isRoot)) problems.push(`apps/elitea-web/Containerfile: stage ${stage.name || "(final)"} switches back to root`);
  }
  return problems;
}

/** Problems with the code runner's rust-runtime toolchain permissions. */
export function checkRustRuntime(text) {
  const stage = stages(text).find((s) => s.name === "rust-runtime");
  if (!stage) return ["services/elitea-code-runner/Containerfile has no `rust-runtime` stage"];
  const chmod = stage.lines.findIndex((l) => /^RUN\s.*chmod\s+-R\s+go-w\s+\/usr\/local\/cargo\s+\/usr\/local\/rustup\b/i.test(l));
  const user = stage.lines.findIndex((l) => /^USER\s/i.test(l));
  const problems = [];
  if (chmod < 0) problems.push("services/elitea-code-runner/Containerfile: rust-runtime must `chmod -R go-w /usr/local/cargo /usr/local/rustup`");
  if (user < 0 || isRoot(users(stage).at(-1))) problems.push("services/elitea-code-runner/Containerfile: rust-runtime must end on a non-root USER");
  if (chmod >= 0 && user >= 0 && chmod > user) problems.push("services/elitea-code-runner/Containerfile: rust-runtime's chmod must run before its USER");
  return problems;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const read = (p) => readFileSync(path.join(root, p), "utf8");
  const problems = [
    ...checkWeb(read("apps/elitea-web/Containerfile")),
    ...checkRustRuntime(read("services/elitea-code-runner/Containerfile")),
  ];
  for (const p of problems) console.error(`::error::${p}`);
  if (problems.length > 0) process.exit(1);
  console.log("check-image-users: elitea-web runs unprivileged; rust-runtime has no writable toolchain on PATH");
}
