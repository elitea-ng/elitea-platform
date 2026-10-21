// Keep ONE GitHub issue per published image in step with its nightly scan.
//
// .github/workflows/nightly-image-scan.yml scans what `ghcr.io/<org>/<image>:latest`
// resolves to every night and runs this once per image with the trivy report,
// through actions/github-script:
//
//   findings at or above the threshold, no issue     -> open one
//   findings, an OPEN issue with the title           -> rewrite its body; comment
//                                                       only when the finding set
//                                                       changed (no nightly noise)
//   findings, only a CLOSED issue with the title     -> reopen it, with a comment
//   no findings, an open issue                       -> comment and close it
//   no findings, no open issue                       -> nothing
//
// The title is stable, `Image scan: <image> — vulnerabilities in latest`, and it
// is the dedupe key: one image, one issue, for ever. Issues are found by listing
// the repository's `image-scan` issues in the state wanted and keeping the
// EXACT title; no fuzzy search decides which issue is this image's.
//
// A report that does not prove the scan ran (missing, not JSON, or without the
// expected target type) is REFUSED with exit code 2 and touches no issue. "No
// findings" must never be read off a scan that never reached the image — that is
// the #433 shape, and here it would also close an open issue on a dead scan.
// scripts/ci/image-scan-gate.sh, run after this, fails the job for the same
// report.
//
// Only the standard library and the `github` client actions/github-script hands
// in (GITHUB_TOKEN with issues: write). scripts/ci/image-scan-issue.test.mjs
// covers every arm with a fake client.

import crypto from "node:crypto";
import fs from "node:fs";

import { pyRepr } from "./py-compat.mjs";

export const LABELS = ["security", "image-scan"];
const SEVERITY_ORDER = { CRITICAL: 0, HIGH: 1 };
export const FINGERPRINT_PREFIX = "<!-- image-scan-fingerprint: ";
// GitHub refuses an issue body above 65536 characters. A table that long is
// cut, with the count of the rows left out, rather than failing the update.
export const MAX_ROWS = 300;

/** The report does not prove the scan ran; nothing may be concluded. */
export class ReportError extends Error {}

export function titleFor(image) {
  return `Image scan: ${image} — vulnerabilities in latest`;
}

const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

export function loadFindings(reportPath, expectType) {
  let stat = null;
  try {
    stat = fs.statSync(reportPath);
  } catch {
    stat = null;
  }
  if (!stat || !stat.isFile() || stat.size === 0) {
    throw new ReportError(`no report at ${reportPath}: the scan did not run`);
  }
  let report;
  try {
    report = JSON.parse(fs.readFileSync(reportPath, "utf8"));
  } catch (error) {
    throw new ReportError(`the report is not valid JSON (${error.message}): the scan did not finish`);
  }
  const results = (report && typeof report === "object" && !Array.isArray(report) && report.Results) || [];
  if (!results.some((r) => r && r.Type === expectType)) {
    throw new ReportError(`the report holds no ${pyRepr(expectType)} target: the scan did not reach what the image ships`);
  }
  const rows = new Map();
  for (const result of results) {
    for (const vuln of result.Vulnerabilities || []) {
      const row = [
        vuln.Severity ?? "UNKNOWN",
        vuln.VulnerabilityID ?? "?",
        vuln.PkgName ?? "?",
        vuln.InstalledVersion ?? "?",
        vuln.FixedVersion || "",
        result.Target ?? "?",
      ];
      rows.set(JSON.stringify(row), row);
    }
  }
  const findings = [...rows.values()].map(([severity, id, pkg, installed, fixed, target]) => ({
    severity,
    id,
    package: pkg,
    installed,
    fixed,
    target,
  }));
  // The Python order (severity, id, package, target), then installed and fixed
  // so two rows that tie on those four still sort the same way every night.
  const rank = (f) => (Object.hasOwn(SEVERITY_ORDER, f.severity) ? SEVERITY_ORDER[f.severity] : 9);
  findings.sort(
    (a, b) =>
      rank(a) - rank(b) ||
      cmp(a.id, b.id) ||
      cmp(a.package, b.package) ||
      cmp(a.target, b.target) ||
      cmp(a.installed, b.installed) ||
      cmp(a.fixed, b.fixed),
  );
  return findings;
}

/** Stable over the finding SET; the digest and the run do not enter it. */
export function fingerprint(findings) {
  const text = findings.map((f) => [f.severity, f.id, f.package, f.installed, f.fixed].join("|")).join("\n");
  return crypto.createHash("sha256").update(text, "utf8").digest("hex").slice(0, 16);
}

const cell = (text) => text.replaceAll("|", "\\|").replaceAll("\n", " ");

export function renderBody(image, findings, { digest, version, runUrl, exempt }) {
  const critical = findings.filter((f) => f.severity === "CRITICAL").length;
  const high = findings.filter((f) => f.severity === "HIGH").length;
  const lines = [
    `The nightly scan of the **latest published** \`${image}\` image reports ` +
      `**${findings.length}** HIGH or CRITICAL finding(s) (${critical} CRITICAL, ${high} HIGH).`,
    "",
    `- Image: \`${image}:latest\``,
    `- Digest scanned: \`${digest}\` (linux/amd64)`,
    `- Version: \`${version || "unknown"}\``,
    `- Run: ${runUrl}`,
    "",
  ];
  if (exempt) {
    lines.push(
      "> This image is **exempt** in `.github/image-scan-exempt.txt`: its findings do " +
        "not block a release. The entry there names the cause and what ends it.",
      "",
    );
  }
  lines.push("| CVE | Package | Installed → Fixed | Severity | Target |", "|---|---|---|---|---|");
  for (const f of findings.slice(0, MAX_ROWS)) {
    const fixed = f.fixed || "no fix yet";
    lines.push(
      `| ${cell(f.id)} | ${cell(f.package)} | ${cell(f.installed)} → ${cell(fixed)} ` +
        `| ${f.severity} | ${cell(f.target)} |`,
    );
  }
  if (findings.length > MAX_ROWS) {
    lines.push(`| … | ${findings.length - MAX_ROWS} more row(s) in the run's report | | | |`);
  }
  lines.push(
    "",
    "Raise the pinned versions (base image digest, toolchain, dependency) and " +
      "release; the next nightly scan of `latest` closes this issue when it is clean. " +
      "The threshold is not lowered and no CVE is ignored.",
    "",
    "_Opened and updated by `.github/workflows/nightly-image-scan.yml` " +
      "(`scripts/ci/image-scan-issue.mjs`). One issue per image; edits to this body are overwritten._",
    "",
    `${FINGERPRINT_PREFIX}${fingerprint(findings)} -->`,
  );
  return lines.join("\n");
}

export function bodyFingerprint(body) {
  for (const line of (body || "").split(/\r\n|\r|\n/)) {
    if (line.startsWith(FINGERPRINT_PREFIX)) {
      let rest = line.slice(FINGERPRINT_PREFIX.length);
      if (rest.endsWith(" -->")) rest = rest.slice(0, -" -->".length);
      return rest.trim();
    }
  }
  return null;
}

/** The issue with EXACTLY this title, or null. */
export async function findIssue(github, owner, repo, title, state) {
  const issues = await github.paginate(github.rest.issues.listForRepo, {
    owner,
    repo,
    state,
    labels: "image-scan",
    per_page: 100,
  });
  const matches = issues.filter((i) => !i.pull_request && i.title === title);
  if (!matches.length) return null;
  return matches.reduce((low, i) => (i.number < low.number ? i : low));
}

/** Apply the table at the top of this file. Returns what it did. */
export async function sync(github, { repo: fullRepo, image, findings, digest, version, runUrl, exempt }) {
  const [owner, repo] = fullRepo.split("/");
  const title = titleFor(image);
  const current = await findIssue(github, owner, repo, title, "open");

  if (!findings.length) {
    if (current === null) return "clean; no open issue";
    await github.rest.issues.createComment({
      owner,
      repo,
      issue_number: current.number,
      body:
        `The nightly scan of \`${image}:latest\` (\`${digest}\`, version \`${version || "unknown"}\`) ` +
        `reports no HIGH or CRITICAL finding. Closing. Run: ${runUrl}`,
    });
    await github.rest.issues.update({ owner, repo, issue_number: current.number, state: "closed", state_reason: "completed" });
    return `clean; closed #${current.number}`;
  }

  const body = renderBody(image, findings, { digest, version, runUrl, exempt });
  const newPrint = fingerprint(findings);

  if (current !== null) {
    const oldPrint = bodyFingerprint(current.body || "");
    await github.rest.issues.update({ owner, repo, issue_number: current.number, body });
    if (oldPrint === newPrint) return `unchanged findings; refreshed #${current.number}`;
    await github.rest.issues.createComment({
      owner,
      repo,
      issue_number: current.number,
      body:
        `The finding set changed: now ${findings.length} HIGH or CRITICAL finding(s) in ` +
        `\`${image}:latest\` (\`${digest}\`). The table above is current. Run: ${runUrl}`,
    });
    return `findings changed; updated #${current.number}`;
  }

  const closed = await findIssue(github, owner, repo, title, "closed");
  if (closed !== null) {
    await github.rest.issues.update({ owner, repo, issue_number: closed.number, state: "open" });
    await github.rest.issues.update({ owner, repo, issue_number: closed.number, body });
    await github.rest.issues.createComment({
      owner,
      repo,
      issue_number: closed.number,
      body:
        `Reopened: the nightly scan of \`${image}:latest\` (\`${digest}\`) reports ` +
        `${findings.length} HIGH or CRITICAL finding(s) again. Run: ${runUrl}`,
    });
    return `regressed; reopened #${closed.number}`;
  }

  const created = await github.rest.issues.create({ owner, repo, title, body, labels: [...LABELS] });
  return `opened ${created.data.html_url}`;
}

const REQUIRED = ["report", "image", "expectType", "blocking", "digest", "runUrl", "repo"];
const FLAG = { report: "--report", image: "--image", expectType: "--expect-type", blocking: "--blocking", digest: "--digest", runUrl: "--run-url", repo: "--repo" };

/**
 * The Python CLI's arguments as an object (`expectType` for --expect-type,
 * `runUrl` for --run-url). Returns its exit code: 0, or 2 when the report
 * proves nothing or an argument is missing. A failed API call throws.
 */
export async function main({ github, args, log = console.log }) {
  const missing = REQUIRED.filter((k) => args[k] === undefined || args[k] === null);
  if (missing.length) {
    log(`::error::the following arguments are required: ${missing.map((k) => FLAG[k]).join(", ")}`);
    return 2;
  }
  if (!["true", "false"].includes(String(args.blocking))) {
    log(`::error::argument --blocking: invalid choice: ${pyRepr(String(args.blocking))} (choose from 'true', 'false')`);
    return 2;
  }
  if (!/^[^/]+\/[^/]+$/.test(args.repo)) {
    log(`::error::argument --repo: expected OWNER/REPO, got ${pyRepr(args.repo)}`);
    return 2;
  }

  let findings;
  try {
    findings = loadFindings(args.report, args.expectType);
  } catch (error) {
    if (!(error instanceof ReportError)) throw error;
    log(`::error::${args.image}: ${error.message}; no issue was opened, updated or closed`);
    return 2;
  }

  const outcome = await sync(github, {
    repo: args.repo,
    image: args.image,
    findings,
    digest: args.digest,
    version: args.version || "",
    runUrl: args.runUrl,
    exempt: String(args.blocking) === "false",
  });
  log(`${args.image}: ${findings.length} finding(s); ${outcome}`);
  return 0;
}
