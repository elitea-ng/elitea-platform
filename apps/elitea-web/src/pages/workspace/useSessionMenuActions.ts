/**
 * What the Thread and Folder menus (and ⇧⌘U, ⌘L, ⇧⌘P) reach on the thread on
 * screen, registered with the shell (`SessionActions`). Each acts through a
 * latest-value ref, so the registration itself never goes stale.
 */
import { useEffect, useRef, type RefObject } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { ThreadHandoff } from './useThreadHandoff';

export interface SessionMenuInput {
  handoff: ThreadHandoff;
  conversationId: string;
  hasIndex: boolean;
  agentRef: RefObject<HTMLDivElement | null>;
  openIndexSettings: () => void;
}

export function useSessionMenuActions(input: SessionMenuInput): void {
  const navigate = useNavigate();
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
      indexSettings: () => {
        if (latest.current.hasIndex) latest.current.openIndexSettings();
      },
      handoffSettings: () => {
        if (latest.current.handoff.ipc !== undefined) latest.current.handoff.open('settings');
      },
    });
  }, [navigate]);
}
