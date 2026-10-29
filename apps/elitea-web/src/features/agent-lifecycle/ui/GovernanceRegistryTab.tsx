import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import type { UseQueryResult } from '@tanstack/react-query';

import type { AgentRegistryRecord, AgentRegistryUpdate, AgentRiskTier } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useUpdateAgentRegistry } from '../api/governanceApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { RISK_TIERS, certificationStateLabel, formatUser, formatWhen, tierLabel } from './governanceLabels';

/**
 * The Registry tab: who owns the agent, its risk tier, what it is for and
 * the data it touches, and where its certification stands.
 *
 * Only the CHANGED fields are sent. The server reads an absent field as
 * unchanged, and raising the tier is open to an owner while lowering it needs
 * the governance role — a save that resent the tier on every edit of the
 * intended use would trip that check for nothing.
 */
export interface GovernanceRegistryTabProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly registry: UseQueryResult<AgentRegistryRecord>;
}

export function GovernanceRegistryTab({ projectId, applicationId, registry }: GovernanceRegistryTabProps): ReactNode {
  if (projectId === undefined || applicationId === undefined) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.governance.notSaved', 'Save the agent first; its registry record starts with it.')}
      </Typography>
    );
  }
  if (registry.isPending) return <CircularProgress size="1.5rem" />;
  if (registry.isError) return <Alert severity="error">{lifecycleErrorMessage(registry.error)}</Alert>;
  return (
    <RegistryForm
      key={registry.data.updated_at}
      projectId={projectId}
      applicationId={applicationId}
      record={registry.data}
    />
  );
}

interface RegistryFormState {
  readonly businessOwner: string;
  readonly technicalOwner: string;
  readonly tier: string;
  readonly intendedUse: string;
  readonly outOfScope: string;
  readonly dataClasses: string;
}

function initialState(record: AgentRegistryRecord): RegistryFormState {
  return {
    businessOwner: record.business_owner_id === null ? '' : String(record.business_owner_id),
    technicalOwner: record.technical_owner_id === null ? '' : String(record.technical_owner_id),
    tier: record.risk_tier,
    intendedUse: record.intended_use,
    outOfScope: record.out_of_scope_use,
    dataClasses: record.data_classes.join(', '),
  };
}

/** An owner field: blank clears the owner (0 on the wire). */
function ownerValue(value: string): number {
  const parsed = Number(value.trim());
  return value.trim() === '' || !Number.isInteger(parsed) || parsed < 0 ? 0 : parsed;
}

function parseDataClasses(value: string): string[] {
  return value
    .split(',')
    .map((item) => item.trim().toLowerCase())
    .filter((item) => item !== '');
}

/** The fields that differ from the stored record, and nothing else. */
function registryChanges(before: RegistryFormState, after: RegistryFormState): AgentRegistryUpdate {
  const update: AgentRegistryUpdate = {};
  if (after.businessOwner !== before.businessOwner) update.business_owner_id = ownerValue(after.businessOwner);
  if (after.technicalOwner !== before.technicalOwner) update.technical_owner_id = ownerValue(after.technicalOwner);
  if (after.tier !== before.tier) update.risk_tier = after.tier as AgentRiskTier;
  if (after.intendedUse !== before.intendedUse) update.intended_use = after.intendedUse;
  if (after.outOfScope !== before.outOfScope) update.out_of_scope_use = after.outOfScope;
  if (after.dataClasses !== before.dataClasses) update.data_classes = parseDataClasses(after.dataClasses);
  return update;
}

function RegistryForm({
  projectId,
  applicationId,
  record,
}: {
  readonly projectId: string;
  readonly applicationId: number;
  readonly record: AgentRegistryRecord;
}): ReactNode {
  const initial = initialState(record);
  const [form, setForm] = useState<RegistryFormState>(initial);
  const save = useUpdateAgentRegistry(projectId, applicationId);
  const changes = registryChanges(initial, form);
  const dirty = Object.keys(changes).length > 0;
  const field = (name: keyof RegistryFormState) => ({
    value: form[name],
    onChange: (event: { target: { value: string } }) => setForm({ ...form, [name]: event.target.value }),
  });

  const fields = {
    businessOwner: field('businessOwner'),
    technicalOwner: field('technicalOwner'),
    tier: field('tier'),
    intendedUse: field('intendedUse'),
    outOfScope: field('outOfScope'),
    dataClasses: field('dataClasses'),
  };
  return (
    <Box
      sx={formSx}
      data-testid="governance-registry"
    >
      <CertificationSummary record={record} />
      <Box sx={rowSx}>
        <TextField
          label={t('features.agentLifecycle.registry.businessOwner', 'Business owner (user id)')}
          {...fields.businessOwner}
          size="small"
          slotProps={{ htmlInput: { inputMode: 'numeric', 'data-testid': 'registry-business-owner' } }}
        />
        <TextField
          label={t('features.agentLifecycle.registry.technicalOwner', 'Technical owner (user id)')}
          {...fields.technicalOwner}
          size="small"
          slotProps={{ htmlInput: { inputMode: 'numeric', 'data-testid': 'registry-technical-owner' } }}
        />
        <TextField
          select
          label={t('features.agentLifecycle.registry.tier', 'Risk tier')}
          {...fields.tier}
          size="small"
          sx={{ minWidth: '10rem' }}
          slotProps={{ htmlInput: { 'data-testid': 'registry-tier' } }}
        >
          {RISK_TIERS.map((tier) => (
            <MenuItem
              key={tier}
              value={tier}
            >
              {tierLabel(tier)}
            </MenuItem>
          ))}
        </TextField>
      </Box>
      <TextField
        label={t('features.agentLifecycle.registry.intendedUse', 'Intended use')}
        {...fields.intendedUse}
        multiline
        minRows={2}
        slotProps={{ htmlInput: { maxLength: 2000 } }}
      />
      <TextField
        label={t('features.agentLifecycle.registry.outOfScope', 'Out of scope')}
        {...fields.outOfScope}
        multiline
        minRows={2}
        slotProps={{ htmlInput: { maxLength: 2000 } }}
      />
      <TextField
        label={t('features.agentLifecycle.registry.dataClasses', 'Data classes')}
        helperText={t(
          'features.agentLifecycle.registry.dataClassesHelp',
          'Comma-separated, for example customer_pii, financial. Lower-case letters, digits, dot, hyphen and underscore.',
        )}
        {...fields.dataClasses}
        size="small"
      />
      {save.error !== null && <Alert severity="error">{lifecycleErrorMessage(save.error)}</Alert>}
      <Box>
        <Button
          variant="contained"
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate(changes)}
          data-testid="registry-save"
        >
          {t('features.agentLifecycle.registry.save', 'Save')}
        </Button>
      </Box>
    </Box>
  );
}

function CertificationSummary({ record }: { readonly record: AgentRegistryRecord }): ReactNode {
  const rows: readonly (readonly [string, string])[] = [
    [t('features.agentLifecycle.registry.certification', 'Certification'), certificationStateLabel(record.certification_state)],
    [
      t('features.agentLifecycle.registry.certifiedRelease', 'Certified release'),
      record.certified_release_id === null ? '—' : `#${record.certified_release_id}`,
    ],
    [t('features.agentLifecycle.registry.certifiedAt', 'Certified'), formatWhen(record.certified_at)],
    [t('features.agentLifecycle.registry.recertifyBy', 'Recertify by'), formatWhen(record.recertify_by)],
    [t('features.agentLifecycle.registry.stateReason', 'Last state change'), stateReason(record)],
    [t('features.agentLifecycle.registry.owners', 'Owners'), `${formatUser(record.business_owner_id)} / ${formatUser(record.technical_owner_id)}`],
  ];
  return (
    <Box
      component="dl"
      sx={summarySx}
      data-testid="registry-certification"
    >
      {rows.map(([label, value]) => (
        <Box
          key={label}
          sx={{ display: 'contents' }}
        >
          <Typography
            component="dt"
            variant="labelSmall"
            color="text.secondary"
          >
            {label}
          </Typography>
          <Typography
            component="dd"
            variant="bodySmall"
            sx={{ margin: 0 }}
          >
            {value}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function stateReason(record: AgentRegistryRecord): string {
  const when = formatWhen(record.state_changed_at);
  return record.state_reason === '' ? when : `${record.state_reason} · ${when}`;
}

const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const rowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.75rem' };
const summarySx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: 'max-content 1fr',
  columnGap: '1rem',
  rowGap: '0.25rem',
  margin: 0,
};
