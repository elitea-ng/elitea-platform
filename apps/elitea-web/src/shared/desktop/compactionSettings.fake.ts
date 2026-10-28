/**
 * An in-memory `CompactionIpc` for tests and the dev harness. `set` refuses
 * what the host refuses (`compactionSettingsError`) and emits
 * `compaction-settings-changed`; `holds` records each hold call; `compact`
 * plays a `workspace-context-compacted` event.
 */
import { compactionSettingsError, DEFAULT_COMPACTION_SETTINGS, type CompactionIpc, type CompactionSettings, type ContextCompactedEvent } from './compactionSettings';

export interface FakeCompactionIpc extends CompactionIpc {
  readonly calls: { sets: CompactionSettings[]; holds: { workspaceId: string; conversationId: string; hold: boolean }[] };
  /** The settings the host keeps now. */
  current(): CompactionSettings;
  /** The host summarized a thread. */
  compact(event: ContextCompactedEvent): void;
  /** Another window changed the settings. */
  change(settings: CompactionSettings): void;
}

export function createFakeCompactionIpc(initial: CompactionSettings = DEFAULT_COMPACTION_SETTINGS): FakeCompactionIpc {
  let settings = { ...initial };
  const calls: FakeCompactionIpc['calls'] = { sets: [], holds: [] };
  const changed = new Set<(settings: CompactionSettings) => void>();
  const compacted = new Set<(event: ContextCompactedEvent) => void>();
  const subscribe = <T>(set: Set<T>, handler: T): Promise<() => void> => {
    set.add(handler);
    return Promise.resolve(() => void set.delete(handler));
  };
  const change = (next: CompactionSettings): void => {
    settings = { ...next };
    for (const handler of changed) handler({ ...settings });
  };
  return {
    calls,
    current: () => ({ ...settings }),
    compact: (event) => {
      for (const handler of compacted) handler(event);
    },
    change,
    get: () => Promise.resolve({ ...settings }),
    set(next) {
      calls.sets.push({ ...next });
      const error = compactionSettingsError(next);
      if (error !== null) return Promise.reject(new Error(error));
      change(next);
      return Promise.resolve({ ...settings });
    },
    hold(workspaceId, conversationId, hold) {
      calls.holds.push({ workspaceId, conversationId, hold });
      return Promise.resolve();
    },
    onSettingsChanged: (handler) => subscribe(changed, handler),
    onCompacted: (handler) => subscribe(compacted, handler),
  };
}
