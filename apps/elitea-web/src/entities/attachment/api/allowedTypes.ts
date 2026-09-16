/**
 * The backend-served attachment allow-list (issue #940 A15, ELITEA-0484,
 * 0486, 0487, 0488, 0489).
 *
 * ## The backend half already existed; the client half did not
 *
 * `GET /elitea_core/index_types/prompt_lib/{projectId}` has served
 * `document_types`, `image_types` and `code_types` — extension → MIME maps —
 * since issue #394 (`internal/api/v2/indextypes`, pinned out of the SDK's own
 * loader registry). The baseline reads exactly those three maps
 * (`apps/elitea-ui/src/slices/fileTypes.js:26-28`).
 *
 * This port never read them, so `validateAttachmentFiles` checked count and
 * size and never TYPE — its own doc comment disclosed that as a gap ("no such
 * data source exists anywhere in this codebase yet"). The data source does
 * exist; this module is the missing reader.
 *
 * ## `.svg` is added, and that is the baseline's behaviour, not an invention
 *
 * `fileTypes.js:30` appends `{'.svg': 'image/svg+xml'}` to the transformed map
 * after processing the three served categories, because the indexer does not
 * load SVG but the composer accepts it (and `shared/lib/attachments.ts`
 * already gives SVG the non-image size cap, "excluding SVG"). Dropping it here
 * would make the composer reject a file this app otherwise handles.
 *
 * ## An EMPTY list and an ABSENT list are different answers
 *
 * ELITEA-0489 asks for the attach control to be disabled when the backend
 * returns no code types. That is a served answer: the deployment ran the
 * capability and said "nothing". It must disable the control.
 *
 * A 501 is a different statement — `ELITEA_INDEX_TYPES_ENABLED` is off, so
 * this deployment cannot enumerate loaders at all. Treating that as "nothing
 * is allowed" would silently disable attachments on every installation with
 * the capability off, which is not a decision anyone made about attachments.
 * Those deployments fall back to {@link DEFAULT_ATTACHMENT_EXTENSIONS} and the
 * control stays live. `isServed` is what tells the two apart, and it is the
 * only reason this module returns a struct rather than a bare array.
 */
import { useMemo } from 'react';

import type { DocumentLoadersResponse } from '@/shared/api/generated/model';
import { useGetDocumentLoaders } from '@/shared/api/generated/applications/applications';

/** What the composer needs to know about which files it may accept. */
export interface AllowedAttachmentTypes {
  /** Lower-case, leading-dot extensions (`.pdf`). Empty means "nothing is allowed". */
  readonly extensions: readonly string[];
  /** The MIME types behind them, for the file picker's `accept` hint. */
  readonly mimeTypes: readonly string[];
  /**
   * True when the DEPLOYMENT answered with a list — including an empty one.
   * False while the answer is still in flight, or when the deployment cannot
   * enumerate loaders at all (501) and the defaults below are standing in.
   */
  readonly isServed: boolean;
}

/**
 * The stand-in for a deployment that does not run the index-type capability.
 *
 * Deliberately a SMALL, conservative set — the plain-text and document
 * extensions the composer has always accepted — rather than a copy of the
 * SDK's full registry, which would go stale silently. A deployment that wants
 * the real list turns the capability on.
 */
export const DEFAULT_ATTACHMENT_EXTENSIONS: readonly string[] = [
  '.csv', '.doc', '.docx', '.gif', '.htm', '.html', '.jpeg', '.jpg', '.json',
  '.md', '.pdf', '.png', '.pptx', '.svg', '.txt', '.webp', '.xls', '.xlsx',
  '.xml', '.yaml', '.yml',
];

const SVG_EXTENSION = '.svg';
const SVG_MIME_TYPE = 'image/svg+xml';

/** Lower-cases and guarantees the leading dot, so `PDF`, `.PDF` and `pdf` are one key. */
export function normaliseExtension(raw: string): string {
  const trimmed = raw.trim().toLowerCase();
  if (trimmed === '') return '';
  return trimmed.startsWith('.') ? trimmed : `.${trimmed}`;
}

/** The extension of a file name, normalised. `''` for a name with no dot. */
export function fileExtension(name: string): string {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return '';
  return normaliseExtension(name.slice(dot));
}

/**
 * Folds the three served maps into one allow-list.
 *
 * Exported and pure so the rule is testable without a query client — the
 * hook below is a thin wrapper, and every case in ELITEA-0484/0486/0487/0488
 * is a question about THIS function.
 */
export function deriveAllowedAttachmentTypes(
  body: DocumentLoadersResponse | undefined,
): AllowedAttachmentTypes {
  if (body === undefined) {
    return { extensions: [], mimeTypes: [], isServed: false };
  }
  const extensions = new Set<string>();
  const mimeTypes = new Set<string>();
  for (const category of [body.document_types, body.image_types, body.code_types]) {
    for (const [extension, mimeType] of Object.entries(category ?? {})) {
      const normalised = normaliseExtension(extension);
      if (normalised === '') continue;
      extensions.add(normalised);
      if (mimeType !== '') mimeTypes.add(mimeType);
    }
  }
  // An answer that served nothing stays empty — including for SVG. The
  // baseline appends SVG to a POPULATED map; appending it to an empty one
  // would turn "nothing is allowed" into "one thing is allowed" and hide the
  // very state ELITEA-0489 is about.
  if (extensions.size > 0) {
    extensions.add(SVG_EXTENSION);
    mimeTypes.add(SVG_MIME_TYPE);
  }
  return {
    extensions: [...extensions].sort(),
    mimeTypes: [...mimeTypes].sort(),
    isServed: true,
  };
}

/** What {@link useAllowedAttachmentTypes} reports while it is still asking. */
export interface AllowedAttachmentTypesState extends AllowedAttachmentTypes {
  readonly isLoading: boolean;
}

/**
 * Reads the deployment's allow-list for one project.
 *
 * WHILE LOADING the extension list is the defaults, not empty: an empty list
 * disables the control, and a control that flickers disabled on every mount
 * is worse than one that briefly accepts a file the server would refuse (the
 * upload itself is still server-validated).
 */
export function useAllowedAttachmentTypes(projectId: string | undefined): AllowedAttachmentTypesState {
  const query = useGetDocumentLoaders(projectId ?? '', {
    query: {
      enabled: projectId !== undefined && projectId !== '',
      staleTime: 5 * 60 * 1000,
      // 501 is final (queryClient's `isFinalClientAnswer`), so a deployment
      // with the capability off asks once and then uses the defaults.
      retry: false,
    },
  });

  return useMemo<AllowedAttachmentTypesState>(() => {
    const response = query.data;
    if (response !== undefined && response.status === 200) {
      return { ...deriveAllowedAttachmentTypes(response.data), isLoading: false };
    }
    // Either still in flight, or the deployment cannot enumerate loaders.
    // Both stand on the defaults, and neither claims to have been served.
    return {
      extensions: DEFAULT_ATTACHMENT_EXTENSIONS,
      mimeTypes: [],
      isServed: false,
      isLoading: query.isLoading,
    };
  }, [query.data, query.isLoading]);
}
