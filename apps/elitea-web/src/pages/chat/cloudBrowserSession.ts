/**
 * Which cloud browser a chat shows (ADR-0049 decision 6, #1598): the
 * session of the newest answer whose run used the browser tools. A run's
 * session name is derived from its execution id (`browserSessionNameFor`),
 * and an answer names its run: a live answer in `executionId` (stamped by
 * the transport), a reloaded one in `taskId` (from the message list while
 * the turn streams). A newer run has a session of its own; the older one has ended
 * or lingers watch-only, so only the newest is shown.
 */
import { browserSessionNameFor } from '@/shared/lib/browserSessionName';

/** The `browser` toolkit family (`libs/rust/agent-runtime/src/toolkits/families/browser`). */
const BROWSER_TOOLKIT_TYPE = 'browser';
const BROWSER_TOOL = /^browser_[a-z_]+$/;
const EXECUTION_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,255}$/;

interface MessageShape {
  readonly role?: unknown;
  readonly taskId?: unknown;
  readonly executionId?: unknown;
  readonly toolActions?: readonly unknown[];
}

function usesBrowser(action: unknown): boolean {
  const value = action as { readonly name?: unknown; readonly original_name?: unknown; readonly toolMeta?: { readonly toolkit_type?: unknown } } | null;
  if (value === null || typeof value !== 'object') return false;
  if (value.toolMeta?.toolkit_type === BROWSER_TOOLKIT_TYPE) return true;
  return [value.name, value.original_name].some((name) => typeof name === 'string' && BROWSER_TOOL.test(name));
}

/** The browser session of the newest answer that used the browser, or `undefined`. */
export function cloudBrowserSessionOf(messages: readonly unknown[]): string | undefined {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index] as MessageShape | null;
    if (message === null || typeof message !== 'object' || message.role !== 'assistant') continue;
    if (!(message.toolActions ?? []).some(usesBrowser)) continue;
    const named = typeof message.executionId === 'string' ? message.executionId : message.taskId;
    const executionId = typeof named === 'string' ? named.trim() : '';
    return EXECUTION_ID.test(executionId) ? browserSessionNameFor(executionId) : undefined;
  }
  return undefined;
}
