#!/usr/bin/env node
/**
 * Incident escalation for .github/workflows/deepwiki-real-engine.yml
 * (DWIKI-014). That workflow is weekly cron + workflow_dispatch only, same
 * shape as ci-web-mutation.yml and for the same reason: a real, expensive
 * pass that cannot afford to run per-PR. A weekly job nobody is watching by
 * design needs the same accountability mutate-spotcheck.mjs already has —
 * see createOrUpdateGitHubIssue in scripts/lib/github-issue.mjs — so a red
 * run files (or comments on) a de-duplicated GitHub issue instead of sitting
 * unread in the Actions tab until the drift it was built to catch has aged
 * a month.
 *
 * Invoked as the workflow's last step, `if: failure()`. Never fails the
 * build itself (always exits 0) — this only reports, it is not a gate.
 */
import { createOrUpdateGitHubIssue } from './lib/github-issue.mjs';

async function main() {
  const runUrl = process.env.GITHUB_SERVER_URL && process.env.GITHUB_REPOSITORY && process.env.GITHUB_RUN_ID
    ? `${process.env.GITHUB_SERVER_URL}/${process.env.GITHUB_REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}`
    : '(local run)';
  const engineImage = process.env.DEEPWIKI_ENGINE_IMAGE || '(default: local bake of elitea-deepwiki-engine-native)';
  const sha = process.env.GITHUB_SHA || '(unknown)';
  // The native engine is the only one. The title is the one its leg had
  // when the workflow ran two engines, so an open issue keeps collecting
  // its comments.
  const issueTitle = 'DeepWiki real-engine incident (native engine): weekly run failed';
  const issueBody = [
    '## DeepWiki real-engine — incident report',
    '',
    'The weekly `deepwiki-real-engine.yml` run (DWIKI-014: the product against',
    'the REAL DeepWiki analysis engine, not the fixture one `deepwiki-stack`',
    'gates every PR against) failed.',
    '',
    '| Field | Value |',
    '|-------|-------|',
    `| Run | ${runUrl} |`,
    `| Commit | ${sha} |`,
    '| Engine | native |',
    `| Engine image | ${engineImage} |`,
    '',
    '### What this job alone catches',
    '',
    'Drift between the fixture engine (`deepwiki-stack`, on every PR) and the',
    'real one — a schema the real engine emits that the fixture never learned,',
    'a real model call the callback bearer or the sidecar wiring rejects. That',
    'drift moves on the scale of weeks, which is why this job is weekly and not',
    'per-PR (see deepwiki-real-engine.yml header).',
    '',
    '### Action required',
    '',
    'Open the run above, pull the `playwright-report-deepwiki-real-engine`',
    'artifact, and determine whether this is real drift or an infrastructure',
    'flake. Re-run via `workflow_dispatch` after a fix; pass `engine_image`',
    'to pin a known-good image while triaging.',
    '',
    'If the log stops at "is at <sha>, not the pinned <sha>",',
    'github.com/kharkevich-engineering-lab/floe moved past the pin. That is',
    'not drift in the engine: re-verify and move the pin (see the workflow',
    'header), or dispatch with `native_ref` set to a tag at the pin.',
  ].join('\n');

  await createOrUpdateGitHubIssue(issueTitle, issueBody, {
    labels: ['deepwiki', 'test-quality', 'ci-incident'],
    logPrefix: '[deepwiki-real-engine-incident]',
    userAgent: 'elitea-deepwiki-real-engine-incident/1.0',
  });

  process.exit(0);
}

main().catch((err) => {
  console.error('[deepwiki-real-engine-incident] Unexpected error:', err);
  // Never fail the build over the escalation itself.
  process.exit(0);
});
