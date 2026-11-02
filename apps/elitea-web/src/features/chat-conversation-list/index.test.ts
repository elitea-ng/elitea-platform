import { describe, expect, it } from 'vitest';

import * as slice from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is the value-export subset only. Precedent: `entities/
 * conversation/index.test.ts`, `features/pipelines/index.test.ts`.
 */
const PUBLIC_SURFACE = [
  'ShareLinkDialog',
  'useArchiveConversation',
  'useConversationActivity',
  'useConversationRail',
  'useDragAndDrop',
  'useForkConversation',
  'useMarkConversationReadOnView',
] as const;

describe('features/chat-conversation-list public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(slice).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
