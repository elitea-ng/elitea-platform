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
