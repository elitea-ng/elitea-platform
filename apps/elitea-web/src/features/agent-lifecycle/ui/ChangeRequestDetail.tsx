import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Divider from '@mui/material/Divider';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentChangeRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useChangeRequest } from '../api/governanceApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { ChangeRequestActions } from './ChangeRequestActions';
import { ChangeRequestGate } from './ChangeRequestGate';
import { changeRequestKindLabel, changeRequestStateLabel, formatUser, formatWhen } from './governanceLabels';

/**
 * One change request: what it proposes, its release gate, who approved it and
 * when, and the actions its state allows. The single read is the one that
 * carries the gate state, so this reads it rather than the list row.
 */
export interface ChangeRequestDetailProps {
  readonly projectId: string;
  readonly applicationId: number;
  readonly changeRequestId: number;
}

export function ChangeRequestDetail({ projectId, applicationId, changeRequestId }: ChangeRequestDetailProps): ReactNode {
  const detail = useChangeRequest(projectId, applicationId, changeRequestId);
  if (detail.isPending) return <CircularProgress size="1.5rem" />;
  if (detail.isError) return <Alert severity="error">{lifecycleErrorMessage(detail.error)}</Alert>;
  const request = detail.data;
  return (
    <Box
      sx={stackSx}
      data-testid="change-request-detail"
    >
      <Divider />
      <Typography variant="labelMedium">
        {t('features.agentLifecycle.changeRequest.heading', 'Change request #{{id}} · {{kind}} · {{state}}', {
          id: request.id,
          kind: changeRequestKindLabel(request.kind),
          state: changeRequestStateLabel(request.state),
        })}
      </Typography>
      <Typography
        variant="bodySmall"
        sx={{ whiteSpace: 'pre-wrap' }}
      >
        {request.summary}
      </Typography>
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {proposalLine(request)}
      </Typography>
      {request.decided_at !== null && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          data-testid="change-request-decision"
        >
          {t('features.agentLifecycle.changeRequest.decided', 'Decided by {{who}} on {{when}}{{reason}}', {
            who: formatUser(request.decided_by),
            when: formatWhen(request.decided_at),
            reason: request.decision_reason === '' ? '' : `: ${request.decision_reason}`,
          })}
        </Typography>
      )}
      <ChangeRequestGate
        projectId={projectId}
        applicationId={applicationId}
        request={request}
      />
      <Approvals request={request} />
      <ChangeRequestActions
        projectId={projectId}
        applicationId={applicationId}
        request={request}
      />
    </Box>
  );
}

function proposalLine(request: AgentChangeRequest): string {
  return t(
    'features.agentLifecycle.changeRequest.proposal',
    'Proposed by {{who}} on {{when}} · version {{version}} · tier {{tier}} · needs {{approvals}} approval(s), {{governance}} from the governance role',
    {
      who: formatUser(request.proposed_by),
      when: formatWhen(request.submitted_at ?? request.created_at),
      version: request.version_name === '' ? '—' : request.version_name,
      tier: request.risk_tier,
      approvals: request.required_approvals,
      governance: request.required_governance_approvals,
    },
  );
}

function Approvals({ request }: { readonly request: AgentChangeRequest }): ReactNode {
  return (
    <Box data-testid="change-request-approvals">
      <Typography variant="labelSmall">
        {t('features.agentLifecycle.changeRequest.approvals', 'Approvals ({{count}} of {{required}})', {
          count: request.approvals.length,
          required: request.required_approvals,
        })}
      </Typography>
      {request.approvals.length === 0 ? (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          component="div"
        >
          {t('features.agentLifecycle.changeRequest.noApprovals', 'No approvals yet.')}
        </Typography>
      ) : (
        <Box
          component="ul"
          sx={listSx}
        >
          {request.approvals.map((approval) => (
            <Typography
              key={`${approval.approver_id}:${approval.created_at}`}
              component="li"
              variant="bodySmall"
            >
              {t('features.agentLifecycle.changeRequest.approvalLine', '{{who}} on {{when}}{{role}}{{note}}', {
                who: formatUser(approval.approver_id),
                when: formatWhen(approval.created_at),
                role: approval.holds_governance_role
                  ? t('features.agentLifecycle.changeRequest.governanceRole', ' (governance role)')
                  : '',
                note: approval.note === '' ? '' : `: ${approval.note}`,
              })}
            </Typography>
          ))}
        </Box>
      )}
    </Box>
  );
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const listSx: SxProps<Theme> = { margin: 0, paddingLeft: '1.25rem' };
