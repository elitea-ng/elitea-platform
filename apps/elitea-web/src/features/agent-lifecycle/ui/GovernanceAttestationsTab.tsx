import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Link from '@mui/material/Link';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentAttestation } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useAgentAttestations, useRevokeAttestation } from '../api/attestationsApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { AttestationAddForm } from './AttestationAddForm';
import { attestationKindLabel, formatWhen } from './governanceLabels';

/**
 * The Attestations tab: reviews done outside the platform (architecture,
 * legal, model risk, data owner, data accuracy, custom) recorded against this
 * agent. A tier's policy can make some kinds mandatory before approval; a
 * missing, expired or revoked one is what `attestation_missing` names.
 * Revoking stamps the row, it never deletes it.
 */
export interface GovernanceAttestationsTabProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

export function GovernanceAttestationsTab({ projectId, applicationId }: GovernanceAttestationsTabProps): ReactNode {
  const list = useAgentAttestations(projectId, applicationId);
  const revoke = useRevokeAttestation(projectId);
  const [adding, setAdding] = useState(false);
  if (projectId === undefined || applicationId === undefined) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.governance.notSaved', 'Save the agent first; its registry record starts with it.')}
      </Typography>
    );
  }
  if (list.isPending) return <CircularProgress size="1.5rem" />;
  if (list.isError) return <Alert severity="error">{lifecycleErrorMessage(list.error)}</Alert>;
  return (
    <Box
      sx={stackSx}
      data-testid="governance-attestations"
    >
      {adding ? (
        <AttestationAddForm
          projectId={projectId}
          applicationId={applicationId}
          onDone={() => setAdding(false)}
        />
      ) : (
        <Box>
          <Button
            variant="outlined"
            size="small"
            onClick={() => setAdding(true)}
            data-testid="attestation-add"
          >
            {t('features.agentLifecycle.attestation.add', 'Add attestation')}
          </Button>
        </Box>
      )}
      {revoke.error !== null && <Alert severity="error">{lifecycleErrorMessage(revoke.error)}</Alert>}
      {list.data.rows.length === 0 ? (
        <Typography variant="bodyMedium">
          {t('features.agentLifecycle.attestation.empty', 'No attestations are recorded for this agent.')}
        </Typography>
      ) : (
        <Table
          size="small"
          aria-label={t('features.agentLifecycle.governance.tabAttestations', 'Attestations')}
        >
          <TableHead>
            <TableRow>
              <TableCell>{t('features.agentLifecycle.attestation.kind', 'Kind')}</TableCell>
              <TableCell>{t('features.agentLifecycle.attestation.attester', 'Attested by')}</TableCell>
              <TableCell>{t('features.agentLifecycle.attestation.validUntil', 'Valid until')}</TableCell>
              <TableCell>{t('features.agentLifecycle.attestation.status', 'Status')}</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {list.data.rows.map((row) => (
              <AttestationRow
                key={row.id}
                row={row}
                revoking={revoke.isPending}
                onRevoke={() => revoke.mutate({ id: row.id, reason: '' })}
              />
            ))}
          </TableBody>
        </Table>
      )}
    </Box>
  );
}

function AttestationRow({
  row,
  revoking,
  onRevoke,
}: {
  readonly row: AgentAttestation;
  readonly revoking: boolean;
  readonly onRevoke: () => void;
}): ReactNode {
  return (
    <TableRow data-testid={`attestation-row-${row.id}`}>
      <TableCell>
        {row.kind === 'custom' ? row.custom_kind : attestationKindLabel(row.kind)}
        {row.evidence_url !== '' && (
          <>
            {' · '}
            <Link
              href={row.evidence_url}
              target="_blank"
              rel="noopener noreferrer"
            >
              {t('features.agentLifecycle.attestation.evidence', 'evidence')}
            </Link>
          </>
        )}
        {row.note !== '' && (
          <Typography
            variant="bodySmall"
            color="text.secondary"
            component="div"
          >
            {row.note}
          </Typography>
        )}
      </TableCell>
      <TableCell>{row.attester}</TableCell>
      <TableCell>{formatWhen(row.valid_until)}</TableCell>
      <TableCell>{attestationStatus(row)}</TableCell>
      <TableCell align="right">
        {row.revoked_at === null && (
          <Button
            size="small"
            color="error"
            disabled={revoking}
            onClick={onRevoke}
            data-testid={`attestation-revoke-${row.id}`}
          >
            {t('features.agentLifecycle.attestation.revoke', 'Revoke')}
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}

function attestationStatus(row: AgentAttestation): string {
  if (row.revoked_at !== null) {
    return t('features.agentLifecycle.attestation.revoked', 'Revoked {{when}}', { when: formatWhen(row.revoked_at) });
  }
  return row.valid
    ? t('features.agentLifecycle.attestation.valid', 'Valid')
    : t('features.agentLifecycle.attestation.expired', 'Expired');
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
