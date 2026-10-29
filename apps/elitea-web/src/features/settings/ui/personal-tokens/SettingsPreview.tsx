/**
 * IDE settings preview panel — shows generated VSCode / JetBrains config for
 * a given personal token.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/personal-tokes/
 * SettingsPreview.jsx`.
 *
 * Deviations:
 *  - Content renders through the shared read-only `CodeMirrorEditor`
 *    (`@/shared/ui/CodeMirrorEditor`), with `@codemirror/lang-json`
 *    highlighting for the VSCode branch; the JetBrains branch is XML, for
 *    which this app installs no CodeMirror language package, so it renders
 *    unhighlighted (same disclosed gap `features/toolkits`'
 *    `codeLanguageExtensions.ts` records).
 *  - Uses `@/shared/i18n` for i18n
 *  - Simpler copy/download UX — direct clipboard/file ops (no toast in shared/ui)
 *  - Uses `openEyeIcon` and `RemoveIcon` (existing in shared/ui/icons)
 *  - Close button uses "✕" text (close-icon doesn't exist)
 *  - Server URL resolves via `getConfig().config.vite_server_url` (falling
 *    back to `window.location.origin`) — old-app parity for tiers 2/3 of
 *    `user.api_url || VITE_SERVER_URL?.replace('/api/v2','') ||
 *    window.location.origin`. Tier 1, `user.api_url`, has no equivalent in
 *    this app's `AuthUser` context (`src/app/router-context.ts`, outside
 *    this cluster's file scope) and is dropped — needs a follow-up there
 *    (adding `api_url` to `AuthUser` and reading it here) for full parity.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import { json } from '@codemirror/lang-json';
import type { Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import SvgIcon from '@mui/material/SvgIcon';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import type { SxProps, Theme } from '@mui/material/styles';

import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';
import { OpenEyeIcon } from '@/shared/ui/icons/open-eye-icon';
import { t } from '@/shared/i18n';
import { getConfig } from '@/shared/config';
import { SETTINGS_PREVIEW_LABELS, SETTINGS_PREVIEW_TYPES } from '@/entities/token';

export interface SettingsPreviewProps {
  /** Whether the preview is open. */
  open: boolean;
  /** The raw token string. */
  token: string;
  /** Model configuration: { id, name } from useListModelsQuery. */
  model?: { id: string | number; name: string } | null;
  /** Current project ID for IDE settings. */
  projectId?: string;
  /** Callback when the user closes the panel. */
  onClose: () => void;
}

/** Generate VSCode settings JSON for the given values. */
function generateVSCodeSettings(
  token: string,
  model: SettingsPreviewProps['model'],
  projectId: string,
  serverUrl: string,
): string {
  return JSON.stringify(
    {
      'eliteacode.providerServerURL': serverUrl,
      'eliteacode.LLMServerUrl': serverUrl,
      'eliteacode.modelName': model?.name ?? '',
      'eliteacode.LLMModelName': model?.name ?? '',
      'eliteacode.authToken': token || 'Your_Personal_Token',
      'eliteacode.LLMAuthToken': token || 'Your_Personal_Token',
      'eliteacode.projectId': projectId || '',
      'eliteacode.integrationUid': model?.id ?? '',
      'eliteacode.defaultViewMode': 'split',
      'eliteacode.verifySsl': false,
      'eliteacode.displayType': 'split',
      'eliteacode.debug': false,
    },
    null,
    2,
  );
}

/** Generate JetBrains .idea settings XML for the given values. */
function generateJetBrainsSettings(
  model: SettingsPreviewProps['model'],
  projectId: string,
  serverUrl: string,
): string {
  return `<?xml version="1.0" encoding="UTF-8"?>
<project version="4">
  <component name="EliteASettings">
    <option name="displayType" value="SPLIT" />
    <option name="integrationName" value="${model?.name ?? ''}" />
    <option name="integrationUid" value="${model?.id ?? ''}" />
    <option name="llmCustomModelEnabled" value="true" />
    <option name="llmCustomModelName" value="${model?.name ?? ''}" />
    <option name="llmServerUrl" value="${serverUrl}" />
    <option name="projectId" value="${projectId ?? ''}" />
    <option name="provider" value="ELITEA_EYE" />
  </component>
</project>`;
}

export const SettingsPreview = memo(function SettingsPreview({
  open,
  onClose,
  token,
  model,
  projectId,
}: SettingsPreviewProps) {
  const theme = useTheme();
  const styles = getStyles();

  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const ideOptions = useMemo(
    () =>
      Object.values(SETTINGS_PREVIEW_TYPES).map((type) => ({
        label: SETTINGS_PREVIEW_LABELS[type],
        value: type,
      })),
    [],
  );

  const [selectedIDE, setSelectedIDE] = useState<'vscode' | 'jetbrains'>('vscode');

  /** old-app: `user.api_url || VITE_SERVER_URL?.replace('/api/v2','') || window.location.origin` — see file-header deviation note for the dropped first tier. */
  const serverUrl = useMemo(() => {
    const config = getConfig();
    const viteServerUrl = config.status === 'ok' ? config.config.vite_server_url : undefined;
    return viteServerUrl?.replace('/api/v2', '') || window.location.origin;
  }, []);

  const settingsContent = useMemo(() => {
    if (selectedIDE === 'vscode') {
      return generateVSCodeSettings(token, model, projectId ?? '', serverUrl);
    }
    return generateJetBrainsSettings(model, projectId ?? '', serverUrl);
  }, [selectedIDE, token, model, projectId, serverUrl]);

  /** Only `@codemirror/lang-json` is installed in this app — the JetBrains XML branch gets a plain, unhighlighted editor. */
  const editorExtensions = useMemo<Extension[]>(
    () => (selectedIDE === 'vscode' ? [json(), EditorView.lineWrapping] : [EditorView.lineWrapping]),
    [selectedIDE],
  );

  const handleCopy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(settingsContent);
    } catch {
      // clipboard write failed — no toast in shared/ui
    }
  }, [settingsContent]);

  const handleDownload = useCallback(() => {
    const ext = selectedIDE === 'vscode' ? 'json' : 'xml';
    const mimeType = selectedIDE === 'vscode' ? 'application/json' : 'application/xml';
    const element = document.createElement('a');
    const file = new Blob([settingsContent], { type: mimeType });
    element.href = URL.createObjectURL(file);
    element.download = `settings.${ext}`;
    document.body.appendChild(element);
    element.click();
    document.body.removeChild(element);
    URL.revokeObjectURL(element.href);
  }, [settingsContent, selectedIDE]);

  if (!open) return null;

  const ideLabel = SETTINGS_PREVIEW_LABELS[selectedIDE] || selectedIDE;

  return (
    <Box sx={styles.root}>
      <Box sx={styles.header}>
        <Box sx={styles.headerLeft}>
          <IconButton
            size="small"
            onClick={onClose}
            aria-label={t('entities.token.preview.close', 'Close preview')}
          >
            <span style={{ fontSize: theme.typography.headingSmall.fontSize }}>{t('common.close', '✕')}</span>
          </IconButton>
          <Typography
            variant="headingSmall"
            color="text.secondary"
            sx={styles.title}
          >
            {`${ideLabel} ${t('entities.token.preview.settingsLabel', 'Settings')}`}
          </Typography>
        </Box>
        <Box sx={styles.headerRight}>
          {/* IDE selector */}
          {/* Describes, not names: the button shows the IDE's name (axe label-content-name-mismatch, WCAG 2.5.3). */}
          <Tooltip
            title={t('entities.token.preview.selectIde', 'Select IDE')}
            describeChild
          >
            <IconButton
              size="small"
              onClick={(e) => setAnchorEl(e.currentTarget)}
            >
              <Typography variant="bodySmall">{ideLabel}</Typography>
            </IconButton>
          </Tooltip>
          <Menu
            anchorEl={anchorEl}
            open={Boolean(anchorEl)}
            onClose={() => setAnchorEl(null)}
          >
            {ideOptions.map((opt) => (
              <MenuItem
                key={opt.value}
                selected={opt.value === selectedIDE}
                onClick={() => {
                  setSelectedIDE(opt.value);
                  setAnchorEl(null);
                }}
              >
                {opt.label}
              </MenuItem>
            ))}
          </Menu>

          <Tooltip title={t('entities.token.preview.copy', 'Copy settings')}>
            <IconButton
              size="small"
              onClick={() => void handleCopy()}
            >
              <SvgIcon
                component={OpenEyeIcon}
                inheritViewBox
                sx={{ width: '0.875rem', height: '0.875rem' }}
              />
            </IconButton>
          </Tooltip>

          <Tooltip title={t('entities.token.preview.download', 'Download settings')}>
            <IconButton
              size="small"
              onClick={handleDownload}
            >
              <SvgIcon
                component={RemoveIcon}
                inheritViewBox
                sx={{ width: '0.875rem', height: '0.875rem' }}
              />
            </IconButton>
          </Tooltip>
        </Box>
      </Box>
      <Box sx={styles.content}>
        <CodeMirrorEditor
          value={settingsContent}
          readOnly
          extensions={editorExtensions}
          height="100%"
          minHeight="100%"
          aria-label={t('entities.token.preview.editorAriaLabel', 'IDE settings content')}
        />
      </Box>
    </Box>
  );
});

const getStyles = (): {
  root: SxProps<Theme>;
  header: SxProps<Theme>;
  headerLeft: SxProps<Theme>;
  headerRight: SxProps<Theme>;
  title: SxProps<Theme>;
  content: SxProps<Theme>;
} => ({
  root: {
    height: '100%',
    display: 'flex',
    flexDirection: 'column',
    borderRadius: 'var(--el-shape-radiusSm, 4px)',
    overflow: 'hidden',
    minHeight: 0,
    minWidth: '18.75rem',
  },
  header: (theme) => ({
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: '0.5rem',
    padding: '0.5rem 0.75rem',
    backgroundColor: theme.vars.palette.background.tabPanel,
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    minHeight: '3.75rem',
    flexShrink: 0,
  }),
  headerLeft: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    flex: 1,
    minWidth: 0,
  },
  headerRight: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
  },
  title: {
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
  },
  content: (theme) => ({
    flex: 1,
    overflow: 'hidden',
    position: 'relative',
    backgroundColor: theme.vars.palette.background.paper,
    minHeight: 0,
  }),
});
