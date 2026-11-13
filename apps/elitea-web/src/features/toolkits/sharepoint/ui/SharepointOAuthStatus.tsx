import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { OnlineIcon } from '@/shared/ui/icons/online-icon';

import { logout, setConnectionVerified } from '../lib/helpers/mcpTokenStorage.helpers';
import type { SharepointConfigRef, SharepointResolvedConfig } from '../lib/hooks/useResolvedSharepointConfig.hooks';
import { useResolvedSharepointConfig } from '../lib/hooks/useResolvedSharepointConfig.hooks';
import { useSharepointCheckConnection } from '../lib/hooks/useSharepointCheckConnection.hooks';
import { useSharepointTokenStatus } from '../lib/hooks/useSharepointTokenStatus.hooks';
import type { SharepointAuthModalSlotProps } from '../lib/hooks/useSharepointAuthModal.hooks';
import { useSharepointAuthModal } from '../lib/hooks/useSharepointAuthModal.hooks';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/sharepoint/ui/
 * SharepointOAuthStatus.jsx` (Wave-2 unit A4e) — the connected/not-connected
 * pill + Login/Logout button shown inside a SharePoint toolkit's settings
 * form, with a logout confirmation step.
 *
 * DISCLOSED REDESIGNS:
 *  1. **No `useFormikContext()`.** This app has no Formik dependency (see
 *     `entities/application-form`'s own precedent). `values`/`projectId`
 *     are explicit props instead — the caller (a toolkit-settings form
 *     outside this sub-unit's scope) supplies its current field values,
 *     same "values prop instead of ambient form context" convention
 *     `features/mcps/ui/McpAuthStatusBadge.tsx`'s own doc comment already
 *     established for the analogous MCP case.
 *  2. **No `useToast()`.** `onLogoutSuccess` is an optional callback
 *     instead — this app has no toast implementation anywhere yet (grepped:
 *     zero real call sites), same "caller renders it" convention every
 *     other hook in this session already follows.
 *  3. **The OAuth login modal (`McpAuthModal`) and the logout confirmation
 *     modal (`McpLogoutModal`) are both injected render-prop slots**, not
 *     rendered directly — `no-sideways-features` forbids importing either
 *     from `features/mcps`. See `../lib/hooks/useSharepointAuthModal.hooks.ts`'s
 *     doc comment for the full rationale (mirrors `entities/application-
 *     form`'s `ApplicationConfigurationLayout` slot precedent). The logout
 *     confirmation itself (`showLogoutModal` state, `onConfirmLogout`
 *     calling the local `logout()` storage helper) is real, working logic —
 *     only the MODAL CHROME is a slot.
 *
 * **WIRED (was dead).** Until this change no caller anywhere in `src/`
 * mounted this component at all, let alone filled `renderAuthModal` — the
 * settings-form path stopped three levels up (`ToolkitForm.hooks.ts` had no
 * `slots` concept, so `ToolBase`'s `slots.sharepointOAuthStatus` was never
 * supplied either), so a SharePoint delegated login could never obtain a
 * token and the only thing that ever reported "connected" was
 * `setConnectionVerified`'s own non-delegated header-auth sentinel. The live
 * chain is now `pages/toolkits/EditToolkit.tsx` ->
 * `ConfigurationTab(sharepointAuth=...)` -> `ToolkitForm(slots=...)` ->
 * `ToolBase` -> this component -> a real `<McpAuthModal>`, and
 * `pages/toolkits/__tests__/sharepointOAuthWiring.test.tsx` asserts it from
 * the page down, supplying no slot of its own.
 */
export interface SharepointOAuthStatusValues {
  readonly id?: string;
  readonly settings?: {
    readonly sharepoint_configuration?: SharepointConfigRef;
  };
}

/**
 * Module-private, and it stays that way: `SharepointAuthModalRenderers` below
 * (which IS exported, and is what `ToolBaseSlots.sharepointAuthModals` names)
 * references it, so every consumer reaches this shape structurally through
 * that type without importing it by name — the same reasoning
 * `ui/list/ToolkitsList.tsx`'s own `ToolkitsListState`/`ToolkitsListTypeFilter`
 * doc comment gives. Exporting it as well trips the knip dead-code gate, which
 * is right: an export nothing imports is an advertised seam with no consumer.
 */
interface SharepointLogoutModalSlotProps {
  readonly serverUrl: string;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onConfirm: () => void;
}

/**
 * The two `features/mcps`-owned modals this component cannot import
 * (`no-sideways-features`), as one injectable pair. The real supplier is
 * `pages/toolkits/lib/sharepointAuthModals.tsx`, which renders the actual
 * `<McpAuthModal>`/`<McpLogoutModal>`; it reaches this component through
 * `ConfigurationTab` -> `ToolkitForm` -> `ToolBase` (see
 * `ToolBase.render.tsx`'s `ToolBaseStatusSlots`).
 *
 * Omitting the pair is still legal and still degrades gracefully — but it is
 * no longer the ONLY thing that happens in production, which is what made
 * these slots dead wiring before: nothing anywhere in `src/` supplied them,
 * so "Login" could open nothing and no SharePoint OAuth token was ever
 * obtained.
 */
export interface SharepointAuthModalRenderers {
  readonly renderAuthModal?: (slotProps: SharepointAuthModalSlotProps) => ReactNode;
  readonly renderLogoutModal?: (slotProps: SharepointLogoutModalSlotProps) => ReactNode;
}

export interface SharepointOAuthStatusProps {
  readonly values: SharepointOAuthStatusValues;
  readonly projectId: string | undefined;
  readonly onLogoutSuccess?: () => void;
  /** Renders a real `McpAuthModal` (`features/mcps`) spread with these slot props. */
  readonly renderAuthModal?: (slotProps: SharepointAuthModalSlotProps) => ReactNode;
  /** Renders a real `McpLogoutModal` (`features/mcps`) spread with these slot props. */
  readonly renderLogoutModal?: (slotProps: SharepointLogoutModalSlotProps) => ReactNode;
}

function statusContentSx(isLoggedIn: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(1),
    color: isLoggedIn ? theme.vars.palette.icon.fill.success : theme.vars.palette.icon.fill.attention,
  });
}

function loginStatusTextSx(isLoggedIn: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    color: isLoggedIn ? theme.vars.palette.text.mcp.loginSuccess : theme.vars.palette.text.mcp.logout,
  });
}

/** Only the `client_id`/`client_secret`/`scopes` keys `spConfig` actually defines — `exactOptionalPropertyTypes` forbids an explicit `undefined` value on an optional field, so absent keys are OMITTED, not set to `undefined`. Split out to keep the component's own cyclomatic complexity under the §3.5 budget. */
function buildSharepointCredentials(spConfig: SharepointResolvedConfig | null): {
  readonly client_id?: string;
  readonly client_secret?: string;
  readonly scopes?: string | readonly string[];
} {
  return {
    ...(spConfig?.client_id !== undefined && { client_id: spConfig.client_id }),
    ...(spConfig?.client_secret !== undefined && { client_secret: spConfig.client_secret }),
    ...(spConfig?.scopes !== undefined && { scopes: spConfig.scopes }),
  };
}

/** The button's own label — split out (same §3.5 reason as `buildSharepointCredentials`). */
function resolveActionLabel(isOAuthLoggedIn: boolean, isRunning: boolean): string {
  if (isOAuthLoggedIn) return t('features.toolkits.sharepointOAuthStatus.logout', 'Logout');
  if (isRunning) return t('features.toolkits.sharepointOAuthStatus.loggingIn', 'Logging in...');
  return t('features.toolkits.sharepointOAuthStatus.login', 'Login');
}

function loginStatusContainerSx(isDelegated: boolean, isLoggedIn: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: isDelegated ? 'flex' : 'none',
    marginTop: theme.spacing(2),
    height: '2.75rem',
    width: '100%',
    alignItems: 'center',
    gap: theme.spacing(1),
    marginBottom: theme.spacing(2),
    padding: theme.spacing(0.5, 0.5, 0.5, 1),
    borderRadius: theme.vars.shape.radiusPill ?? 9999,
    backgroundColor: isLoggedIn ? theme.vars.palette.background.mcp.loginSuccess : theme.vars.palette.background.mcp.logout,
    border: `0.0625rem solid ${isLoggedIn ? theme.vars.palette.border.mcp.loginSuccess : theme.vars.palette.border.mcp.logout}`,
    justifyContent: 'space-between',
  });
}

export function SharepointOAuthStatus({
  values,
  projectId,
  onLogoutSuccess,
  renderAuthModal,
  renderLogoutModal,
}: SharepointOAuthStatusProps): ReactNode {
  const spConfigRef = values.settings?.sharepoint_configuration;
  const toolkitId = values.id;
  const { spConfig, oauthEndpoint, oauthTokenKey, connectionTokenKey } = useResolvedSharepointConfig(spConfigRef, projectId);

  const { isLoggedIn: isOAuthLoggedIn } = useSharepointTokenStatus(connectionTokenKey);
  const [showLogoutModal, setShowLogoutModal] = useState(false);

  const onNonDelegatedSuccess = useCallback(() => {
    if (connectionTokenKey) setConnectionVerified(connectionTokenKey);
  }, [connectionTokenKey]);

  const { showModal, handleConfigAuthRequired, modalProps } = useSharepointAuthModal({
    projectId,
    toolkitId,
    credentials: buildSharepointCredentials(spConfig),
  });

  const { runCheck, isRunning, error: checkError } = useSharepointCheckConnection({
    projectId,
    spConfig,
    ...(oauthEndpoint === '' && { onSuccess: onNonDelegatedSuccess }),
  });

  const onLogin = useCallback(() => {
    void runCheck(handleConfigAuthRequired, oauthTokenKey);
  }, [runCheck, handleConfigAuthRequired, oauthTokenKey]);

  const onLogout = useCallback(() => setShowLogoutModal(true), []);
  const onConfirmLogout = useCallback(() => {
    logout(oauthTokenKey);
    setShowLogoutModal(false);
    onLogoutSuccess?.();
  }, [oauthTokenKey, onLogoutSuccess]);
  const onCloseLogoutModal = useCallback(() => setShowLogoutModal(false), []);

  const containerSx = useMemo(() => loginStatusContainerSx(oauthEndpoint !== '', isOAuthLoggedIn), [oauthEndpoint, isOAuthLoggedIn]);
  const contentSx = useMemo(() => statusContentSx(isOAuthLoggedIn), [isOAuthLoggedIn]);
  const textSx = useMemo(() => loginStatusTextSx(isOAuthLoggedIn), [isOAuthLoggedIn]);

  // No credential configured at all — nothing to render (after every hook has run).
  if (!spConfig) return null;

  return (
    <>
      <Box sx={containerSx}>
        <Box sx={contentSx}>
          <OnlineIcon
            width={14}
            height={14}
          />
          <Typography
            variant="bodySmall"
            sx={textSx}
          >
            {isOAuthLoggedIn
              ? t('features.toolkits.sharepointOAuthStatus.connected', 'Connected!')
              : t('features.toolkits.sharepointOAuthStatus.notConnected', 'Not Connected')}
          </Typography>
        </Box>
        <BaseBtn
          onClick={isOAuthLoggedIn ? onLogout : onLogin}
          disabled={isRunning}
          variant="secondary"
        >
          {resolveActionLabel(isOAuthLoggedIn, isRunning)}
        </BaseBtn>
      </Box>
      {checkError !== undefined && (
        <Typography
          variant="bodySmall"
          role="alert"
          sx={{ color: 'error.main', marginBottom: 1 }}
        >
          {checkError}
        </Typography>
      )}
      {showModal && renderAuthModal && renderAuthModal(modalProps)}
      {renderLogoutModal &&
        renderLogoutModal({
          serverUrl: oauthTokenKey,
          open: showLogoutModal,
          onClose: onCloseLogoutModal,
          onConfirm: onConfirmLogout,
        })}
    </>
  );
}
