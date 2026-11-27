import { t } from '@/shared/i18n';

import type { EntityExportDocument } from '../api/lifecycleApi';
import { ImportDocumentError } from './importError';
import type { ReadImportResult } from './importTypes';
import { assemble } from './mergeVersionFiles';
import { MAX_FILE_BYTES, parseMarkdownFull, tooLarge } from './parseAgentMarkdown';
import type { ImportTeam } from './teamManifest';
import { readZipEntries } from './importZip';

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
  /** Set when the file is a team zip: the team its agents form. */
  readonly team?: { readonly name: string; readonly lead: string | undefined } | undefined;
  readonly toolkitCount: number;
  readonly skillCount: number;
  /** Parts of the file the import cannot carry; shown, not dropped silently. */
  readonly skipped: readonly string[];
}

export { ImportDocumentError } from './importError';

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
  team?: ImportTeam,
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
    ...(team === undefined ? {} : { team: { name: team.name, lead: team.leadName } }),
  };
}

/* ── Markdown and zip import (#1996, issue 3137) ─────────────────────────── */

export type { ReadImportResult } from './importTypes';

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

/** True for the extensions Import reads: the JSON export, a Markdown agent file, or a zip of them. */
export function isImportableFileName(name: string): boolean {
  return /\.(json|md|zip)$/i.test(name);
}

/** A lone `.md` cannot carry the files its avatar map names: the agent imports without them, and the summary says so. */
async function readMarkdownFile(file: File): Promise<ReadImportResult> {
  if (file.size > MAX_FILE_BYTES) throw tooLarge(file.name, MAX_FILE_BYTES);
  const parsed = parseMarkdownFull(await file.text());
  const assembled = assemble([parsed]);
  if (parsed.avatarPaths === undefined) return assembled;
  return {
    ...assembled,
    skipped: [
      ...assembled.skipped,
      t('features.agentLifecycle.import.avatarNeedsZip', '{{name}}: avatar (its files travel only in an .agent.zip)', {
        name: String(parsed.entry['name']),
      }),
    ],
  };
}

/** Reads an import file by its extension into the document the wizard accepts. */
export async function readImportFile(file: File): Promise<ReadImportResult> {
  const lower = file.name.toLowerCase();
  // A zip with a team.yaml is a team (issue 3010); without one it is a set of agents, as before.
  if (lower.endsWith('.zip')) return readZipEntries(file);
  if (lower.endsWith('.md')) return readMarkdownFile(file);
  return { document: parseImportDocument(await file.text()), skipped: [] };
}
