import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { VersionSummary } from '@/entities/version';
import { t } from '@/shared/i18n';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { VersionIcon } from '@/shared/ui/icons/version-icon';

import { useEditorStateStore } from '@/shared/lib/editorState';

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-input/
 * VersionSelector.jsx` (unit C3, "chat-input" cluster — composed inside
 * `AgentEditorPanel.tsx`, which is composed inside `NewChatInput.tsx`).
 *
 * Reuses `entities/version`'s `LATEST_VERSION_NAME`/`sortVersionsForPicker`/
 * `isVersionNotFound` — wait, none of those are actually needed by THIS
 * component directly (the baseline `VersionSelector.jsx` itself does no
 * default-resolution or sorting of its own; it renders whatever `versions`
 * array it is handed, in order, and `AgentEditorPanel.tsx` is the one that
 * resolves `selectedVersion` via `entities/version`'s `selectDefaultVersion`
 * before ever calling this component). Kept as a note, not a red herring:
 * confirmed by reading the baseline in full before starting this port.
 *
 * Two disclosed baseline bug fixes (not reproduced):
 *  1. `versionSelectorStyles().isonButton` — a typo for `iconButton`; the
 *     JSX referenced `styles.iconButton` (undefined), so the refresh
 *     button's own sx NEVER applied in production. Fixed here: the sx key
 *     is spelled correctly and wired to the button that actually uses it.
 *  2. `handleRefresh`'s `setTimeout(() => setIsRefreshing(false), 500)` had
 *     no cleanup — if the component unmounted (or `onRefresh` changed)
 *     before the timer fired, it would call `setIsRefreshing` on an
 *     unmounted component. Fixed with a ref-tracked cleanup (`clearTimeout`
 *     on unmount / re-trigger).
 *
 * The baseline's `useNavBlocker()` Redux read (`isEditingAgent`/
 * `isEditingPipeline` -> `isAnyEditorOpen`) maps directly to this
 * codebase's already-built `shared/lib/editorState.ts` (`useEditorStateStore`,
 * unit C1) — read there instead of re-deriving.
 */
export interface VersionSelectorProps {
  readonly versions: readonly VersionSummary[];
  readonly selectedVersion: VersionSummary | undefined;
  readonly onSelect: (version: VersionSummary) => void;
  readonly onCloseEditor?: (() => void) | undefined;
  readonly isEditorDirty?: boolean | undefined;
  /** Baseline: `onShowVersionChangeAlert(proceed)` — shows a confirm dialog and calls `proceed()` if the user confirms discarding unsaved changes. */
  readonly onShowVersionChangeAlert?: ((proceed: () => void) => void) | undefined;
  readonly isSmallView?: boolean | undefined;
  readonly onRefresh?: (() => Promise<void> | void) | undefined;
}

const REFRESH_MIN_SPINNER_MS = 500;

function useIsEditorOpenForNav(): boolean {
  const isEditingAgent = useEditorStateStore((s) => s.isEditingAgent);
  const isEditingPipeline = useEditorStateStore((s) => s.isEditingPipeline);
  return isEditingAgent || isEditingPipeline;
}

function useHandleRefresh(onRefresh: VersionSelectorProps['onRefresh']): {
  readonly isRefreshing: boolean;
  readonly handleRefresh: (event?: { stopPropagation?: () => void }) => Promise<void>;
} {
  const [isRefreshing, setIsRefreshing] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current);
    },
    [],
  );

  const handleRefresh = useCallback(
    async (event?: { stopPropagation?: () => void }) => {
      event?.stopPropagation?.();
      if (!onRefresh) return;
      setIsRefreshing(true);
      try {
        await onRefresh();
      } finally {
        if (timerRef.current) clearTimeout(timerRef.current);
        timerRef.current = setTimeout(() => setIsRefreshing(false), REFRESH_MIN_SPINNER_MS);
      }
    },
    [onRefresh],
  );

  return { isRefreshing, handleRefresh };
}

function RefreshVersionsHeader({
  isRefreshing,
  onRefresh,
}: {
  readonly isRefreshing: boolean;
  readonly onRefresh: (event?: { stopPropagation?: () => void }) => Promise<void>;
}): ReactNode {
  return (
    <Box sx={refreshWrapperSx}>
      <Typography
        variant="labelSmall"
        sx={versionsLabelSx}
      >
        {t('chatInput.versionSelector.versions', 'Versions')}
      </Typography>
      <Tooltip
        title={t('chatInput.versionSelector.refresh', 'Refresh versions')}
        placement="top"
      >
        <IconButton
          color="tertiary"
          size="small"
          onClick={(event) => void onRefresh(event)}
          disabled={isRefreshing}
          sx={iconButtonSx}
          aria-label={t('chatInput.versionSelector.refresh', 'Refresh versions')}
        >
          {isRefreshing ? <CircularProgress size={12} /> : <RefreshIcon style={refreshIconStyle} />}
        </IconButton>
      </Tooltip>
    </Box>
  );
}

export function VersionSelector(props: VersionSelectorProps): ReactNode {
  const { versions, selectedVersion, onSelect, onCloseEditor, isEditorDirty, onShowVersionChangeAlert, isSmallView, onRefresh } = props;

  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const menuOpen = Boolean(anchorEl);
  const isAnyEditorOpen = useIsEditorOpenForNav();
  const { isRefreshing, handleRefresh } = useHandleRefresh(onRefresh);

  const handleClose = useCallback(() => setAnchorEl(null), []);
  const handleMenuClick = useCallback((event: { currentTarget: HTMLElement }) => setAnchorEl(event.currentTarget), []);

  const commitSelection = useCallback(
    (index: number) => {
      const version = versions[index];
      if (onCloseEditor) onCloseEditor();
      if (version) onSelect(version);
      handleClose();
    },
    [versions, onCloseEditor, onSelect, handleClose],
  );

  const handleVersionSelect = useCallback(
    (index: number) => {
      if (isAnyEditorOpen && isEditorDirty && onShowVersionChangeAlert) {
        onShowVersionChangeAlert(() => commitSelection(index));
        return;
      }
      commitSelection(index);
    },
    [isAnyEditorOpen, isEditorDirty, onShowVersionChangeAlert, commitSelection],
  );

  return (
    <>
      <Tooltip
        placement="top"
        title={t('chatInput.versionSelector.tooltip', 'Version selector')}
      >
        <Button
          size="small"
          variant="elitea"
          color="secondary"
          aria-expanded={menuOpen ? 'true' : undefined}
          aria-label={t('chatInput.versionSelector.menuLabel', 'version selector menu')}
          aria-haspopup="menu"
          onClick={handleMenuClick}
        >
          {isSmallView ? <VersionIcon style={versionIconStyle} /> : selectedVersion?.name}
        </Button>
      </Tooltip>
      <Menu
        anchorEl={anchorEl}
        open={menuOpen}
        onClose={handleClose}
      >
        {onRefresh && (
          <RefreshVersionsHeader
            isRefreshing={isRefreshing}
            onRefresh={handleRefresh}
          />
        )}
        {versions.map((version, index) => (
          <MenuItem
            key={version.id}
            selected={version.id === selectedVersion?.id}
            onClick={() => handleVersionSelect(index)}
          >
            {version.name}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}

const versionIconStyle = { width: '1rem', height: '1rem' };
const refreshIconStyle = { width: '.75rem', height: '.75rem' };

const refreshWrapperSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  borderBottom: `.0625rem solid ${theme.vars.palette.border.lines}`,
  minHeight: '1.75rem',
  padding: '.25rem .75rem',
});

const versionsLabelSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  textTransform: 'uppercase',
});

const iconButtonSx: SxProps<Theme> = { padding: '.125rem', minWidth: '1.25rem', width: '1.25rem', height: '1.25rem' };
