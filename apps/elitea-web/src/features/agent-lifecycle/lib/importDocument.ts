import { dump as dumpYaml, load as loadYaml } from 'js-yaml';
import JSZip from 'jszip';

import { t } from '@/shared/i18n';

import type { EntityExportDocument } from '../api/lifecycleApi';

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

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function defined(entries: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(entries).filter(([, value]) => value !== undefined && value !== null));
}

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

function newKey(prefix: string): string {
  return `${prefix}-${globalThis.crypto.randomUUID()}`;
}

function asTags(value: unknown): { name: string; data?: unknown }[] {
  if (!Array.isArray(value)) return [];
  const tags: { name: string; data?: unknown }[] = [];
  for (const item of value) {
    if (typeof item === 'string' && item.trim() !== '') {
      tags.push({ name: item.trim() });
    } else {
      const record = asRecord(item);
      if (typeof record['name'] === 'string' && record['name'] !== '') {
        tags.push({ name: record['name'], ...(record['data'] === undefined ? {} : { data: record['data'] }) });
      }
    }
  }
  return tags;
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
  const name = typeof front['name'] === 'string' ? front['name'].trim() : '';
  if (name === '') {
    throw new ImportDocumentError(t('features.agentLifecycle.import.noName', 'The frontmatter of this file has no name.'));
  }
  const body = (match[2] ?? '').trim();
  const agentType = typeof front['agent_type'] === 'string' ? front['agent_type'] : 'agent';
  const isPipeline = agentType === 'pipeline';
  const instructions = isPipeline
    ? dumpYaml(
        defined({
          state: front['state'],
          entry_point: front['entry_point'],
          interrupt_after: front['interrupt_after'],
          interrupt_before: front['interrupt_before'],
          nodes: front['nodes'],
        }),
      )
    : body;
  const version = typeof front['version'] === 'string' && front['version'] !== '' ? front['version'] : 'base';
  // A fresh key per entry. A slug of the name collided for non-Latin names and
  // for names differing only in punctuation, and the server keys the import on
  // it, so two agents of one zip would have merged.
  const uuid = newKey('md');
  const skipped: string[] = [];

  const toolkits: Record<string, unknown>[] = [];
  const tools: { import_uuid: string }[] = [];
  for (const raw of Array.isArray(front['toolkits']) ? front['toolkits'] : []) {
    const block = asRecord(raw);
    const toolkitName = typeof block['toolkit'] === 'string' ? block['toolkit'] : '';
    const type = typeof block['type'] === 'string' && block['type'] !== '' ? block['type'] : 'custom';
    if (type === 'application') {
      // A nested agent is exported as a reference with no settings; there is
      // nothing in this file to rebuild it from.
      skipped.push(`${name}: ${toolkitName || type} (nested agent)`);
      continue;
    }
    const importUuid = newKey('tool');
    const settings = { ...asRecord(block['settings']) };
    if (Array.isArray(block['tools']) && block['tools'].length > 0) settings['selected_tools'] = block['tools'];
    toolkits.push(
      defined({ entity: 'toolkits', import_uuid: importUuid, name: toolkitName, type, settings, meta: block['meta'] }),
    );
    tools.push({ import_uuid: importUuid });
  }

  const skills: Record<string, unknown>[] = [];
  const skillRefs: { import_uuid: string; version_name: string }[] = [];
  for (const raw of Array.isArray(front['skills']) ? front['skills'] : []) {
    const block = asRecord(raw);
    const skillName = typeof block['name'] === 'string' ? block['name'] : '';
    if (skillName === '') {
      skipped.push(`${name}: skill without a name`);
      continue;
    }
    const skillVersion = typeof block['version'] === 'string' && block['version'] !== '' ? block['version'] : 'base';
    const importUuid = newKey('skill');
    skills.push({
      entity: 'skills',
      import_uuid: importUuid,
      name: skillName,
      description: typeof block['description'] === 'string' ? block['description'] : '',
      versions: [{ name: skillVersion, instructions: typeof block['instructions'] === 'string' ? block['instructions'] : '' }],
    });
    skillRefs.push({ import_uuid: importUuid, version_name: skillVersion });
  }
  if (front['pipeline_settings'] !== undefined) skipped.push(`${name}: pipeline_settings`);

  const entry = {
    entity: isPipeline ? 'pipelines' : 'agents',
    name,
    description: typeof front['description'] === 'string' ? front['description'] : '',
    import_uuid: uuid,
    versions: [
      {
        name: version,
        import_version_uuid: `${uuid}-${version}`,
        instructions,
        agent_type: agentType,
        llm_settings: defined({
          model_name: front['model'],
          temperature: front['temperature'],
          max_tokens: front['max_tokens'],
          top_p: front['top_p'],
        }),
        meta: defined({ step_limit: front['step_limit'], internal_tools: front['internal_tools'] }),
        tools,
        skills: skillRefs,
        variables: Array.isArray(front['variables']) ? front['variables'] : [],
        conversation_starters: Array.isArray(front['conversation_starters']) ? front['conversation_starters'] : [],
        welcome_message: typeof front['welcome_message'] === 'string' ? front['welcome_message'] : '',
        tags: asTags(front['tags']),
        ...(typeof front['soul'] === 'object' && front['soul'] !== null ? { soul: front['soul'] } : {}),
      },
    ],
  };
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

async function readZipEntries(file: File): Promise<ReadImportResult> {
  let zip: JSZip;
  try {
    zip = await JSZip.loadAsync(await file.arrayBuffer());
  } catch {
    throw new ImportDocumentError(t('features.agentLifecycle.import.badZip', 'This zip file could not be read.'));
  }
  const names = Object.keys(zip.files)
    .filter((entry) => !(zip.files[entry]?.dir ?? true) && /\.md$/i.test(entry) && !entry.startsWith('__MACOSX/'))
    .sort();
  if (names.length > MAX_ZIP_ENTRIES) {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.tooManyEntries', 'This zip file holds more than {{limit}} agent files.', {
        limit: MAX_ZIP_ENTRIES,
      }),
    );
  }
  // Sizes are checked from the central directory BEFORE anything is inflated,
  // so a small archive that expands to gigabytes is refused unread.
  let total = 0;
  for (const entry of names) {
    const declared = (zip.files[entry] as unknown as { _data?: { uncompressedSize?: number } } | undefined)?._data
      ?.uncompressedSize;
    if (typeof declared === 'number') {
      if (declared > MAX_FILE_BYTES) throw tooLarge(entry, MAX_FILE_BYTES);
      total += declared;
      if (total > MAX_ZIP_TOTAL_BYTES) throw tooLarge(file.name, MAX_ZIP_TOTAL_BYTES);
    }
  }
  const parsed: ParsedMarkdown[] = [];
  let inflated = 0;
  for (const entry of names) {
    const text = (await zip.files[entry]?.async('string')) ?? '';
    inflated += text.length;
    if (text.length > MAX_FILE_BYTES) throw tooLarge(entry, MAX_FILE_BYTES);
    if (inflated > MAX_ZIP_TOTAL_BYTES) throw tooLarge(file.name, MAX_ZIP_TOTAL_BYTES);
    parsed.push(parseMarkdownFull(text));
  }
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
