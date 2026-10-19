/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-continue/
 * ChatContinue.jsx` — renders a "Continue" button for resuming MCP/agent
 * execution that was paused.
 *
 * The real `McpAuthModal` (`features/mcps/ui/McpAuthModal.tsx`) cannot be
 * imported here (`no-sideways-features` — `chat-messages` -> `mcps` is a
 * forbidden sideways feature import) and this component has no page/
 * composition-root consumer yet to legally unlock it (the pattern other
 * Wave-2 units use — see `features/chat-input/ui/NewChatInput.types.ts`'s
 * `NewChatInputSlots` doc comment for the precedent). So this component:
 *  - re-implements `extractMcpAuthMetadata`'s field-priority extraction
 *    locally (`extractLocalMcpAuthMetadata` below) against a local,
 *    structurally-equivalent source type, rather than importing the real
 *    one from `features/mcps/lib/discoveryMetadata.ts`;
 *  - accepts a `renderAuthModal` slot a future page/composition-root fills
 *    with the real `McpAuthModal`, instead of hard-importing it.
 *
 * Behavior this restores vs. the pre-fix version: clicking "Authorize"
 * only OPENS the auth modal (via the slot) — `onAuthSuccess` fires
 * exclusively from the modal's own close callback, and only on a real
 * successful login (baseline: `ChatContinue.jsx`'s `handleCloseModal`).
 */
import type { ReactNode } from 'react';
import { useCallback, useId, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { ArrowForwardIcon } from '@/shared/ui/icons/arrow-forward-icon';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

import {
  buildToolkitAuthorizationMessage,
  getToolkitAuthorizationContext,
  type ToolkitAuthorizationContext,
} from '../../lib/toolkitAuthorizationContext';

/**
 * Loosely-typed source an MCP-auth-required tool action carries its OAuth
 * discovery metadata in — a local, structurally-equivalent re-declaration of
 * `features/mcps/lib/discoveryMetadata.ts`'s `McpAuthMetadataSource`
 * (cannot import it — see module doc). Every field optional/defensive, same
 * posture as that source.
 */
export interface McpAuthRequiredAction {
  readonly id?: string;
  readonly name?: string;
  readonly authorizationRequestId?: string;
  readonly response_metadata?: {
    readonly resource_metadata?: RawMcpResourceMetadata;
    readonly provided_settings?: McpProvidedSettingsLocal;
    readonly authorization_servers?: readonly string[];
    readonly toolkit_id?: string;
  };
  readonly toolMeta?: {
    readonly authorization_servers?: readonly string[];
    readonly resource_metadata?: RawMcpResourceMetadata;
    readonly provided_settings?: McpProvidedSettingsLocal;
    readonly toolkit_id?: string;
    readonly server_url?: string;
    readonly toolkit_type?: string;
  };
  readonly toolOutputs?: {
    readonly authorization_servers?: readonly string[];
    readonly server_url?: string;
  };
}

interface RawMcpResourceMetadata {
  readonly oauth_authorization_server?: Record<string, unknown>;
  readonly authorization_servers?: readonly string[];
  readonly provided_settings?: McpProvidedSettingsLocal;
  readonly scopes_supported?: readonly string[];
  readonly configuration_uuid?: string;
  readonly toolkit_id?: string;
}

interface McpProvidedSettingsLocal {
  readonly mcp_client_id?: string;
  readonly mcp_client_secret?: string;
  readonly scopes?: string | readonly string[];
}

/** The shape the auth-modal slot receives — what the real `McpAuthModal` needs to configure itself. */
export interface McpAuthMetadataLocal {
  readonly authServers?: readonly string[] | undefined;
  readonly oauthAuthorizationServer?: Record<string, unknown> | undefined;
  readonly providedSettings?: McpProvidedSettingsLocal | undefined;
  readonly resourceScopes?: readonly string[] | undefined;
  readonly configurationUuid?: string | undefined;
  readonly toolkitId?: string | undefined;
}

/**
 * Local re-implementation of `extractMcpAuthMetadata`'s field-priority
 * chain (`response_metadata` -> `toolMeta` -> `resource_metadata` fallback
 * order) — see module doc for why this can't just import the real one.
 */
function extractLocalMcpAuthMetadata(source: McpAuthRequiredAction | null | undefined): McpAuthMetadataLocal {
  const responseMetadata = source?.response_metadata ?? {};
  const toolMeta = source?.toolMeta ?? {};
  const toolOutputs = source?.toolOutputs ?? {};
  const resourceMetadata = responseMetadata.resource_metadata ?? toolMeta.resource_metadata ?? {};

  return {
    authServers:
      resourceMetadata.authorization_servers ?? responseMetadata.authorization_servers
        ?? toolMeta.authorization_servers ?? toolOutputs.authorization_servers,
    oauthAuthorizationServer: resourceMetadata.oauth_authorization_server,
    providedSettings: responseMetadata.provided_settings ?? toolMeta.provided_settings ?? resourceMetadata.provided_settings,
    resourceScopes: resourceMetadata.scopes_supported,
    configurationUuid: resourceMetadata.configuration_uuid,
    toolkitId: responseMetadata.toolkit_id ?? toolMeta.toolkit_id ?? resourceMetadata.toolkit_id,
  };
}

/** Props the `renderAuthModal` slot receives — a future caller maps these onto the real `McpAuthModal`. */
export interface ChatContinueAuthModalSlotProps {
  readonly open: boolean;
  readonly mcpAuthMetadata: McpAuthMetadataLocal | null;
  readonly serverUrl: string | undefined;
  readonly tokenStorageKey: string | undefined;
  readonly toolkitId: string | undefined;
  readonly toolkitType: string | undefined;
  /** `success` true only on a real completed login — the only case that should trigger `onAuthSuccess`. */
  readonly onClose: (success?: boolean) => void;
  readonly onCancel: () => void;
}

/** @public Props for `ChatContinue`. */
export interface ChatContinueProps {
  /** Skip the authorization for this run (the auth panel's second button). Falls back to `onContinue`. */
  readonly onContinueWithoutAuth?: (() => void) | undefined;
  /** Called after a real, successful authentication (never on the initial "Authorize" click). */
  readonly onAuthSuccess?: (() => void) | undefined;
  /** Called when the user requests to continue. */
  readonly onContinue?: (() => void) | undefined;
  /** Whether authentication is required. */
  readonly authRequired?: boolean;
  /** Whether confirmation is required. */
  readonly requiresConfirmation?: boolean;
  /** Whether the buttons are disabled. */
  readonly disabled?: boolean;
  /** The confirmation branch's text, and the auth message when no action context exists. */
  readonly message?: string | undefined;
  /** The auth panel's second button label (baseline default `Continue`). */
  readonly continueLabel?: string | undefined;
  /** The MCP-auth-required tool action/message to extract OAuth metadata from — baseline's `authRequiredAction` prop. */
  readonly authRequiredAction?: McpAuthRequiredAction | undefined;
  /** Renders the real auth modal when open — see module doc for why this is a slot, not a hard import. */
  readonly renderAuthModal?: ((props: ChatContinueAuthModalSlotProps) => ReactNode) | undefined;
}

/** Configured OAuth requires discovered endpoints; never guess them when discovery failed. */
function hasUsableAuthDetails(metadata: McpAuthMetadataLocal | null): boolean {
  if ((metadata?.authServers?.length ?? 0) === 0) return false;
  if (!metadata?.providedSettings) return true;
  return (
    typeof metadata.oauthAuthorizationServer?.['authorization_endpoint'] === 'string' &&
    typeof metadata.oauthAuthorizationServer?.['token_endpoint'] === 'string'
  );
}

/** The collapsible "Authorization details" rows of the auth panel. */
function AuthorizationDetails({ context }: { readonly context: ToolkitAuthorizationContext | null }): ReactNode {
  const [showDetails, setShowDetails] = useState(false);
  const detailsId = useId();
  if (!context) return null;
  const rows = [
    [t('features.chatMessages.toolkitAuthorization.toolkitServer', 'Toolkit server'), context.serverUrl],
    [t('features.chatMessages.toolkitAuthorization.resourceMetadata', 'Resource metadata'), context.resourceMetadataUrl],
    [t('features.chatMessages.toolkitAuthorization.authorizationServer', 'Authorization server'), context.authorizationServers.join(', ')],
    [t('features.chatMessages.toolkitAuthorization.scopes', 'Scopes'), context.scopes.join(', ')],
  ].filter(([, value]) => value);
  if (rows.length === 0) return null;
  const toggle = (): void => setShowDetails((current) => !current);

  return (
    <Box sx={detailsWrapperSx}>
      <Box
        component="button"
        type="button"
        aria-expanded={showDetails}
        aria-controls={detailsId}
        onClick={toggle}
        sx={detailsHeaderSx}
      >
        <Typography variant="labelSmall" sx={detailsLabelSx}>
          {t('features.chatMessages.toolkitAuthorization.details', 'Authorization details')} {showDetails ? '▾' : '▸'}
        </Typography>
      </Box>
      <Collapse in={showDetails} id={detailsId}>
        <Box sx={detailsListSx}>
          {rows.map(([label, value]) => (
            <Box key={label} sx={detailsRowSx}>
              <Typography variant="labelSmall" sx={detailsLabelSx}>
                {label}:
              </Typography>
              <Typography variant="labelSmall" sx={detailsValueSx}>
                {value}
              </Typography>
            </Box>
          ))}
        </Box>
      </Collapse>
    </Box>
  );
}

/**
 * `ChatContinue` — the toolkit-authorization panel (Authorize / skip) or the
 * plain confirmation panel (Continue) of a paused MCP/agent run.
 *
 * Not ported: the baseline's automatic family-token reuse on mount
 * (`McpAuthHelpers.reuseAuthFamilyToken`), which needs the `mcps` feature.
 */
export function ChatContinue({
  onContinueWithoutAuth,
  onAuthSuccess,
  onContinue,
  authRequired = false,
  requiresConfirmation = false,
  disabled = false,
  message,
  continueLabel,
  authRequiredAction,
  renderAuthModal,
}: ChatContinueProps): ReactNode {
  const [showAuthModal, setShowAuthModal] = useState(false);

  const mcpAuthMetadata = useMemo(
    () => (authRequiredAction ? extractLocalMcpAuthMetadata(authRequiredAction) : null),
    [authRequiredAction],
  );
  const authorizationContext = useMemo(
    () => (authRequiredAction ? getToolkitAuthorizationContext(authRequiredAction) : null),
    [authRequiredAction],
  );
  const hasAuthDetails = hasUsableAuthDetails(mcpAuthMetadata);

  const handleCloseModal = useCallback(
    (success?: boolean) => {
      setShowAuthModal(false);
      if (success) {
        onAuthSuccess?.();
      }
    },
    [onAuthSuccess],
  );

  if (authRequired) {
    return (
      <>
        <Box
          data-testid="toolkit-authorization-panel"
          data-authorization-request-id={authRequiredAction?.authorizationRequestId || undefined}
          sx={authorizationContainerSx}
        >
          <Typography variant="headingSmall" sx={authorizationTitleSx}>
            {t('features.chatMessages.toolkitAuthorization.title', '⚠️ Toolkit Authorization Required')}
          </Typography>
          <Typography variant="bodySmall" sx={secondaryTextSx}>
            {buildToolkitAuthorizationMessage(authorizationContext, message)}
          </Typography>
          {!hasAuthDetails && (
            <Typography component="output" variant="bodySmall" sx={secondaryTextSx}>
              {t('features.chatMessages.toolkitAuthorization.detailsUnavailable', 'Authorization details are unavailable. You can skip this tool.')}
            </Typography>
          )}
          <AuthorizationDetails context={authorizationContext} />
          <Box sx={buttonContainerSx}>
            <BaseBtn
              data-testid="toolkit-authorization-authorize-button"
              variant="positive"
              startIcon={<CheckedIcon />}
              onClick={() => setShowAuthModal(true)}
              disabled={disabled || !hasAuthDetails || !renderAuthModal}
            >
              {t('features.chatMessages.toolkitAuthorization.authorize', 'Authorize')}
            </BaseBtn>
            <BaseBtn
              data-testid="toolkit-authorization-skip-button"
              variant="neutral"
              startIcon={<ArrowForwardIcon />}
              onClick={onContinueWithoutAuth ?? onContinue}
              disabled={disabled}
            >
              {continueLabel ?? t('features.chatMessages.continue', 'Continue')}
            </BaseBtn>
          </Box>
        </Box>
        {showAuthModal &&
          renderAuthModal?.({
            open: showAuthModal,
            mcpAuthMetadata,
            serverUrl: authRequiredAction?.toolMeta?.server_url,
            tokenStorageKey: authRequiredAction?.toolOutputs?.server_url,
            toolkitId: mcpAuthMetadata?.toolkitId,
            toolkitType: authRequiredAction?.toolMeta?.toolkit_type,
            onClose: handleCloseModal,
            onCancel: () => setShowAuthModal(false),
          })}
      </>
    );
  }

  if (!requiresConfirmation) return null;

  return (
    <Box data-testid="chat-continue-panel" sx={confirmationContainerSx}>
      {message && (
        <Typography variant="bodyMedium" sx={secondaryTextSx}>
          {message}
        </Typography>
      )}
      <Box sx={buttonContainerSx}>
        <BaseBtn variant="neutral" startIcon={<ArrowForwardIcon />} onClick={onContinue} disabled={disabled}>
          {t('features.chatMessages.continue', 'Continue')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

const confirmationContainerSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  padding: '0.75rem 1rem 1rem 1rem',
  gap: '0.75rem',
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.background.chatContinueBackground,
  alignItems: 'flex-start',
});
const authorizationContainerSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  padding: '0.625rem 0.75rem',
  gap: '0.5rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.chatContinueBackground,
  border: `0.0625rem solid ${theme.vars.palette.warning.main}`,
  alignItems: 'flex-start',
});
const authorizationTitleSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.warning.main });
const secondaryTextSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
const detailsWrapperSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  borderRadius: theme.vars.shape.radiusSm,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'hidden',
});
const detailsHeaderSx: SxProps<Theme> = (theme) => ({
  display: 'block',
  width: '100%',
  border: 'none',
  textAlign: 'left',
  font: 'inherit',
  padding: '0.375rem 0.625rem',
  cursor: 'pointer',
  userSelect: 'none',
  backgroundColor: theme.vars.palette.action.hover,
});
const detailsListSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem', padding: '0.5rem 0.625rem' };
const detailsRowSx: SxProps<Theme> = { display: 'flex', gap: '0.5rem', alignItems: 'flex-start' };
const detailsLabelSx: SxProps<Theme> = (theme) => ({ fontWeight: 600, color: theme.vars.palette.text.secondary, flexShrink: 0 });
const detailsValueSx: SxProps<Theme> = (theme) => ({ fontWeight: 400, color: theme.vars.palette.text.primary, overflowWrap: 'anywhere' });
const buttonContainerSx: SxProps<Theme> = { display: 'flex', gap: '0.5rem', alignItems: 'center' };
