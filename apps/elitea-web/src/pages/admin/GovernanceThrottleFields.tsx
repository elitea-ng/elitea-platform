/**
 * The throttle fields of a rate limit in the LLM Governance dialog (ADR-0041
 * decision 4). Split out of `GovernanceDialogFields.tsx` to keep that file
 * short.
 */
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import type { draftFieldErrors } from './governanceIdLists';
import type { GovernanceDraft } from './useGatewayGovernancePage';

/** The throttle dimensions of a rate limit (ADR-0041 decision 4). */
export function ThrottleScopeFields({
  draft,
  errors,
  onChange,
}: {
  readonly draft: GovernanceDraft;
  readonly errors: ReturnType<typeof draftFieldErrors>;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}) {
  return (
    <>
      <TextField
        label={t('pages.admin.governance.field.agentIds', 'Throttled agents')}
        error={errors.scopeAgentIds !== undefined}
        helperText={
          errors.scopeAgentIds ??
          t(
            'pages.admin.governance.field.agentIdsHelp',
            'Comma-separated agent ids of the ONE project in Projects. Each agent gets its own counter. At most 100.',
          )
        }
        value={draft.scopeAgentIds}
        onChange={(event) => onChange({ scopeAgentIds: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-scope-agents' } }}
      />
      <TextField
        label={t('pages.admin.governance.field.userIds', 'Throttled users')}
        error={errors.scopeUserIds !== undefined}
        helperText={
          errors.scopeUserIds ??
          t(
            'pages.admin.governance.field.userIdsHelp',
            'Comma-separated user ids. Each user gets its own counter across the selected projects. At most 100.',
          )
        }
        value={draft.scopeUserIds}
        onChange={(event) => onChange({ scopeUserIds: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-scope-users' } }}
      />
    </>
  );
}
