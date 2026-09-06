/**
 * ConfigurationCard — displays a single configuration/model card.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/ai-configuration/Configuration/ConfigurationCard.jsx`.
 */
import { memo, useCallback, useMemo } from 'react';
import { useTheme, type Theme } from '@mui/material/styles';

import RefreshIcon from '@mui/icons-material/Refresh';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { GradientIconWrapper } from '@/shared/ui/GradientIconWrapper';
import { t } from '@/shared/i18n';
import { toConfigurationId } from '@/features/settings/lib/ai-configuration/useStoredConnectionHealth';
import type { StoredConnectionHealth } from '@/features/settings/lib/ai-configuration/useStoredConnectionHealth';
import {
  getConfigurationDisplayName,
  getConfigurationStatus,
  isConfigurationEditable,
  getIconTypeKey,
} from '@/features/settings/lib/ai-configuration/configuration.helpers';

const CONFIG_PROVIDER_ICONS: Record<string, { path: string }> = {
  VERTEX_AI: {
    path: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-1 17.93c-3.95-.49-7-3.85-7-7.93 0-.62.08-1.21.21-1.79L9 15v1c0 1.1.9 2 2 2v1.93zm6.9-2.54c-.26-.81-1-1.39-1.9-1.39h-1v-3c0-.55-.45-1-1-1H8v-2h2c.55 0 1-.45 1-1V7h2c1.1 0 2-.9 2-2v-.41c2.93 1.19 5 4.06 5 7.41 0 2.08-.8 3.97-2.1 5.39z',
  },
  AI_DIAL: {
    path: 'M21 3H3c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h18c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm0 16H3V5h18v14zm-10-7h9v6h-9z',
  },
  OPEN_AI: {
    path: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8zm3.5-9c.83 0 1.5-.67 1.5-1.5S16.33 8 15.5 8 14 8.67 14 9.5s.67 1.5 1.5 1.5zm-7 0c.83 0 1.5-.67 1.5-1.5S9.33 8 8.5 8 7 8.67 7 9.5 7.67 11 8.5 11zm3.5 6.5c2.33 0 4.31-1.46 5.11-3.5H6.89c.8 2.04 2.78 3.5 5.11 3.5z',
  },
  CLAUDE: {
    path: 'M9.5 2c-1.1 0-2 .9-2 2v1c0 1.1.9 2 2 2h.5c.3-1.2 1.1-2.2 2.1-2.8C10.6 3.4 8.7 2.2 6.5 2H9.5zm1 4h3v2h-3V6zm-3 1h2v4h-2V7zm4 0h2v4h-2V7zm-3.5 5c-1.4 0-2.5 1.1-2.5 2.5S5.1 17 6.5 17H9.5v-4h-3zm4.5 0v4H12c.8 0 1.5-.7 1.5-1.5V12h-1v3zm3-2.5c-1.4 0-2.5 1.1-2.5 2.5s1.1 2.5 2.5 2.5h1.5v-5H15zm-3 1h2v3h-2v-3zM7 13.5C7 12.1 8.1 11 9.5 11h.5v4H9.5C8.1 15 7 13.9 7 12.5zm3 1v-2h1v2h-1z',
  },
  OLLAMA: {
    path: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-1 17.93c-3.95-.49-7-3.85-7-7.93 0-.62.08-1.21.21-1.79L9 15v1c0 1.1.9 2 2 2v1.93zm6.9-2.54c-.26-.81-1-1.39-1.9-1.39h-1v-3c0-.55-.45-1-1-1H8v-2h2c.55 0 1-.45 1-1V7h2c1.1 0 2-.9 2-2v-.41c2.93 1.19 5 4.06 5 7.41 0 2.08-.8 3.97-2.1 5.39z',
  },
  AMAZON_BEDROCK: {
    path: 'M19 3H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm0 16H5V5h14v14z',
  },
  AMAZON: {
    path: 'M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 14H4V6h16v12z',
  },
  HUGGING_FACE: {
    path: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8zm-5.5-2.5l7.51-3.49L17.5 6.5 9.99 9.99 6.5 17.5zm5.5-6c.83 0 1.5-.67 1.5-1.5S13.33 7 12.5 7 11 7.67 11 8.5s.67 1.5 1.5 1.5z',
  },
  CHROMA: {
    path: 'M12 2l-5.5 9h11L12 2zm0 3.84L13.93 9h-3.87L12 5.84zM17.5 13c-2.49 0-4.5 2.01-4.5 4.5s2.01 4.5 4.5 4.5 4.5-2.01 4.5-4.5-2.01-4.5-4.5-4.5zm0 7c-1.38 0-2.5-1.12-2.5-2.5s1.12-2.5 2.5-2.5 2.5 1.12 2.5 2.5-1.12 2.5-2.5 2.5zM3 21.5h8v-8H3v8zm2-6h4v4H5v-4z',
  },
  AZURE: {
    path: 'M20 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 14H4V8h16v10z',
  },
  PGVECTOR: {
    path: 'M4 22h16a2 2 0 002-2V2H2v18a2 2 0 002 2zm1-13h12v2H5V9zm0 4h8v2H5v-2zm0-8h12v2H5V5z',
  },
};

const resolveIconPath = (iconType: string): string => {
  return CONFIG_PROVIDER_ICONS[iconType]?.path ?? '';
};

const ProviderIcon = memo(({ iconType }: { iconType: string }) => {
  const path = resolveIconPath(iconType);
  return (
    <Box
      component="svg"
      viewBox="0 0 24 24"
      sx={{ width: '1.25rem', height: '1.25rem', fill: 'currentColor' }}
      dangerouslySetInnerHTML={{ __html: `<path d="${path}" />` }}
    />
  );
});

ProviderIcon.displayName = 'ProviderIcon';

interface ConfigurationCardProps {
  configuration: Record<string, unknown>;
  /** Currently-selected project id — compared against the configuration's
   * own `project_id` by `isConfigurationEditable` (old app: `useSelectedProjectId()`
   * read inside `ConfigurationCard.jsx`, not the configuration's own id). */
  projectId: string;
  canEdit: boolean;
  isDefault: boolean;
  onClick?: (configurationId: string) => void;
  /** This row's last connection-check verdict. Absent = the panel's
   * "Check connections" button has not been pressed, which is NOT the same as
   * healthy — the dot renders its own `unchecked` state for that. */
  health?: StoredConnectionHealth | undefined;
  /** Re-runs ADMISSION for this row (`POST /configurations/revalidate/...`)
   * and repaints the dot from the `status_ok` it returns. Absent = the
   * action is not offered. */
  onRevalidate?: ((configurationId: string) => void) | undefined;
  isRevalidating?: boolean | undefined;
}

/*
 * All three are declared `| undefined` rather than merely optional:
 * `ConfigurationSection` (this file's only caller, and a `@ts-nocheck` file)
 * threads them straight off a lookup that misses for any row nobody has
 * checked, so an explicit `undefined` is what actually arrives. Under
 * `exactOptionalPropertyTypes` a plain `?:` would reject that — today
 * silently, because the caller is unchecked, and loudly the day that
 * `@ts-nocheck` comes off.
 */

/** The dot's four resting states, plus `checking`. Colour is the only signal that fits beside a card title, so the tooltip carries the words. */
const HEALTH_DOT_COLORS: Record<StoredConnectionHealth['status'], (theme: Theme) => string> = {
  unchecked: (theme) => theme.vars.palette.text.disabled,
  checking: (theme) => theme.vars.palette.text.disabled,
  ok: (theme) => theme.vars.palette.status.publishedText,
  failed: (theme) => theme.vars.palette.status.rejectedText,
  // Amber, never red: a type this build has no checker for has not failed
  // anything, and the red dot would send someone to fix a working credential.
  unsupported: (theme) => theme.vars.palette.status.warningText,
};

function healthTooltip(health: StoredConnectionHealth): string {
  if (health.status === 'ok') return t('ai-configuration.health.ok', 'Connection OK');
  if (health.status === 'checking') return t('ai-configuration.health.checking', 'Checking connection...');
  if (health.status === 'unsupported') return t('ai-configuration.health.unsupported', 'Checking connection is not supported yet for this configuration type.');
  if (health.status === 'failed') return health.message ?? t('ai-configuration.health.failed', 'Connection failed');
  return t('ai-configuration.health.unchecked', 'Connection not checked yet');
}

/**
 * The dot and its Re-validate button. Split out of the card body so the card
 * itself stays within the §3.5 complexity budget, and so the `stopPropagation`
 * that keeps the button from also opening the card sits next to the click it
 * has to stop.
 */
const ConfigurationHealth = memo(({ configurationId, health, onRevalidate, isRevalidating }: {
  configurationId: string;
  health: StoredConnectionHealth;
  onRevalidate?: ((configurationId: string) => void) | undefined;
  isRevalidating?: boolean | undefined;
}) => {
  const handleRevalidate = useCallback((event: { stopPropagation: () => void }) => {
    // The whole card is a click target that navigates to the edit screen.
    event.stopPropagation();
    onRevalidate?.(configurationId);
  }, [configurationId, onRevalidate]);

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.25rem', flexShrink: 0 }}>
      <Tooltip title={healthTooltip(health)} placement="top">
        <Box
          data-testid={`configuration-health-dot-${configurationId}`}
          data-health={health.status}
          aria-label={healthTooltip(health)}
          sx={(theme: Theme) => ({
            width: '0.5rem',
            height: '0.5rem',
            borderRadius: 'var(--el-shape-radiusPill, 9999px)',
            flexShrink: 0,
            backgroundColor: HEALTH_DOT_COLORS[health.status](theme),
            opacity: health.status === 'checking' ? 0.5 : 1,
          })}
        />
      </Tooltip>
      {onRevalidate !== undefined && (
        <Tooltip title={t('ai-configuration.health.revalidate', 'Re-validate')} placement="top">
          <IconButton
            size="small"
            data-testid={`configuration-revalidate-${configurationId}`}
            aria-label={t('ai-configuration.health.revalidate', 'Re-validate')}
            onClick={handleRevalidate}
            disabled={isRevalidating === true}
            sx={{ width: '1.5rem', height: '1.5rem' }}
          >
            {isRevalidating === true ? <CircularProgress size={12} /> : <RefreshIcon fontSize="inherit" />}
          </IconButton>
        </Tooltip>
      )}
    </Box>
  );
});

ConfigurationHealth.displayName = 'ConfigurationHealth';

const UNCHECKED: StoredConnectionHealth = { status: 'unchecked' };

export default memo(
  ({ configuration, projectId, canEdit, isDefault, onClick, health, onRevalidate, isRevalidating }: ConfigurationCardProps) => {
    const theme = useTheme();
    const styles = getStyles(theme);
    const configData = (configuration.data ?? {}) as Record<string, unknown>;
    const isShared = configuration.shared === true;

    const disabled = useMemo(() => {
      return !isConfigurationEditable(configuration, projectId, canEdit);
    }, [canEdit, configuration, projectId]);

    const displayName = useMemo(() => getConfigurationDisplayName(configuration), [configuration]);
    // Old app: `getConfigurationStatus(configuration, isShared)` — the whole
    // (always-truthy) configuration object was passed as `statusOk`, so the
    // status text is always "OK" • never "In Progress" — regardless of
    // whether the configuration is the project default. Replicated literally
    // (not tied to `configuration.default`, which is an unrelated field —
    // the "Default" badge below already covers that via `isDefault`).
    // ... UNTIL a Re-validate re-derives it: that route returns the row, and
    // its `status_ok` is the real answer to the question this line was
    // hardcoding. `?? true` keeps the ported behaviour for every row nobody
    // has re-validated in this session.
    const statusText = useMemo(() => getConfigurationStatus(health?.statusOk ?? true, isShared), [health?.statusOk, isShared]);

    const handleCardClick = useCallback(() => {
      // `configuration.id` is a NUMBER on this wire (`ConfigurationItem.id`),
      // and the `as string` cast that used to sit here handed that number to
      // a `(configurationId: string)` callback — a declaration the value never
      // satisfied. `toConfigurationId` is the same narrowing the health dot
      // beside this title already uses. A row with no usable id opens nothing,
      // rather than routing to an edit URL with `undefined` in it.
      const configurationId = toConfigurationId(configuration.id);
      if (!disabled && configurationId !== '') {
        onClick?.(configurationId);
      }
    }, [disabled, onClick, configuration.id]);

    const iconType = getIconTypeKey(
      configuration.name as string | undefined,
      configuration.type as string | undefined,
      configuration.label as string | undefined,
    );

    return (
      <Box
        onClick={handleCardClick}
        sx={styles.cardContainer(disabled)}
      >
        <Box sx={styles.content}>
          <Box sx={styles.iconContainer}>
            <GradientIconWrapper size="2.75rem">
              <ProviderIcon iconType={iconType} />
            </GradientIconWrapper>
          </Box>
          <Box sx={styles.textContainer}>
            <Box sx={styles.titleRow}>
              <Typography variant="bodyMedium" color="text.secondary" sx={styles.displayName}>
                {displayName}
              </Typography>
              {disabled && (
                <Typography variant="bodySmall" color="text.disabled" sx={styles.disabledLabel}>
                  {t('ai-configuration.card.noPermissions', 'No edit permissions')}
                </Typography>
              )}
              <Box sx={styles.healthSlot}>
                <ConfigurationHealth
                  configurationId={toConfigurationId(configuration.id)}
                  health={health ?? UNCHECKED}
                  onRevalidate={onRevalidate}
                  isRevalidating={isRevalidating}
                />
              </Box>
            </Box>
            <Typography variant="bodySmall" color="text.default" sx={styles.statusText}>
              {statusText}
              {(configData.high_tier as boolean) && (
                <Typography variant="bodySmall" color="text.secondary" sx={styles.badge}>
                  {t('ai-configuration.card.highTier', 'High-Tier')}
                </Typography>
              )}
              {(configData.low_tier as boolean) && (
                <Typography variant="bodySmall" color="text.secondary" sx={styles.badge}>
                  {t('ai-configuration.card.lowTier', 'Low-Tier')}
                </Typography>
              )}
              {isDefault && (
                <Typography variant="bodySmall" color="text.secondary" sx={styles.badge}>
                  {t('ai-configuration.card.default', 'Default')}
                </Typography>
              )}
            </Typography>
          </Box>
        </Box>
      </Box>
    );
  },
);

function getStyles(theme: ReturnType<typeof useTheme>) {
  const t = theme as Theme;
  return {
    cardContainer: (disabled: boolean) => ({
      boxSizing: 'border-box',
      overflow: 'hidden',
      display: 'flex',
      flexDirection: 'row',
      justifyContent: 'space-between',
      alignItems: 'center',
      flex: '0 0 calc((100% - 1.5rem) / 3)',
      maxWidth: 'calc((100% - 1.5rem) / 3)',
      minWidth: '20rem',
      background: `linear-gradient(135deg, ${t.vars.palette.background.eliteaDefault} 0%, ${t.vars.palette.border.lines} 100%)`,
      border: '1px solid transparent',
      borderRadius: 'var(--el-shape-radiusMd, 8px)',
      padding: '0.5rem',
      cursor: disabled ? 'default' : 'pointer',
      transition: 'all 0.2s ease-in-out',
      '&:hover': !disabled ? {
        border: `1px solid ${t.vars.palette.scrollbar.thumb}`,
        backgroundColor: t.vars.palette.background.eliteaDefault,
      } : {},
      '@media (max-width: 1200px)': {
        flex: '0 0 calc((100% - 1.5rem) / 2)',
        maxWidth: 'calc((100% - 1.5rem) / 2)',
      },
      '@media (max-width: 960px)': {
        flex: '0 0 100%',
        maxWidth: '100%',
      },
    }),
    content: {
      display: 'flex',
      padding: '0.75rem 0.5rem',
      gap: '0.75rem',
      width: '100%',
      alignItems: 'center',
    },
    iconContainer: {
      flexShrink: 0,
    },
    textContainer: {
      display: 'flex',
      flexDirection: 'column',
      justifyContent: 'center',
      flex: 1,
      minWidth: 0,
    },
    titleRow: {
      display: 'flex',
      alignItems: 'center',
      gap: '0.5rem',
      minWidth: 0,
    },
    displayName: {
      overflow: 'hidden',
      textOverflow: 'ellipsis',
      whiteSpace: 'nowrap',
      fontWeight: 500,
    },
    disabledLabel: {
      marginLeft: 'auto',
      flexShrink: 0,
    },
    healthSlot: {
      marginLeft: 'auto',
      display: 'flex',
      alignItems: 'center',
      flexShrink: 0,
    },
    statusText: {
      overflow: 'hidden',
      textOverflow: 'ellipsis',
      display: 'flex',
      gap: '0.625rem',
      alignItems: 'center',
    },
    badge: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      borderRadius: 'var(--el-shape-radiusLg, 16px)',
      padding: '0.125rem 0.5rem',
    },
  };
}
