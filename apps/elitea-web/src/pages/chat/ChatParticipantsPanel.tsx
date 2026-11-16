/**
 * The chat's participants panel — participants, agents, toolkits and the
 * context budget — as the Conversation tab of the shell's right panel
 * (`shared/ui/side-panel`): the desktop frame's inspector, which owns the
 * width, the show / hide toggle (⌘⌥\), the tab's group and its own window,
 * or the web shell's side panel, which owns the width and the toggle in the
 * title row. The same `ParticipantsWrapper` renders there with
 * `appearance="inspector"`, through a portal, so every handler and query
 * below is the page's own wherever the tab is shown.
 *
 * Without a shell panel (the chat page mounted alone in a test) it falls
 * back to the baseline's inline rail, collapsible and default collapsed
 * (`NewChat.jsx:166`).
 *
 * Split out of `index.tsx` (the page is at its §3.5 complexity ceiling).
 */
import type { ComponentProps, ReactNode } from 'react';
import { useCallback, useState } from 'react';

import { ParticipantsWrapper } from '@/features/chat-participants';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { SidePanelTab, useSidePanelSlot } from '@/shared/ui/side-panel';
import { t } from '@/shared/i18n';
import type { ChatBoxProps } from '@/widgets/chat-box';
import { AgentContextMeter, ContextBudget } from '@/widgets/context-budget';

import { useAgentLeadSlot } from './ChatAgentLead';
import { ConversationSkillsSection } from './ConversationSkillsSection';

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
  const inspector = useSidePanelSlot() !== null;
  // The open conversation names itself in the shell's title row (`Chat ▸ name`).
  // The desktop frame's title row reads the chat rail instead and ignores this.
  const conversationName = activeConversation === undefined || activeConversation.isNew ? '' : (activeConversation.name ?? '');
  usePageCrumbs(conversationName === '' ? [] : [{ label: conversationName }]);
  // Baseline default: collapsed (`NewChat.jsx:166`). The inspector never collapses.
  const [collapsed, setCollapsed] = useState(true);
  // Each agent's own meter in its row (issue 1610); one query with the panel.
  const renderAgentContext = useCallback(
    ({ conversationId, participantId }: { conversationId: string | number; participantId: number }) => (
      <AgentContextMeter projectId={projectId} conversationId={conversationId} participantId={participantId} />
    ),
    [projectId],
  );
  // ADR-0050 7d (issue 1612): the lead badge and menu on each agent row, with two or more agents.
  const leadSlot = useAgentLeadSlot(projectId, activeConversation);

  // ADR-0050 amendment 1, A1.3: the skills pinned to the conversation, in their own section beside
  // the participants and never among them. Only a saved conversation can hold a pin.
  const savedConversationId =
    activeConversation === undefined || activeConversation.isNew || activeConversation.isPlayback ? undefined : activeConversationId(activeConversation);
  const skills = (
    <ConversationSkillsSection
      projectId={projectId}
      conversationId={savedConversationId}
      personName={personNameOf(activeConversation)}
    />
  );

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
      renderAgentContext={renderAgentContext}
      {...leadSlot}
      renderContextBudget={({ conversationId: budgetConversationId, collapsed: budgetCollapsed }) => (
        <ContextBudget
          conversationId={activeConversation?.isNew || activeConversation?.isPlayback ? undefined : budgetConversationId}
          projectId={projectId}
          collapsed={budgetCollapsed ?? false}
        />
      )}
    />
  );

  return inspector ? (
    <SidePanelTab id="conversation" title={t('desktop.inspector.chat', 'Conversation')} subject={activeConversation?.name ?? ''}>
      {panel}
      {skills}
    </SidePanelTab>
  ) : (
    <>
      {panel}
      {skills}
    </>
  );
}

/** The conversation's id as the pin routes take it: its uuid when it has one, else its numeric id. */
export function activeConversationId(conversation: NonNullable<ActiveConversation>): string | undefined {
  if (conversation.uuid !== undefined && conversation.uuid !== '') return conversation.uuid;
  return conversation.id === undefined || conversation.id === '' ? undefined : String(conversation.id);
}

/** Names of the people in the conversation by user id, for "Pinned by ...". */
export function personNameOf(conversation: ActiveConversation | undefined): (userId: number) => string | undefined {
  const names = new Map<string, string>();
  for (const participant of (conversation?.participants ?? []) as readonly unknown[]) {
    const row = participant as { readonly entity_name?: unknown; readonly entity_meta?: { readonly id?: unknown; readonly name?: unknown } };
    if (row.entity_name !== 'user' || row.entity_meta === undefined) continue;
    const id = row.entity_meta.id;
    const name = row.entity_meta.name;
    if ((typeof id === 'string' || typeof id === 'number') && typeof name === 'string') names.set(String(id), name);
  }
  return (userId) => names.get(String(userId));
}
