import { memo, useCallback, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { OfflineIcon } from '@/shared/ui/icons/offline-icon';

import type { PickerToolEntry, ToolkitListRow } from '../../lib/tool-picker/toolPicker.types';

export interface PickerMcpStatusProps {
  readonly mcp?: PickerToolEntry | ToolkitListRow | null | undefined;
  /** Override for client-side MCP token login state when known by the host. */
  readonly isLoggedIn?: boolean | undefined;
  /** Optional click handler for inline "Log in" (e.g. opening the host MCP auth modal). */
  readonly onLoginClick?: ((mcp: PickerToolEntry | ToolkitListRow) => void) | undefined;
  /** Optional custom login action slot (e.g. `McpLogInButton` injected from a higher layer). */
  readonly renderLoginAction?: ((mcp: PickerToolEntry | ToolkitListRow) => ReactNode) | undefined;
  readonly 'data-testid'?: string | undefined;
}

const rootSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  flexShrink: 0,
});

const offlineSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  color: theme.vars.palette.icon.fill.attention,
  '& svg': { width: '1rem', height: '1rem' },
});

const loginBtnSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: 0,
  paddingBlock: 0,
  paddingInline: theme.spacing(0.5),
});

/** Connection state of an MCP with an inline "Log in" action, matching the tool card. */
export const PickerMcpStatus = memo(function PickerMcpStatus({
  mcp,
  isLoggedIn = false,
  onLoginClick,
  renderLoginAction,
  'data-testid': testId,
}: PickerMcpStatusProps): ReactNode {
  const stopPropagation = useCallback((event: SyntheticEvent) => {
    event.stopPropagation();
  }, []);

  const handleLogin = useCallback(() => {
    if (mcp) onLoginClick?.(mcp);
  }, [mcp, onLoginClick]);

  if (!mcp || mcp.online || isLoggedIn) return null;

  const mcpName = mcp.name || 'MCP';
  const disconnectedTitle = t(
    'pipelines.toolPicker.mcpDisconnected',
    'The {{name}} server is disconnected. Log in to use it.',
    { name: mcpName },
  );

  return (
    <Box
      sx={rootSx}
      onClick={stopPropagation}
      onMouseDown={stopPropagation}
      onKeyDown={stopPropagation}
      data-testid={testId}
    >
      <Tooltip title={disconnectedTitle} placement="top">
        <Box sx={offlineSx}>
          <OfflineIcon />
        </Box>
      </Tooltip>
      {renderLoginAction ? (
        renderLoginAction(mcp)
      ) : onLoginClick ? (
        <BaseBtn
          variant={BUTTON_VARIANTS.tertiary}
          onClick={handleLogin}
          sx={loginBtnSx}
          data-testid={testId ? `${testId}-login` : undefined}
        >
          {t('pipelines.toolPicker.mcpLogin', 'Log in')}
        </BaseBtn>
      ) : null}
    </Box>
  );
});
