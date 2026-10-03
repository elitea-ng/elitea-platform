/**
 * The CEL routing-rule editor (#218, design-governance-config-authoring §3.1).
 *
 * The design chose a guided editor over the generic JSON-editor fallback: a
 * routing rule is a CEL predicate plus a weighted target list, and raw JSON
 * gives an operator no CEL help and no weight arithmetic. This is that editor.
 *
 * Both of its checks are HINTS. The Σ = 1.0 indicator and the Validate CEL
 * button tell the operator early; the server compiles the expression and
 * re-verifies the sum on every save, and rejects a rule that fails whatever
 * this showed.
 */
import AddIcon from '@mui/icons-material/Add';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { useValidateCel } from './api/adminGovernanceApi';
import {
  targetWeightSum,
  type GovernanceDraft,
  type RoutingTargetDraft,
} from './useGatewayGovernancePage';

/** How far a weight sum may sit from 1.0 before the hint calls it wrong. */
const WEIGHT_EPSILON = 1e-6;

export function RoutingFields({
  draft,
  onChange,
}: {
  readonly draft: GovernanceDraft;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}) {
  const validate = useValidateCel();
  const sum = targetWeightSum(draft.targets);
  const sumIsOne = Math.abs(sum - 1) <= WEIGHT_EPSILON;

  const patchTarget = (index: number, patch: Partial<RoutingTargetDraft>) => {
    onChange({
      targets: draft.targets.map((target, i) => (i === index ? { ...target, ...patch } : target)),
    });
  };

  return (
    <>
      <TextField
        label={t('pages.admin.governance.field.cel', 'CEL expression')}
        helperText={t(
          'pages.admin.governance.field.celHelp',
          'A boolean predicate over provider, model, customer_id, budget_used and params. team_id, tokens_used, complexity_tier and headers are declared but cannot be evaluated, and a rule that names one is refused on save.',
        )}
        value={draft.cel}
        onChange={(event) => onChange({ cel: event.target.value })}
        size="small"
        fullWidth
        multiline
        minRows={2}
        slotProps={{ htmlInput: { 'data-testid': 'governance-cel' } }}
      />
      <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
        <Button
          size="small"
          variant="secondary"
          disabled={draft.cel.trim() === '' || validate.isPending}
          onClick={() => validate.mutate(draft.cel)}
          data-testid="governance-validate-cel"
        >
          {t('pages.admin.governance.action.validateCel', 'Validate CEL')}
        </Button>
        {validate.data === undefined ? null : (
          <Typography
            variant="bodySmall"
            color={validate.data.valid ? 'success.main' : 'error.main'}
            data-testid="governance-cel-result"
          >
            {validate.data.valid
              ? t('pages.admin.governance.celValid', 'The expression compiles.')
              : (validate.data.error ??
                t('pages.admin.governance.celInvalid', 'The expression does not compile.'))}
          </Typography>
        )}
      </Box>
      <TextField
        label={t('pages.admin.governance.field.priority', 'Priority')}
        helperText={t(
          'pages.admin.governance.field.priorityHelp',
          'The highest-priority matching rule wins. Ties are broken by name, so every gateway replica picks the same rule.',
        )}
        value={draft.priority}
        onChange={(event) => onChange({ priority: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { inputMode: 'numeric' } }}
      />

      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <Typography variant="headingSmall" component="h3">
          {t('pages.admin.governance.field.targets', 'Weighted targets')}
        </Typography>
        <Typography
          variant="bodySmall"
          color={sumIsOne ? 'success.main' : 'error.main'}
          data-testid="governance-weight-sum"
        >
          {t('pages.admin.governance.field.weightSum', 'Σ weight')} = {sum.toFixed(3)}
        </Typography>
      </Box>
      {draft.targets.map((target, index) => (
        // The index IS the identity here: a target has no id, and two rows may
        // legitimately hold the same provider and model while one is being
        // typed. Reordering is not offered, so the index is stable.
        // eslint-disable-next-line react/no-array-index-key
        <Box key={index} sx={{ display: 'flex', gap: '0.5rem', alignItems: 'flex-start' }}>
          <TextField
            label={t('pages.admin.governance.field.targetProvider', 'Provider')}
            value={target.provider}
            onChange={(event) => patchTarget(index, { provider: event.target.value })}
            size="small"
            sx={{ flex: 1 }}
          />
          <TextField
            label={t('pages.admin.governance.field.targetModel', 'Model')}
            value={target.model}
            onChange={(event) => patchTarget(index, { model: event.target.value })}
            size="small"
            sx={{ flex: 1 }}
          />
          <TextField
            label={t('pages.admin.governance.field.targetWeight', 'Weight')}
            value={target.weight}
            onChange={(event) => patchTarget(index, { weight: event.target.value })}
            size="small"
            sx={{ width: '6rem' }}
            slotProps={{ htmlInput: { inputMode: 'decimal' } }}
          />
          <IconButton
            aria-label={t('pages.admin.governance.action.removeTarget', 'Remove target')}
            disabled={draft.targets.length <= 1}
            onClick={() => onChange({ targets: draft.targets.filter((_, i) => i !== index) })}
            size="small"
          >
            <DeleteOutlinedIcon fontSize="small" />
          </IconButton>
        </Box>
      ))}
      <Button
        size="small"
        startIcon={<AddIcon />}
        onClick={() => onChange({ targets: [...draft.targets, { provider: '', model: '', weight: '0' }] })}
      >
        {t('pages.admin.governance.action.addTarget', 'Add target')}
      </Button>
      {sumIsOne ? null : (
        <Alert severity="warning" data-testid="governance-weight-warning">
          {t(
            'pages.admin.governance.weightWarning',
            'The target weights must sum to exactly 1.0. The server re-checks this on save and rejects a rule that fails.',
          )}
        </Alert>
      )}
    </>
  );
}
