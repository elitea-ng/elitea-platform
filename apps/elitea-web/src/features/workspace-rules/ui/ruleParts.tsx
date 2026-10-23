/**
 * The pieces the rules panel and the proposal review share: a rule's
 * decision, pattern, bounds (sandbox and network an Allow covers), origin,
 * justification and examples; and why a file loads no rules.
 */
import { Alert, Box, Chip, Stack, Typography } from '@mui/material';

import type { PrefixRule, RuleDecision, RuleLoadError } from '@/shared/desktop/rulesIpc';
import { t } from '@/shared/i18n';

import { exampleCount, formatPattern } from '../model/patternText';

export const decisionLabel = (decision: RuleDecision | undefined): string => {
  if (decision === 'forbidden') return t('workspace.rules.decisionForbidden', 'Forbidden');
  if (decision === 'prompt') return t('workspace.rules.decisionPrompt', 'Ask');
  return t('workspace.rules.decisionAllow', 'Allow');
};

const decisionColor = (decision: RuleDecision | undefined): 'error' | 'warning' | 'success' => {
  if (decision === 'forbidden') return 'error';
  if (decision === 'prompt') return 'warning';
  return 'success';
};

/** What an Allow covers ("the workspace-write sandbox, no network"); `null` for rules that tighten. */
function boundsOf(rule: PrefixRule): string | null {
  if ((rule.decision ?? 'allow') !== 'allow') return null;
  const sandbox = rule.sandbox ?? 'workspace-write';
  return rule.network === true ? t('workspace.rules.boundsNetwork', 'Covers the {{sandbox}} sandbox, with network access', { sandbox }) : t('workspace.rules.bounds', 'Covers the {{sandbox}} sandbox, without network access', { sandbox });
}

export function LoadErrorAlert({ error }: { error: RuleLoadError }): React.JSX.Element {
  return (
    <Alert severity="error" data-testid="rules-error">
      <Typography variant="bodySmall" sx={{ fontWeight: 600 }}>
        {error.rule_index === undefined ? t('workspace.rules.fileRefused', 'These rules are not used') : t('workspace.rules.ruleRefused', 'Rule {{n}} is refused, so none of these rules are used', { n: error.rule_index })}
      </Typography>
      <Typography variant="bodySmall" sx={{ overflowWrap: 'anywhere' }}>
        {error.message}
      </Typography>
      {error.example !== undefined && (
        <Typography variant="bodySmall" data-testid="rules-error-example">
          {t('workspace.rules.exampleFailed', 'Example `{{example}}`: expected {{expected}}, but it {{got}}.', {
            example: error.example,
            expected: error.expected === 'not_match' ? t('workspace.rules.expectNoMatch', 'no match') : t('workspace.rules.expectMatch', 'a match'),
            got: error.got ?? '',
          })}
        </Typography>
      )}
    </Alert>
  );
}

/** One rule as a line of the list: everything it decides and why. */
export function RuleSummary({ rule }: { rule: PrefixRule }): React.JSX.Element {
  const examples = exampleCount(rule);
  const bounds = boundsOf(rule);
  return (
    <>
      <Stack direction="row" spacing={0.5} sx={{ flexShrink: 0 }}>
        <Chip size="small" color={decisionColor(rule.decision)} label={decisionLabel(rule.decision)} />
        {rule.origin === 'repository' && <Chip size="small" variant="outlined" label={t('workspace.rules.fromRepository', 'From the repository')} />}
      </Stack>
      <Box sx={{ flex: 1, minWidth: 0 }}>
        <Typography variant="bodyMedium" sx={{ fontFamily: (theme) => theme.typography.fontFamilyMono, overflowWrap: 'anywhere' }}>
          {formatPattern(rule.pattern)}
        </Typography>
        {rule.justification !== undefined && (
          <Typography variant="bodySmall" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
            {rule.justification}
          </Typography>
        )}
        {bounds !== null && (
          <Typography variant="bodySmall" color="text.secondary">
            {bounds}
          </Typography>
        )}
        <Typography variant="bodySmall" color="text.secondary">
          {examples === 0 ? t('workspace.rules.noExamples', 'No examples') : t('workspace.rules.examplesPass', '{{n}} examples checked', { n: examples })}
        </Typography>
      </Box>
    </>
  );
}
