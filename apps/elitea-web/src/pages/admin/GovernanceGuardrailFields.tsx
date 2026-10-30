/**
 * The fields of a content guardrail in the LLM Governance dialog (ADR-0043
 * decision 1). Split out of `GovernanceDialogFields.tsx` to keep that file
 * short.
 *
 * The detectors are JSON text. Each detector type has different keys (regex
 * patterns and terms, secrets rule ids, an HTTP adapter URL and a secret
 * reference), and the server checks every key on write. The dialog checks
 * only that the text is a JSON array of objects.
 */
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import type { draftFieldErrors } from './governanceIdLists';
import type { GovernanceDraft } from './useGatewayGovernancePage';

interface GuardrailFieldsProps {
  readonly draft: GovernanceDraft;
  readonly errors: ReturnType<typeof draftFieldErrors>;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}

/** The agent scope of a guardrail: agents of the one project in Projects. */
export function GuardrailScopeFields({ draft, errors, onChange }: GuardrailFieldsProps) {
  return (
    <TextField
      label={t('pages.admin.governance.field.guardrailAgentIds', 'Agents')}
      error={errors.scopeAgentIds !== undefined}
      helperText={
        errors.scopeAgentIds ??
        t(
          'pages.admin.governance.field.guardrailAgentIdsHelp',
          'Comma-separated agent ids of the ONE project in Projects. Empty means every caller of the selected scope.',
        )
      }
      value={draft.scopeAgentIds}
      onChange={(event) => onChange({ scopeAgentIds: event.target.value })}
      size="small"
      fullWidth
      slotProps={{ htmlInput: { 'data-testid': 'governance-scope-agents' } }}
    />
  );
}

/** The stage, detectors, timeout, fail mode and stream options of a guardrail. */
export function GuardrailFields({ draft, errors, onChange }: GuardrailFieldsProps) {
  const isInput = draft.guardrailStage === 'input';
  return (
    <>
      <TextField
        select
        label={t('pages.admin.governance.field.guardrailStage', 'Stage')}
        helperText={t(
          'pages.admin.governance.field.guardrailStageHelp',
          'Input checks the request before the provider call. Output checks the answer, streamed or not.',
        )}
        value={draft.guardrailStage}
        onChange={(event) => onChange({ guardrailStage: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-guardrail-stage' } }}
      >
        <MenuItem value="input">{t('pages.admin.governance.field.guardrailStageInput', 'Input')}</MenuItem>
        <MenuItem value="output">{t('pages.admin.governance.field.guardrailStageOutput', 'Output')}</MenuItem>
      </TextField>
      <TextField
        label={t('pages.admin.governance.field.guardrailDetectors', 'Detectors (JSON)')}
        error={errors.guardrailDetectors !== undefined}
        helperText={
          errors.guardrailDetectors ??
          t(
            'pages.admin.governance.field.guardrailDetectorsHelp',
            'An ordered JSON array. Each detector has a type (regex, secrets or http) and an action (block, mask or flag). An http detector takes its auth header from a {{secret.NAME}} reference, never a raw value.',
          )
        }
        value={draft.guardrailDetectors}
        onChange={(event) => onChange({ guardrailDetectors: event.target.value })}
        size="small"
        fullWidth
        multiline
        minRows={4}
        slotProps={{ htmlInput: { 'data-testid': 'governance-guardrail-detectors', spellCheck: false } }}
      />
      <TextField
        label={t('pages.admin.governance.field.guardrailTimeoutMs', 'Timeout (ms)')}
        helperText={t(
          'pages.admin.governance.field.guardrailTimeoutMsHelp',
          'Between 50 and 30000. Empty means 2000. The timeout applies to the full entry.',
        )}
        value={draft.guardrailTimeoutMs}
        onChange={(event) => onChange({ guardrailTimeoutMs: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { inputMode: 'numeric' } }}
      />
      <TextField
        select
        label={t('pages.admin.governance.field.guardrailFailMode', 'Fail mode')}
        helperText={t(
          'pages.admin.governance.field.guardrailFailModeHelp',
          'What occurs when a detector fails or times out. Closed refuses the request. Open continues and records the failure.',
        )}
        value={draft.guardrailFailMode}
        onChange={(event) => onChange({ guardrailFailMode: event.target.value })}
        size="small"
        fullWidth
      >
        <MenuItem value="">
          {t(
            'pages.admin.governance.field.guardrailFailModeDefault',
            'Default: closed for block and mask, open for flag only',
          )}
        </MenuItem>
        <MenuItem value="closed">{t('pages.admin.governance.field.guardrailFailClosed', 'Closed')}</MenuItem>
        <MenuItem value="open">{t('pages.admin.governance.field.guardrailFailOpen', 'Open')}</MenuItem>
      </TextField>
      {isInput ? (
        <FormControlLabel
          control={
            <Switch
              checked={draft.guardrailApplyToEmbeddings}
              onChange={(event) => onChange({ guardrailApplyToEmbeddings: event.target.checked })}
              data-testid="governance-guardrail-embeddings"
            />
          }
          label={t('pages.admin.governance.field.guardrailEmbeddings', 'Also check embedding requests')}
        />
      ) : (
        <TextField
          label={t('pages.admin.governance.field.guardrailStreamWindow', 'Stream look-behind (bytes)')}
          helperText={t(
            'pages.admin.governance.field.guardrailStreamWindowHelp',
            'Between 64 and 8192. Empty means 256. A mask detector holds back this much streamed text, so that a match across two chunks is masked.',
          )}
          value={draft.guardrailStreamWindow}
          onChange={(event) => onChange({ guardrailStreamWindow: event.target.value })}
          size="small"
          fullWidth
          slotProps={{ htmlInput: { inputMode: 'numeric' } }}
        />
      )}
    </>
  );
}
