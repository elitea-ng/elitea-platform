import { describe, expect, it } from 'vitest';

import * as slice from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is deliberately the value-export subset only — see
 * `./index.ts`'s own module doc for the full (type + value) surface and the
 * reasoning behind each inclusion/exclusion. Precedent: `entities/
 * conversation/index.test.ts`, `features/pipelines/index.test.ts`.
 */
const PUBLIC_SURFACE = [
  'Conversations',
  'useArchiveConversation',
  'useConversationRail',
  'useCreateFolder',
  'useDateGroupExpansion',
  'useDeleteFolder',
  'useDragAndDrop',
  'useEditFolder',
  'useMoveToFolderConversation',
  'usePinConversation',
  'useQueryFoldersList',
  'useReorderFolders',
] as const;

describe('features/chat-conversation-list public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(slice).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
