/**
 * The way past a run cap (ADR-0041 decision 3).
 *
 * A run that reached a cap an administrator set ends with the public code
 * CAP_REACHED; the response already shows the cap's message. A caller with
 * `safety.override_cap` in the project (the project admin) also gets one
 * action: let the conversation's next run pass that cap once. The server
 * doubles that one cap for the next agent run of the conversation and
 * records a security audit event. The button is presentation, the route is
 * the authorisation.
 */
import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useMutation } from '@tanstack/react-query';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { createRunCapOverride } from '@/shared/api/generated/settings/settings';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/** @public The public failure code of a run stopped at a run cap. */
export const CAP_REACHED_CODE = 'CAP_REACHED';

/** @public The permission the override route is gated on (shared 0212). */
export const OVERRIDE_CAP_PERMISSION = 'safety.override_cap';

/** @public The words for a refused override, from the server's code. */
export function describeCapOverrideError(error: unknown): string {
  const failure = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure : undefined;
  const body = failure?.body;
  const code = typeof body === 'object' && body !== null ? (body as { code?: unknown }).code : undefined;
  switch (code) {
    case 'already_granted':
      return t('chatMessages.capOverride.alreadyGranted', 'This response was already allowed past its cap.');
    case 'open_grant':
      return t('chatMessages.capOverride.openGrant', 'This conversation already has an unused allowance. Send a message to use it.');
    case 'tier_caps_not_saved':
      return t('chatMessages.capOverride.tierCapsNotSaved', 'This cap cannot be raised until an administrator saves the run caps.');
    default:
      return failure?.status === 403
        ? t('chatMessages.capOverride.forbidden', 'You do not have permission to override run caps.')
        : t('chatMessages.capOverride.failed', 'Could not allow the next run past the cap. Try again.');
  }
}

function useCanOverrideCap(projectId: string): boolean {
  const query = usePermissionList(projectId);
  return useMemo(() => {
    const list = unwrapBody(query.data) as Permission[] | undefined;
    return Array.isArray(list) && list.some((entry) => entry.enabled && entry.name === OVERRIDE_CAP_PERMISSION);
  }, [query.data]);
}

export interface CapOverrideActionProps {
  readonly projectId: string | undefined;
  readonly messageId: string;
  readonly code: string | undefined;
}

/** Renders nothing for any other failure, so other answers query nothing. */
export function CapOverrideAction({ projectId, messageId, code }: CapOverrideActionProps): ReactNode {
  if (code !== CAP_REACHED_CODE || projectId === undefined || projectId === '') return null;
  return <CapOverrideButton projectId={projectId} messageId={messageId} />;
}

function CapOverrideButton({ projectId, messageId }: { readonly projectId: string; readonly messageId: string }): ReactNode {
  const canOverride = useCanOverrideCap(projectId);
  const [error, setError] = useState<string | undefined>(undefined);
  const grant = useMutation({
    mutationFn: async () => unwrapBody(await createRunCapOverride(projectId, { message_id: messageId })),
    onError: (failure) => setError(describeCapOverrideError(failure)),
  });
  if (!canOverride) return null;
  if (grant.isSuccess) {
    return (
      <Typography variant="bodySmall" data-testid="cap-override-granted" sx={{ display: 'block', mt: 0.5, color: 'text.secondary' }}>
        {t('chatMessages.capOverride.granted', 'The next run of this conversation can pass this cap once. Send a message to continue.')}
      </Typography>
    );
  }
  return (
    <Box sx={{ mt: 0.5 }}>
      <Button
        size="small"
        variant="outlined"
        data-testid="cap-override-button"
        disabled={grant.isPending}
        onClick={() => {
          setError(undefined);
          grant.mutate();
        }}
      >
        {t('chatMessages.capOverride.action', 'Allow the next run past this cap')}
      </Button>
      {error !== undefined && (
        <Typography variant="bodySmall" role="alert" sx={{ display: 'block', mt: 0.5, color: 'error.main' }}>
          {error}
        </Typography>
      )}
    </Box>
  );
}
