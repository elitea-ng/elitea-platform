/**
 * The Heartbeat tab's schedule (issue 3137, ADR-0050 decision 2):
 * `meta.proactivity`, saved with the version — how often the agent wakes, its
 * quiet hours, what a wake reads, where it acts and its daily limits. The
 * heartbeat reads the block of the PUBLISHED release, so a change takes
 * effect after publishing; turning the heartbeat on is the panel below,
 * a per-install decision that never travels with the agent.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { MetaNumberField, MetaSelectField, MetaTextField, useMetaBlock, type MetaSectionProps } from './metaControls';
import { HeartbeatQuietHours, HeartbeatWatchList } from './HeartbeatScheduleParts';
import { metaFieldsSx, metaSectionSx } from './metaSection.styles';

export function HeartbeatScheduleSection({ versionFields, disabled }: MetaSectionProps): ReactNode {
  const schedule = useMetaBlock(versionFields, 'proactivity');
  // An absent block is off; a present block without a mode is heartbeat (v2.yaml AgentProactivity).
  const mode = schedule.block !== null && schedule.get(['mode']) !== 'off' ? 'heartbeat' : 'off';
  return (
    <Box component="section" sx={metaSectionSx} data-testid="agent-meta-heartbeat" aria-labelledby="agent-meta-heartbeat-title">
      <Typography id="agent-meta-heartbeat-title" component="h3" variant="headingSmall">
        {t('pages.agents.editApplication.meta.heartbeat.title', 'Heartbeat schedule')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.agents.editApplication.meta.heartbeat.intro',
          'Saved with the version; the heartbeat reads the published release. Turn the heartbeat on below once a release with this schedule is published.',
        )}
      </Typography>
      <Box sx={metaFieldsSx}>
        <TextField
          select
          label={t('pages.agents.editApplication.meta.heartbeat.mode', 'Mode')}
          value={mode}
          onChange={(event) => schedule.set(['mode'], event.target.value)}
          disabled={disabled}
          size="small"
          slotProps={{ htmlInput: { 'data-testid': 'agent-meta-heartbeat-mode' } }}
        >
          <MenuItem value="off">{t('pages.agents.editApplication.meta.heartbeat.off', 'Off')}</MenuItem>
          <MenuItem value="heartbeat">{t('pages.agents.editApplication.meta.heartbeat.on', 'Heartbeat')}</MenuItem>
        </TextField>
        <MetaTextField handle={schedule} path={['interval']} label={t('pages.agents.editApplication.meta.heartbeat.interval', 'Wake every')} helper={t('pages.agents.editApplication.meta.heartbeat.intervalHelper', '15m to 24h, for example 30m or 1h. Default 1h.')} placeholder={t('pages.agents.editApplication.meta.heartbeat.intervalPlaceholder', '1h')} disabled={disabled} testId="agent-meta-heartbeat-interval" />
        <MetaNumberField handle={schedule} path={['jitter']} label={t('pages.agents.editApplication.meta.heartbeat.jitter', 'Jitter (%)')} helper={t('pages.agents.editApplication.meta.heartbeat.jitterHelper', '0 to 50 percent of the interval. Default 10.')} disabled={disabled} testId="agent-meta-heartbeat-jitter" />
        <MetaSelectField
          handle={schedule}
          path={['act_target']}
          label={t('pages.agents.editApplication.meta.heartbeat.actTarget', 'Act in')}
          defaultLabel={t('pages.agents.editApplication.meta.heartbeat.homeThreadDefault', 'Default (home thread)')}
          options={[
            { value: 'home_thread', label: t('pages.agents.editApplication.meta.heartbeat.homeThread', 'The home thread') },
            { value: 'new_conversation', label: t('pages.agents.editApplication.meta.heartbeat.newConversation', 'A new conversation') },
          ]}
          disabled={disabled}
          testId="agent-meta-heartbeat-act-target"
        />
      </Box>
      <HeartbeatQuietHours handle={schedule} disabled={disabled} />
      <HeartbeatWatchList handle={schedule} disabled={disabled} />
      <Typography component="h4" variant="labelMedium">
        {t('pages.agents.editApplication.meta.heartbeat.limits', 'Daily limits')}
      </Typography>
      <Box sx={metaFieldsSx}>
        <MetaNumberField handle={schedule} path={['limits', 'wakes_per_day']} label={t('pages.agents.editApplication.meta.heartbeat.wakes', 'Wakes per day')} helper={t('pages.agents.editApplication.meta.rangeDefault', 'From {{min}} to {{max}}. Default {{fallback}}.', { min: 1, max: 96, fallback: 24 })} disabled={disabled} testId="agent-meta-heartbeat-wakes" />
        <MetaNumberField handle={schedule} path={['limits', 'acts_per_day']} label={t('pages.agents.editApplication.meta.heartbeat.acts', 'Acts per day')} helper={t('pages.agents.editApplication.meta.rangeDefault', 'From {{min}} to {{max}}. Default {{fallback}}.', { min: 0, max: 96, fallback: 4 })} disabled={disabled} testId="agent-meta-heartbeat-acts" />
        <MetaNumberField handle={schedule} path={['limits', 'spend_per_day_usd']} label={t('pages.agents.editApplication.meta.heartbeat.spend', 'Spend per day (USD)')} helper={t('pages.agents.editApplication.meta.rangeDefault', 'From {{min}} to {{max}}. Default {{fallback}}.', { min: 0, max: 1000, fallback: 2 })} disabled={disabled} testId="agent-meta-heartbeat-spend" />
      </Box>
    </Box>
  );
}
