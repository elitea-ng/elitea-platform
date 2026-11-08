/**
 * An in-memory `ComputerIpc` for tests. `setEnabled(true)` refuses what the
 * host refuses (no policy, no Accessibility); `session` plays the host's
 * session state machine (pause, resume only while paused, stop); `emit`
 * plays a `computer://event` from the host (the guard, the shortcut).
 */
import { IDLE_SESSION, type ComputerIpc, type ComputerSettings, type PrivacyGrant, type SessionView } from './computerIpc';
import { WorkspaceIpcError } from './workspaceIpc';

export interface FakeComputerIpc extends ComputerIpc {
  readonly calls: { opened: PrivacyGrant[]; forgotten: string[]; actions: string[] };
  /** The host changed the session (input, the shortcut, a turn). */
  emit(view: SessionView): void;
  /** The person changed a grant in System Settings. */
  grant(change: Partial<Pick<ComputerSettings, 'accessibility' | 'screen_recording'>>): void;
}

const FAKE_COMPUTER_SETTINGS: ComputerSettings = {
  policy_allows: true,
  enabled: false,
  supported: true,
  read_only_reason: null,
  accessibility: 'granted',
  screen_recording: 'denied',
  screenshot_tools: ['computer_screenshot', 'computer_click_at'],
  stop_shortcut: 'ctrl+option+escape',
  remembered: [],
};

export function createFakeComputerIpc(initial: Partial<ComputerSettings> = {}, session: SessionView = IDLE_SESSION): FakeComputerIpc {
  let settings: ComputerSettings = { ...FAKE_COMPUTER_SETTINGS, ...initial };
  let view = { ...session };
  const calls: FakeComputerIpc['calls'] = { opened: [], forgotten: [], actions: [] };
  const listeners = new Set<(view: SessionView) => void>();
  const emit = (next: SessionView): void => {
    view = { ...next };
    for (const handler of listeners) handler({ ...view });
  };
  return {
    calls,
    emit,
    grant: (change) => {
      settings = { ...settings, ...change };
    },
    settings: () => Promise.resolve(structuredClone(settings)),
    setEnabled(enabled) {
      if (enabled && !settings.policy_allows) return Promise.reject(new WorkspaceIpcError('computer_policy_off', 'Your organisation does not allow computer use.'));
      if (enabled && settings.accessibility !== 'granted') return Promise.reject(new WorkspaceIpcError('computer_accessibility_missing', 'Grant Accessibility to Elitea first.'));
      settings = { ...settings, enabled };
      return Promise.resolve(structuredClone(settings));
    },
    forgetApp(bundleId) {
      calls.forgotten.push(bundleId);
      settings = { ...settings, remembered: settings.remembered.filter((app) => app.bundle_id !== bundleId) };
      return Promise.resolve(structuredClone(settings));
    },
    openPrivacy(grant) {
      calls.opened.push(grant);
      return Promise.resolve();
    },
    session(action) {
      calls.actions.push(action);
      const live = view.phase === 'active' || view.phase === 'paused';
      if (action === 'pause' && view.phase === 'active') emit({ ...view, phase: 'paused', paused_by: 'button' });
      if (action === 'resume' && view.phase === 'paused') emit({ ...view, phase: 'active', paused_by: null });
      if (action === 'stop' && live) emit({ ...view, phase: 'stopped', paused_by: null });
      return Promise.resolve({ ...view });
    },
    onSession(handler) {
      listeners.add(handler);
      return Promise.resolve(() => void listeners.delete(handler));
    },
  };
}
