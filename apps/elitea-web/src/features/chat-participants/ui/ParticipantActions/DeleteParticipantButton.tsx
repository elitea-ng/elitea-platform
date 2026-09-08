// @ts-nocheck
/**
 * DeleteParticipantButton — delete confirmation button for a participant.
 *
 * Ported from `[fsd]/features/chat/participants/ui/ParticipantActions/DeleteParticipantButton.jsx`.
 *
 * DISCLOSED DEVIATION: the old app's bespoke `AlertDialog` (custom title
 * icon + hand-rolled dialog) is replaced with the shared `DeleteEntityModal`
 * (`@/shared/ui/DeleteEntityModal`) — the same confirmation-dialog
 * component `features/agents/ui/DeleteApplicationButton.tsx` and
 * `features/toolkits/ui/DeleteToolkitButton.tsx` already use for
 * destructive actions in this rewrite. Unlike those two callers, this
 * button never performs its own delete request — `onDelete` stays a
 * caller-supplied callback exactly like the old app's `onConfirmAlert →
 * onDelete(participant)` — so `shouldRequestInputName` is intentionally
 * left off: the old app's own dialog here never required typing the name
 * either (that stricter "type to confirm" behaviour is
 * `DeleteApplicationButton`'s own convention for an irreversible entity
 * delete, not this lighter "remove from conversation" action).
 *
 * FIXED regression: the previous version of this file called `onDelete`
 * synchronously from the icon's `onClick`, with no confirmation step at
 * all. The old app's `onClickDelete` only ever opened the dialog
 * (`setOpenAlert(true)`); `onDelete(participant)` fired exclusively from
 * `onConfirmAlert`, i.e. after explicit user confirmation. Restored below.
 */
import { memo, useCallback, useState } from 'react';

import type { ReactNode } from 'react';

import { IconButton, Tooltip } from '@mui/material';

import DeleteIcon from '@mui/icons-material/Delete';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { ChatParticipantType } from '../../model/constants';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface DeleteParticipantButtonProps {
  participant: Record<string, unknown>;
  onDelete?: (participant: Record<string, unknown>) => void;
  disabled?: boolean;
  /** Full override of the confirmation body — mirrors the old app's `warningMessage` prop. */
  warningMessage?: ReactNode;
}

/**
 * Resolves the human-readable entity-type word used in the confirmation
 * sentence/tooltip. Ported from `DeleteParticipantButton.jsx`'s `entityType`
 * memo.
 */
function resolveEntityType(participant: Record<string, unknown>): string | undefined {
  const entitySettings = participant.entity_settings as Record<string, unknown> | undefined;
  const entityName = participant.entity_name;
  const agentType = (entitySettings?.agent_type as string | undefined) ?? (participant.agent_type as string | undefined);

  if (entityName === ChatParticipantType.Toolkits) return t('chat-participants.entityType.toolkit', 'toolkit');
  if (entityName === ChatParticipantType.Pipelines) return t('chat-participants.entityType.pipeline', 'pipeline');
  if (entityName === ChatParticipantType.Users) return t('chat-participants.entityType.user', 'user');
  if (entityName === ChatParticipantType.Applications) {
    return agentType === ChatParticipantType.Pipelines
      ? t('chat-participants.entityType.pipeline', 'pipeline')
      : t('chat-participants.entityType.agent', 'agent');
  }
  return undefined;
}

/**
 * DeleteParticipantButton component.
 */
const DeleteParticipantButton = memo((props: DeleteParticipantButtonProps): React.ReactElement | null => {
  const { participant, onDelete, disabled, warningMessage } = props;
  const [open, setOpen] = useState(false);

  const entityType = resolveEntityType(participant);
  // `meta.user_name` is in the chain because a user participant stored over
  // the REST path carries only `entity_meta.id` — the server resolves the
  // display name into `meta.user_name`. Without it the confirmation for
  // detaching a person read "…remove Participant user from conversation?",
  // naming nobody at the moment it asks for consent.
  const displayName = String(
    participant.entity_meta?.name ||
      participant.entity_meta?.model_name ||
      participant.meta?.user_name ||
      t('chat-participants.common.participant', 'Participant'),
  );
  const removeLabel = entityType
    ? t('chat-participants.tooltip.removeEntity', 'Remove {{entityType}}', { entityType })
    : t('chat-participants.tooltip.remove', 'Remove participant');

  const openModal = useCallback((event: React.MouseEvent) => {
    event.stopPropagation();
    setOpen(true);
  }, []);

  const closeModal = useCallback(() => setOpen(false), []);

  const confirmDelete = useCallback(() => {
    onDelete?.(participant);
    setOpen(false);
  }, [onDelete, participant]);

  if (!onDelete) return null;

  return (
    <>
      <Tooltip title={removeLabel}>
        <span>
          <IconButton
            size="small"
            aria-label={removeLabel}
            onClick={openModal}
            disabled={disabled}
            sx={{ color: 'text.secondary' }}
          >
            <DeleteIcon fontSize="small" />
          </IconButton>
        </span>
      </Tooltip>
      <DeleteEntityModal
        open={open}
        onClose={closeModal}
        onConfirm={confirmDelete}
        name={displayName}
        copy={{
          title: `${removeLabel}?`,
          textContent: t('chat-participants.deleteModal.textContent', 'Are you sure to remove '),
          confirmText: t('chat-participants.deleteModal.confirm', 'Remove'),
        }}
        content={
          warningMessage !== undefined
            ? { custom: warningMessage }
            : {
                inline: entityType
                  ? ` ${entityType} ${t('chat-participants.deleteModal.fromConversation', 'from conversation?')}`
                  : ` ${t('chat-participants.deleteModal.fromTheConversation', 'from the conversation?')}`,
              }
        }
      />
    </>
  );
});

DeleteParticipantButton.displayName = 'DeleteParticipantButton';

export default DeleteParticipantButton;
