/**
 * The four steps of the hire wizard (ADR-0051 decision 3). Each step is a
 * fieldset with a legend, so the dialog has one heading and a screen reader
 * reads the step as a group.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Link from '@mui/material/Link';
import MenuItem from '@mui/material/MenuItem';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { Link as RouterLink } from '@tanstack/react-router';

import type { MarketplaceRequirement, MarketplaceVersion } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { DISABLED_VALUE, isHireable, mustBind, needsApproval, type BindingChoices, type HireMode } from './hireModel';
import type { BindingOption } from './useBindingTargets';

function StepGroup({ title, children }: { readonly title: string; readonly children: ReactNode }): ReactNode {
  return (
    <Box component="fieldset" sx={fieldsetSx}>
      <Typography component="legend" variant="labelMedium">
        {title}
      </Typography>
      {children}
    </Box>
  );
}

export function VersionStep({
  versions,
  value,
  onChange,
}: {
  readonly versions: readonly MarketplaceVersion[];
  readonly value: string;
  readonly onChange: (id: string) => void;
}): ReactNode {
  return (
    <StepGroup title={t('marketplace.hire.version.legend', 'Choose a version')}>
      <RadioGroup
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-label={t('marketplace.hire.version.legend', 'Choose a version')}
      >
        {versions.map((version) => (
          <Box key={version.id} sx={versionRowSx}>
            <FormControlLabel
              value={version.id}
              disabled={!isHireable(version)}
              control={<Radio size="small" />}
              label={`${version.name} (${version.state})`}
              data-testid={`hire-version-${version.id}`}
            />
            {version.state === 'deprecated' && (
              <Typography variant="bodySmall" color="warning.main">
                {t(
                  'marketplace.hire.version.deprecated',
                  'The publisher deprecated this version. You can still hire it.',
                )}
              </Typography>
            )}
            {version.state === 'yanked' && (
              <Typography variant="bodySmall" color="error.main">
                {t('marketplace.hire.version.yanked', 'This version was yanked. You cannot hire it.')}
              </Typography>
            )}
            {version.release_notes !== '' && (
              <Typography variant="bodySmall" color="text.secondary" sx={{ whiteSpace: 'pre-wrap' }}>
                {version.release_notes}
              </Typography>
            )}
          </Box>
        ))}
      </RadioGroup>
    </StepGroup>
  );
}

export function ModeStep({
  value,
  onChange,
}: {
  readonly value: HireMode;
  readonly onChange: (mode: HireMode) => void;
}): ReactNode {
  return (
    <StepGroup title={t('marketplace.hire.mode.legend', 'Choose how to hire')}>
      <RadioGroup
        value={value}
        onChange={(event) => onChange(event.target.value === 'fork' ? 'fork' : 'install')}
        aria-label={t('marketplace.hire.mode.legend', 'Choose how to hire')}
      >
        <FormControlLabel
          value="install"
          control={<Radio size="small" />}
          label={t('marketplace.hire.mode.install', 'Install (recommended)')}
          data-testid="hire-mode-install"
        />
        <Typography variant="bodySmall" color="text.secondary" sx={helpSx}>
          {t(
            'marketplace.hire.mode.installHelp',
            'The definition is read-only and pinned to this version. Upgrades are your choice.',
          )}
        </Typography>
        <FormControlLabel
          value="fork"
          control={<Radio size="small" />}
          label={t('marketplace.hire.mode.fork', 'Fork')}
          data-testid="hire-mode-fork"
        />
        <Typography variant="bodySmall" color="text.secondary" sx={helpSx}>
          {t('marketplace.hire.mode.forkHelp', 'You get an editable copy. It remembers the version it came from.')}
        </Typography>
      </RadioGroup>
    </StepGroup>
  );
}

interface BindingRowProps {
  readonly requirement: MarketplaceRequirement;
  readonly options: readonly BindingOption[];
  readonly value: string;
  readonly onChange: (value: string) => void;
}

function BindingRow({ requirement, options, value, onChange }: BindingRowProps): ReactNode {
  const label = t('marketplace.hire.bindings.field', '{{type}} for {{name}}', {
    type: requirement.type,
    name: requirement.name || requirement.type,
  });
  return (
    <Box sx={versionRowSx}>
      <TextField
        select
        size="small"
        label={label}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        slotProps={{ htmlInput: { 'aria-label': label } }}
        data-testid={`hire-binding-${requirement.key}`}
      >
        <MenuItem value="">{t('marketplace.hire.bindings.choose', 'Choose')}</MenuItem>
        {options.map((option) => (
          <MenuItem key={option.value} value={option.value}>
            {option.label}
          </MenuItem>
        ))}
        {mustBind(requirement) && (
          <MenuItem value={DISABLED_VALUE}>{t('marketplace.hire.bindings.disable', 'Disable for now')}</MenuItem>
        )}
      </TextField>
      {requirement.has_write_tools && (
        <Typography variant="bodySmall" color="warning.main">
          {t('marketplace.hire.bindings.writes', 'This agent can write with it.')}
        </Typography>
      )}
      {options.length === 0 && mustBind(requirement) && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.hire.bindings.none', 'This project has no {{type}} toolkit.', { type: requirement.type })}{' '}
          <Link component={RouterLink} to="/toolkits/create" target="_blank" rel="noopener">
            {t('marketplace.hire.bindings.create', 'Create a toolkit')}
          </Link>
        </Typography>
      )}
    </Box>
  );
}

interface BindingsStepProps {
  readonly requirements: MarketplaceVersion['requirements'];
  readonly choices: BindingChoices;
  readonly optionsFor: (requirement: MarketplaceRequirement) => readonly BindingOption[];
  readonly onChoose: (key: string, value: string) => void;
}

export function BindingsStep({ requirements, choices, optionsFor, onChoose }: BindingsStepProps): ReactNode {
  if (requirements.items.length === 0) {
    return (
      <StepGroup title={t('marketplace.hire.bindings.legend', 'Bind what the agent needs')}>
        <Typography variant="bodySmall">
          {t('marketplace.hire.bindings.nothing', 'This agent needs nothing from your project.')}
        </Typography>
      </StepGroup>
    );
  }
  return (
    <StepGroup title={t('marketplace.hire.bindings.legend', 'Bind what the agent needs')}>
      {requirements.items.map((requirement) => {
        if (requirement.kind === 'agent') {
          return (
            <Typography
              key={requirement.key}
              variant="bodySmall"
              color="text.secondary"
              data-testid={`hire-agent-${requirement.key}`}
            >
              {t('marketplace.hire.bindings.agent', 'Nested agent {{name}} comes with this agent.', {
                name: requirement.name || requirement.type,
              })}
            </Typography>
          );
        }
        const choice = choices[requirement.key];
        const value = choice === undefined ? '' : choice.kind === 'disabled' ? DISABLED_VALUE : choice.id;
        return (
          <BindingRow
            key={requirement.key}
            requirement={requirement}
            options={optionsFor(requirement)}
            value={value}
            onChange={(next) => onChoose(requirement.key, next)}
          />
        );
      })}
    </StepGroup>
  );
}

export function ReviewStep({
  requirements,
  mode,
  version,
}: {
  readonly requirements: MarketplaceVersion['requirements'];
  readonly mode: HireMode;
  readonly version: MarketplaceVersion | undefined;
}): ReactNode {
  const tools = requirements.items.flatMap((requirement) => requirement.tools);
  return (
    <StepGroup title={t('marketplace.hire.review.legend', 'Review the hire')}>
      <Typography variant="bodySmall">
        {t('marketplace.hire.review.summary', '{{mode}} version {{version}}. Risk tier: {{tier}}.', {
          mode:
            mode === 'install'
              ? t('marketplace.hire.review.install', 'Install')
              : t('marketplace.hire.review.fork', 'Fork'),
          version: version?.name ?? '',
          tier: requirements.tier,
        })}
      </Typography>
      <Typography variant="bodySmall" data-testid="hire-review-tools">
        {tools.length > 0
          ? t('marketplace.hire.review.tools', 'Tools: {{tools}}', { tools: tools.join(', ') })
          : t('marketplace.hire.review.noTools', 'This agent has no tools.')}
      </Typography>
      {requirements.has_write_tools && (
        <Alert severity="warning">{t('marketplace.hire.review.writes', 'This agent has tools that can write.')}</Alert>
      )}
      {needsApproval(requirements) && (
        <Alert severity="info" data-testid="hire-review-approval">
          {t(
            'marketplace.hire.review.approval',
            'The agent starts with every tool off. A change request of kind hire turns them on after a person approves it. In a personal project, a hire that needs approval needs a shared project.',
          )}
        </Alert>
      )}
    </StepGroup>
  );
}

const fieldsetSx: SxProps<Theme> = {
  border: 0,
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: 1.5,
  minWidth: 0,
};
const versionRowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: 0.5 };
const helpSx: SxProps<Theme> = { marginLeft: '2rem', marginTop: '-0.25rem' };
