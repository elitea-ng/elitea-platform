/**
 * The Workforce tab's controls (issue 3137): what the agent may do beyond its
 * tools (`meta.capabilities`, ADR-0050 decisions 5a and 5l) and its tool
 * envelope (`meta.tool_envelope`, decision 3) — per-tool call limits, the
 * tools it may use in a turn nobody waits on, and the tools its sub-agents
 * may get. Every envelope control only narrows the tools the agent has.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { formatToolLimits, formatToolList, parseToolLimits, parseToolList } from '../../lib/toolEnvelopeText';
import { MetaSwitchField, useMetaBlock, type MetaBlockHandle, type MetaSectionProps } from './metaControls';
import { metaSectionSx } from './metaSection.styles';

interface LinesFieldProps {
  readonly handle: MetaBlockHandle;
  readonly path: 'tool_limits' | 'unattended_tools' | 'subagent_tools';
  readonly label: string;
  readonly helper: string;
  readonly testId: string;
  readonly disabled?: boolean | undefined;
}

const CODECS = {
  tool_limits: { format: formatToolLimits, parse: parseToolLimits },
  unattended_tools: { format: formatToolList, parse: parseToolList },
  subagent_tools: { format: formatToolList, parse: parseToolList },
} as const;

/**
 * A one-entry-per-line field. The typed text is held here, so a half-typed
 * line or a trailing newline survives the round trip through the wire value;
 * a change from outside (a version switch, Discard) replaces it.
 */
function LinesField({ handle, path, label, helper, testId, disabled }: LinesFieldProps): ReactNode {
  const { format, parse } = CODECS[path];
  const value = handle.get([path]);
  const [text, setText] = useState(() => format(value));
  if (format(parse(text)) !== format(value)) setText(format(value));
  const problem = handle.problem([path]);
  return (
    <TextField
      multiline
      minRows={2}
      label={label}
      value={text}
      onChange={(event) => {
        setText(event.target.value);
        handle.set([path], parse(event.target.value));
      }}
      error={problem !== undefined}
      helperText={problem ?? helper}
      disabled={disabled}
      slotProps={{ htmlInput: { 'data-testid': testId } }}
    />
  );
}

export function WorkforceControlsSection({ versionFields, disabled }: MetaSectionProps): ReactNode {
  const capabilities = useMetaBlock(versionFields, 'capabilities');
  const envelope = useMetaBlock(versionFields, 'tool_envelope');
  return (
    <Box component="section" sx={metaSectionSx} data-testid="agent-meta-workforce" aria-labelledby="agent-meta-workforce-title">
      <Typography id="agent-meta-workforce-title" component="h3" variant="headingSmall">
        {t('pages.agents.editApplication.meta.workforce.title', 'Capabilities and tool envelope')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('pages.agents.editApplication.meta.workforce.intro', 'Saved with the version.')}
      </Typography>
      <MetaSwitchField handle={capabilities} path={['can_propose_agents']} label={t('pages.agents.editApplication.meta.workforce.propose', 'May propose saving a sub-agent it started as a new agent')} disabled={disabled} testId="agent-meta-can-propose" />
      <MetaSwitchField handle={capabilities} path={['can_review_agents']} label={t('pages.agents.editApplication.meta.workforce.review', 'Workforce manager: may propose a ladder stage for other agents')} disabled={disabled} testId="agent-meta-can-review" />
      <LinesField
        handle={envelope}
        path="tool_limits"
        label={t('pages.agents.editApplication.meta.workforce.toolLimits', 'Tool call limits per turn')}
        helper={t('pages.agents.editApplication.meta.workforce.toolLimitsHelper', 'One "toolkit/tool = limit" per line; "toolkit/*" for every tool of a toolkit.')}
        testId="agent-meta-tool-limits"
        disabled={disabled}
      />
      <LinesField
        handle={envelope}
        path="unattended_tools"
        label={t('pages.agents.editApplication.meta.workforce.unattended', 'Tools in a turn nobody waits on')}
        helper={t('pages.agents.editApplication.meta.workforce.unattendedHelper', 'One "toolkit/tool" per line. Empty keeps the default: the read-only tools.')}
        testId="agent-meta-unattended-tools"
        disabled={disabled}
      />
      <LinesField
        handle={envelope}
        path="subagent_tools"
        label={t('pages.agents.editApplication.meta.workforce.subagent', 'Tools a sub-agent may get')}
        helper={t('pages.agents.editApplication.meta.workforce.subagentHelper', 'One "toolkit/tool" per line, writes included. Empty keeps the default: the read-only tools.')}
        testId="agent-meta-subagent-tools"
        disabled={disabled}
      />
    </Box>
  );
}
