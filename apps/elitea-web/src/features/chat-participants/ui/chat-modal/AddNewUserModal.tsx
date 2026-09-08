// @ts-nocheck
/**
 * AddNewUserModal — modal for adding users to a conversation.
 *
 * Ported from `[fsd]/features/chat/ui/chat-modal/AddNewUserModal.jsx`.
 *
 * Cross-cutting: uses `entities/participant`'s `useParticipants` and
 * `useFilteredEntityItems` for candidate browsing (issue #33, item 8).
 */
import { memo, useCallback, useMemo, useState } from 'react';

import {
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  TextField,
  Typography,
} from '@mui/material';
import CheckCircleRounded from '@mui/icons-material/CheckCircleRounded';

import { useRouteContext } from '@tanstack/react-router';

import type { Participant } from '@/entities/participant';
import { useParticipants } from '@/entities/participant';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';

import { t } from '@/shared/i18n';

/**
 * "Currently selected project id" — local duplicate of `features/chat-input/
 * api/useSelectedProjectId.ts` (and every other Wave-2 feature slice's own
 * copy), NOT an import of it: `no-sideways-features` forbids one
 * `features/*` slice importing another, and no `shared`/`entities`
 * primitive for "the selected project id" exists yet.
 *
 * `@/shared/config` (this file's previous import source) only exports
 * `getConfig`/`Config`/`MissingEnvPage` — it has never exported a
 * `useSelectedProjectId` hook, so that import silently failed to resolve at
 * module load (a real, pre-existing defect independent of the confirmed C5
 * findings this unit fixes).
 */
function useSelectedProjectId(): string | undefined {
  const context: unknown = useRouteContext({ strict: false });
  if (typeof context !== 'object' || context === null) return undefined;
  const auth = (context as { readonly auth?: { readonly getSelectedProjectId?: () => string | undefined } }).auth;
  return auth?.getSelectedProjectId?.();
}

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

/** Not exported: the barrel re-exports the component only, and no other slice names this shape. */
interface AddNewUserModalProps {
  open: boolean;
  onClose: () => void;
  onAddUsers: (users: Record<string, unknown>[]) => void;
  /**
   * The conversation's CURRENT participants — already-added users are
   * excluded from the candidate list (baseline: `AddNewUserModal.jsx:
   * 14-32`'s `excludedUserIds`, built from `participants.filter(item =>
   * item.entity_name === ChatParticipantType.Users)`).
   *
   * Accepts EITHER shape, and reads both below. The domain rows this slice
   * normalises are camelCase (`entityName`/`entityMeta`); the conversation
   * payload the chat page holds is the raw snake_case wire row
   * (`entity_name`/`entity_meta`). A filter that knew only the first is the
   * exact mismatch that made the rail drop every user row before it was
   * fixed — here it would have re-offered a person who is already in the
   * conversation, and the second add would then 500 or duplicate.
   */
  participants?: readonly (Participant | Readonly<Record<string, unknown>>)[];
  /**
   * The current participant type filter (applications, pipelines, etc.).
   * Defaults to `['user']` — this modal's own baseline
   * (`AddNewUserModal.jsx`) is user-specific by construction (its
   * `DialogTitle` says "Add Participants" but it only ever lists users), and
   * `useParticipants`'s own doc comment documents an *omitted* `types` as
   * "applications + pipelines only, never users" — the opposite of this
   * component's evident purpose.
   */
  types?: string[];
  /** The project filter. */
  projectFilter?: 'all' | 'public' | 'teamProject';
}

/**
 * Current-user identity, resolved the same way every other Wave-2 unit's own
 * duplicated equivalent does per `entities/participant`'s own doc comment
 * (`no-upward-from-entities` forces every caller to resolve this itself) —
 * `useGetCurrentAuthor()` (`shared/api/generated/social/social.ts`), matching
 * `features/chat-conversation-list/ui/folders/FolderItem.tsx`'s own
 * `useCurrentUserId` and `pages/chat/useChatPageData.ts`'s own
 * `currentAuthorOf`. The cast mirrors both of those call sites' own
 * established precedent (`eliteaFetch` throws on a non-2xx response rather
 * than resolving with the error variant, so the declared `N401Response`
 * union arm is unreachable here).
 */
function useCurrentAuthorProfile(): SocialAuthorProfile | undefined {
  const query = useGetCurrentAuthor();
  return query.data?.data as SocialAuthorProfile | undefined;
}

/**
 * AddNewUserModal component — modal for browsing and adding users/toolkits/etc.
 * to a conversation.
 */
const AddNewUserModal = memo((props: AddNewUserModalProps): React.ReactElement => {
  const { open, onClose, onAddUsers, participants, types = ['user'], projectFilter } = props;

  const projectId = useSelectedProjectId();
  const currentAuthor = useCurrentAuthorProfile();
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedUsers, setSelectedUsers] = useState<Record<string, unknown>[]>([]);

  // Use the entity hook for candidate aggregation
  const { participants: candidates, isLoading } = useParticipants({
    projectId,
    publicProjectId: import.meta.env?.VITE_PUBLIC_PROJECT_ID || '',
    privateProjectId: currentAuthor?.personal_project_id,
    currentUserId: currentAuthor?.id,
    canListPublicAgents: true,
    query: searchQuery,
    types: types as unknown as Parameters<typeof useParticipants>[0]['types'],
    projectFilter,
    enabled: open,
  });

  // Already-added users are excluded from the candidate list — baseline:
  // `AddNewUserModal.jsx:14-32`'s `excludedUserIds`.
  const excludedUserIds = useMemo(() => {
    const ids = (participants ?? [])
      .filter((item) => (item.entityName ?? item.entity_name) === 'user')
      .map((item) => (item.entityMeta ?? item.entity_meta)?.id)
      .filter((id) => id !== undefined)
      .map((id) => String(id));
    return new Set(ids);
  }, [participants]);

  const visibleCandidates = useMemo(
    () =>
      candidates.filter((candidate) => {
        if (candidate.participantType !== 'user') return true;
        const id = candidate.data?.id;
        if (typeof id !== 'string' && typeof id !== 'number') return true;
        return !excludedUserIds.has(String(id));
      }),
    [candidates, excludedUserIds],
  );

  const handleSelectUser = useCallback((user: Record<string, unknown>) => {
    setSelectedUsers((prev) => {
      const exists = prev.some((u) => u.id === user.id);
      if (exists) return prev.filter((u) => u.id !== user.id);
      return [...prev, user];
    });
  }, []);

  const handleAddSelected = useCallback(() => {
    onAddUsers(selectedUsers);
    setSelectedUsers([]);
    setSearchQuery('');
    onClose();
  }, [selectedUsers, onAddUsers, onClose]);

  // Baseline: `AddNewUserModal.jsx:82-90`'s `handleKeyDown` — Enter submits
  // the current selection immediately, matching the "Add Selected" button.
  const handleKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLDivElement>) => {
      if (event.key !== 'Enter' || selectedUsers.length === 0) return;
      event.preventDefault();
      handleAddSelected();
    },
    [selectedUsers, handleAddSelected],
  );

  return (
    <Dialog open={open} onClose={onClose} onKeyDown={handleKeyDown} maxWidth="sm" fullWidth data-testid="add-participants-dialog">
      <DialogTitle>{t('chat-participants.modal.title', 'Add Participants')}</DialogTitle>
      <DialogContent>
        <Box sx={{ mb: 2 }}>
          <TextField
            fullWidth
            placeholder={t('chat-participants.modal.search', 'Search participants...')}
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            size="small"
            autoComplete="off"
            slotProps={{ htmlInput: { 'data-testid': 'add-participants-search' } }}
          />
        </Box>
        <Box sx={{ maxHeight: 300, overflow: 'auto' }}>
          {isLoading && <Typography variant="body2" color="text.disabled">{t('chat-participants.modal.loading', 'Loading...')}</Typography>}
          {!isLoading && visibleCandidates.length === 0 && (
            <Typography variant="body2" color="text.disabled">{t('chat-participants.modal.noResults', 'No participants found')}</Typography>
          )}
          {visibleCandidates.map((candidate) => {
            // `candidate` is a `ParticipantEntityItem` (`label`/`data`, not
            // `entity_meta`/`id`) — `candidate.data` is the raw wire row
            // (e.g. `UserRecord`), which is what `onAddUsers` should receive.
            const candidateId = candidate.data?.id;
            const rowKey = typeof candidateId === 'string' || typeof candidateId === 'number' ? String(candidateId) : candidate.label;
            const name = candidate.label || t('chat-participants.common.unknown', 'Unknown');
            const isSelected = selectedUsers.some((u) => u.id === candidateId);
            return (
              <Box
                key={rowKey}
                data-testid={`add-participant-option-${rowKey}`}
                onClick={() => handleSelectUser(candidate.data)}
                sx={{
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  p: 1,
                  borderRadius: 'var(--el-shape-radiusSm, 4px)',
                  cursor: 'pointer',
                  backgroundColor: isSelected ? 'action.selected' : 'transparent',
                  '&:hover': { backgroundColor: 'action.hover' },
                }}
              >
                <Typography variant="body2">{name}</Typography>
                {isSelected && <CheckCircleRounded sx={{ color: 'primary.main' }} />}
              </Box>
            );
          })}
        </Box>
        {selectedUsers.length > 0 && (
          <Box sx={{ mt: 2, p: 1, backgroundColor: 'action.selected', borderRadius: 'var(--el-shape-radiusSm, 4px)' }}>
            {/* eslint-disable-next-line i18next/no-literal-string -- placeholder for i18n interpolation */}
            <Typography variant="body2" sx={{ mb: 0.5 }}>{`${t('chat-participants.modal.selected', 'Selected ({count}):').replace('{count}', '')} ${selectedUsers.length}`}</Typography>
            {selectedUsers.map((u) => (
              <Typography key={u.id} variant="bodySmall" color="text.secondary">
                • {u.name || u.entity_meta?.name || t('chat-participants.common.unknown', 'Unknown')}
              </Typography>
            ))}
          </Box>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('chat-participants.modal.cancel', 'Cancel')}</Button>
        <Button
          onClick={handleAddSelected}
          disabled={selectedUsers.length === 0}
          variant="contained"
          data-testid="add-participants-confirm"
        >
          {t('chat-participants.modal.addSelected', 'Add Selected')}
        </Button>
      </DialogActions>
    </Dialog>
  );
});

AddNewUserModal.displayName = 'AddNewUserModal';

export default AddNewUserModal;
