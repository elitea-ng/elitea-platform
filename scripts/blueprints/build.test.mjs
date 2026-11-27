// Tests of the blueprint builder (issue 3137): the real blueprints/ tree
// builds, every gallery entry names a built file, the team zips are the
// importable layout, the release asset names are stable, and the avatar
// checks refuse what the platform refuses. Run: node --test scripts/blueprints/build.test.mjs
// (needs scripts/ci's `yaml`: npm ci --prefix scripts/ci).

import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { inflateRawSync } from 'node:zlib';

import { buildBlueprints, checkAvatar, releaseAssetName, releaseNotes, zipFiles, AVATAR_SLOTS } from './build.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const yaml = createRequire(path.join(root, 'scripts/ci/package.json'))('yaml');
const io = { root, parseYaml: (text) => yaml.parse(text), dumpYaml: (value) => yaml.stringify(value) };

/** The entries of a zip, read from its central directory: name -> bytes. */
function unzip(buffer) {
  const end = buffer.lastIndexOf(Buffer.from([0x50, 0x4b, 0x05, 0x06]));
  const count = buffer.readUInt16LE(end + 10);
  let at = buffer.readUInt32LE(end + 16);
  const entries = new Map();
  for (let i = 0; i < count; i += 1) {
    const method = buffer.readUInt16LE(at + 10);
    const size = buffer.readUInt32LE(at + 20);
    const nameLength = buffer.readUInt16LE(at + 28);
    const local = buffer.readUInt32LE(at + 42);
    const name = buffer.subarray(at + 46, at + 46 + nameLength).toString('utf8');
    const dataStart = local + 30 + buffer.readUInt16LE(local + 26) + buffer.readUInt16LE(local + 28);
    const raw = buffer.subarray(dataStart, dataStart + size);
    entries.set(name, method === 8 ? inflateRawSync(raw) : raw);
    at += 46 + nameLength;
  }
  return entries;
}

const built = await buildBlueprints(io);

test('the tree holds the owner-approved blueprints', () => {
  const agents = built.index.agents.map((agent) => agent.slug).sort();
  assert.deepEqual(agents, ['agent-smith', 'code-reviewer', 'daily-ops-digest', 'data-analyst', 'research-scout', 'writing-editor']);
  const teams = built.index.teams.map((team) => team.slug).sort();
  assert.deepEqual(teams, ['content-studio', 'elitea-dev-team', 'incident-desk']);
});

test('every gallery entry names a built file and carries a brief', () => {
  for (const entry of [...built.index.agents, ...built.index.teams]) {
    assert.ok(built.files.has(entry.file), `${entry.slug}: ${entry.file} was not built`);
    assert.ok(entry.brief.length >= 40, `${entry.slug}: brief`);
    assert.ok(entry.showcases.length > 0, `${entry.slug}: showcases`);
    const still = entry.avatar?.still;
    if (still !== undefined) assert.ok(built.files.has(still), `${entry.slug}: ${still}`);
  }
});

test('a team zip is the importable layout: team.yaml, its members and their avatars', () => {
  const team = built.index.teams.find((entry) => entry.slug === 'elitea-dev-team');
  assert.equal(team.members.length, 8);
  assert.equal(team.lead, 'dev-coordinator');
  const entries = unzip(built.files.get(team.file));
  const manifest = yaml.parse(entries.get('team.yaml').toString('utf8'));
  assert.equal(manifest.schema, 'elitea.team/v1');
  assert.equal(manifest.floor_policy, 'lead');
  assert.equal(manifest.lead, 'members/dev-coordinator.agent.md');
  for (const member of manifest.members) assert.ok(entries.has(member), member);
  for (const member of team.members) {
    for (const slot of AVATAR_SLOTS) {
      const file = `avatars/${member.slug}/${slot}.webp`;
      assert.ok(entries.has(file), file);
      checkAvatar(entries.get(file), file);
    }
  }
});

test('a member that lives beside the single agents is copied into the team zip', () => {
  const entries = unzip(built.files.get('teams/content-studio.team.zip'));
  const manifest = yaml.parse(entries.get('team.yaml').toString('utf8'));
  assert.deepEqual(manifest.members, ['members/research-scout.agent.md', 'members/fact-checker.agent.md', 'members/writing-editor.agent.md']);
  assert.equal(entries.get('members/research-scout.agent.md').toString('utf8'), built.files.get('agents/research-scout.agent.md').toString('utf8'));
});

test('two builds of the same tree are byte-identical', async () => {
  const again = await buildBlueprints(io);
  for (const [name, data] of built.files) assert.ok(Buffer.compare(data, again.files.get(name)) === 0, name);
});

test('release assets: one importable file per blueprint, named by kind, slug and version', () => {
  assert.equal(releaseAssetName('teams/incident-desk.team.zip', '1.2.3'), 'elitea-team-incident-desk-1.2.3.team.zip');
  assert.equal(releaseAssetName('agents/research-scout.agent.md', '1.2.3'), 'elitea-agent-research-scout-1.2.3.agent.md');
  assert.equal(releaseAssetName('agents/x.agent.zip', 'nightly'), 'elitea-agent-x-nightly.agent.zip');
  assert.equal(releaseAssetName('index.json', '1.2.3'), undefined);
  assert.equal(releaseAssetName('avatars/a/b/still.webp', '1.2.3'), undefined);
  const assets = [...built.files.keys()].filter((name) => releaseAssetName(name, 'v') !== undefined);
  assert.equal(assets.length, built.index.agents.length + built.index.teams.length);
  const notes = releaseNotes(built.index, (file) => releaseAssetName(file, '1.2.3'));
  for (const name of assets) assert.match(notes, new RegExp(releaseAssetName(name, '1.2.3').replaceAll('.', '\\.')));
});

test('the avatar checks refuse what the platform refuses', () => {
  // A minimal animated WebP: VP8X 600x600, then two ANMF frames of 2.5 s.
  const chunk = (kind, body) => {
    const head = Buffer.alloc(8);
    head.write(kind, 0, 'latin1');
    head.writeUInt32LE(body.length, 4);
    return Buffer.concat([head, body, Buffer.alloc(body.length & 1)]);
  };
  const webp = (edge, frameMs, frames) => {
    const vp8x = Buffer.alloc(10);
    vp8x.writeUIntLE(edge - 1, 4, 3);
    vp8x.writeUIntLE(edge - 1, 7, 3);
    const anmf = Buffer.alloc(16);
    anmf.writeUIntLE(frameMs, 12, 3);
    const body = Buffer.concat([Buffer.from('WEBP', 'latin1'), chunk('VP8X', vp8x), ...Array.from({ length: frames }, () => chunk('ANMF', anmf))]);
    const head = Buffer.alloc(8);
    head.write('RIFF', 0, 'latin1');
    head.writeUInt32LE(body.length, 4);
    return Buffer.concat([head, body]);
  };
  assert.doesNotThrow(() => checkAvatar(webp(512, 1000, 3), 'ok.webp'));
  assert.throws(() => checkAvatar(webp(600, 100, 1), 'big.webp'), /over 512 px/);
  assert.throws(() => checkAvatar(webp(512, 2500, 2), 'long.webp'), /over the 4000 ms/);
  assert.throws(() => checkAvatar(webp(512, 100, 1), 'named.png'), /not \.png/);
  assert.throws(() => checkAvatar(Buffer.alloc(3 << 20), 'huge.webp'), /avatar limit/);
  assert.throws(() => checkAvatar(Buffer.from('not an image at all'), 'x.webp'), /not a WebP/);
});

test('the zip writer round-trips stored and deflated entries', () => {
  const entries = unzip(zipFiles([['a.md', Buffer.from('hello hello hello')], ['b.webp', Buffer.from([1, 2, 3])]]));
  assert.equal(entries.get('a.md').toString('utf8'), 'hello hello hello');
  assert.deepEqual([...entries.get('b.webp')], [1, 2, 3]);
});

test('the release notes section is appended once and replaced on a re-run', async () => {
  const { mergeNotes } = await import('./build.mjs');
  const section = releaseNotes(built.index, (file) => releaseAssetName(file, '1.2.3'));
  const once = mergeNotes('Release body.\n', section);
  assert.ok(once.startsWith('Release body.\n\n<!-- blueprints -->'));
  const twice = mergeNotes(`${once}\nAfter.\n`, section.replace('1.2.3', '1.2.4'));
  assert.equal(twice.split('<!-- blueprints -->').length, 2);
  assert.match(twice, /After\./);
});
