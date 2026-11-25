/**
 * The pieces the rules panel and the proposal review share: a rule's
 * decision, pattern, bounds (sandbox and network an Allow covers), origin,
 * justification and examples; and why a file loads no rules.
 */
import { Alert, Box, Chip, Stack, Tooltip, Typography } from '@mui/material';
import { createContext, useContext } from 'react';

import type { PrefixRule, RuleDecision, RuleLoadError } from '@/shared/desktop/rulesIpc';
import { t } from '@/shared/i18n';

import { conditionParts } from '../model/grantText';
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

/** Whether the folder whose rules are shown is trusted now (ADR-0061): `requires_trust` rules match only then. */
export const FolderTrustedContext = createContext(false);

/** What an Allow covers ("the workspace-write sandbox, no network"); `null` for rules that tighten. */
function boundsOf(rule: PrefixRule): string | null {
  if ((rule.decision ?? 'allow') !== 'allow') return null;
  if (rule.sandbox === 'host') return t('workspace.rules.boundsHost', 'Runs without a sandbox, only while this folder is trusted');
  const sandbox = rule.sandbox ?? 'workspace-write';
  if (rule.credential !== undefined || rule.hosts !== undefined) return grantOf(rule, sandbox);
  return rule.network === true ? t('workspace.rules.boundsNetwork', 'Covers the {{sandbox}} sandbox, with network access', { sandbox }) : t('workspace.rules.bounds', 'Covers the {{sandbox}} sandbox, without network access', { sandbox });
}

/** A grant's bounds: the login, and the hosts (the login's own when none are listed). */
function grantOf(rule: PrefixRule, sandbox: string): string {
  const hosts = rule.hosts === undefined ? t('workspace.rules.grantDefaultHosts', 'its login’s hosts') : rule.hosts.join(', ');
  return rule.credential === undefined
    ? t('workspace.rules.boundsHosts', 'Covers the {{sandbox}} sandbox, reaching only {{hosts}}', { sandbox, hosts })
    : t('workspace.rules.boundsGrant', 'Covers the {{sandbox}} sandbox with the {{credential}} login, reaching only {{hosts}}', { sandbox, credential: rule.credential, hosts });
}

/** The argument conditions as one line, or `null` when there are none. */
function conditionsOf(rule: PrefixRule): string | null {
  const { forbid, require, values, graphql } = conditionParts(rule.conditions);
  const parts = [
    ...(forbid.length > 0 ? [t('workspace.rules.conditionForbid', 'never {{flags}}', { flags: forbid.join(' ') })] : []),
    ...(require.length > 0 ? [t('workspace.rules.conditionRequire', 'always {{flags}}', { flags: require.join(' ') })] : []),
    ...values.map((value) => t('workspace.rules.conditionValues', 'only {{value}}', { value })),
    ...(graphql ? [t('workspace.rules.conditionGraphql', 'GraphQL queries only')] : []),
  ];
  return parts.length === 0 ? null : t('workspace.rules.conditions', 'Only when: {{conditions}}', { conditions: parts.join('; ') });
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

/** A rule that matches only in a trusted folder, shown while this one is not: it is inert, never a sandboxed allow. */
function NeedsTrust({ rule }: { rule: PrefixRule }): React.JSX.Element | null {
  const trusted = useContext(FolderTrustedContext);
  if (rule.requires_trust !== true || trusted) return null;
  return (
    <Tooltip title={t('workspace.rules.needsTrustHelp', 'This rule was remembered in a trusted folder. It applies only while the folder is trusted, and does nothing until then.')}>
      <Chip size="small" color="warning" variant="outlined" data-testid="rule-needs-trust" label={t('workspace.rules.needsTrust', 'Needs trust')} />
    </Tooltip>
  );
}

/** One rule as a line of the list: everything it decides and why. */
export function RuleSummary({ rule }: { rule: PrefixRule }): React.JSX.Element {
  const examples = exampleCount(rule);
  const bounds = boundsOf(rule);
  const conditions = conditionsOf(rule);
  return (
    <>
      <Stack direction="row" spacing={0.5} sx={{ flexShrink: 0 }}>
        <Chip size="small" color={decisionColor(rule.decision)} label={decisionLabel(rule.decision)} />
        {rule.origin === 'repository' && <Chip size="small" variant="outlined" label={t('workspace.rules.fromRepository', 'From the repository')} />}
        <NeedsTrust rule={rule} />
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
        {conditions !== null && (
          <Typography variant="bodySmall" color="text.secondary" data-testid="rule-conditions" sx={{ overflowWrap: 'anywhere' }}>
            {conditions}
          </Typography>
        )}
        <Typography variant="bodySmall" color="text.secondary">
          {examples === 0 ? t('workspace.rules.noExamples', 'No examples') : t('workspace.rules.examplesPass', '{{n}} examples checked', { n: examples })}
        </Typography>
      </Box>
    </>
  );
}
