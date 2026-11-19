/**
 * The project skills pinned to a local thread (ADR-0050 amendment 1, A1.5; #2183), for the composer's "/" menu. A pin
 * is kept by the desktop host with the thread, not here: this reads the list and asks the host to pin or unpin. The
 * host applies every pin to each turn of the thread, so a pinned skill needs no `/name` in the message.
 */
import { useCallback, useMemo } from 'react';

import { useQuery, useQueryClient } from '@tanstack/react-query';

import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

import type { ComposerSkill } from './useAgentSkills';

/** What the "/" menu needs to pin and unpin: the pinned skills' ids (as the menu holds them) and the two actions. Rejects with the host's refusal. */
export interface SkillPinControls {
  readonly pinned: ReadonlySet<string>;
  readonly pin: (skill: ComposerSkill) => Promise<void>;
  readonly unpin: (skill: ComposerSkill) => Promise<void>;
}

const NO_PINS: readonly string[] = [];

const threadSkillsKey = (workspaceId: string, conversationId: string) => ['workspace', 'thread-skills', workspaceId, conversationId] as const;

/**
 * The thread's pin controls; `undefined` while there is nothing to pin to (a new thread has no saved conversation
 * yet) or the thread does not run here (the cloud copy takes the server's pins).
 */
export function useThreadSkillPins(ipc: WorkspaceIpc, workspaceId: string, projectId: number, conversationId: string, enabled: boolean): SkillPinControls | undefined {
  const on = enabled && conversationId !== '';
  const client = useQueryClient();
  const key = threadSkillsKey(workspaceId, conversationId);
  const read = useQuery({
    queryKey: key,
    queryFn: async (): Promise<readonly string[]> => (await ipc.threadSkills(workspaceId, conversationId)).filter((pin) => pin.projectId === projectId).map((pin) => String(pin.skillId)),
    enabled: on,
    retry: false,
  });
  const refresh = useCallback(() => client.invalidateQueries({ queryKey: key }), [client, key]);
  const ids = read.data ?? NO_PINS;
  const pinned = useMemo(() => new Set(ids), [ids]);
  const pin = useCallback(
    async (skill: ComposerSkill): Promise<void> => {
      try {
        await ipc.pinThreadSkill(workspaceId, conversationId, { projectId, skillId: Number(skill.id) });
      } finally {
        await refresh();
      }
    },
    [ipc, workspaceId, conversationId, projectId, refresh],
  );
  const unpin = useCallback(
    async (skill: ComposerSkill): Promise<void> => {
      try {
        await ipc.unpinThreadSkill(workspaceId, conversationId, Number(skill.id));
      } finally {
        await refresh();
      }
    },
    [ipc, workspaceId, conversationId, refresh],
  );
  return useMemo(() => (on ? { pinned, pin, unpin } : undefined), [on, pinned, pin, unpin]);
}
