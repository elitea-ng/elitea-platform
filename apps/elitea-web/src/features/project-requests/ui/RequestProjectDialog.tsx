/**
 * RequestProjectDialog — the self-service "request a project" flow (#871).
 *
 * ## The wire
 *
 * `POST /admin/moderation_status/project_request` (internal/api/v2/
 * moderation/project_requests.go) — a NEW route, but the SAME table and the
 * SAME shape as the App Catalogue's "Request Access" button and
 * `RequestModelConnection.tsx`'s "Request a model connection" dialog: one
 * row in `centry.moderation_state`, always `pending`, always authored by
 * the caller. What is different from those two: approving THIS issue type
 * (`"Project Request"`) actually creates the project, with the requester as
 * its admin — see project_requests.go's file header. Rejecting or leaving
 * it pending changes nothing, exactly like the other two.
 *
 * ## Why this dialog also shows past requests
 *
 * "The requester sees status" is part of the issue's own acceptance. There
 * is no separate "my requests" screen anywhere in this app (App Requests is
 * the OPERATOR's queue, gated on `admin.moderation`, which most members do
 * not hold) — so the read lives in the same place the write does: open the
 * dialog, see what you have already asked for, ask for another if you like.
 *
 * ## Why this is its own feature slice, not folded into `widgets/sidebar`
 *
 * `ProjectSwitcher.tsx` renders the trigger row this dialog opens from, but
 * owns no query/mutation of its own (every prop is a callback its parent
 * supplies) — adding React Query state there would be the one exception to
 * that shape. A feature slice keeps the mutation, the dialog and its own
 * toast self-contained, the same shape `RequestModelConnection.tsx` uses for
 * an equivalent self-contained "file a moderation request" affordance.
 */
import { useCallback, useState } from 'react';
import type { ChangeEvent } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';
import type { ChipProps } from '@mui/material/Chip';
import type { Theme } from '@mui/material/styles';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  createProjectRequest,
  getListMyProjectRequestsQueryKey,
  listMyProjectRequests,
} from '@/shared/api/generated/admin/admin';
import type { ModerationRequestRow } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

export interface RequestProjectDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
}

const STATUS_COLOUR: Record<ModerationRequestRow['status'], ChipProps['color']> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'error',
};

function statusLabel(status: ModerationRequestRow['status']): string {
  switch (status) {
    case 'pending':
      return t('features.projectRequests.status.pending', 'Pending');
    case 'approved':
      return t('features.projectRequests.status.approved', 'Approved');
    case 'rejected':
      return t('features.projectRequests.status.rejected', 'Rejected');
  }
}

const contentSx = { display: 'flex', flexDirection: 'column' as const, gap: '1.25rem' };
const fieldWrapperSx = { display: 'flex', flexDirection: 'column' as const, gap: '0.5rem' };
const historySx = { display: 'flex', flexDirection: 'column' as const, gap: '0.5rem', maxHeight: '12rem', overflowY: 'auto' as const };
const historyRowSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'flex-start',
  justifyContent: 'space-between',
  gap: '0.5rem',
  padding: '0.5rem 0.75rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `1px solid ${theme.vars.palette.border.lines}`,
});
function secondaryTextSx(theme: Theme) {
  return { color: theme.vars.palette.text.secondary };
}

/** The form half — split out so the dialog host stays under the §3.5 complexity budget. */
function RequestProjectForm({
  onSubmit,
  isSubmitting,
}: {
  onSubmit: (name: string, justification: string) => void;
  isSubmitting: boolean;
}) {
  const [name, setName] = useState('');
  const [justification, setJustification] = useState('');
  const [nameError, setNameError] = useState('');
  const [justificationError, setJustificationError] = useState('');

  const handleNameChange = useCallback((event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setName(event.target.value);
    setNameError('');
  }, []);
  const handleJustificationChange = useCallback((event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setJustification(event.target.value);
    setJustificationError('');
  }, []);

  const submit = useCallback(() => {
    const trimmedName = name.trim();
    const trimmedJustification = justification.trim();
    setNameError(trimmedName ? '' : t('features.projectRequests.form.nameRequired', 'Name the project'));
    setJustificationError(
      trimmedJustification ? '' : t('features.projectRequests.form.justificationRequired', 'Say why you need it'),
    );
    if (!trimmedName || !trimmedJustification) return;
    onSubmit(trimmedName, trimmedJustification);
    setName('');
    setJustification('');
  }, [name, justification, onSubmit]);

  return (
    <Box sx={fieldWrapperSx}>
      <InputBase
        label={t('features.projectRequests.form.nameLabel', 'Project name *')}
        placeholder={t('features.projectRequests.form.namePlaceholder', 'For example: Marketing Automation')}
        value={name}
        onChange={handleNameChange}
        error={nameError !== ''}
        helperText={nameError || undefined}
        data-testid="request-project-name"
      />
      <InputBase
        label={t('features.projectRequests.form.justificationLabel', 'Why do you need it? *')}
        placeholder={t('features.projectRequests.form.justificationPlaceholder', "Describe what this project is for")}
        expand={{ minRows: 2, maxRows: 4 }}
        value={justification}
        onChange={handleJustificationChange}
        error={justificationError !== ''}
        helperText={justificationError || undefined}
        data-testid="request-project-justification"
      />
      <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
        <BaseBtn
          variant="contained"
          color="primary"
          onClick={submit}
          disabled={isSubmitting}
          data-testid="request-project-submit"
        >
          {t('features.projectRequests.form.submit', 'Submit request')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

/** The history half — the requester's own past requests, newest first. */
function RequestProjectHistory({ rows }: { rows: readonly ModerationRequestRow[] }) {
  if (rows.length === 0) {
    return (
      <Typography variant="bodySmall" sx={secondaryTextSx}>
        {t('features.projectRequests.history.empty', "You haven't requested a project yet.")}
      </Typography>
    );
  }
  return (
    <Box sx={historySx}>
      {rows.map((row) => (
        <Box key={row.id} sx={historyRowSx} data-testid={`request-project-history-row-${row.id}`}>
          <Box sx={{ minWidth: 0 }}>
            <Typography variant="bodyMedium" sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {row.entity_id}
            </Typography>
            {row.status === 'rejected' && row.rejection_comment && (
              <Typography variant="bodySmall" color="error.main">
                {t('features.projectRequests.history.reasonPrefix', 'Reason:')} {row.rejection_comment}
              </Typography>
            )}
            {row.status === 'approved' && row.created_project_id !== undefined && (
              <Typography variant="bodySmall" color="success.main">
                {t('features.projectRequests.history.projectId', 'Project #{{id}} is ready', { id: row.created_project_id })}
              </Typography>
            )}
          </Box>
          <Chip size="small" color={STATUS_COLOUR[row.status]} label={statusLabel(row.status)} />
        </Box>
      ))}
    </Box>
  );
}

export function RequestProjectDialog({ open, onClose }: RequestProjectDialogProps) {
  const queryClient = useQueryClient();
  const [toast, setToast] = useState('');

  const historyQuery = useQuery({
    queryKey: getListMyProjectRequestsQueryKey(),
    queryFn: () => listMyProjectRequests(),
    enabled: open,
  });
  const rows = (historyQuery.data?.data as { rows?: ModerationRequestRow[] } | undefined)?.rows ?? [];

  const submitMutation = useMutation({
    mutationFn: (params: { name: string; description: string }) => createProjectRequest(params),
    onSuccess: () => {
      setToast(t('features.projectRequests.toast.submitted', 'Request submitted. An operator will review it.'));
      void queryClient.invalidateQueries({ queryKey: getListMyProjectRequestsQueryKey() });
    },
    onError: () => {
      setToast(t('features.projectRequests.toast.failed', 'Failed to submit the request.'));
    },
  });

  const handleSubmit = useCallback(
    (name: string, justification: string) => {
      submitMutation.mutate({ name, description: justification });
    },
    [submitMutation],
  );

  return (
    <>
      <BaseModal
        open={open}
        onClose={onClose}
        title={t('features.projectRequests.title', 'Request a project')}
        content={
          <Box sx={contentSx}>
            <Typography variant="bodyMedium" sx={secondaryTextSx}>
              {t(
                'features.projectRequests.description',
                'Ask an operator to create a new project for you. If approved, you become its admin.',
              )}
            </Typography>
            <RequestProjectForm onSubmit={handleSubmit} isSubmitting={submitMutation.isPending} />
            <Box sx={fieldWrapperSx}>
              <Typography variant="labelMedium">
                {t('features.projectRequests.history.title', 'Your requests')}
              </Typography>
              <RequestProjectHistory rows={rows} />
            </Box>
          </Box>
        }
        actions={{ cancelText: t('features.projectRequests.close', 'Close') }}
        data-testid="request-project-dialog"
      />
      <Snackbar
        open={toast !== ''}
        autoHideDuration={4000}
        onClose={() => setToast('')}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        {toast !== '' ? (
          <Alert severity={submitMutation.isError ? 'error' : 'success'} onClose={() => setToast('')} variant="filled">
            {toast}
          </Alert>
        ) : undefined}
      </Snackbar>
    </>
  );
}
