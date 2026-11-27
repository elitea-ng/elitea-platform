// Every blueprint in the repository's blueprints/ tree (issue 3137) read
// through the real readImportFile: each front-matter key lands where the
// import sends it, nothing is dropped, and avatar files reach the version's
// pack. Only the media upload is stubbed. A key the mapping below does not
// know fails the test, so a new format key cannot be silently lost.
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it, vi } from 'vitest';

import { load as loadYaml } from 'js-yaml';
import JSZip from 'jszip';

import { attachImportAvatars } from './importAvatars';
import { readImportFile, type ReadImportResult } from './importDocument';

type Rec = Record<string, unknown>;

const BLUEPRINTS = join(import.meta.dirname, '../../../../../../blueprints');
const META_KEYS = ['step_limit', 'internal_tools', 'memory', 'context', 'proactivity', 'tool_envelope', 'capabilities', 'run_caps', 'output_contract', 'citations'];
const IGNORED_KEYS = ['schema', 'blueprint'];
// Floors: the tree holds 6 agents and 3 teams (18 agent files in all) today.
const MIN_AGENTS = 6;
const MIN_TEAMS = 3;
const MIN_FILES = 18;

function filesUnder(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}

function split(text: string): { front: Rec; body: string } {
  const match = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*\r?\n?([\s\S]*)$/.exec(text);
  if (match === null) throw new Error('no front matter');
  return { front: loadYaml(match[1] ?? '') as Rec, body: match[2] ?? '' };
}

function versionOf(entry: Rec): Rec {
  const versions = entry['versions'];
  return Array.isArray(versions) && typeof versions[0] === 'object' && versions[0] !== null ? (versions[0] as Rec) : {};
}

interface Landing {
  readonly entry: Rec;
  readonly version: Rec;
  readonly result: ReadImportResult;
}

type Check = (value: unknown, landing: Landing, where: string) => void;

const atEntry: Check = (value, { entry }, where) => expect(entry[where.split(': ')[1] ?? ''], where).toEqual(value);
const atVersion: Check = (value, { version }, where) => expect(version[where.split(': ')[1] ?? ''], where).toEqual(value);
const atMeta: Check = (value, { version }, where) => expect((version['meta'] as Rec)[where.split(': ')[1] ?? ''], where).toEqual(value);

/** Where each front-matter key of the format lands. */
const LANDS: Readonly<Record<string, Check>> = {
  ...Object.fromEntries(META_KEYS.map((key) => [key, atMeta])),
  ...Object.fromEntries(['name', 'description', 'evaluation', 'memory_seeds'].map((key) => [key, atEntry])),
  ...Object.fromEntries(['agent_type', 'welcome_message', 'conversation_starters', 'soul'].map((key) => [key, atVersion])),
  ...Object.fromEntries(IGNORED_KEYS.map((key) => [key, () => undefined])),
  reasoning_effort: (value, { version }, where) => expect((version['llm_settings'] as Rec)['reasoning_effort'], where).toEqual(value),
  tags: (value, { version }, where) => expect((version['tags'] as Rec[]).map((tag) => tag['name']), where).toEqual(value),
  skills: (value, { version, result }, where) => {
    const names = (value as Rec[]).map((skill) => skill['name']);
    expect((result.document.skills as Rec[]).map((skill) => skill['name']), where).toEqual(expect.arrayContaining(names));
    expect(version['skills'], where).toHaveLength(names.length);
  },
  avatar: (value, { version, result }, where) => expectAvatar(value, version, result, where),
};

/** Asserts that every key of `front` reached the entry, the version or the document. */
function expectLossless(front: Rec, body: string, entry: Rec, result: ReadImportResult, file: string): void {
  const landing = { entry, version: versionOf(entry), result };
  for (const [key, value] of Object.entries(front)) {
    const land = LANDS[key];
    if (land === undefined) expect.fail(`${file}: ${key} is a key the import does not map`);
    else land(value, landing, `${file}: ${key}`);
  }
  expect(String(landing.version['instructions']), file).toContain(body.trim().slice(0, 80));
}

function expectAvatar(value: unknown, version: Rec, result: ReadImportResult, where: string): void {
  if (typeof value === 'string') {
    expect((version['avatar_pack'] as Rec | undefined)?.['slots'], where).toEqual({ still: { url: value } });
    return;
  }
  const key = String(version['import_version_uuid']);
  const slots = (result.avatars ?? []).filter((avatar) => avatar.versionKey === key).map((avatar) => avatar.slot);
  expect(slots.sort(), where).toEqual(Object.keys(value as Rec).sort());
}

async function zipOf(files: Readonly<Record<string, string>>, name: string): Promise<File> {
  const zip = new JSZip();
  for (const [path, source] of Object.entries(files)) zip.file(path, readFileSync(source));
  return new File([await zip.generateAsync({ type: 'blob' })], name, { type: 'application/zip' });
}

/** The team folder at teams/<slug>/ beside agents/, so a member named ../../agents/x.agent.md resolves as in the tree. */
function teamZip(slug: string): Promise<File> {
  const files: Record<string, string> = {};
  for (const path of [...filesUnder(join(BLUEPRINTS, 'teams', slug)), ...filesUnder(join(BLUEPRINTS, 'agents'))]) {
    files[relative(BLUEPRINTS, path)] = path;
  }
  return zipOf(files, `${slug}.team.zip`);
}

/** Asserts the team's name, roster and lead; answers the member paths. */
function expectTeam(result: ReadImportResult, slug: string): string[] {
  const manifest = loadYaml(readFileSync(join(BLUEPRINTS, 'teams', slug, 'team.yaml'), 'utf8')) as Rec;
  const members = manifest['members'] as string[];
  expect(result.team?.name).toBe(manifest['name']);
  expect(result.team?.members).toHaveLength(members.length);
  const lead = manifest['lead'];
  if (typeof lead === 'string') expect(result.team?.lead).toBe(result.team?.members[members.indexOf(lead)]);
  return members;
}

/** Uploads the avatar media through a stub; every member then has a pack with a still. Answers the uploads. */
async function expectPacks(result: ReadImportResult): Promise<number> {
  const upload = vi.fn((file: File) => Promise.resolve({ url: `/media/${file.name}` }));
  const document = await attachImportAvatars(result.document, result.avatars ?? [], upload);
  expect(upload).toHaveBeenCalledTimes(result.avatars?.length ?? 0);
  for (const entry of document.applications as Rec[]) {
    const pack = versionOf(entry)['avatar_pack'] as Rec | undefined;
    expect(pack?.['schema'], JSON.stringify(entry['name'])).toBe('elitea.avatar_pack/v1');
    expect((pack?.['slots'] as Rec | undefined)?.['still'], JSON.stringify(entry['name'])).toBeDefined();
  }
  return upload.mock.calls.length;
}

const agentFiles = readdirSync(join(BLUEPRINTS, 'agents')).filter((name) => name.endsWith('.agent.md'));
const teamDirs = readdirSync(join(BLUEPRINTS, 'teams')).filter((name) => statSync(join(BLUEPRINTS, 'teams', name, 'team.yaml'), { throwIfNoEntry: false }) !== undefined);
let filesRead = 0;
let avatarsUploaded = 0;

describe('repository blueprints through readImportFile (issue 3137)', () => {
  it('finds the blueprints it must read', () => {
    expect(agentFiles.length).toBeGreaterThanOrEqual(MIN_AGENTS);
    expect(teamDirs.length).toBeGreaterThanOrEqual(MIN_TEAMS);
  });

  it.each(agentFiles)('reads agents/%s losslessly', async (name) => {
    const text = readFileSync(join(BLUEPRINTS, 'agents', name), 'utf8');
    const result = await readImportFile(new File([text], name, { type: 'text/markdown' }));
    const { front, body } = split(text);
    expect(result.document.applications).toHaveLength(1);
    expectLossless(front, body, result.document.applications?.[0] as Rec, result, name);
    expect(result.skipped, name).toEqual([]);
    filesRead += 1;
  });

  it.each(teamDirs)('reads teams/%s losslessly, avatars into packs', async (slug) => {
    const result = await readImportFile(await teamZip(slug));
    const members = expectTeam(result, slug);
    for (const [index, member] of members.entries()) {
      const { front, body } = split(readFileSync(join(BLUEPRINTS, 'teams', slug, member), 'utf8'));
      expectLossless(front, body, result.document.applications?.[index] as Rec, result, `${slug}/${member}`);
      filesRead += 1;
    }
    expect(result.skipped, slug).toEqual([]);
    avatarsUploaded += await expectPacks(result);
  });

  it('read every agent file of the tree and uploaded the avatar media a team carries', () => {
    expect(filesRead).toBeGreaterThanOrEqual(MIN_FILES);
    expect(avatarsUploaded).toBeGreaterThan(0);
  });
});
