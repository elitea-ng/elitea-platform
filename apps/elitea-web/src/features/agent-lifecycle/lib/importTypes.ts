import type { EntityExportDocument } from '../api/lifecycleApi';
import type { ImportTeam } from './teamManifest';

/** One parsed `.agent.md`: its agent entry, its toolkit and skill entries, and what it could not carry. */
export interface ParsedMarkdown {
  readonly entry: Record<string, unknown>;
  readonly toolkits: Record<string, unknown>[];
  readonly skills: Record<string, unknown>[];
  /** Frontmatter blocks the import cannot carry, named for the summary. */
  readonly skipped: string[];
  /** The file's `avatar:` slot -> path map, read against the zip (issue 3137). */
  readonly avatarPaths?: Readonly<Record<string, string>> | undefined;
}

/**
 * An avatar file of a zip (issue 3137), read and size-checked but not yet
 * uploaded: Import uploads it on confirm and puts the answered url into the
 * slot, so a cancelled preview leaves nothing behind.
 */
export interface PendingAvatar {
  /** The `import_version_uuid` of the version entry the file became (a pack is per version). */
  readonly versionKey: string;
  readonly slot: string;
  /** The file's path inside the zip; its extension names the media type. */
  readonly path: string;
  readonly bytes: Uint8Array;
}

/** What `readImportFile` hands the wizard: the document, plus what it could not carry. */
export interface ReadImportResult {
  readonly document: EntityExportDocument;
  readonly skipped: readonly string[];
  /** Set for a zip that carries a `team.yaml`. */
  readonly team?: ImportTeam | undefined;
  /** Avatar media of a zip, uploaded on confirm. */
  readonly avatars?: readonly PendingAvatar[] | undefined;
}
