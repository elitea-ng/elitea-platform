// node --test scripts/ci/brand-output-scan.test.mjs (run by no-binaries.yml)
//
// The sample-brand output scan (ADR-0052 W4) on fixture trees: words fail,
// identifiers in any form fail, code names pass, NOTICE and licence files are
// skipped, the allow-list is reviewed, and a binary is read through its strings.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { ALLOWED, eliteaIdentifiers, isBinary, printableStrings, report, scan, scanText } from './brand-output-scan.mjs';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const SCRIPT = path.join(REPO, 'scripts/ci/brand-output-scan.mjs');
const MANIFEST = JSON.parse(fs.readFileSync(path.join(REPO, 'brand/elitea/brand.json'), 'utf8'));
const IDS = eliteaIdentifiers(MANIFEST);
const find = (text, lane = 'web') => scanText(text, { file: 'f.js', lane, identifiers: IDS }).map((f) => f.match);

function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'brand-scan-'));
  for (const [name, content] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), content);
  }
  return root;
}

test('the ELITEA identifiers snapshot: what brand/elitea names today', () => {
  assert.deepEqual(IDS, ['ai.elitea.app.widgets', 'SupportAlita@epam.com', 'group.ai.elitea.app', 'ai.elitea.desktop', 'ai.elitea.app', 'elitea.ai', 'elitea://']);
});

test('a product word a person reads fails', () => {
  assert.deepEqual(find('title:`Elitea Assistant`'), ['Elitea']);
  assert.deepEqual(find('"Ask me about ELITEA."'), ['ELITEA']);
  assert.deepEqual(find('by EPAM Systems'), ['EPAM', 'EPAM']);
  assert.deepEqual(find('Elitea-Handoff: 1'), ['Elitea']);
});

test('a name in code passes: identifiers, header names, lower-case paths', () => {
  assert.deepEqual(find('EliteaAssistant eliteaFetch e.elitea_title TEliteaAssistantRef'), []);
  assert.deepEqual(find('"X-Elitea-Trigger-Token" "X-Elitea-Wiki-Chat"'), []);
  assert.deepEqual(find('/api/v2/elitea_core/folder/prompt_lib/ .elitea-assistant-markdown --elitea-avatar-peak'), []);
});

test('an identifier fails in any form, even packed against the next string', () => {
  assert.deepEqual(find('placeholder="acme.elitea.ai"'), ['elitea.ai']);
  assert.deepEqual(find('Set by acme.elitea.aimpact.medium'), ['elitea.ai']);
  assert.deepEqual(find('com.apple.x ai.elitea.desktop'), ['ai.elitea.desktop']);
  assert.deepEqual(find('elitea://connect?x'), ['elitea://']);
  assert.deepEqual(find('mailto:supportalita@epam.com'), ['epam', 'supportalita@epam.com']);
});

test('NOTICE and licence files are skipped; everything else is read', () => {
  const root = tree({
    NOTICE: 'Copyright EPAM Systems. ELITEA.',
    'assets/vendor.js.LICENSE.txt': 'Elitea',
    'LICENSE-MIT': 'Elitea',
    'assets/app.js': 'ok',
    'index.html': '<title>Sample</title>',
  });
  const { findings, scanned } = scan([root], { lane: 'web', identifiers: IDS, cwd: root });
  assert.deepEqual(findings, []);
  assert.equal(scanned, 2);
});

test('the allow-list is reviewed: each entry has a reason and a lane, and lets only its text through', () => {
  for (const a of ALLOWED) {
    assert.ok(a.why.length > 40, `ALLOWED "${a.text ?? a.re}" needs a reason`);
    assert.ok((typeof a.text === 'string') !== (a.re instanceof RegExp), 'an entry has a text or a re, not both');
    assert.ok(['*', 'web', 'mobile', 'desktop'].includes(a.lane));
  }
  assert.deepEqual(find('[`Project Management`,`Elitea`,`Epam`,`Other`]'), []);
  assert.deepEqual(find('`Elitea` alone'), ['Elitea']);
  assert.deepEqual(find('Elitea-Handoff: 1', 'desktop'), []);
  assert.deepEqual(find('Elitea-Handoff: 1', 'mobile'), ['Elitea']);
  assert.deepEqual(find('Elitea-managed folder is read-only', 'desktop'), ['Elitea']);
  assert.deepEqual(find('ai.elitea.desktopcom.apple.keychainaccess', 'desktop'), []);
  assert.deepEqual(find('ai.elitea.desktoporg.keepassxc.keepassxc', 'desktop'), []);
  assert.deepEqual(find('x\nai.elitea.desktop\ny', 'desktop'), []);
  assert.deepEqual(find('Open ai.elitea.desktop.plist', 'desktop'), ['ai.elitea.desktop']);
});

test('a binary is read through its printable strings', () => {
  const bytes = Buffer.concat([Buffer.from([0xc6, 0x1f, 0, 0, 1]), Buffer.from('About Elitea'), Buffer.from([0, 2, 0]), Buffer.from('ok')]);
  assert.equal(isBinary(bytes), true);
  assert.equal(printableStrings(bytes), 'About Elitea');
  const root = tree({ 'Sample.app/Contents/MacOS/sample': bytes });
  const { findings } = scan([root], { lane: 'desktop', identifiers: IDS, cwd: root });
  assert.deepEqual(report(findings), ['Sample.app/Contents/MacOS/sample: "Elitea" in …About Elitea…']);
});

test('a UTF-16 string (Hermes, for text with a non-ASCII character) is read too', () => {
  const utf16 = Buffer.from('Back to Elitea\u2019s home', 'utf16le');
  for (const pad of [0, 1]) {
    const bytes = Buffer.concat([Buffer.alloc(pad), Buffer.from([0xc6, 0x1f, 0, 0]), utf16, Buffer.from([0, 0, 0, 0])]);
    assert.match(printableStrings(bytes), /Back to Elitea s home/);
    assert.deepEqual(scanText(printableStrings(bytes), { file: 'b', lane: 'mobile', identifiers: IDS }).map((f) => f.match), ['Elitea']);
  }
});

test('the CLI fails on a literal, fails on nothing scanned, refuses another brand, passes clean output', () => {
  const run = (args, brand = 'sample') => spawnSync(process.execPath, [SCRIPT, ...args], { env: { PATH: process.env.PATH, ELITEA_BRAND: brand }, encoding: 'utf8' });
  const leak = tree({ 'app.js': 'title:`Elitea Assistant`' });
  const bad = run(['--lane', 'web', leak]);
  assert.equal(bad.status, 1);
  assert.match(bad.stdout, /::error::.*app\.js: "Elitea" in …title:`Elitea Assistant`…/);
  const empty = tree({});
  assert.match(run(['--lane', 'web', empty]).stdout, /no file under/);
  assert.equal(run(['--lane', 'web', empty]).status, 1);
  assert.equal(run(['--lane', 'web', leak], 'elitea').status, 2);
  assert.equal(run(['--lane', 'web', leak], '../acme/sample').status, 2);
  assert.equal(run(['--lane', 'web', leak], path.join(REPO, 'brand/sample')).status, 1);
  assert.equal(run(['--lane', 'tv', leak]).status, 2);
  const clean = run(['--lane', 'web', tree({ 'app.js': 'title:`Sample Assistant` eliteaFetch' })]);
  assert.equal(clean.status, 0, clean.stdout);
  assert.match(clean.stdout, /1 file\(s\), no ELITEA literal/);
});
