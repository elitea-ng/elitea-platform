/**
 * Run caps on the Configuration tab (issue 3137, ADR-0041 decision 3):
 * `meta.run_caps`, the author's own limits on one run of this agent. Each
 * LOWERS the platform's cap for the agent's risk tier and may not exceed it;
 * an empty field keeps the tier's cap. The tier and its caps come from
 * `getAgentRunCaps` when the caller may read them.
 */
import type { ReactNode } from 'react';
import { useEffect } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { RUN_CAP_KEYS, type RunCapKey, type TierRunCaps } from '../../lib/agentMetaBlocks';
import type { EditApplicationVersionFieldsState } from '../../lib/useEditApplicationVersionFields';
import { useTierRunCaps } from '../../lib/useTierRunCaps';
import { MetaNumberField, useMetaBlock, type MetaSectionProps } from './metaControls';
import { metaFieldsSx, metaSectionSx } from './metaSection.styles';

/** One object for "no caps known", so the effect below does not re-run on every render. */
const NO_TIER_CAPS: TierRunCaps = {};

interface RunCapsSectionProps extends MetaSectionProps {
  readonly versionFields: MetaSectionProps['versionFields'] & Pick<EditApplicationVersionFieldsState, 'setTierRunCaps'>;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

function capLabel(key: RunCapKey): string {
  switch (key) {
    case 'max_tool_calls':
      return t('pages.agents.editApplication.meta.runCaps.toolCalls', 'Tool calls per run');
    case 'max_model_turns':
      return t('pages.agents.editApplication.meta.runCaps.modelTurns', 'Model turns per run');
    case 'max_records_read':
      return t('pages.agents.editApplication.meta.runCaps.recordsRead', 'Records read per run');
    case 'max_spend':
      return t('pages.agents.editApplication.meta.runCaps.spend', 'Spend per run (USD)');
  }
}

/** The helper under a field: the tier's cap, or that the tier sets none, or nothing known. */
function capHelper(cap: number | undefined): string {
  if (cap === undefined) return t('pages.agents.editApplication.meta.runCaps.emptyKeeps', 'Empty keeps the platform cap.');
  if (cap <= 0) return t('pages.agents.editApplication.meta.runCaps.noTierCap', 'Empty: no cap at this tier.');
  return t('pages.agents.editApplication.meta.runCaps.tierCap', 'Empty keeps the tier cap of {{cap}}; a value may not exceed it.', { cap });
}

export function RunCapsSection({ versionFields, disabled, projectId, applicationId }: RunCapsSectionProps): ReactNode {
  const runCaps = useMetaBlock(versionFields, 'run_caps');
  const tier = useTierRunCaps(projectId, applicationId);
  const caps: TierRunCaps | undefined = tier.data?.caps;
  const { setTierRunCaps } = versionFields;
  // The editor's Save gate validates against the same caps the fields show.
  useEffect(() => setTierRunCaps(caps ?? NO_TIER_CAPS), [caps, setTierRunCaps]);
  return (
    <Box component="section" sx={metaSectionSx} data-testid="agent-meta-run-caps" aria-labelledby="agent-meta-run-caps-title">
      <Typography id="agent-meta-run-caps-title" component="h3" variant="headingSmall">
        {t('pages.agents.editApplication.meta.runCaps.title', 'Run caps')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary" data-testid="agent-meta-run-caps-tier">
        {tier.data === undefined
          ? t('pages.agents.editApplication.meta.runCaps.intro', 'Limits on one run of this agent. Each lowers the platform cap for the agent\'s risk tier.')
          : t('pages.agents.editApplication.meta.runCaps.introTier', 'Limits on one run of this agent. Each lowers the platform cap for its risk tier, {{tier}}.', { tier: tier.data.tier })}
      </Typography>
      <Box sx={metaFieldsSx}>
        {RUN_CAP_KEYS.map((key) => (
          <MetaNumberField
            key={key}
            handle={runCaps}
            path={[key]}
            label={capLabel(key)}
            helper={capHelper(caps?.[key])}
            placeholder={caps?.[key] !== undefined && caps[key] > 0 ? String(caps[key]) : undefined}
            disabled={disabled}
            testId={`agent-meta-run-caps-${key.replaceAll('_', '-')}`}
          />
        ))}
      </Box>
    </Box>
  );
}
