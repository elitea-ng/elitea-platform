import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentAttestationCreate, AgentAttestationSubjectType } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useRecordAttestation } from '../api/attestationsApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { ATTESTATION_KINDS, attestationKindLabel, type AttestationKind } from './governanceLabels';

/**
 * Records one attestation. The subject defaults to this agent; "release"
 * takes a release id (Release history lists them). `valid_until` is a date:
 * the attestation stays valid through the end of that day, UTC.
 */
export interface AttestationAddFormProps {
  readonly projectId: string;
  readonly applicationId: number;
  readonly onDone: () => void;
}

interface AttestationForm {
  readonly kind: AttestationKind;
  readonly customKind: string;
  readonly subjectType: Extract<AgentAttestationSubjectType, 'agent' | 'release'>;
  readonly subjectId: string;
  readonly attester: string;
  readonly validUntil: string;
  readonly evidenceUrl: string;
  readonly note: string;
}

function toRequest(form: AttestationForm): AgentAttestationCreate {
  return {
    kind: form.kind,
    ...(form.kind === 'custom' ? { custom_kind: form.customKind.trim() } : {}),
    subject_type: form.subjectType,
    subject_id: form.subjectId.trim(),
    attester: form.attester.trim(),
    ...(form.validUntil === '' ? {} : { valid_until: `${form.validUntil}T23:59:59Z` }),
    ...(form.evidenceUrl.trim() === '' ? {} : { evidence_url: form.evidenceUrl.trim() }),
    ...(form.note.trim() === '' ? {} : { note: form.note.trim() }),
  };
}

function isComplete(form: AttestationForm): boolean {
  return (
    form.attester.trim() !== '' &&
    /^[A-Za-z0-9-]{1,64}$/.test(form.subjectId.trim()) &&
    (form.kind !== 'custom' || /^[a-z0-9][a-z0-9_.-]{0,63}$/.test(form.customKind.trim()))
  );
}

export function AttestationAddForm({ projectId, applicationId, onDone }: AttestationAddFormProps): ReactNode {
  const [form, setForm] = useState<AttestationForm>({
    kind: 'architecture',
    customKind: '',
    subjectType: 'agent',
    subjectId: String(applicationId),
    attester: '',
    validUntil: '',
    evidenceUrl: '',
    note: '',
  });
  const record = useRecordAttestation(projectId);
  const field = (name: keyof AttestationForm) => ({
    value: form[name],
    onChange: (event: { target: { value: string } }) => setForm({ ...form, [name]: event.target.value }),
    size: 'small' as const,
  });

  const fields = {
    kind: field('kind'),
    customKind: field('customKind'),
    subjectType: field('subjectType'),
    subjectId: field('subjectId'),
    attester: field('attester'),
    validUntil: field('validUntil'),
    evidenceUrl: field('evidenceUrl'),
    note: field('note'),
  };
  return (
    <Box
      sx={formSx}
      data-testid="attestation-add-form"
    >
      <Box sx={rowSx}>
        <TextField
          select
          label={t('features.agentLifecycle.attestation.kind', 'Kind')}
          {...fields.kind}
          sx={{ minWidth: '12rem' }}
        >
          {ATTESTATION_KINDS.map((kind) => (
            <MenuItem
              key={kind}
              value={kind}
            >
              {attestationKindLabel(kind)}
            </MenuItem>
          ))}
        </TextField>
        {form.kind === 'custom' && (
          <TextField
            label={t('features.agentLifecycle.attestation.customKind', 'Custom kind')}
            {...fields.customKind}
          />
        )}
        <TextField
          select
          label={t('features.agentLifecycle.attestation.subjectType', 'Subject')}
          {...fields.subjectType}
          sx={{ minWidth: '8rem' }}
        >
          <MenuItem value="agent">{t('features.agentLifecycle.attestation.subject.agent', 'This agent')}</MenuItem>
          <MenuItem value="release">{t('features.agentLifecycle.attestation.subject.release', 'A release')}</MenuItem>
        </TextField>
        <TextField
          label={t('features.agentLifecycle.attestation.subjectId', 'Subject id')}
          {...fields.subjectId}
          slotProps={{ htmlInput: { 'data-testid': 'attestation-subject-id' } }}
        />
      </Box>
      <Box sx={rowSx}>
        <TextField
          label={t('features.agentLifecycle.attestation.attester', 'Attested by')}
          {...fields.attester}
          slotProps={{ htmlInput: { maxLength: 256, 'data-testid': 'attestation-attester' } }}
        />
        <TextField
          type="date"
          label={t('features.agentLifecycle.attestation.validUntil', 'Valid until')}
          {...fields.validUntil}
          slotProps={{ inputLabel: { shrink: true }, htmlInput: { 'data-testid': 'attestation-valid-until' } }}
        />
        <TextField
          label={t('features.agentLifecycle.attestation.evidenceUrl', 'Evidence URL')}
          {...fields.evidenceUrl}
          type="url"
          sx={{ flex: 1, minWidth: '14rem' }}
          slotProps={{ htmlInput: { maxLength: 2048 } }}
        />
      </Box>
      <TextField
        label={t('features.agentLifecycle.attestation.note', 'Note')}
        {...fields.note}
        multiline
        slotProps={{ htmlInput: { maxLength: 2000 } }}
      />
      {record.error !== null && <Alert severity="error">{lifecycleErrorMessage(record.error)}</Alert>}
      <Box sx={rowSx}>
        <Button
          variant="contained"
          size="small"
          disabled={!isComplete(form) || record.isPending}
          onClick={() => record.mutate(toRequest(form), { onSuccess: onDone })}
          data-testid="attestation-save"
        >
          {t('features.agentLifecycle.attestation.save', 'Record attestation')}
        </Button>
        <Button
          size="small"
          onClick={onDone}
        >
          {t('features.agentLifecycle.requestPublish.cancel', 'Cancel')}
        </Button>
      </Box>
    </Box>
  );
}

const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const rowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.75rem' };
