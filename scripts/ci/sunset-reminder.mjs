#!/usr/bin/env node
// The sunset reminder of ADR-0059 decision 2 (story R0.14b, #2590).
//
// From REMIND_DAYS before SunsetAfter on, the daily run of
// .github/workflows/sunset-reminder.yml opens ONE tracking issue for the
// removal of the legacy items, so nobody meets case 4 of
// scripts/ci/check-sunset.mjs by surprise. Unlike the check, it reads the
// clock; it gates nothing.
//
// Idempotent: the issue title holds the date, and the run looks for an issue
// of that exact title in ANY state first. A second run opens none, and one
// closed by hand is not reopened. A new date (a later R3.15 edit) is a new
// title, so it gets its own issue.
//
//   GITHUB_TOKEN=... GITHUB_REPOSITORY=owner/name node scripts/ci/sunset-reminder.mjs [--today YYYY-MM-DD]

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { dayNumber, parseConstants, REGISTRY } from './check-sunset.mjs';

export const REMIND_DAYS = 30;

export const title = (sunsetAfter) => `Sunset on ${sunsetAfter}: remove the legacy administration items (ADR-0059)`;

export function body(sunsetAfter) {
  return [
    `The sunset date of ADR-0059 decision 2 is **${sunsetAfter}** (\`SunsetAfter\` in \`${REGISTRY}\`).`,
    '',
    'On that date the removal may merge, and from 14 days after it `scripts/ci/check-sunset.mjs` fails every pull request that still holds a legacy item:',
    '',
    '- the permission names `runtime.plugins` and `configuration.governance` (R4.2),',
    '- `inherit` token administration access (R4.7),',
    '- the reason warning period: requests without a reason (R4.8).',
    '',
    'One branch, one `feat!` pull request. Opened by `.github/workflows/sunset-reminder.yml`.',
  ].join('\n');
}

/**
 * What to do today. `existing` is the list of issue titles that already
 * carry this date's title. Answers { open: boolean, reason }.
 */
export function decide({ today, sunsetAfter, existing }) {
  if (!sunsetAfter) return { open: false, reason: 'SunsetAfter is empty: no date to remind of' };
  const due = dayNumber(sunsetAfter);
  const now = dayNumber(today);
  if (Number.isNaN(due)) throw new Error(`SunsetAfter "${sunsetAfter}" is not a YYYY-MM-DD date`);
  if (Number.isNaN(now)) throw new Error(`today "${today}" is not a YYYY-MM-DD date`);
  const left = due - now;
  if (left > REMIND_DAYS) return { open: false, reason: `${left} days before ${sunsetAfter}; the reminder opens ${REMIND_DAYS} days before` };
  if (existing.includes(title(sunsetAfter))) return { open: false, reason: `the issue "${title(sunsetAfter)}" exists` };
  return { open: true, reason: `${left} days before ${sunsetAfter} and no issue yet` };
}

/**
 * One run: read the date, ask `github` for the issues of the title, open one
 * when decide() says so. `github` is { findTitles(title) -> [titles], create({title, body}) }.
 */
export async function run({ root, today, github }) {
  const { sunsetAfter } = parseConstants(fs.readFileSync(path.join(root, REGISTRY), 'utf8'));
  const existing = sunsetAfter ? await github.findTitles(title(sunsetAfter)) : [];
  const decision = decide({ today, sunsetAfter, existing });
  if (decision.open) await github.create({ title: title(sunsetAfter), body: body(sunsetAfter) });
  return decision;
}

/** The REST client of the workflow: the issue search, exact-title filtered. */
export function restClient({ token, repository, fetchImpl = fetch }) {
  const api = 'https://api.github.com';
  const headers = { authorization: `Bearer ${token}`, accept: 'application/vnd.github+json', 'x-github-api-version': '2022-11-28' };
  const call = async (url, init = {}) => {
    const res = await fetchImpl(url, { ...init, headers: { ...headers, ...(init.headers || {}) } });
    if (!res.ok) throw new Error(`${init.method || 'GET'} ${url}: ${res.status} ${await res.text()}`);
    return res.json();
  };
  return {
    async findTitles(wanted) {
      const q = encodeURIComponent(`repo:${repository} is:issue in:title "${wanted}"`);
      const found = await call(`${api}/search/issues?q=${q}&per_page=100`);
      return found.items.map((item) => item.title).filter((t) => t === wanted);
    },
    async create(issue) {
      await call(`${api}/repos/${repository}/issues`, { method: 'POST', body: JSON.stringify(issue), headers: { 'content-type': 'application/json' } });
    },
  };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const i = process.argv.indexOf('--today');
  const today = i >= 0 ? process.argv[i + 1] : new Date().toISOString().slice(0, 10);
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  const { GITHUB_TOKEN: token, GITHUB_REPOSITORY: repository } = process.env;
  if (!token || !repository) {
    console.log('::error::sunset reminder: GITHUB_TOKEN and GITHUB_REPOSITORY are required');
    process.exit(1);
  }
  const decision = await run({ root, today, github: restClient({ token, repository }) });
  console.log(`sunset reminder (${today}): ${decision.open ? 'opened the issue' : 'nothing to do'}: ${decision.reason}`);
}
