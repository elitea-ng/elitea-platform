#!/usr/bin/env node
/**
 * tool-categories.mjs — the Category column of the toolkit reference pages.
 *
 * The single source is libs/proto/contracts/tool-categories-v1.json
 * (ADR-0041 decision 5). The Rust worker reads it for the approval policy and
 * elitea-main reads it for the pipeline approval rule. The docs show the same
 * value, so this script GENERATES the column from the contract. Do not edit
 * the column by hand.
 *
 * Each toolkit page lists its tools in a "| Tool | Category |" table under
 * "## Available tools". A tool name is the display form of the tool id
 * ("Apply git patch" is apply_git_patch). A tool that the contract does not
 * list has the contract's default category (write).
 *
 * Usage (from apps/elitea-docs):
 *   node scripts/tool-categories.mjs           write the column into the pages
 *   node scripts/tool-categories.mjs --check   fail when a page does not match
 *
 * The check also fails when the contract names a tool that its page does not
 * list, or names a toolkit that no page maps to, so a stale page cannot hide.
 */
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

export const CONTRACT_PATH = fileURLToPath(
  new URL('../../../libs/proto/contracts/tool-categories-v1.json', import.meta.url),
);
export const PAGES_DIR = fileURLToPath(new URL('../src/content/docs/integrations/toolkits/', import.meta.url));

/**
 * The toolkit type of each page whose file name is not "<type>_toolkit.mdx".
 * null: the page has no tool table (the tools are defined by the user).
 */
export const PAGE_TOOLKIT = {
  'custom_toolkit.mdx': null,
  'openapi_toolkit.mdx': null,
  'web_search_toolkit.mdx': null,
  'reportportal_toolkit.mdx': 'report_portal',
  'servicenow_toolkit.mdx': 'service_now',
  'xray_toolkit.mdx': 'xray_cloud',
};

/**
 * Contract toolkits that have no reference page of their own. They are
 * listed on other-toolkits.mdx or are internal. A new contract toolkit that
 * is in neither list fails the check, so someone decides where it goes.
 */
export const NO_PAGE = new Set([
  'azure',
  'azure_search',
  'bigquery',
  'carrier',
  'data_analysis',
  'delta_lake',
  'elastic',
  'kubernetes',
  'localgit',
  'sandbox',
  'vectorstore',
  'yagmail',
  'zephyr',
  'zephyr_essential',
  'zephyr_squad',
]);

/** The line above the table. It is part of the generated block. */
export const NOTE =
  'The category of a tool decides when a run needs human approval to call it. ' +
  'See [Runtime safety](/admin/enterprise-controls/runtime-safety#tool-categories-and-approval-policy).';
const HEADER = '| Tool | Category |\n|---|---|\n';
const TABLE = /^(?:The category of a tool decides[^\n]*\n\n)?\| Tool \|(?: Category \|)?\n\|---\|(?:---\|)?\n((?:\|.*\|\n)*)/m;

/** apply_git_patch for "Apply git patch". */
export function toolId(displayName) {
  return displayName.trim().toLowerCase().replace(/\s+/g, '_');
}

/** The toolkit type a page documents, null for a page with no tool table. */
export function pageToolkit(file) {
  if (Object.hasOwn(PAGE_TOOLKIT, file)) return PAGE_TOOLKIT[file];
  return file.endsWith('_toolkit.mdx') ? file.slice(0, -'_toolkit.mdx'.length) : null;
}

/**
 * Renders a page with its Category column taken from the contract. Returns
 * the new text and the problems found (an empty list when the page is good).
 */
export function renderPage(text, toolkit, contract) {
  const match = TABLE.exec(text);
  if (!match) return { text, problems: [`no "| Tool |" table under "## Available tools"`] };
  const categories = contract.toolkits[toolkit] ?? {};
  const names = match[1]
    .split('\n')
    .filter((line) => line.startsWith('|'))
    .map((line) => line.split('|')[1].trim());
  const problems = [];
  const listed = new Set(names.map(toolId));
  for (const tool of Object.keys(categories)) {
    if (!listed.has(tool)) problems.push(`the contract names ${toolkit}.${tool}, but the page does not list it`);
  }
  for (const [tool, category] of Object.entries(categories)) {
    if (!contract.categories.includes(category)) problems.push(`${toolkit}.${tool}: unknown category ${category}`);
  }
  const rows = names
    .map((name) => `| ${name} | \`${categories[toolId(name)] ?? contract.default_category}\` |\n`)
    .join('');
  const table = `${NOTE}\n\n${HEADER}${rows}`;
  return { text: text.slice(0, match.index) + table + text.slice(match.index + match[0].length), problems };
}

/** Runs over every page. Returns { changed: [file], problems: [string] }. */
export function run({ write }) {
  const contract = JSON.parse(readFileSync(CONTRACT_PATH, 'utf8'));
  const changed = [];
  const problems = [];
  const mapped = new Set();
  for (const file of readdirSync(PAGES_DIR).filter((f) => f.endsWith('_toolkit.mdx')).sort()) {
    const toolkit = pageToolkit(file);
    if (toolkit === null) continue;
    mapped.add(toolkit);
    const path = join(PAGES_DIR, file);
    const before = readFileSync(path, 'utf8');
    const { text, problems: pageProblems } = renderPage(before, toolkit, contract);
    problems.push(...pageProblems.map((p) => `${file}: ${p}`));
    if (text !== before) {
      changed.push(file);
      if (write) writeFileSync(path, text);
    }
  }
  for (const toolkit of Object.keys(contract.toolkits)) {
    if (!mapped.has(toolkit) && !NO_PAGE.has(toolkit)) {
      problems.push(`the contract names toolkit ${toolkit}, but no page maps to it and it is not in NO_PAGE`);
    }
  }
  return { changed, problems };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const check = process.argv.includes('--check');
  const { changed, problems } = run({ write: !check });
  for (const p of problems) console.error(`tool-categories: ${p}`);
  if (check && changed.length > 0) {
    console.error(
      `tool-categories: ${changed.length} page(s) do not match ${CONTRACT_PATH}: ${changed.join(', ')}. ` +
        'Run "node scripts/tool-categories.mjs" in apps/elitea-docs and commit the result.',
    );
  }
  if (problems.length > 0 || (check && changed.length > 0)) process.exit(1);
  console.log(check ? 'tool-categories --check: OK' : `tool-categories: wrote ${changed.length} page(s)`);
}
