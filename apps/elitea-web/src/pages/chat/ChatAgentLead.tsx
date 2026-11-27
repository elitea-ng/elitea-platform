/**
 * The conversation's lead agent (ADR-0050 decision 7d, issue 1612): a "Lead"
 * badge on the lead's row in the participants panel and a row menu to make an
 * agent the lead or clear it, and "Model in this thread…" (#3145, as the desktop's Participants tab has it): the
 * agent participant's own model settings in this conversation (`llm_settings`, the dialog the composer's agent panel
 * opens for the active agent).
 *
 * The floor lives in the conversation meta (`floor`), and updating a
 * conversation replaces its whole meta, so the write goes through
 * `metaWithLead` (`@elitea/client-core`), which keeps every other key.
 */
import type { MouseEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import MoreVertIcon from '@mui/icons-material/MoreVert';
import { useQueryClient } from '@tanstack/react-query';

import { floorOf, metaWithLead } from '@elitea/client-core/conversation';
import { conversationApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { toParticipant, useChatBoxLlmSettingsDialog } from '@/widgets/chat-box';

export interface UseConversationLeadParams {
  readonly projectId: string | undefined;
  readonly conversationId: string | number | undefined;
  readonly meta: unknown;
}

export interface UseConversationLeadResult {
  /** The lead's participant id; 0 with no lead. */
  readonly leadParticipantId: number;
  /** Makes `participantId` the lead; 0 clears it. */
  readonly setLead: (participantId: number) => Promise<void>;
  readonly isSaving: boolean;
}

export function useConversationLead({ projectId, conversationId, meta }: UseConversationLeadParams): UseConversationLeadResult {
  const queryClient = useQueryClient();
  const { mutateAsync: editConversation, isPending } = conversationApi.useEdit();
  // The value just written, shown until the conversation read catches up.
  const [written, setWritten] = useState<{ readonly meta: unknown; readonly lead: number } | undefined>(undefined);
  const leadParticipantId = written !== undefined && written.meta === meta ? written.lead : floorOf(meta).leadParticipantId;

  const setLead = useCallback(async (participantId: number) => {
    if (projectId === undefined || conversationId === undefined) return;
    await editConversation({ projectId, id: conversationId, meta: metaWithLead(meta, participantId) });
    setWritten({ meta, lead: floorOf(metaWithLead(meta, participantId)).leadParticipantId });
    await queryClient.invalidateQueries({ queryKey: ['conversation', 'details', projectId] });
  }, [projectId, conversationId, meta, editConversation, queryClient]);

  return { leadParticipantId, setLead, isSaving: isPending };
}

/** "Model in this thread…" for one agent row (#3145): the participant's model settings dialog. */
function useParticipantModelDialog(projectId: string | undefined, conversationId: string | number | undefined, participants: readonly unknown[] | undefined) {
  const [participantId, setParticipantId] = useState<number | null>(null);
  const raw = participantId === null ? undefined : (participants ?? []).find((row) => Number((row as { readonly id?: unknown } | null)?.id) === participantId);
  const llm = useChatBoxLlmSettingsDialog({ projectId, conversationId, participant: toParticipant(raw) });
  const { onEdit } = llm;
  const open = useCallback((id: number) => {
    setParticipantId(id);
    onEdit();
  }, [onEdit]);
  return { open, dialog: llm.dialog };
}

/**
 * The participants panel's `renderAgentLead` slot for a conversation, present with two or more agents, and the model
 * dialog its rows open (`dialog`, rendered beside the panel).
 */
export function useAgentLeadSlot(
  projectId: string | undefined,
  conversation: { readonly id?: string | number; readonly isNew?: boolean; readonly meta?: unknown; readonly participants?: readonly unknown[] } | undefined,
): { renderAgentLead?: (props: { conversationId: string | number; participantId: number }) => ReactNode; dialog: ReactNode } {
  const conversationId = conversation?.isNew === true ? undefined : conversation?.id;
  const { leadParticipantId, setLead, isSaving } = useConversationLead({ projectId, conversationId, meta: conversation?.meta });
  const model = useParticipantModelDialog(projectId, conversationId, conversation?.participants);
  const severalAgents = hasSeveralAgents(conversation?.participants);
  const { open, dialog } = model;
  return useMemo(() => (severalAgents
    ? {
        dialog,
        renderAgentLead: ({ participantId }: { participantId: number }) => (
          <AgentLeadControl
            participantId={participantId}
            leadParticipantId={leadParticipantId}
            disabled={isSaving}
            onSetLead={(next) => { setLead(next).catch((error: unknown) => { console.error('[chat] failed to save the lead', error); }); }}
            onModel={conversationId === undefined ? undefined : () => open(participantId)}
          />
        ),
      }
    : { dialog }), [severalAgents, leadParticipantId, setLead, isSaving, conversationId, open, dialog]);
}

/** The lead matters only with two or more agents (with one, it answers anyway). */
export function hasSeveralAgents(participants: readonly unknown[] | undefined): boolean {
  return (participants ?? []).filter((row) => (row as { readonly entity_name?: unknown } | null)?.entity_name === 'application').length >= 2;
}

export interface AgentLeadControlProps {
  readonly participantId: number;
  readonly leadParticipantId: number;
  readonly onSetLead: (participantId: number) => void;
  readonly disabled?: boolean;
  /** "Model in this thread…": the agent's model settings in this conversation (#3145); absent before it exists. */
  readonly onModel?: (() => void) | undefined;
}

/** The badge and the menu of one agent row. */
export function AgentLeadControl({ participantId, leadParticipantId, onSetLead, disabled = false, onModel }: AgentLeadControlProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const isLead = participantId === leadParticipantId;
  const stop = (event: MouseEvent) => { event.stopPropagation(); };
  const choose = (next: number) => (event: MouseEvent) => {
    event.stopPropagation();
    setAnchor(null);
    onSetLead(next);
  };
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.25, flexShrink: 0 }} onClick={stop} data-testid="participant-lead" data-participant-id={participantId}>
      {isLead && (
        <Chip size="small" label={t('chat.lead.badge', 'Lead')} data-testid="participant-lead-badge" />
      )}
      <IconButton
        size="small"
        disabled={disabled}
        aria-label={t('chat.lead.menu', 'Floor options')}
        data-testid="participant-lead-menu"
        onClick={(event) => { event.stopPropagation(); setAnchor(event.currentTarget); }}
      >
        <MoreVertIcon fontSize="small" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} onClick={stop}>
        {isLead ? (
          <MenuItem onClick={choose(0)} data-testid="participant-lead-clear">{t('chat.lead.clear', 'Clear lead')}</MenuItem>
        ) : (
          <MenuItem onClick={choose(participantId)} data-testid="participant-lead-make">{t('chat.lead.make', 'Make lead')}</MenuItem>
        )}
        {onModel !== undefined && (
          <MenuItem
            onClick={(event) => { event.stopPropagation(); setAnchor(null); onModel(); }}
            data-testid="participant-model"
          >
            {t('chat.participants.model', 'Model in this thread…')}
          </MenuItem>
        )}
      </Menu>
    </Box>
  );
}
