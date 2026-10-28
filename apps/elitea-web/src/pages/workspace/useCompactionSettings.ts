/**
 * The app's auto-compaction settings for the Context tab
 * (`compactionSettings`): read from the host (after the old idle switch
 * moved there), kept in step with `compaction-settings-changed`, and saved
 * at once on each change. A refused save goes back to what the host keeps
 * and says why.
 */
import { useEffect, useState } from 'react';

import { compactionIpc, readCompactionSettings, writeCompactionSettings, type CompactionSettings } from '@/shared/desktop/compactionSettings';

export interface CompactionSettingsControl {
  /** `undefined` while reading, or outside the desktop host (`available` false). */
  settings: CompactionSettings | undefined;
  available: boolean;
  /** Why the last read or save failed. */
  error: string | null;
  save: (next: CompactionSettings) => void;
}

const messageOf = (error: unknown): string => (error instanceof Error ? error.message : String(error));

export function useCompactionSettings(): CompactionSettingsControl {
  const [ipc] = useState(compactionIpc);
  const [settings, setSettings] = useState<CompactionSettings | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let closed = false;
    let off: (() => void) | undefined;
    readCompactionSettings(ipc).then(
      (read) => !closed && setSettings(read),
      (failure: unknown) => !closed && setError(messageOf(failure)),
    );
    void ipc
      .onSettingsChanged((changed) => setSettings(changed))
      .then((unsubscribe) => {
        if (closed) unsubscribe();
        else off = unsubscribe;
      })
      .catch(() => undefined);
    return () => {
      closed = true;
      off?.();
    };
  }, [ipc]);
  const save = (next: CompactionSettings): void => {
    if (ipc === undefined) return;
    const previous = settings;
    setSettings(next);
    setError(null);
    writeCompactionSettings(ipc, next).then(setSettings, (failure: unknown) => {
      setSettings(previous);
      setError(messageOf(failure));
    });
  };
  return { settings, available: ipc !== undefined, error, save };
}
