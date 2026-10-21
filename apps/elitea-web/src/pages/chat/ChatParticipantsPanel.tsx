/**
 * The chat's participants panel — participants, agents, toolkits and the
 * context budget — in one of two places:
 *
 *  - the web build: the right rail of the chat page, collapsible, default
 *    collapsed (the baseline, `NewChat.jsx:166`);
 *  - inside the desktop frame (`shared/desktop/inspectorSlot` has a slot):
 *    the frame's full-height inspector pane, which owns the width and the
 *    show / hide toggle (⌘⌥\). The same `ParticipantsWrapper` renders there
 *    with `appearance="inspector"`, through a portal, so every handler and
 *    query below is the page's own either way.
 *
 * Split out of `index.tsx` (the page is at its §3.5 complexity ceiling).
 */
import type { ComponentProps, ReactNode } from 'react';
import { useState } from 'react';

import { ParticipantsWrapper } from '@/features/chat-participants';
import { InspectorPortal, useInspectorSlot } from '@/shared/desktop/inspectorSlot';
import type { ChatBoxProps } from '@/widgets/chat-box';
import { ContextBudget } from '@/widgets/context-budget';

/**
 * Baseline `rightPanelWidth` (`NewChat.jsx:187`). `ParticipantsWrapper`'s own
 * default is 320, which is not the production width.
 */
const PARTICIPANTS_PANEL_WIDTH = 276;

type WrapperProps = ComponentProps<typeof ParticipantsWrapper>;
type ActiveConversation = NonNullable<ChatBoxProps['conversation']>['active'];

export interface ChatParticipantsPanelProps {
  readonly activeConversation: ActiveConversation | undefined;
  readonly activeParticipant: unknown;
  readonly projectId: string | undefined;
  readonly isMcpVisible: boolean;
  readonly handlers: Pick<WrapperProps, 'onSelectParticipant' | 'onDeleteParticipant' | 'onEditParticipant' | 'onAddParticipants'>;
}

export function ChatParticipantsPanel({ activeConversation, activeParticipant, projectId, isMcpVisible, handlers }: ChatParticipantsPanelProps): ReactNode {
  const inspector = useInspectorSlot() !== null;
  // Baseline default: collapsed (`NewChat.jsx:166`). The inspector never collapses.
  const [collapsed, setCollapsed] = useState(true);

  const panel = (
    <ParticipantsWrapper
      appearance={inspector ? 'inspector' : 'rail'}
      collapsed={collapsed}
      onCollapsed={() => setCollapsed((prev) => !prev)}
      panelWidth={PARTICIPANTS_PANEL_WIDTH}
      isMcpVisible={isMcpVisible}
      {...(activeConversation
        // `ChatBoxActiveConversation` types `participants` as `unknown[]`,
        // the rail's own prop as `Record<string, unknown>[]`. Same rows, two
        // slices of the app describing them at different precision; the cast
        // is the boundary, not a claim about the data.
        ? { activeConversation: activeConversation as NonNullable<WrapperProps['activeConversation']> }
        : {})}
      {...(activeParticipant ? { activeParticipant: activeParticipant as Record<string, unknown> } : {})}
      {...handlers}
      /*
       * The context-budget panel. `ParticipantsWrapper` has always accepted
       * this slot (and gates the `conversationId` it hands over on the
       * conversation being neither new nor playback); `features/` may not
       * import `widgets/`, which is why the slot is filled by the page.
       */
      renderContextBudget={({ conversationId: budgetConversationId, collapsed: budgetCollapsed }) => (
        <ContextBudget
          conversationId={activeConversation?.isNew || activeConversation?.isPlayback ? undefined : budgetConversationId}
          projectId={projectId}
          collapsed={budgetCollapsed ?? false}
        />
      )}
    />
  );

  return inspector ? <InspectorPortal kind="chat">{panel}</InspectorPortal> : panel;
}
