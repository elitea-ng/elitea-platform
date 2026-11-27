/**
 * Zip import (#1996, issue 3010, issue 3137): a zip of `.agent.md` files
 * (a multi-version export, or an `.agent.zip` of one agent and its avatar
 * media), or a team zip (`team.yaml` + `members/*.agent.md` + `avatars/`).
 *
 * Every size is checked from the central directory BEFORE anything is
 * inflated, so a small archive that expands to gigabytes is refused unread,
 * and checked again against the bytes actually inflated.
 */
import JSZip from 'jszip';

import { t } from '@/shared/i18n';

import { ImportDocumentError } from './importError';
import type { ParsedMarkdown, PendingAvatar, ReadImportResult } from './importTypes';
import { assemble } from './mergeVersionFiles';
import { MAX_FILE_BYTES, parseMarkdownFull, tooLarge } from './parseAgentMarkdown';
import { findTeamManifest, parseTeamManifest, resolveZipPath } from './teamManifest';

const MAX_ZIP_ENTRIES = 50;
const MAX_ZIP_TOTAL_BYTES = 10 * 1024 * 1024;
/** The platform's limit for one avatar file (agentmedia.Inspect). */
const MAX_AVATAR_BYTES = 2 * 1024 * 1024;
/** Every avatar file of one zip: 16 members with 9 slots each fits with room. */
const MAX_AVATAR_TOTAL_BYTES = 64 * 1024 * 1024;
const MAX_AVATAR_FILES = 16 * 9;

async function loadZip(file: File): Promise<JSZip> {
  try {
    return await JSZip.loadAsync(await file.arrayBuffer());
  } catch {
    throw new ImportDocumentError(t('features.agentLifecycle.import.badZip', 'This zip file could not be read.'));
  }
}

function isFile(zip: JSZip, entry: string): boolean {
  return zip.files[entry] !== undefined && !(zip.files[entry]?.dir ?? true);
}

function zipMarkdownNames(zip: JSZip): string[] {
  return Object.keys(zip.files)
    .filter((entry) => isFile(zip, entry) && /\.md$/i.test(entry) && !entry.startsWith('__MACOSX/'))
    .sort();
}

function declaredSize(zip: JSZip, entry: string): unknown {
  return (zip.files[entry] as unknown as { _data?: { uncompressedSize?: number } } | undefined)?._data?.uncompressedSize;
}

function checkDeclaredSizes(zip: JSZip, names: readonly string[], zipName: string, limits: { readonly each: number; readonly total: number }): void {
  let total = 0;
  for (const entry of names) {
    const declared = declaredSize(zip, entry);
    if (typeof declared !== 'number') continue;
    if (declared > limits.each) throw tooLarge(entry, limits.each);
    total += declared;
    if (total > limits.total) throw tooLarge(zipName, limits.total);
  }
}

const MARKDOWN_LIMITS = { each: MAX_FILE_BYTES, total: MAX_ZIP_TOTAL_BYTES };
const AVATAR_LIMITS = { each: MAX_AVATAR_BYTES, total: MAX_AVATAR_TOTAL_BYTES };

async function parseZipEntries(zip: JSZip, names: readonly string[], zipName: string): Promise<ParsedMarkdown[]> {
  const parsed: ParsedMarkdown[] = [];
  let inflated = 0;
  for (const entry of names) {
    const text = (await zip.files[entry]?.async('string')) ?? '';
    inflated += text.length;
    if (text.length > MAX_FILE_BYTES) throw tooLarge(entry, MAX_FILE_BYTES);
    if (inflated > MAX_ZIP_TOTAL_BYTES) throw tooLarge(zipName, MAX_ZIP_TOTAL_BYTES);
    parsed.push(parseMarkdownFull(text));
  }
  return parsed;
}

/* ── avatars ────────────────────────────────────────────────────────────── */

interface PlannedAvatar {
  readonly versionKey: string;
  readonly slot: string;
  readonly path: string;
}

function versionKeyOf(entry: Record<string, unknown>): string {
  const versions = Array.isArray(entry['versions']) ? (entry['versions'] as Record<string, unknown>[]) : [];
  const key = versions[0]?.['import_version_uuid'];
  return typeof key === 'string' ? key : '';
}

/** Each file's slot -> path map resolved against `base` (the folder of team.yaml, or the zip root). */
function planAvatars(zip: JSZip, parsed: readonly ParsedMarkdown[], base: string): { planned: PlannedAvatar[]; skipped: string[] } {
  const planned: PlannedAvatar[] = [];
  const skipped: string[] = [];
  for (const file of parsed) {
    const paths = file.avatarPaths;
    if (paths === undefined) continue;
    const name = String(file.entry['name']);
    if (paths['still'] === undefined) {
      // The server refuses a pack without a still; the agent imports without one.
      skipped.push(t('features.agentLifecycle.import.avatarNoStill', '{{name}}: avatar (it names no still image)', { name }));
      continue;
    }
    for (const [slot, relative] of Object.entries(paths)) {
      const path = resolveZipPath(base, relative);
      if (!isFile(zip, path)) {
        throw new ImportDocumentError(
          t('features.agentLifecycle.import.avatarMissing', 'The avatar {{path}} of {{name}} is not in the zip.', { path: relative, name }),
        );
      }
      planned.push({ versionKey: versionKeyOf(file.entry), slot, path });
    }
  }
  return { planned, skipped };
}

async function readAvatars(zip: JSZip, parsed: readonly ParsedMarkdown[], base: string, zipName: string): Promise<{ avatars: PendingAvatar[]; skipped: string[] }> {
  const { planned, skipped } = planAvatars(zip, parsed, base);
  if (planned.length > MAX_AVATAR_FILES) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.tooManyAvatars', 'This zip holds more than {{limit}} avatar files.', { limit: MAX_AVATAR_FILES }),
    );
  }
  checkDeclaredSizes(zip, planned.map((avatar) => avatar.path), zipName, AVATAR_LIMITS);
  const avatars: PendingAvatar[] = [];
  let inflated = 0;
  for (const avatar of planned) {
    const bytes = (await zip.files[avatar.path]?.async('uint8array')) ?? new Uint8Array();
    inflated += bytes.length;
    if (bytes.length > MAX_AVATAR_BYTES) throw tooLarge(avatar.path, MAX_AVATAR_BYTES);
    if (inflated > MAX_AVATAR_TOTAL_BYTES) throw tooLarge(zipName, MAX_AVATAR_TOTAL_BYTES);
    avatars.push({ ...avatar, bytes });
  }
  return { avatars, skipped };
}

/* ── the two zip shapes ─────────────────────────────────────────────────── */

/**
 * Reads a team zip: one `.agent.md` per member, listed by `team.yaml`. Only
 * the member files are read (a README beside them is not an agent), and each
 * file is its own agent, because a team pins one version of each member.
 */
async function readTeamZip(zip: JSZip, manifestPath: string, zipName: string): Promise<ReadImportResult> {
  const manifest = parseTeamManifest((await zip.files[manifestPath]?.async('string')) ?? '', manifestPath);
  const missing = manifest.members.filter((member) => !isFile(zip, member));
  if (missing.length > 0) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.team.missingMember', '{{file}} names files the zip does not hold: {{files}}.', {
        file: manifestPath,
        files: missing.join(', '),
      }),
    );
  }
  checkDeclaredSizes(zip, manifest.members, zipName, MARKDOWN_LIMITS);
  const parsed = await parseZipEntries(zip, manifest.members, zipName);
  const { avatars, skipped } = await readAvatars(zip, parsed, manifestPath, zipName);
  const uuids = parsed.map((file) => String(file.entry['import_uuid']));
  const leadIndex = manifest.lead === undefined ? -1 : manifest.members.indexOf(manifest.lead);
  const leadEntry = parsed[leadIndex]?.entry;
  return {
    document: {
      ok: true,
      applications: parsed.map((p) => p.entry),
      toolkits: parsed.flatMap((p) => p.toolkits),
      skills: parsed.flatMap((p) => p.skills),
    },
    skipped: [...parsed.flatMap((p) => p.skipped), ...skipped],
    avatars,
    team: {
      name: manifest.name,
      description: manifest.description,
      ...(manifest.floorPolicy === undefined ? {} : { floor_policy: manifest.floorPolicy }),
      ...(leadEntry === undefined ? {} : { lead: uuids[leadIndex], leadName: String(leadEntry['name']) }),
      members: uuids,
    },
  };
}

/** A zip with a team.yaml is a team (issue 3010); without one it is a set of agents (an `.agent.zip` among them). */
export async function readZipEntries(file: File): Promise<ReadImportResult> {
  const zip = await loadZip(file);
  const manifest = findTeamManifest(zip);
  if (manifest !== undefined) return readTeamZip(zip, manifest, file.name);
  const names = zipMarkdownNames(zip);
  if (names.length > MAX_ZIP_ENTRIES) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.tooManyEntries', 'This zip file holds more than {{limit}} agent files.', {
        limit: MAX_ZIP_ENTRIES,
      }),
    );
  }
  checkDeclaredSizes(zip, names, file.name, MARKDOWN_LIMITS);
  const parsed = await parseZipEntries(zip, names, file.name);
  if (parsed.length === 0) {
    throw new ImportDocumentError(t('features.agentLifecycle.import.emptyZip', 'This zip file holds no .md agent files.'));
  }
  // Paths of an agent zip are relative to the zip root. Read before the merge,
  // which keeps each file's version key and so its avatar's target.
  const { avatars, skipped } = await readAvatars(zip, parsed, '', file.name);
  const assembled = assemble(parsed);
  return { ...assembled, skipped: [...assembled.skipped, ...skipped], avatars };
}
