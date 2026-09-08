/**
 * Which wiki VERSION a toolkit was last read at.
 *
 * A repository that has been analysed more than once has more than one
 * manifest in the bucket, and the browser opens one of them. The legacy app
 * remembered which: `wikiVersionStorageKey` in DeepWikiApp.jsx:733, written on
 * every version change (:1388) and read back when the listing loaded (:1284).
 * Without it, reopening a wiki silently drops the user back onto the newest
 * manifest — a different wiki from the one they were reading, on a screen that
 * says nothing about having changed it.
 *
 * THE RESTORE RULE IS THE LEGACY ONE. A stored id is used only when the
 * listing STILL CONTAINS it; otherwise the first wiki opens. A deleted or
 * filtered-out version must not leave the reader on nothing, and it must not
 * be re-stored either — which is why the restore does not write back.
 *
 * THE NAMESPACE is the change from legacy. `localStorage.setItem` on a raw
 * key survives sign-out — `clearNamespace()` sweeps the `el.` prefix and
 * nothing else — so the next user of the machine inherits the last one's
 * reading position (issue #22). `createStorage` puts this key inside the
 * sweep, the same way `chatStorage.ts` and `generationStorage.ts` do.
 */
import { createStorage } from '@/shared/lib/storage';

const store = createStorage('local');

function keyFor(projectId: string | number, toolkitId: string | number): string {
  return `deepwiki.selectedWiki.${String(projectId)}.${String(toolkitId)}`;
}

export interface WikiVersionStorage {
  /** The wiki_id chosen last time, or null when there is none stored. */
  load(): string | null;
  save(wikiId: string): void;
  /** Forgets the choice — what a delete leaves behind is not a choice. */
  clear(): void;
}

export function createWikiVersionStorage(
  projectId: string | number,
  toolkitId: string | number,
): WikiVersionStorage {
  const key = keyFor(projectId, toolkitId);
  return {
    load: () => store.get(key),
    save: (wikiId: string) => {
      store.set(key, wikiId);
    },
    clear: () => {
      store.remove(key);
    },
  };
}
