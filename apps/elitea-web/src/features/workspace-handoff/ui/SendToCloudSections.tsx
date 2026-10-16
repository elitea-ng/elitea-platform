/**
 * The send dialog's lists: what is left out (grouped by who decided it,
 * with "Include anyway" where the policy allows it), what the folder's
 * settings include anyway (with "Remove include"), the include warnings and
 * their confirmation, and the environment variables to send.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ExcludedPath, ExcludeReason, HandoffPreview, IncludedPath, PreviewEnv, SecretFinding } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { formatBytes } from '../model/format';
import { EnvChecklist } from './EnvChecklist';

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const listSx = { margin: 0, paddingLeft: 2.5 } as const;
const sectionSx = { display: 'flex', flexDirection: 'column', gap: 0.5 } as const;
const EXCLUDED_SHOWN = 8;

function reasonLabel(reason: ExcludeReason): string {
  switch (reason) {
    case 'platform':
      return t('workspace.handoff.reason.platform', 'never sent (secrets and keys)');
    case 'project':
      return t('workspace.handoff.reason.project', 'left out by this project');
    case 'user':
      return t('workspace.handoff.reason.user', 'matches your folder’s exclusions');
    case 'path_deny':
      return t('workspace.handoff.reason.pathDeny', 'blocked by your organisation');
    case 'eliteaignore':
      return t('workspace.handoff.reason.eliteaignore', 'listed in .eliteaignore');
    case 'leave_on_laptop':
      return t('workspace.handoff.reason.leave', 'left on your laptop');
    case 'not_a_file':
      return t('workspace.handoff.reason.notAFile', 'not a regular file');
  }
}

type ExcludedGroupId = 'platform' | 'project' | 'folder' | 'path_deny' | 'other';

const GROUP_OF: Record<ExcludeReason, ExcludedGroupId> = {
  platform: 'platform',
  project: 'project',
  user: 'folder',
  eliteaignore: 'folder',
  leave_on_laptop: 'folder',
  path_deny: 'path_deny',
  not_a_file: 'other',
};

const GROUPS: readonly { id: ExcludedGroupId; title: () => string }[] = [
  { id: 'platform', title: () => t('workspace.handoff.group.platform', 'Platform') },
  { id: 'project', title: () => t('workspace.handoff.group.project', 'Project') },
  { id: 'folder', title: () => t('workspace.handoff.group.folder', 'Your folder') },
  { id: 'path_deny', title: () => t('workspace.handoff.group.pathDeny', 'Hard deny') },
  { id: 'other', title: () => t('workspace.handoff.group.other', 'Other') },
];

interface ExcludedGroupProps {
  id: ExcludedGroupId;
  title: string;
  entries: readonly ExcludedPath[];
  canInclude: boolean;
  disabled: boolean;
  onInclude: (path: string) => void;
}

function ExcludedGroup({ id, title, entries, canInclude, disabled, onInclude }: ExcludedGroupProps): React.JSX.Element {
  const [all, setAll] = useState(false);
  const shown = all ? entries : entries.slice(0, EXCLUDED_SHOWN);
  // The folder group mixes three reasons; every other group is one reason, its heading says it.
  const withReason = id === 'folder' || id === 'other';
  return (
    <Box component="section" data-testid={`handoff-excluded-${id.replace('_', '-')}`} sx={sectionSx}>
      <Typography variant="labelSmall" component="h4" sx={{ margin: 0 }}>
        {`${title} (${String(entries.length)})`}
      </Typography>
      <Box component="ul" sx={listSx}>
        {shown.map((entry) => (
          <li key={entry.path}>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
              <Typography variant="bodySmall" sx={{ wordBreak: 'break-all', flex: 1, minWidth: 0 }}>
                {withReason ? `${entry.path} · ${reasonLabel(entry.reason)}` : entry.path}
              </Typography>
              {canInclude && entry.includable && (
                <Button
                  size="small"
                  disabled={disabled}
                  onClick={() => onInclude(entry.path)}
                  aria-label={t('workspace.handoff.includeNamed', 'Include {{path}} anyway', { path: entry.path })}
                >
                  {t('workspace.handoff.include', 'Include anyway')}
                </Button>
              )}
            </Box>
          </li>
        ))}
      </Box>
      {entries.length > EXCLUDED_SHOWN && (
        <Button size="small" sx={{ alignSelf: 'flex-start' }} onClick={() => setAll((value) => !value)}>
          {all ? t('workspace.handoff.showFewer', 'Show fewer') : t('workspace.handoff.excludedMore', 'and {{n}} more', { n: String(entries.length - EXCLUDED_SHOWN) })}
        </Button>
      )}
    </Box>
  );
}

export function Excluded({ preview, disabled, onInclude }: { preview: HandoffPreview; disabled: boolean; onInclude: (path: string) => void }): React.JSX.Element | null {
  const { excluded } = preview;
  if (excluded.length === 0) return null;
  return (
    <Box component="section" data-testid="handoff-excluded" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.handoff.excludedTitle', 'Left out ({{n}})', { n: String(excluded.length) })}
      </Typography>
      {GROUPS.map((group) => {
        const entries = excluded.filter((entry) => GROUP_OF[entry.reason] === group.id);
        if (entries.length === 0) return null;
        return (
          <ExcludedGroup
            key={group.id}
            id={group.id}
            title={group.title()}
            entries={entries}
            canInclude={preview.policy.allow_user_include}
            disabled={disabled}
            onInclude={onInclude}
          />
        );
      })}
    </Box>
  );
}

export function Included({ included, disabled, onRemove }: { included: readonly IncludedPath[]; disabled: boolean; onRemove: (path: string) => void }): React.JSX.Element | null {
  if (included.length === 0) return null;
  return (
    <Box component="section" data-testid="handoff-included" sx={sectionSx}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.handoff.includedTitle', 'Included by your folder’s settings ({{n}})', { n: String(included.length) })}
      </Typography>
      <Box component="ul" sx={listSx}>
        {included.map((entry) => (
          <li key={entry.path}>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
              <Typography variant="bodySmall" sx={{ wordBreak: 'break-all', flex: 1, minWidth: 0 }}>
                {`${entry.path} · ${reasonLabel(entry.reason)}`}
              </Typography>
              <Button
                size="small"
                disabled={disabled}
                onClick={() => onRemove(entry.path)}
                aria-label={t('workspace.handoff.removeIncludeNamed', 'Stop including {{path}}', { path: entry.path })}
              >
                {t('workspace.handoff.removeInclude', 'Remove include')}
              </Button>
            </Box>
          </li>
        ))}
      </Box>
    </Box>
  );
}

export function IncludeWarnings({ findings, confirmed, onConfirm, disabled }: { findings: readonly SecretFinding[]; confirmed: boolean; onConfirm: (confirmed: boolean) => void; disabled: boolean }): React.JSX.Element {
  return (
    <Alert severity="warning" data-testid="handoff-include-findings">
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
        <Typography variant="bodyMedium">
          {t('workspace.handoff.includeFindingsTitle', 'Files you chose to include look like they hold secrets. They are sent only if you confirm it for this send.')}
        </Typography>
        <Box component="ul" sx={listSx}>
          {findings.map((finding) => (
            <li key={`${finding.path}:${String(finding.line)}:${finding.rule}`}>
              <Typography variant="bodySmall">{`${finding.path}:${String(finding.line)} · ${finding.rule}`}</Typography>
            </li>
          ))}
        </Box>
        <FormControlLabel
          control={<BaseCheckbox size="small" checked={confirmed} disabled={disabled} onChange={(event) => onConfirm(event.target.checked)} />}
          label={t('workspace.handoff.includeConfirm', 'I confirm sending these included files for this send')}
        />
      </Box>
    </Alert>
  );
}

export function EnvSection({ env, preview, disabled, onToggle }: { env: PreviewEnv; preview: HandoffPreview; disabled: boolean; onToggle: (name: string) => void }): React.JSX.Element {
  const limits = preview.policy.env_limits;
  return (
    <Box component="section" data-testid="handoff-env" sx={sectionSx}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.handoff.env.title', 'Environment variables')}
      </Typography>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.handoff.env.intro', 'Choose the variables the cloud needs. Only their names are kept; the values are read when you send.')}
      </Typography>
      {env.notice !== null && (
        <Alert severity="warning" data-testid="handoff-env-notice">
          {t('workspace.handoff.env.notice', 'Your login shell’s variables could not be read ({{notice}}), so none are offered.', { notice: env.notice })}
        </Alert>
      )}
      <EnvChecklist offered={env.offered} selected={env.selected} onToggle={onToggle} disabled={disabled} />
      <Typography variant="bodySmall" data-testid="handoff-env-totals" sx={mutedSx}>
        {t('workspace.handoff.env.totals', '{{vars}} of at most {{maxVars}} variables · {{size}} of at most {{maxSize}}', {
          vars: String(env.vars),
          maxVars: String(limits.max_vars),
          size: formatBytes(env.bytes),
          maxSize: formatBytes(limits.max_bytes),
        })}
      </Typography>
      {env.missing.length > 0 && (
        <Alert severity="info" data-testid="handoff-env-missing">
          {t('workspace.handoff.env.missing', 'Chosen but not set on this computer now: {{names}}', { names: env.missing.join(', ') })}
        </Alert>
      )}
      {env.over_limit && (
        <Alert severity="error" data-testid="handoff-env-over">
          {t('workspace.handoff.env.over', 'The chosen variables are over the limit. Choose fewer, or shorter ones, to send.')}
        </Alert>
      )}
    </Box>
  );
}
