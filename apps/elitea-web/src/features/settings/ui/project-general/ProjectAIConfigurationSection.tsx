/**
 * The "AI Configurations" accordion of Settings › General.
 *
 * Ported from `EliteaUI/.../project-general/project-ai-configurations/`
 * (`ProjectAIConfigurations.jsx` + `AIConfiguration.jsx` +
 * `AIConfigurationToggle.jsx`): a Basic / OpenAI Template toggle, one copy
 * button for the whole panel, and a bordered box of label/value rows.
 *
 * NOT the same component as `ui/ai-configuration/ProjectAIConfiguration.tsx`,
 * which draws the same three values for the AI Providers tab in a different
 * frame (flat panel, per-field copy buttons, 12px type). This one reproduces
 * what a live deployment renders on General: a 12px-radius bordered card, a
 * 132px label column in `text.primary`, values in `text.secondary` on a pill,
 * all at 14px, and one 28px copy button pinned to the panel's top-right.
 *
 * `OpenAI-Project:` is deliberately absent, as it already is on AI Providers.
 * `ProjectAIConfiguration.tsx` records the reasoning in full: the `/llm` edge
 * discards that header, and the value the reference shows there is the
 * project that owns the default MODEL, sitting unlabelled beside the project
 * that PAYS. Reproducing it would need the six extra model-section queries
 * `AIConfiguration.jsx` fires purely to derive it.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { toAbsoluteApiUrl, toOpenAiBaseUrl } from '@/shared/lib/api-url';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';

import OpenAITemplate from '../ai-configuration/OpenAITemplate';

const BASIC_TAB = 'basic';
const TEMPLATE_TAB = 'openai-template';

export interface ProjectAIConfigurationSectionProps {
  readonly projectId: string;
}

/**
 * `vite_server_url` is this app's equivalent of the baseline's Redux
 * `state.user.api_url` — see `pages/settings/AIConfiguration.tsx`'s
 * `useUserApiUrl` for the same read and the same reasoning.
 */
function useUserApiUrl(): string {
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_server_url : '';
}

function ConfigRow({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={rowSx}>
      <Typography variant="bodyMedium" sx={rowLabelSx}>
        {label}
      </Typography>
      <Typography variant="bodyMedium" sx={rowValueSx}>
        {value}
      </Typography>
    </Box>
  );
}

export const ProjectAIConfigurationSection = memo(function ProjectAIConfigurationSection({
  projectId,
}: ProjectAIConfigurationSectionProps) {
  const [tab, setTab] = useState<string>(BASIC_TAB);
  const userApiUrl = useUserApiUrl();

  const notConfigured = t('ai-configuration.projectConfig.notConfigured', 'Not configured');
  const rows = useMemo(
    () => [
      {
        label: t('ai-configuration.projectConfig.openaiBase', 'OpenAI-BaseURL:'),
        value: userApiUrl ? toOpenAiBaseUrl(userApiUrl) : notConfigured,
      },
      {
        label: t('ai-configuration.projectConfig.serverUrl', 'Server URL:'),
        value: userApiUrl ? toAbsoluteApiUrl(userApiUrl) : notConfigured,
      },
      {
        label: t('ai-configuration.projectConfig.projectId', 'Project ID:'),
        value: projectId || notConfigured,
      },
    ],
    [notConfigured, projectId, userApiUrl],
  );

  /* The reference's `handleCopyCardInformation`: the whole panel at once, one
   * `label value` per line. */
  const handleCopyAll = useCallback(() => {
    const text = rows.map((row) => `${row.label} ${row.value}`).join('\n');
    void navigator.clipboard?.writeText(text);
  }, [rows]);

  const items = useMemo(
    () => [
      { value: BASIC_TAB, label: t('settings.projectGeneral.aiConfig.basic', 'Basic') },
      {
        value: TEMPLATE_TAB,
        label: t('settings.projectGeneral.aiConfig.openAiTemplate', 'OpenAI Template'),
      },
    ],
    [],
  );

  return (
    <Box sx={rootSx} data-testid="project-general-ai-configurations">
      <Box sx={headerSx}>
        <TabGroupButton
          items={items}
          value={tab}
          onChange={setTab}
          disableTooltip
          ariaLabel={t('settings.projectGeneral.aiConfig.toggle', 'AI configuration view')}
        />
        <Tooltip title={t('settings.projectGeneral.aiConfig.copy', 'Copy to clipboard')} placement="top">
          <IconButton onClick={handleCopyAll} sx={copyButtonSx} aria-label={t('settings.projectGeneral.aiConfig.copy', 'Copy to clipboard')}>
            <ContentCopyIcon sx={copyIconSx} />
          </IconButton>
        </Tooltip>
      </Box>

      {tab === BASIC_TAB ? (
        <Box sx={panelSx}>
          {rows.map((row) => (
            <ConfigRow key={row.label} label={row.label} value={row.value} />
          ))}
        </Box>
      ) : (
        <OpenAITemplate projectId={projectId} />
      )}
    </Box>
  );
});

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  width: '100%',
};

/* Reference `toggleContainer`: `paddingTop: 0.5rem`, `paddingBottom: 1rem`.
 * The copy button shares the row rather than being absolutely positioned —
 * the live page puts it on the same 28px baseline as the toggle. */
const headerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '1rem',
  paddingTop: '0.5rem',
  paddingBottom: '1rem',
};

const copyButtonSx: SxProps<Theme> = (theme) => ({
  width: '1.75rem',
  height: '1.75rem',
  padding: 0,
  borderRadius: theme.vars.shape.radiusPill,
  backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
  color: theme.vars.palette.text.secondary,
});

const copyIconSx: SxProps<Theme> = { width: '0.875rem', height: '0.875rem' };

/* Live page: `border-radius: 12px`, a 1px `border.lines` rule, `1rem 1.5rem`
 * of padding. */
const panelSx: SxProps<Theme> = (theme) => ({
  flexShrink: 0,
  display: 'flex',
  flexDirection: 'column',
  padding: '1rem 1.5rem',
  gap: '0.5rem',
  width: '100%',
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  boxSizing: 'border-box',
});

const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

/* 132px label column, muted; value white on a pill — the same label/value
 * treatment Settings › Profile uses, and the same one the live page measures
 * to here (label `rgb(169, 183, 193)`, value `rgb(255, 255, 255)`). */
const rowLabelSx: SxProps<Theme> = (theme) => ({
  width: '8.25rem',
  flexShrink: 0,
  color: theme.vars.palette.text.primary,
});

const rowValueSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  padding: '0 0.5rem',
  borderRadius: theme.vars.shape.radiusPill,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
