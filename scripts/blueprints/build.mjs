#!/usr/bin/env node
// The blueprint builder (issue 3137): the ONE reader of the repository's
// blueprints/ tree. Everything that ships a blueprint builds it here, so the
// gallery of the web app (apps/elitea-web/vite.config.ts, plugin
// `elitea-blueprints`), the docs site's downloads and the release assets
// (.github/workflows/publish.yml) are the same bytes.
//
// Source tree (docs: apps/elitea-docs/src/content/docs/reference/agent-file-format.mdx):
//
//   blueprints/agents/<slug>.agent.md          a single agent (elitea.agent/v1)
//   blueprints/agents/avatars/<slug>/<slot>.*  its uploaded avatar media, if any
//   blueprints/teams/<slug>/team.yaml          a team (elitea.team/v1); a member
//                                              path may leave the folder
//                                              (../../agents/x.agent.md), so a
//                                              team reuses a single agent's file
//   blueprints/teams/<slug>/members/*.agent.md members of that team only
//   blueprints/teams/<slug>/avatars/<member>/<slot>.*
//
// Built tree (every file importable as it is through Import):
//
//   index.json                  what the gallery reads
//   agents/<slug>.agent.md      a single agent with no media, or
//   agents/<slug>.agent.zip     one with uploaded avatar media
//   teams/<slug>.team.zip       team.yaml + members/*.agent.md + avatars/
//   avatars/<team>/<member>/still.<ext>, avatars/agents/<slug>/still.<ext>
//                               the gallery's card images
//   release-notes.md            the release body's list of assets
//
// YAML is injected (`parseYaml`, `dumpYaml`): the web build passes js-yaml,
// the CLI below loads scripts/ci's `yaml`. The builder has no dependency of its
// own, so each caller keeps the YAML library it already pins.

import { readdir, readFile, mkdir, writeFile, stat } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateRawSync } from 'node:zlib';

export const AGENT_SCHEMA = 'elitea.agent/v1';
export const TEAM_SCHEMA = 'elitea.team/v1';
export const INDEX_SCHEMA = 'elitea.blueprints/v1';

/** The avatar limits of the platform (services/elitea-main/internal/domain/agentmedia/validate.go). */
export const AVATAR_MAX_BYTES = 2 << 20;
export const AVATAR_MAX_EDGE = 512;
export const AVATAR_MAX_DURATION_MS = 4000;
export const AVATAR_SLOTS = ['still', 'idle', 'thinking', 'speaking', 'working', 'waiting', 'success', 'error', 'waking'];
const DEFAULT_GLYPHS = ['owl', 'hawk', 'fox', 'wolf', 'bear', 'otter', 'heron', 'lynx'];
/** A team has 1 to 16 agents (agentteams.MaxMembers). */
const MAX_TEAM_MEMBERS = 16;
/**
 * The whole tree's weight. WebP does not delta-compress, so every revision of
 * an avatar is a full copy in history; this keeps the set from growing by
 * accident (the dev team's 72 animations are ~34 MB).
 */
export const MAX_TREE_BYTES = 40 * 1024 * 1024;

/* ── zip (store/deflate, deterministic) ──────────────────────────────────── */

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n += 1) {
    let c = n;
    for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

function crc32(data) {
  let crc = 0xffffffff;
  for (const byte of data) crc = CRC_TABLE[(crc ^ byte) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

/**
 * A zip of `entries` ([name, bytes] in order). Dates are fixed (1980-01-01) so
 * two builds of the same tree are byte-identical. Media that is already
 * compressed (webp, png, jpg, mp4, webm) is stored; text is deflated.
 */
export function zipFiles(entries) {
  const locals = [];
  const centrals = [];
  let offset = 0;
  for (const [name, raw] of entries) {
    const data = Buffer.from(raw);
    const nameBytes = Buffer.from(name, 'utf8');
    const store = /\.(webp|png|jpe?g|mp4|webm|zip)$/i.test(name);
    const body = store ? data : deflateRawSync(data, { level: 9 });
    const method = store ? 0 : 8;
    const crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0x0800, 6); // UTF-8 names
    local.writeUInt16LE(method, 8);
    local.writeUInt16LE(0, 10); // time
    local.writeUInt16LE(0x21, 12); // 1980-01-01
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(body.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(nameBytes.length, 26);
    local.writeUInt16LE(0, 28);
    locals.push(local, nameBytes, body);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0x0800, 8);
    central.writeUInt16LE(method, 10);
    central.writeUInt16LE(0, 12);
    central.writeUInt16LE(0x21, 14);
    central.writeUInt32LE(crc, 16);
    central.writeUInt32LE(body.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(nameBytes.length, 28);
    central.writeUInt32LE(offset, 42);
    centrals.push(central, nameBytes);
    offset += local.length + nameBytes.length + body.length;
  }
  const centralSize = centrals.reduce((sum, part) => sum + part.length, 0);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralSize, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...locals, ...centrals, end]);
}

/* ── avatar media checks ─────────────────────────────────────────────────── */

/**
 * Width, height and total animation length of a WebP, PNG or JPEG, read from
 * its own headers (the platform sniffs the content the same way; the
 * extension is not trusted). Throws on anything else.
 */
export function inspectImage(data, name) {
  const ascii = (start, length) => data.subarray(start, start + length).toString('latin1');
  if (ascii(0, 4) === 'RIFF' && ascii(8, 4) === 'WEBP') {
    let width = 0;
    let height = 0;
    let durationMs = 0;
    for (let at = 12; at + 8 <= data.length; ) {
      const kind = ascii(at, 4);
      const size = data.readUInt32LE(at + 4);
      const chunk = at + 8;
      if (kind === 'VP8X') {
        width = 1 + data.readUIntLE(chunk + 4, 3);
        height = 1 + data.readUIntLE(chunk + 7, 3);
      } else if (kind === 'VP8 ' && width === 0) {
        width = data.readUInt16LE(chunk + 6) & 0x3fff;
        height = data.readUInt16LE(chunk + 8) & 0x3fff;
      } else if (kind === 'VP8L' && width === 0) {
        const bits = data.readUInt32LE(chunk + 1);
        width = 1 + (bits & 0x3fff);
        height = 1 + ((bits >> 14) & 0x3fff);
      } else if (kind === 'ANMF') {
        durationMs += data.readUIntLE(chunk + 12, 3);
      }
      at = chunk + size + (size & 1);
    }
    return { type: 'webp', width, height, durationMs };
  }
  if (data.readUInt32BE(0) === 0x89504e47) {
    return { type: 'png', width: data.readUInt32BE(16), height: data.readUInt32BE(20), durationMs: 0 };
  }
  if (data[0] === 0xff && data[1] === 0xd8) {
    for (let at = 2; at + 9 < data.length; ) {
      if (data[at] !== 0xff) break;
      const marker = data[at + 1];
      const length = data.readUInt16BE(at + 2);
      if (marker >= 0xc0 && marker <= 0xc3) {
        return { type: 'jpg', width: data.readUInt16BE(at + 7), height: data.readUInt16BE(at + 5), durationMs: 0 };
      }
      at += 2 + length;
    }
  }
  throw new Error(`${name}: not a WebP, PNG or JPEG image`);
}

/** Throws when an avatar file breaks a platform limit; answers its inspection. */
export function checkAvatar(data, name) {
  if (data.length > AVATAR_MAX_BYTES) throw new Error(`${name}: ${data.length} bytes, over the ${AVATAR_MAX_BYTES}-byte avatar limit`);
  const media = inspectImage(data, name);
  const extension = path.extname(name).slice(1).replace('jpeg', 'jpg');
  if (extension !== media.type) throw new Error(`${name}: the bytes are ${media.type}, not .${extension}`);
  if (Math.max(media.width, media.height) > AVATAR_MAX_EDGE) {
    throw new Error(`${name}: ${media.width}x${media.height}, over ${AVATAR_MAX_EDGE} px`);
  }
  if (media.durationMs > AVATAR_MAX_DURATION_MS) {
    throw new Error(`${name}: ${media.durationMs} ms, over the ${AVATAR_MAX_DURATION_MS} ms avatar limit`);
  }
  return media;
}

/* ── reading the source tree ─────────────────────────────────────────────── */

const FRONT_MATTER = /^---\s*\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/;

function frontMatter(text, file, parseYaml) {
  const match = FRONT_MATTER.exec(text);
  if (match === null) throw new Error(`${file}: no YAML front matter`);
  const front = parseYaml(match[1]);
  if (front === null || typeof front !== 'object' || Array.isArray(front)) throw new Error(`${file}: front matter is not a mapping`);
  return front;
}

function blueprintMeta(front, file) {
  const meta = front.blueprint;
  if (meta === null || typeof meta !== 'object') throw new Error(`${file}: a blueprint needs a \`blueprint:\` block (brief, showcases, surface)`);
  if (typeof meta.brief !== 'string' || meta.brief.trim().length < 40) throw new Error(`${file}: blueprint.brief must be a sentence or more`);
  if (!Array.isArray(meta.showcases) || meta.showcases.length === 0) throw new Error(`${file}: blueprint.showcases must list what it shows`);
  if (meta.surface !== 'cloud' && meta.surface !== 'desktop') throw new Error(`${file}: blueprint.surface must be cloud or desktop`);
  const requirements = (meta.requirements ?? []).map((item) => {
    if (typeof item?.type !== 'string' || typeof item?.label !== 'string') throw new Error(`${file}: each blueprint.requirements entry needs type and label`);
    return { type: item.type, label: item.label };
  });
  return { brief: meta.brief.trim(), showcases: meta.showcases.map(String), requirements, surface: meta.surface };
}

function slugOf(file) {
  return path.basename(file).replace(/\.agent\.md$/, '');
}

/**
 * The avatar of an agent file: `{ default }` for a drawn glyph, or
 * `{ files: [[slot, absolutePath, zipPath]] }` for uploaded media whose paths
 * are relative to `base` (the team folder, or blueprints/agents).
 */
async function agentAvatar(front, file, base) {
  const avatar = front.avatar;
  if (avatar === undefined) return undefined;
  if (typeof avatar === 'string') {
    const glyph = avatar.replace(/^default:/, '');
    if (!avatar.startsWith('default:') || !DEFAULT_GLYPHS.includes(glyph)) throw new Error(`${file}: avatar ${avatar} is not default:{${DEFAULT_GLYPHS.join(',')}}`);
    return { default: glyph };
  }
  if (typeof avatar !== 'object' || avatar === null || typeof avatar.still !== 'string') throw new Error(`${file}: avatar must be default:<glyph> or a slot map with a still`);
  const files = [];
  for (const [slot, relative] of Object.entries(avatar)) {
    if (!AVATAR_SLOTS.includes(slot)) throw new Error(`${file}: avatar has an unknown slot ${slot}`);
    if (typeof relative !== 'string' || relative.includes('..') || path.isAbsolute(relative)) throw new Error(`${file}: avatar.${slot} must be a path inside the blueprint`);
    const absolute = path.join(base, relative);
    checkAvatar(await readFile(absolute), path.relative(base, absolute));
    files.push([slot, absolute, relative.split(path.sep).join('/')]);
  }
  return { files };
}

function checkAgentFront(front, file) {
  if (front.schema !== AGENT_SCHEMA) throw new Error(`${file}: schema must be ${AGENT_SCHEMA}`);
  for (const key of ['name', 'description']) {
    if (typeof front[key] !== 'string' || front[key].trim() === '') throw new Error(`${file}: ${key} is required`);
  }
}

async function listFiles(dir, predicate) {
  if (!existsSync(dir)) return [];
  const names = await readdir(dir);
  return names.filter(predicate).sort().map((name) => path.join(dir, name));
}

async function treeBytes(dir) {
  let total = 0;
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    total += entry.isDirectory() ? await treeBytes(full) : (await stat(full)).size;
  }
  return total;
}

function stillPath(avatar, prefix) {
  if (avatar === undefined) return undefined;
  if (avatar.default !== undefined) return { default: avatar.default };
  const still = avatar.files.find(([slot]) => slot === 'still');
  return { still: `${prefix}/still${path.extname(still[1])}` };
}

/* ── the build ───────────────────────────────────────────────────────────── */

async function buildAgent(file, root, parseYaml, out) {
  const text = await readFile(file, 'utf8');
  const front = frontMatter(text, path.relative(root, file), parseYaml);
  const relative = path.relative(root, file);
  checkAgentFront(front, relative);
  const meta = blueprintMeta(front, relative);
  const slug = slugOf(file);
  const avatar = await agentAvatar(front, relative, path.dirname(file));
  let built = `agents/${slug}.agent.md`;
  if (avatar?.files !== undefined) {
    // Uploaded media cannot travel in a markdown file: the agent ships as an
    // .agent.zip holding the file and its avatars, which Import reads.
    built = `agents/${slug}.agent.zip`;
    const entries = [[`${slug}.agent.md`, Buffer.from(text)]];
    for (const [, absolute, zipPath] of avatar.files) entries.push([zipPath, await readFile(absolute)]);
    out.set(built, zipFiles(entries));
    const still = avatar.files.find(([slot]) => slot === 'still');
    out.set(`avatars/agents/${slug}/still${path.extname(still[1])}`, await readFile(still[1]));
  } else {
    out.set(built, Buffer.from(text));
  }
  return {
    slug,
    name: front.name.trim(),
    description: front.description.trim(),
    file: built,
    ...(avatar === undefined ? {} : { avatar: stillPath(avatar, `avatars/agents/${slug}`) }),
    ...meta,
  };
}

function checkTeamManifest(manifest, file) {
  if (manifest.schema !== TEAM_SCHEMA) throw new Error(`${file}: schema must be ${TEAM_SCHEMA}`);
  if (manifest.kind !== undefined && manifest.kind !== 'team') throw new Error(`${file}: kind must be team`);
  if (typeof manifest.name !== 'string' || manifest.name.trim() === '') throw new Error(`${file}: name is required`);
  if (!Array.isArray(manifest.members) || manifest.members.length === 0 || manifest.members.length > MAX_TEAM_MEMBERS) {
    throw new Error(`${file}: members must list 1 to ${MAX_TEAM_MEMBERS} agent files`);
  }
  if (manifest.floor_policy !== undefined && !['addressed', 'lead'].includes(manifest.floor_policy)) throw new Error(`${file}: floor_policy must be addressed or lead`);
  if (manifest.lead !== undefined && !manifest.members.includes(manifest.lead)) throw new Error(`${file}: lead must be one of the members`);
  if (manifest.floor_policy === 'lead' && manifest.lead === undefined) throw new Error(`${file}: floor_policy lead needs a lead`);
}

async function buildTeam(dir, root, parseYaml, dumpYaml, out) {
  const manifestFile = path.join(dir, 'team.yaml');
  const relativeManifest = path.relative(root, manifestFile);
  const manifest = parseYaml(await readFile(manifestFile, 'utf8'));
  checkTeamManifest(manifest, relativeManifest);
  const meta = blueprintMeta(manifest, relativeManifest);
  const slug = path.basename(dir);
  const entries = [];
  const zipPaths = new Map();
  const members = [];
  const used = new Set();
  for (const member of manifest.members) {
    const absolute = path.resolve(dir, member);
    if (!absolute.startsWith(path.join(root, 'blueprints') + path.sep)) throw new Error(`${relativeManifest}: member ${member} is outside blueprints/`);
    const memberSlug = slugOf(absolute);
    const zipPath = `members/${memberSlug}.agent.md`;
    if (used.has(zipPath)) throw new Error(`${relativeManifest}: two members are named ${memberSlug}`);
    used.add(zipPath);
    zipPaths.set(member, zipPath);
    const text = await readFile(absolute, 'utf8');
    const relative = path.relative(root, absolute);
    const front = frontMatter(text, relative, parseYaml);
    checkAgentFront(front, relative);
    // Avatar paths of a member are relative to the team folder, which is the
    // zip root, so its media is the team's avatars/ entries.
    const avatar = await agentAvatar(front, relative, dir);
    entries.push([zipPath, Buffer.from(text)]);
    for (const [, file, mediaPath] of avatar?.files ?? []) entries.push([mediaPath, await readFile(file)]);
    if (avatar?.files !== undefined) {
      const still = avatar.files.find(([slot]) => slot === 'still');
      out.set(`avatars/${slug}/${memberSlug}/still${path.extname(still[1])}`, await readFile(still[1]));
    }
    members.push({
      slug: memberSlug,
      name: front.name.trim(),
      description: front.description.trim(),
      ...(avatar === undefined ? {} : { avatar: stillPath(avatar, `avatars/${slug}/${memberSlug}`) }),
    });
  }
  const builtManifest = {
    schema: TEAM_SCHEMA,
    kind: 'team',
    name: manifest.name,
    ...(manifest.description === undefined ? {} : { description: manifest.description }),
    floor_policy: manifest.floor_policy ?? 'addressed',
    members: manifest.members.map((member) => zipPaths.get(member)),
    ...(manifest.lead === undefined ? {} : { lead: zipPaths.get(manifest.lead) }),
    blueprint: manifest.blueprint,
  };
  const file = `teams/${slug}.team.zip`;
  out.set(file, zipFiles([['team.yaml', Buffer.from(dumpYaml(builtManifest))], ...entries]));
  return {
    slug,
    name: manifest.name.trim(),
    description: String(manifest.description ?? '').trim(),
    file,
    ...(manifest.lead === undefined ? {} : { lead: slugOf(manifest.lead) }),
    members,
    ...meta,
  };
}

/** The release body's list of blueprint assets; `name` maps a built path to its file name. */
export function releaseNotes(index, name = (file) => path.basename(file)) {
  const lines = ['## Blueprints', '', 'Each file imports as it is with **Import** on the Agents page, or **Import team** on the Teams page for a team zip.', ''];
  for (const team of index.teams) lines.push(`- **${team.name}** (team of ${team.members.length}, \`${name(team.file)}\`): ${team.description.split('\n')[0]}`);
  for (const agent of index.agents) lines.push(`- **${agent.name}** (agent, \`${name(agent.file)}\`): ${agent.description.split('\n')[0]}`);
  return `${NOTES_MARKER}\n${lines.join('\n')}\n${NOTES_END}\n`;
}

/** The section's first and last lines: a re-run replaces what lies between, nothing else. */
export const NOTES_MARKER = '<!-- blueprints -->';
export const NOTES_END = '<!-- /blueprints -->';

/** The release body with the blueprints section appended, or put in place of an earlier run's. */
export function mergeNotes(body, section) {
  const start = body.indexOf(NOTES_MARKER);
  const end = body.indexOf(NOTES_END);
  if (start !== -1 && end > start) return `${body.slice(0, start)}${section.trimEnd()}${body.slice(end + NOTES_END.length)}`;
  return `${body.trimEnd()}\n\n${section}`;
}

/**
 * Builds the blueprint tree under `<root>/blueprints`. Answers the built files
 * (relative path -> bytes) and the gallery index. Throws, naming the file, on
 * anything Import would refuse or the platform's avatar limits would reject.
 */
export async function buildBlueprints({ root, parseYaml, dumpYaml }) {
  if (typeof parseYaml !== 'function' || typeof dumpYaml !== 'function') throw new Error('buildBlueprints needs parseYaml and dumpYaml');
  const source = path.join(root, 'blueprints');
  const bytes = await treeBytes(source);
  if (bytes > MAX_TREE_BYTES) throw new Error(`blueprints/ is ${bytes} bytes, over the ${MAX_TREE_BYTES}-byte budget`);
  const files = new Map();
  const agents = [];
  for (const file of await listFiles(path.join(source, 'agents'), (name) => name.endsWith('.agent.md'))) {
    agents.push(await buildAgent(file, root, parseYaml, files));
  }
  const teams = [];
  for (const dir of await listFiles(path.join(source, 'teams'), (name) => !name.startsWith('.'))) {
    if (existsSync(path.join(dir, 'team.yaml'))) teams.push(await buildTeam(dir, root, parseYaml, dumpYaml, files));
  }
  if (agents.length === 0 || teams.length === 0) throw new Error('blueprints/ must hold at least one agent and one team');
  const index = { schema: INDEX_SCHEMA, agents, teams };
  files.set('index.json', Buffer.from(`${JSON.stringify(index, null, 2)}\n`));
  files.set('release-notes.md', Buffer.from(releaseNotes(index)));
  return { files, index };
}

/** The release asset name of a built file: elitea-team-<slug>-<v>.team.zip, elitea-agent-<slug>-<v>.agent.md|zip. */
export function releaseAssetName(builtPath, version) {
  const match = /^(agents|teams)\/(.+?)\.(agent\.md|agent\.zip|team\.zip)$/.exec(builtPath);
  if (match === null) return undefined;
  const kind = match[1] === 'teams' ? 'team' : 'agent';
  return `elitea-${kind}-${match[2]}-${version}.${match[3]}`;
}

/* ── CLI ─────────────────────────────────────────────────────────────────── */

function loadYaml(root) {
  // scripts/ci's pinned `yaml` (npm ci --prefix scripts/ci), else the web
  // app's js-yaml: whichever the job installed.
  for (const [dir, adapt] of [
    ['scripts/ci', (lib) => ({ parseYaml: (text) => lib.parse(text), dumpYaml: (value) => lib.stringify(value) })],
    ['apps/elitea-web', (lib) => ({ parseYaml: (text) => lib.load(text), dumpYaml: (value) => lib.dump(value) })],
  ]) {
    try {
      const require = createRequire(path.join(root, dir, 'package.json'));
      return adapt(require(dir === 'scripts/ci' ? 'yaml' : 'js-yaml'));
    } catch {
      // try the next one
    }
  }
  throw new Error('no YAML library: run `npm ci --prefix scripts/ci` first');
}

async function main(argv) {
  const args = new Map();
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i].startsWith('--')) args.set(argv[i].slice(2), argv[i + 1] !== undefined && !argv[i + 1].startsWith('--') ? argv[++i] : 'true');
  }
  // merge-notes <body-file> <section-file>: prints the body with the section in place.
  if (argv[0] === 'merge-notes') {
    const [, bodyFile, sectionFile] = argv;
    if (bodyFile === undefined || sectionFile === undefined) throw new Error('usage: build.mjs merge-notes <body-file> <section-file>');
    process.stdout.write(mergeNotes(await readFile(bodyFile, 'utf8'), await readFile(sectionFile, 'utf8')));
    return;
  }
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
  const { files, index } = await buildBlueprints({ root, ...loadYaml(root) });
  const out = args.get('out');
  if (out !== undefined) {
    for (const [relative, data] of files) {
      const target = path.join(out, relative);
      await mkdir(path.dirname(target), { recursive: true });
      await writeFile(target, data);
    }
  }
  // --release-dir <dir> --version <v>: the release assets, one importable file
  // per blueprint, plus the notes publish.yml appends to the release body.
  const releaseDir = args.get('release-dir');
  if (releaseDir !== undefined) {
    const version = args.get('version');
    if (version === undefined || version === 'true') throw new Error('--release-dir needs --version');
    await mkdir(releaseDir, { recursive: true });
    for (const [relative, data] of files) {
      const name = releaseAssetName(relative, version);
      if (name !== undefined) await writeFile(path.join(releaseDir, name), data);
    }
    await writeFile(path.join(releaseDir, 'blueprints-release-notes.md'), releaseNotes(index, (file) => releaseAssetName(file, version)));
  }
  console.log(`blueprints: ${index.agents.length} agents, ${index.teams.length} teams, ${files.size} files built`);
}

if (process.argv[1] !== undefined && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`blueprints: ${error.message}`);
    process.exit(1);
  });
}
