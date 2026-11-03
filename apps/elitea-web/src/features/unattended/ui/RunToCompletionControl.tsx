/**
 * "Run to completion" (`RunChoice`): one chip in the composer's toolbar,
 * before Send. The chip turns the choice on and off (`aria-pressed`); its
 * chevron opens a small popover with the details:
 *
 *   [x] Continue until the goal is met
 *       up to 20 continuations, …, 12 h   (Change limits)
 *   Also answer for me (off by default)
 *   [ ] Hook asks / [ ] Remote confirmations   (desktop)
 *   [ ] the sensitive tools of the agent        (cloud)
 *
 * The chip is primary while on, muted while off, and has a warning tint while
 * a part that answers for the person is on, so the person sees the stronger
 * grant. Its tooltip gives the grant in one line.
 */
import { useId, useState } from 'react';

import AllInclusiveIcon from '@mui/icons-material/AllInclusive';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import FormControlLabel from '@mui/material/FormControlLabel';
import Popover from '@mui/material/Popover';
import Switch from '@mui/material/Switch';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { chainErrors, type UnattendedGrantValue } from '../model/grant';
import { answersForPerson, type RunChoice, type RunScope } from '../model/runChoice';
import { chainLine, DESKTOP_OPT_INS, sensitiveLabel } from './grantParts';
import { ChainLimits, muted, SensitiveTools, type SensitiveToolOption } from './grantSections';

export interface RunToCompletionControlProps {
  value: RunChoice;
  onChange: (next: RunChoice) => void;
  scope: RunScope;
  /** Cloud: the agent tools the grant can name; the section hides when this is absent. */
  sensitiveTools?: readonly SensitiveToolOption[] | undefined;
  disabled?: boolean;
  /** Why the choice does not apply to the next send (for example, Plan mode); shown in the tooltip. */
  notApplied?: string | undefined;
  testId: string;
}

type Tone = 'off' | 'on' | 'strong' | 'invalid';

function toneOf(value: RunChoice, scope: RunScope, applied: boolean): Tone {
  if (!value.on || !applied) return 'off';
  if (chainErrors(value.grant.chain).length > 0) return 'invalid';
  return answersForPerson(value.grant, scope) ? 'strong' : 'on';
}

const toneColor = (theme: Theme, tone: Tone): string => {
  switch (tone) {
    case 'on':
      return theme.vars.palette.primary.main;
    case 'strong':
      return theme.vars.palette.warning.main;
    case 'invalid':
      return theme.vars.palette.error.main;
    case 'off':
      return theme.vars.palette.text.metrics;
  }
};

/** The parts that answer for the person, by name. */
function optInNames(grant: UnattendedGrantValue, scope: RunScope): string[] {
  if (scope === 'cloud') return grant.sensitive_tools.length === 0 ? [] : [`${sensitiveLabel()}: ${grant.sensitive_tools.map((named) => `${named.toolkit}/${named.tool}`).join(', ')}`];
  return DESKTOP_OPT_INS.filter((part) => grant[part.key]).map((part) => part.label());
}

/** The tooltip: the grant in one line. */
function summary(value: RunChoice, scope: RunScope, notApplied: string | undefined): string {
  if (notApplied !== undefined) return notApplied;
  if (!value.on) return t('unattended.control.offSummary', 'Run to completion is off. The run stops at a limit and waits for you.');
  if (chainErrors(value.grant.chain).length > 0) return t('unattended.control.invalidSummary', 'A chain limit is not valid. Open the settings and correct it.');
  const base = t('unattended.control.onSummary', 'Run to completion: the run continues by itself until the goal is met ({{limits}}).', { limits: chainLine(value.grant.chain) });
  const names = optInNames(value.grant, scope);
  return names.length === 0 ? base : `${base} ${t('unattended.control.alsoSummary', 'It also answers for you: {{parts}}.', { parts: names.join(', ') })}`;
}

const chipSx = (tone: Tone) => (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  flexShrink: 0,
  height: '1.75rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${tone === 'strong' ? theme.vars.palette.warning.main : 'transparent'}`,
  color: toneColor(theme, tone),
});

const partSx = (theme: Theme) => ({
  height: '100%',
  gap: 0.5,
  paddingX: 0.5,
  borderRadius: theme.vars.shape.radiusSm,
  color: 'inherit',
  '& svg': { width: '1rem', height: '1rem' },
  '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
  '&.Mui-disabled': { color: theme.vars.palette.text.metrics, opacity: 0.6 },
});

function OptIns({ value, onChange, scope, sensitiveTools }: Pick<RunToCompletionControlProps, 'value' | 'onChange' | 'scope' | 'sensitiveTools'>): React.JSX.Element | null {
  const setGrant = (grant: UnattendedGrantValue): void => onChange({ ...value, grant });
  if (scope === 'cloud') {
    return sensitiveTools === undefined ? null : <SensitiveTools value={value.grant} onChange={setGrant} disabled={!value.on} options={sensitiveTools} />;
  }
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      {DESKTOP_OPT_INS.map((part) => (
        <Box key={part.key}>
          <FormControlLabel
            disabled={!value.on}
            control={<BaseCheckbox size="small" checked={value.grant[part.key]} onChange={(_event, on) => setGrant({ ...value.grant, [part.key]: on })} />}
            label={<Typography variant="bodySmall">{part.label()}</Typography>}
          />
          <Typography variant="bodySmall" component="p" sx={(theme: Theme) => ({ ...muted(theme), marginLeft: 3.75, marginTop: -0.5 })}>
            {part.help()}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function Details(props: Omit<RunToCompletionControlProps, 'disabled' | 'notApplied' | 'testId'> & { titleId: string }): React.JSX.Element {
  const { value, onChange, titleId } = props;
  const [editing, setEditing] = useState(false);
  const invalid = chainErrors(value.grant.chain).length > 0;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, padding: 2, width: '22rem', maxWidth: 'calc(100vw - 2rem)' }}>
      <Typography id={titleId} variant="labelMedium" component="h2">
        {t('unattended.control.title', 'Run to completion')}
      </Typography>
      <FormControlLabel
        control={<Switch size="small" checked={value.on} onChange={(_event, on) => onChange({ ...value, on })} />}
        label={<Typography variant="bodyMedium">{t('unattended.control.continue', 'Continue until the goal is met')}</Typography>}
      />
      <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap', marginLeft: 5.5, marginTop: -1 }}>
        <Typography variant="bodySmall" sx={(theme: Theme) => (invalid ? { color: theme.vars.palette.error.main } : muted(theme))} data-testid="unattended-chain-line">
          {chainLine(value.grant.chain)}
        </Typography>
        <Button size="small" variant="text" onClick={() => setEditing(!editing)} aria-expanded={editing || invalid} disabled={!value.on} sx={{ minWidth: 0, padding: 0 }}>
          {t('unattended.control.changeLimits', 'Change limits')}
        </Button>
      </Box>
      {(editing || invalid) && <ChainLimits value={value.grant} onChange={(grant) => onChange({ ...value, grant })} disabled={!value.on} />}
      <Divider textAlign="left" sx={{ marginY: 0.5 }}>
        <Typography variant="labelSmall" sx={muted}>
          {t('unattended.control.alsoAnswer', 'Also answer for me (off by default)')}
        </Typography>
      </Divider>
      <OptIns {...props} />
    </Box>
  );
}

export function RunToCompletionControl({ value, onChange, scope, sensitiveTools, disabled = false, notApplied, testId }: RunToCompletionControlProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const titleId = useId();
  const applied = notApplied === undefined;
  const tone = toneOf(value, scope, applied);
  const label = t('unattended.control.title', 'Run to completion');
  return (
    <>
      <Box component="span" sx={chipSx(tone)} data-testid={testId} data-tone={tone}>
        <Tooltip title={summary(value, scope, notApplied)}>
          {/* The span keeps the tooltip on a disabled button (it fires no events). */}
          <Box component="span" sx={{ display: 'inline-flex', height: '100%' }}>
            <ButtonBase aria-pressed={value.on && applied} aria-label={label} disabled={disabled || !applied} onClick={() => onChange({ ...value, on: !value.on })} data-testid={`${testId}-toggle`} sx={partSx}>
              <AllInclusiveIcon aria-hidden />
              <Typography variant="labelSmall" component="span" noWrap sx={{ display: { xs: 'none', sm: 'inline' } }}>
                {t('unattended.control.chip', 'To completion')}
              </Typography>
            </ButtonBase>
          </Box>
        </Tooltip>
        <ButtonBase
          aria-label={t('unattended.control.settings', 'Run to completion settings')}
          aria-haspopup="dialog"
          aria-expanded={anchor !== null}
          disabled={disabled || !applied}
          onClick={(event) => setAnchor(event.currentTarget)}
          data-testid={`${testId}-open`}
          sx={partSx}
        >
          <ExpandMoreIcon aria-hidden />
        </ButtonBase>
      </Box>
      <Popover
        open={anchor !== null}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'top', horizontal: 'right' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'right' }}
        slotProps={{ paper: { role: 'dialog', 'aria-labelledby': titleId, sx: { marginTop: -0.5 } } }}
      >
        <Details value={value} onChange={onChange} scope={scope} sensitiveTools={sensitiveTools} titleId={titleId} />
      </Popover>
    </>
  );
}
