/**
 * The gallery's Import (issue 3137): fetch the blueprint's file, read it with
 * the SAME reader the Import button uses (`readImportFile`), and hand the
 * result to the same dialog and the same write. A blueprint therefore
 * imports exactly as the file would if the person downloaded and imported it.
 */
import { useCallback, useState } from 'react';

import { readImportFile } from '@/features/agent-lifecycle';
import { t } from '@/shared/i18n';
import { fetchStaticFile } from '@/shared/lib/download';

import { blueprintPath, type BlueprintEntry } from './blueprintIndex';

type ReadImportResult = Awaited<ReturnType<typeof readImportFile>>;

export interface BlueprintImport {
  readonly pending: ReadImportResult | undefined;
  /** The slug of the blueprint being fetched and read, if any. */
  readonly loadingSlug: string | undefined;
  readonly error: string | undefined;
  readonly start: (entry: BlueprintEntry) => Promise<void>;
  readonly close: () => void;
}

export function useBlueprintImport(): BlueprintImport {
  const [pending, setPending] = useState<ReadImportResult>();
  const [loadingSlug, setLoadingSlug] = useState<string>();
  const [error, setError] = useState<string>();

  const start = useCallback(async (entry: BlueprintEntry) => {
    setError(undefined);
    setLoadingSlug(entry.slug);
    let blob: Blob;
    try {
      blob = await fetchStaticFile(blueprintPath(entry.file));
    } catch {
      setError(t('widgets.blueprintGallery.importUnavailable', 'The blueprint {{name}} could not be loaded.', { name: entry.name }));
      setLoadingSlug(undefined);
      return;
    }
    try {
      const name = entry.file.split('/').pop() ?? entry.file;
      setPending(await readImportFile(new File([blob], name, { type: blob.type })));
    } catch (cause) {
      // The reader's refusals are sentences meant for a person (ImportDocumentError).
      setError(cause instanceof Error ? cause.message : t('widgets.blueprintGallery.importUnreadable', 'The blueprint {{name}} could not be read.', { name: entry.name }));
    } finally {
      setLoadingSlug(undefined);
    }
  }, []);

  const close = useCallback(() => setPending(undefined), []);

  return { pending, loadingSlug, error, start, close };
}
