import { load as loadYaml } from 'js-yaml';
import JSZip from 'jszip';

import { t } from '@/shared/i18n';

import type { EntityExportDocument } from '../api/lifecycleApi';
import {
  asRecord,
  buildEntry,
  collectSkills,
  collectToolkits,
  frontAgentType,
  frontName,
  frontVersion,
  newKey,
  pipelineInstructions,
} from './importMarkdown';

/**
 * Reads an exported agent or pipeline file back into the document the import
 * wizard accepts.
 *
 * The file this parses is the one this app's own Export produces
 * (`GET /elitea_core/export_import/prompt_lib/{project}/{id}`, JSON form):
 * `{"ok": true, "applications": [...], "toolkits": [...], "skills": [...]}`.
 * The round trip is the point — an Import that could not read an Export is not
 * an import.
 *
 * It refuses rather than guesses. A JSON file with no `applications` array is
 * not an agent export, and posting it would reach the server's own 400 with a
 * message about a key the user never saw. Reporting it here names the file.
 */
export interface ImportDocumentSummary {
  readonly names: readonly string[];
  readonly toolkitCount: number;
  readonly skillCount: number;
  /** Parts of the file the import cannot carry; shown, not dropped silently. */
  readonly skipped: readonly string[];
}

export class ImportDocumentError extends Error {}

export function parseImportDocument(text: string): EntityExportDocument {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.notJson', 'This file is not valid JSON.'),
    );
  }
  if (typeof parsed !== 'object' || parsed === null) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.notADocument', 'This file does not hold an exported agent.'),
    );
  }
  const document = parsed as EntityExportDocument;
  if (!Array.isArray(document.applications) || document.applications.length === 0) {
    throw new ImportDocumentError(
      t(
        'features.agentLifecycle.import.noApplications',
        'This file holds no agents. Export an agent or a pipeline first, then import that file.',
      ),
    );
  }
  return document;
}

/** What the confirmation dialog shows before the user commits to the import. */
export function summarizeImportDocument(
  document: EntityExportDocument,
  skipped: readonly string[] = [],
): ImportDocumentSummary {
  const names = (document.applications ?? []).map((entry, index) => {
    const name = typeof entry === 'object' && entry !== null ? (entry as { name?: unknown }).name : undefined;
    return typeof name === 'string' && name !== '' ? name : `#${String(index + 1)}`;
  });
  return {
    names,
    toolkitCount: document.toolkits?.length ?? 0,
    skillCount: document.skills?.length ?? 0,
    skipped,
  };
}

/* ── Markdown and zip import (#1996) ─────────────────────────────────────── */

const FRONTMATTER = /^---\s*\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n([\s\S]*))?$/;

/** Reads of a single `.md`, and of each zip entry, stop here (bytes). */
const MAX_FILE_BYTES = 1024 * 1024;
const MAX_ZIP_ENTRIES = 50;
const MAX_ZIP_TOTAL_BYTES = 10 * 1024 * 1024;

function tooLarge(name: string, limit: number): ImportDocumentError {
  return new ImportDocumentError(
    t('features.agentLifecycle.import.tooLarge', '{{name}} is larger than {{limit}} MiB, which is the import limit.', {
      name,
      limit: limit / (1024 * 1024),
    }),
  );
}

interface ParsedMarkdown {
  readonly entry: Record<string, unknown>;
  readonly toolkits: Record<string, unknown>[];
  readonly skills: Record<string, unknown>[];
  /** Frontmatter blocks the import cannot carry, named for the summary. */
  readonly skipped: string[];
}

function parseMarkdownFull(text: string): ParsedMarkdown {
  const match = FRONTMATTER.exec(text.replace(/^﻿/, ''));
  if (match === null) {
    throw new ImportDocumentError(
      t(
        'features.agentLifecycle.import.noFrontmatter',
        'This file must start with YAML frontmatter (name, description, model, instructions).',
      ),
    );
  }
  let front: Record<string, unknown>;
  try {
    front = asRecord(loadYaml(match[1] ?? ''));
  } catch {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.badFrontmatter', 'The YAML frontmatter of this file is not valid.'),
    );
  }
  const name = frontName(front);
  if (name === '') {
    throw new ImportDocumentError(t('features.agentLifecycle.import.noName', 'The frontmatter of this file has no name.'));
  }
  const agentType = frontAgentType(front);
  const instructions = agentType === 'pipeline' ? pipelineInstructions(front) : (match[2] ?? '').trim();
  const version = frontVersion(front);
  // A fresh key per entry. A slug of the name collided for non-Latin names and
  // for names differing only in punctuation, and the server keys the import on
  // it, so two agents of one zip would have merged.
  const uuid = newKey('md');
  const { toolkits, tools, skipped: toolkitSkipped } = collectToolkits(front, name);
  const { skills, skillRefs, skipped: skillSkipped } = collectSkills(front, name);
  const skipped = [...toolkitSkipped, ...skillSkipped];
  if (front['pipeline_settings'] !== undefined) skipped.push(`${name}: pipeline_settings`);

  const entry = buildEntry({ front, name, agentType, instructions, version, uuid, tools, skillRefs });
  return { entry, toolkits, skills, skipped };
}

/**
 * Reads one `.agent.md` file (the file Export writes with `?format=md`) into
 * the agent entry the import wizard takes. Toolkits and skills the file holds
 * travel as separate document entries; see `readImportFile`.
 *
 * A pipeline's `state`/`nodes`/`entry_point`/interrupt keys are lifted back into
 * its YAML instructions, the inverse of what the export does.
 */
export function parseMarkdownAgent(text: string): Record<string, unknown> {
  return parseMarkdownFull(text).entry;
}

/** What `readImportFile` hands the wizard: the document, plus what it could not carry. */
export interface ReadImportResult {
  readonly document: EntityExportDocument;
  readonly skipped: readonly string[];
}

function assemble(parsed: ParsedMarkdown[]): ReadImportResult {
  return {
    document: {
      ok: true,
      applications: parsed.map((p) => p.entry),
      toolkits: parsed.flatMap((p) => p.toolkits),
      skills: parsed.flatMap((p) => p.skills),
    },
    skipped: parsed.flatMap((p) => p.skipped),
  };
}

async function loadZip(file: File): Promise<JSZip> {
  try {
    return await JSZip.loadAsync(await file.arrayBuffer());
  } catch {
    throw new ImportDocumentError(t('features.agentLifecycle.import.badZip', 'This zip file could not be read.'));
  }
}

function zipMarkdownNames(zip: JSZip): string[] {
  return Object.keys(zip.files)
    .filter((entry) => !(zip.files[entry]?.dir ?? true) && /\.md$/i.test(entry) && !entry.startsWith('__MACOSX/'))
    .sort();
}

function declaredSize(zip: JSZip, entry: string): unknown {
  return (zip.files[entry] as unknown as { _data?: { uncompressedSize?: number } } | undefined)?._data?.uncompressedSize;
}

// Sizes are checked from the central directory BEFORE anything is inflated,
// so a small archive that expands to gigabytes is refused unread.
function checkDeclaredSizes(zip: JSZip, names: readonly string[], zipName: string): void {
  let total = 0;
  for (const entry of names) {
    const declared = declaredSize(zip, entry);
    if (typeof declared !== 'number') continue;
    if (declared > MAX_FILE_BYTES) throw tooLarge(entry, MAX_FILE_BYTES);
    total += declared;
    if (total > MAX_ZIP_TOTAL_BYTES) throw tooLarge(zipName, MAX_ZIP_TOTAL_BYTES);
  }
}

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

async function readZipEntries(file: File): Promise<ReadImportResult> {
  const zip = await loadZip(file);
  const names = zipMarkdownNames(zip);
  if (names.length > MAX_ZIP_ENTRIES) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.tooManyEntries', 'This zip file holds more than {{limit}} agent files.', {
        limit: MAX_ZIP_ENTRIES,
      }),
    );
  }
  checkDeclaredSizes(zip, names, file.name);
  const parsed = await parseZipEntries(zip, names, file.name);
  if (parsed.length === 0) {
    throw new ImportDocumentError(t('features.agentLifecycle.import.emptyZip', 'This zip file holds no .md agent files.'));
  }
  return assemble(parsed);
}

/** True for the extensions Import reads: the JSON export, a Markdown agent file, or a zip of them. */
export function isImportableFileName(name: string): boolean {
  return /\.(json|md|zip)$/i.test(name);
}

/** Reads an import file by its extension into the document the wizard accepts. */
export async function readImportFile(file: File): Promise<ReadImportResult> {
  const lower = file.name.toLowerCase();
  if (lower.endsWith('.zip')) return readZipEntries(file);
  if (lower.endsWith('.md')) {
    if (file.size > MAX_FILE_BYTES) throw tooLarge(file.name, MAX_FILE_BYTES);
    return assemble([parseMarkdownFull(await file.text())]);
  }
  return { document: parseImportDocument(await file.text()), skipped: [] };
}
