// Tests for deploy/ci-runners/job-started.sh, the eng-runner job-started hook.
// Runs the real script with bash and python3 (both on the runner image and on
// ubuntu-latest) against fixture payloads.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync, copyFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const HOOK = join(dirname(fileURLToPath(import.meta.url)), "../../deploy/ci-runners/job-started.sh");
const REPO = "elitea-ng/elitea-platform";

function run({ event = "pull_request", repo = REPO, actor = "alice", triggeringActor, payload, rawPayload, env = {}, allowedRepos } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "eng-hook-"));
  try {
    // The hook reads its override from a file next to itself, so run a copy.
    const hook = join(dir, "job-started.sh");
    copyFileSync(HOOK, hook);
    if (allowedRepos !== undefined) writeFileSync(join(dir, "allowed-repos"), allowedRepos);
    const path = join(dir, "event.json");
    if (rawPayload !== undefined) writeFileSync(path, rawPayload);
    else writeFileSync(path, JSON.stringify(payload ?? {}));
    const r = spawnSync("bash", [hook], {
      encoding: "utf8",
      env: {
        PATH: process.env.PATH,
        GITHUB_REPOSITORY: repo,
        GITHUB_EVENT_NAME: event,
        GITHUB_EVENT_PATH: path,
        GITHUB_ACTOR: actor,
        GITHUB_TRIGGERING_ACTOR: triggeringActor ?? actor,
        ...env,
      },
    });
    return { code: r.status, out: r.stdout + r.stderr };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

const pr = (head, login = "alice") => ({
  pull_request: { head: { repo: head === null ? null : { full_name: head } }, user: { login } },
});

test("same-repo pull request is accepted", () => {
  const r = run({ payload: pr(REPO) });
  assert.equal(r.code, 0, r.out);
  assert.match(r.out, /accepted pull_request/);
});

test("fork pull request is refused", () => {
  const r = run({ payload: pr("mallory/elitea-platform") });
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /pull request from mallory\/elitea-platform/);
});

test("deleted-fork pull request (head.repo null) is refused", () => {
  const r = run({ payload: pr(null) });
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /a deleted fork/);
});

test("Dependabot pull request is refused by author, actor and re-run actor", () => {
  assert.equal(run({ payload: pr(REPO, "dependabot[bot]") }).code, 1);
  assert.equal(run({ payload: pr(REPO), actor: "dependabot[bot]" }).code, 1);
  assert.equal(run({ payload: pr(REPO), triggeringActor: "dependabot[bot]" }).code, 1);
  assert.equal(run({ event: "push", actor: "dependabot[bot]" }).code, 1);
});

for (const event of ["merge_group", "workflow_dispatch", "schedule", "push"]) {
  test(`${event} of an allowed repository is accepted`, () => {
    assert.equal(run({ event }).code, 0);
  });
}

for (const event of ["pull_request_target", "workflow_run", "issue_comment", "pull_request_review", "pull_request_review_comment", "release", "repository_dispatch"]) {
  test(`${event} is refused`, () => {
    const r = run({ event, payload: pr(REPO) });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, new RegExp(`event ${event} is not allowed`));
  });
}

test("another repository is refused, even on an allowed event", () => {
  const r = run({ repo: "someone/else", event: "schedule" });
  assert.equal(r.code, 1);
  assert.match(r.out, /repository someone\/else is not on the allowlist/);
});

test("a repository whose name is a prefix or suffix of an allowed one is refused", () => {
  assert.equal(run({ repo: "elitea-ng/elitea", event: "schedule" }).code, 1);
  assert.equal(run({ repo: "elitea-ng/elitea-platform-x", event: "schedule" }).code, 1);
});

test("allowed-repos next to the hook replaces the allowlist", () => {
  const file = "# owner/name per line\nelitea-ng/other  # a comment\n\nelitea-ng/third\n";
  assert.equal(run({ event: "schedule", allowedRepos: file }).code, 1, "a built-in entry is no longer allowed");
  assert.equal(run({ repo: "elitea-ng/other", event: "schedule", allowedRepos: file }).code, 0);
  assert.equal(run({ repo: "elitea-ng/third", event: "schedule", allowedRepos: file }).code, 0);
});

test("an empty allowed-repos file refuses everything", () => {
  assert.equal(run({ event: "schedule", allowedRepos: "" }).code, 1);
  assert.equal(run({ event: "schedule", allowedRepos: "# nothing\n" }).code, 1);
});

test("the allowlist cannot be widened from the environment", () => {
  // A workflow's job env may reach the hook on some runner versions.
  const r = run({ repo: "someone/else", event: "schedule", env: { ENG_RUNNER_ALLOWED_REPOS: "someone/else" } });
  assert.equal(r.code, 1, r.out);
});

test("fails closed on missing variables and unreadable payloads", () => {
  assert.equal(run({ repo: "" }).code, 1);
  assert.equal(run({ event: "" }).code, 1);
  assert.equal(run({ rawPayload: "not json" }).code, 1);
  assert.equal(run({ payload: {} }).code, 1, "pull_request event without pull_request object");
  assert.equal(run({ env: { GITHUB_EVENT_PATH: "/nonexistent/event.json" } }).code, 1);
});

test("a head name with a tab cannot smuggle a match", () => {
  const r = run({ payload: pr(`${REPO}\t${REPO}`) });
  assert.equal(r.code, 1);
});
