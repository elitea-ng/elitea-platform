// Keep ONE GitHub issue per CodeQL language in step with the nightly analysis.
//
// .github/workflows/codeql.yml analyses main every night and runs this once per
// language with the SARIF the analysis wrote (`upload: never`: the results are
// not sent to code scanning, which a private repository without Advanced
// Security does not have), through actions/github-script. The same shape as
// scripts/ci/image-scan-issue.mjs, for the same reason (#2246):
//
//   findings, no issue                    -> open one
//   findings, an OPEN issue               -> rewrite its body; comment only when
//                                            the finding set changed (no nightly noise)
//   findings, only a CLOSED issue         -> reopen it, with a comment
//   no findings, an open issue            -> comment and close it
//   no findings, no open issue            -> nothing
//
// The title is stable (`CodeQL: <language> — findings on main`) and is the
// dedupe key. A directory with no readable SARIF is REFUSED (exit 2) and touches
// no issue: "no findings" must never be read off an analysis that never ran.

import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const LABELS = ["security", "codeql"];
export const FINGERPRINT_PREFIX = "<!-- codeql-fingerprint: ";
export const MAX_ROWS = 200;

export class SarifError extends Error {}

export const titleFor = (language) => `CodeQL: ${language} — findings on main`;

/** Every result of every SARIF file under `dir`, as {rule, level, message, where}. */
export function loadResults(dir) {
  let files = [];
  try {
    files = fs.readdirSync(dir).filter((n) => n.endsWith(".sarif")).sort();
  } catch {
    files = [];
  }
  if (files.length === 0) throw new SarifError(`no .sarif file in ${dir}: the analysis did not run`);
  const results = [];
  for (const file of files) {
    let doc;
    try {
      doc = JSON.parse(fs.readFileSync(path.join(dir, file), "utf8"));
    } catch (error) {
      throw new SarifError(`${file} is not JSON (${error.message}): the analysis did not finish`);
    }
    if (!Array.isArray(doc.runs) || doc.runs.length === 0) throw new SarifError(`${file} has no runs: the analysis did not finish`);
    for (const run of doc.runs) {
      for (const r of run.results ?? []) {
        const loc = r.locations?.[0]?.physicalLocation;
        results.push({
          rule: r.ruleId ?? "unknown",
          level: r.level ?? "warning",
          message: String(r.message?.text ?? "").replace(/\s+/g, " ").slice(0, 200),
          where: loc ? `${loc.artifactLocation?.uri ?? "?"}:${loc.region?.startLine ?? 0}` : "?",
        });
      }
    }
  }
  return results.sort((a, b) => (a.where + a.rule < b.where + b.rule ? -1 : 1));
}

export function fingerprint(results) {
  return crypto.createHash("sha256").update(JSON.stringify(results.map((r) => [r.rule, r.where]))).digest("hex").slice(0, 16);
}

export function renderBody(language, results, { runUrl }) {
  const rows = results.slice(0, MAX_ROWS).map((r) => `| \`${r.rule}\` | ${r.level} | \`${r.where}\` | ${r.message.replace(/\|/g, "\\|")} |`);
  const cut = results.length > MAX_ROWS ? `\n\n${results.length - MAX_ROWS} more result(s) are in the run's SARIF.` : "";
  return [
    `The nightly CodeQL analysis of \`main\` (${language}) reports ${results.length} result(s).`,
    "",
    "| Rule | Level | Location | Message |",
    "| --- | --- | --- | --- |",
    ...rows,
    cut,
    "",
    `Run: ${runUrl}`,
    "",
    `${FINGERPRINT_PREFIX}${fingerprint(results)} -->`,
  ].join("\n");
}

const fingerprintOf = (body) => new RegExp(`${FINGERPRINT_PREFIX.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(\\w+)`).exec(body ?? "")?.[1] ?? "";

async function findIssue(github, owner, repo, title, state) {
  const issues = await github.paginate(github.rest.issues.listForRepo, { owner, repo, labels: LABELS.join(","), state, per_page: 100 });
  return issues.find((i) => i.title === title && !i.pull_request);
}

export async function sync(github, { repo: fullRepo, language, results, runUrl }) {
  const [owner, repo] = fullRepo.split("/");
  const title = titleFor(language);
  const open = await findIssue(github, owner, repo, title, "open");
  if (results.length === 0) {
    if (open) {
      await github.rest.issues.createComment({ owner, repo, issue_number: open.number, body: `The nightly analysis found no ${language} results. Closing. ${runUrl}` });
      await github.rest.issues.update({ owner, repo, issue_number: open.number, state: "closed", state_reason: "completed" });
      return "closed";
    }
    return "nothing";
  }
  const body = renderBody(language, results, { runUrl });
  if (open) {
    const changed = fingerprintOf(open.body) !== fingerprint(results);
    await github.rest.issues.update({ owner, repo, issue_number: open.number, body });
    if (changed) await github.rest.issues.createComment({ owner, repo, issue_number: open.number, body: `The set of results changed: now ${results.length}. ${runUrl}` });
    return changed ? "updated" : "unchanged";
  }
  const closed = await findIssue(github, owner, repo, title, "closed");
  if (closed) {
    await github.rest.issues.update({ owner, repo, issue_number: closed.number, state: "open", body });
    await github.rest.issues.createComment({ owner, repo, issue_number: closed.number, body: `Reopened: the nightly analysis reports ${results.length} result(s) again. ${runUrl}` });
    return "reopened";
  }
  await github.rest.issues.create({ owner, repo, title, body, labels: LABELS });
  return "opened";
}

export async function main({ github, args, log = console.log }) {
  let results;
  try {
    results = loadResults(args.sarifDir);
  } catch (error) {
    if (error instanceof SarifError) {
      log(`::error::${error.message}`);
      return 2;
    }
    throw error;
  }
  const outcome = await sync(github, { repo: args.repo, language: args.language, results, runUrl: args.runUrl });
  log(`${args.language}: ${results.length} result(s); ${outcome}`);
  return 0;
}
