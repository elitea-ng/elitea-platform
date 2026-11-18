/**
 * "Use in a chat" and "Add to a conversation" (ADR-0051 amendment 1, A1.4): pin one skill to a conversation the person
 * picks, or to a new chat.
 *
 * Both are a conversation pin (ADR-0050 amendment 1), so the skill reaches whichever model or agent answers. They
 * differ in what is pinned. "Use in a chat" pins as is: a project skill by id, a Marketplace listing version by
 * reference, nothing forked. "Add to a conversation" forks a listing version into the project first and pins the
 * project's copy. The caller says which by the `resolvePin` it passes, so this dialog only picks the conversation and
 * says why a pin was refused (the five-skill bound, a skill the caller cannot read).
 */
import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import { conversationSkillKeys } from '@/entities/conversation-skill';
import { pinConversationSkill, useListConversations } from '@/shared/api/generated/chat/chat';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { PinConversationSkillRequest } from '@/shared/api/generated/model';
import { unwrapList } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { publishErrorMessage } from '../api/skillPublishApi';

/** The value of the "new chat" choice; a conversation's own value is its id. */
const NEW_CHAT = 'new';

export interface SkillToChatDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly skillName: string;
  readonly title: string;
  readonly confirmText: string;
  /** What to pin; "Add to a conversation" forks here first. Rejects with the reason when it cannot. */
  readonly resolvePin: () => Promise<PinConversationSkillRequest>;
  /** Offers "New chat" and starts it with the resolved pin; omitted where the action is for an existing conversation only. */
  readonly onNewChat?: ((pin: PinConversationSkillRequest) => void) | undefined;
  readonly onClose: () => void;
}

interface ConversationRow {
  readonly id?: string | number;
  readonly name?: string;
}

/** Why a pin was refused, in the person's words. */
function pinRefusalText(error: unknown): string {
  const status = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure.status : undefined;
  if (status === 409) return t('skills.toChat.limit', 'A conversation holds at most five skills. Remove one to add another.');
  if (status === 404) return t('skills.toChat.unavailable', 'That skill is not available to you.');
  if (status === 403) return t('skills.toChat.forbidden', 'You cannot change the skills of that conversation.');
  return publishErrorMessage(error, t('skills.toChat.failed', 'The skill could not be added. Try again.'));
}

/** The caller's own conversations, newest first as the server lists them, with a name to read. */
function useConversationChoices(projectId: string | undefined, open: boolean): readonly { readonly id: string; readonly name: string }[] {
  const query = useListConversations(projectId ?? '', { mine: true, limit: 50, offset: 0 }, { query: { enabled: open && projectId !== undefined } });
  return useMemo(
    () =>
      unwrapList<ConversationRow>(query.data, 'skills.toChat.conversations').flatMap((row) =>
        row.id === undefined ? [] : [{ id: String(row.id), name: row.name === undefined || row.name === '' ? t('skills.toChat.untitled', 'Untitled chat') : row.name }],
      ),
    [query.data],
  );
}

export function SkillToChatDialog({
  open,
  projectId,
  skillName,
  title,
  confirmText,
  resolvePin,
  onNewChat,
  onClose,
}: SkillToChatDialogProps): ReactNode {
  const offersNew = onNewChat !== undefined;
  const [choice, setChoice] = useState('');
  const [message, setMessage] = useState<string>();
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const conversations = useConversationChoices(projectId, open);

  // The dialog stays mounted and toggles `open`: a reopened one must not show the last attempt's pick or error.
  useEffect(() => {
    setChoice(offersNew ? NEW_CHAT : '');
    setMessage(undefined);
    setBusy(false);
  }, [open, skillName, offersNew]);

  const confirm = (): void => {
    if (choice === '' || projectId === undefined) return;
    setMessage(undefined);
    setBusy(true);
    void resolvePin()
      .then(async (pin) => {
        if (choice === NEW_CHAT) {
          onNewChat?.(pin);
          onClose();
          return;
        }
        await pinConversationSkill(projectId, choice, pin);
        await queryClient.invalidateQueries({ queryKey: conversationSkillKeys.all(projectId) });
        onClose();
        await navigate({ to: '/chat/$conversationId', params: { conversationId: choice } });
      })
      .catch((error: unknown) => setMessage(pinRefusalText(error)))
      .finally(() => setBusy(false));
  };

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={confirm}
      title={title}
      data-testid="skill-to-chat-dialog"
      actions={{ confirmText, confirming: busy || choice === '' }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 420 }}>
          <Typography variant="bodySmall">
            {t('skills.toChat.explain', 'Pin “{{name}}” to a conversation. It applies to every message there, whichever model or agent answers.', { name: skillName })}
          </Typography>
          <TextField
            select
            size="small"
            label={t('skills.toChat.conversation', 'Conversation')}
            value={choice}
            onChange={(event) => setChoice(event.target.value)}
            slotProps={{ htmlInput: { 'aria-label': t('skills.toChat.conversation', 'Conversation'), 'data-testid': 'skill-to-chat-choice' } }}
          >
            {offersNew && <MenuItem value={NEW_CHAT}>{t('skills.toChat.newChat', 'New chat')}</MenuItem>}
            {conversations.map((conversation) => (
              <MenuItem key={conversation.id} value={conversation.id}>
                {conversation.name}
              </MenuItem>
            ))}
          </TextField>
          {busy && <LinearProgress />}
          {message !== undefined && (
            <Alert severity="error" data-testid="skill-to-chat-error">
              {message}
            </Alert>
          )}
        </Box>
      }
    />
  );
}
