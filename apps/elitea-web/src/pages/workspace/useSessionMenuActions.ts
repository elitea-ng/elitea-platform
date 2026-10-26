/**
 * What the Thread and Folder menus (and ⇧⌘U, ⌘L, ⇧⌘P) reach on the thread on
 * screen, registered with the shell (`SessionActions`). Each acts through a
 * latest-value ref, so the registration itself never goes stale.
 */
import { useEffect, useRef, type RefObject } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { useActivityFeed, useDesktopLayout } from '@/widgets/desktop-shell';

import type { ThreadHandoff } from './useThreadHandoff';

export interface SessionMenuInput {
  handoff: ThreadHandoff;
  conversationId: string;
  hasIndex: boolean;
  agentRef: RefObject<HTMLDivElement | null>;
  openIndexSettings: () => void;
  /** Opens the agent builder (ADR-0037). */
  createAgent: () => void;
  /** The folder, for the island's cloud line. */
  workspace: { id: string; name: string };
}

export function useSessionMenuActions(input: SessionMenuInput): void {
  const navigate = useNavigate();
  useCloudActivity(input);
  const latest = useRef(input);
  useEffect(() => {
    latest.current = input;
  });
  useEffect(() => {
    useDesktopLayout.getState().extendSession({
      sendToCloud: () => {
        if (latest.current.handoff.canSend) latest.current.handoff.open('send');
      },
      openInChat: () => {
        const id = latest.current.conversationId;
        if (id !== '') void navigate({ to: '/chat/$conversationId', params: { conversationId: id } });
      },
      chooseAgent: () => latest.current.agentRef.current?.querySelector<HTMLElement>('button')?.click(),
      createAgent: () => latest.current.createAgent(),
      indexSettings: () => {
        if (latest.current.hasIndex) latest.current.openIndexSettings();
      },
      handoffSettings: () => {
        if (latest.current.handoff.ipc !== undefined) latest.current.handoff.open('settings');
      },
    });
  }, [navigate]);
}

/** A thread running in the cloud (or on its way back) is an activity of the window, whichever screen is open. */
function useCloudActivity({ handoff, conversationId, workspace }: SessionMenuInput): void {
  const { uuid, offLaptop, status } = handoff;
  const state = status?.state;
  useEffect(() => {
    if (uuid === undefined || !offLaptop || state === undefined) return undefined;
    useActivityFeed.getState().setCloud(uuid, { conversationUuid: uuid, state, title: workspace.name, workspaceId: workspace.id, conversationId });
    return () => useActivityFeed.getState().setCloud(uuid, null);
  }, [uuid, offLaptop, state, workspace.id, workspace.name, conversationId]);
}
