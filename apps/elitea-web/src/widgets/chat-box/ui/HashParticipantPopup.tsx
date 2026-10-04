/**
 * The "#" agent/pipeline picker above the composer (#6774).
 *
 * Typing "#" put the composer into a picker mode, but nothing rendered a
 * picker: Send stayed disabled and the chat looked frozen. This popup searches
 * the project's agents and pipelines with the text after the "#". It shows an
 * empty state when nothing matches. A pick attaches the entity through the
 * same handler the "+" menu uses, so an agent that is already attached becomes
 * the active participant instead of a duplicate.
 */
import { useCallback, useMemo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { useParticipants } from '@/entities/participant';
import type { ParticipantEntityItem } from '@/entities/participant';
import { NewParticipantList } from '@/features/chat-recommendations';
import { t } from '@/shared/i18n';

/** Mirrors `useChatBoxState`'s env read; the public project only scopes toolkits, which this picker never lists. */
const PUBLIC_PROJECT_ID = (import.meta.env['VITE_PUBLIC_PROJECT_ID'] as string | undefined) || '0';

/** The card's own wire literal for an agent/pipeline row (`NewParticipantCard`'s `APPLICATIONS_TYPE`). */
const CARD_APPLICATIONS_TYPE = 'applications';

export interface HashParticipantSelection extends Readonly<Record<string, unknown>> {
  readonly id: string;
  readonly name: string;
  /** `'application'` or `'pipeline'`: the values `useAddEntityParticipant` accepts. */
  readonly participantType: string;
}

interface CandidateWireRow {
  readonly id?: unknown;
  readonly project_id?: unknown;
  readonly description?: unknown;
}

/** @public (test seam) A browse row in the selection shape the "+" menu hands to `useAddEntityParticipant`. */
export function toHashSelection(item: ParticipantEntityItem, projectId: string | undefined): HashParticipantSelection {
  const row = item.data as CandidateWireRow;
  const id = typeof row.id === 'string' || typeof row.id === 'number' ? String(row.id) : item.label;
  const rowProjectId = typeof row.project_id === 'string' || typeof row.project_id === 'number'
    ? String(row.project_id)
    : projectId;
  return {
    ...item.data,
    id,
    name: item.label,
    participantType: item.participantType,
    ...(rowProjectId !== undefined ? { project_id: rowProjectId } : {}),
    ...(item.participantType === 'pipeline' ? { agent_type: 'pipeline' } : {}),
  };
}

/** The card's display shape. */
function toCard(selection: HashParticipantSelection): Readonly<Record<string, unknown>> {
  const description = selection['description'];
  return {
    id: selection.id,
    name: selection.name,
    participantType: CARD_APPLICATIONS_TYPE,
    ...(typeof description === 'string' ? { description } : {}),
    ...(typeof selection['project_id'] === 'string' ? { project_id: selection['project_id'] } : {}),
    ...(selection.participantType === 'pipeline' ? { agent_type: 'pipeline' } : {}),
  };
}

export interface HashParticipantPopupProps {
  /** The picker query including its leading "#". */
  readonly query: string;
  readonly projectId: string | undefined;
  readonly onSelect: (selection: HashParticipantSelection) => void;
  readonly onClose: () => void;
}

export function HashParticipantPopup({ query, projectId, onSelect, onClose }: HashParticipantPopupProps): ReactNode {
  const search = query.startsWith('#') ? query.slice(1) : query;
  const { participants, isLoading, isFetching } = useParticipants({
    projectId,
    publicProjectId: PUBLIC_PROJECT_ID,
    canListPublicAgents: false,
    projectFilter: 'teamProject',
    query: search,
    types: ['application'],
    enabled: projectId !== undefined,
  });

  const selections = useMemo(
    () => participants
      .filter((item) => item.participantType === 'application' || item.participantType === 'pipeline')
      .map((item) => toHashSelection(item, projectId)),
    [participants, projectId],
  );
  const cards = useMemo(() => selections.map(toCard), [selections]);

  const onSelectCard = useCallback(
    (card: unknown) => {
      const picked = card as { readonly id?: unknown; readonly agent_type?: unknown };
      const isPipeline = picked.agent_type === 'pipeline';
      const selection = selections.find((s) => s.id === picked.id && (s.participantType === 'pipeline') === isPipeline);
      if (selection) onSelect(selection);
    },
    [selections, onSelect],
  );

  return (
    <Box sx={{ mb: 1 }} data-testid="chat-hash-participant-popup">
      <NewParticipantList
        title={t('widgets.chatBox.hashPicker.title', 'Agents and pipelines')}
        participants={[...cards]}
        isLoading={isLoading}
        isFetching={isFetching}
        onSelectParticipant={onSelectCard}
        onClose={onClose}
      />
    </Box>
  );
}
