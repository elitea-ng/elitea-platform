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
export function summarizeImportDocument(document: EntityExportDocument): ImportDocumentSummary {
  const names = (document.applications ?? []).map((entry, index) => {
    const name = typeof entry === 'object' && entry !== null ? (entry as { name?: unknown }).name : undefined;
    return typeof name === 'string' && name !== '' ? name : `#${String(index + 1)}`;
  });
  return {
    names,
    toolkitCount: document.toolkits?.length ?? 0,
    skillCount: document.skills?.length ?? 0,
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

/**
 * Reads one `.agent.md` file (the file Export writes with `?format=md`) into
 * the entry the import wizard takes. The server's `agent_type: agent` spelling
 * is accepted as written; the platform maps it on import.
 *
 * A pipeline's `state`/`nodes`/`entry_point`/interrupt keys are lifted back into
 * its YAML instructions, the inverse of what the export does.
 */
export function parseMarkdownAgent(text: string): Record<string, unknown> {
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
  const uuid = `md-${name.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`;
  return {
    entity: isPipeline ? 'pipelines' : 'agents',
    name,
    description: typeof front['description'] === 'string' ? front['description'] : '',
    original_exported: true,
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
        tools: [],
        variables: Array.isArray(front['variables']) ? front['variables'] : [],
        conversation_starters: Array.isArray(front['conversation_starters']) ? front['conversation_starters'] : [],
        welcome_message: typeof front['welcome_message'] === 'string' ? front['welcome_message'] : '',
        tags: [],
      },
    ],
  };
}

async function readZipEntries(file: File): Promise<EntityExportDocument> {
  let zip: JSZip;
  try {
    zip = await JSZip.loadAsync(await file.arrayBuffer());
  } catch {
    throw new ImportDocumentError(t('features.agentLifecycle.import.badZip', 'This zip file could not be read.'));
  }
  const names = Object.keys(zip.files)
    .filter((entry) => !(zip.files[entry]?.dir ?? true) && /\.md$/i.test(entry) && !entry.startsWith('__MACOSX/'))
    .sort();
  const applications: unknown[] = [];
  for (const entry of names) {
    applications.push(parseMarkdownAgent((await zip.files[entry]?.async('string')) ?? ''));
  }
  if (applications.length === 0) {
    throw new ImportDocumentError(t('features.agentLifecycle.import.emptyZip', 'This zip file holds no .md agent files.'));
  }
  return { ok: true, applications };
}

/** True for the extensions Import reads: the JSON export, a Markdown agent file, or a zip of them. */
export function isImportableFileName(name: string): boolean {
  return /\.(json|md|zip)$/i.test(name);
}

/** Reads an import file by its extension into the document the wizard accepts. */
export async function readImportFile(file: File): Promise<EntityExportDocument> {
  const lower = file.name.toLowerCase();
  if (lower.endsWith('.zip')) return readZipEntries(file);
  if (lower.endsWith('.md')) return { ok: true, applications: [parseMarkdownAgent(await file.text())] };
  return parseImportDocument(await file.text());
}
