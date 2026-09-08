import { describe, expect, it } from 'vitest';

import * as chatProcess from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: `index.ts` is the
 * only file other layers may import). `export type` interfaces are erased
 * by `verbatimModuleSyntax` and never appear on the runtime namespace
 * object, so this list is deliberately the value-export subset only —
 * precedent: `entities/canvas/index.test.ts`.
 */
const PUBLIC_SURFACE = [
  'useEditorMutex',
  'useCloseEditorAlert',
  'useStreamingNavBlocker',
  'useChatCopyToClipboard',
  'copyEventHooks',
  'useChatInteractionUUID',
  'useInternalToolsConfig',
  'useRefetchAgentVersionDetailsOnClose',
  'useChatEntityBrowser',
  'syncVariableKeys',
  'ChatWithEditors',
] as const;

describe('processes/chat public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(chatProcess).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
