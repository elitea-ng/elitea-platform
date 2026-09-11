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
 * Behavior this restores vs. the pre-fix version: clicking "Continue
 * (Auth)" only OPENS the auth modal (via the slot) — `onAuthSuccess` fires
 * exclusively from the modal's own close callback, and only on a real
 * successful login (baseline: `ChatContinue.jsx`'s `handleCloseModal`).
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

/**
 * Loosely-typed source an MCP-auth-required tool action carries its OAuth
 * discovery metadata in — a local, structurally-equivalent re-declaration of
 * `features/mcps/lib/discoveryMetadata.ts`'s `McpAuthMetadataSource`
 * (cannot import it — see module doc). Every field optional/defensive, same
 * posture as that source.
 */
export interface McpAuthRequiredAction {
  readonly id?: string;
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
  /** Whether to continue execution without authentication (skip auth). */
  readonly onContinueWithoutAuth?: (() => void) | undefined;
  /** Called after a real, successful authentication (never on the initial "Continue (Auth)" click). */
  readonly onAuthSuccess?: (() => void) | undefined;
  /** Called when the user requests to continue. */
  readonly onContinue?: (() => void) | undefined;
  /** Whether authentication is required. */
  readonly authRequired?: boolean;
  /** Whether confirmation is required. */
  readonly requiresConfirmation?: boolean;
  /** Whether the continue button is disabled. */
  readonly disabled?: boolean;
  /** The MCP-auth-required tool action/message to extract OAuth metadata from — baseline's `authRequiredAction` prop. */
  readonly authRequiredAction?: McpAuthRequiredAction | undefined;
  /** Renders the real auth modal when open — see module doc for why this is a slot, not a hard import. */
  readonly renderAuthModal?: ((props: ChatContinueAuthModalSlotProps) => ReactNode) | undefined;
}

/**
 * `ChatContinue` — renders a continuation button with optional auth
 * confirmation flow for MCP tool execution.
 */
export function ChatContinue({
  onContinueWithoutAuth,
  onAuthSuccess,
  onContinue,
  authRequired = false,
  requiresConfirmation = false,
  disabled = false,
  authRequiredAction,
  renderAuthModal,
}: ChatContinueProps): ReactNode {
  const [showAuthModal, setShowAuthModal] = useState(false);

  const mcpAuthMetadata = useMemo(
    () => (authRequiredAction ? extractLocalMcpAuthMetadata(authRequiredAction) : null),
    [authRequiredAction],
  );
  const hasAuthServers = (mcpAuthMetadata?.authServers?.length ?? 0) > 0;
  // Configured OAuth requires discovered endpoints. Do not guess endpoints
  // when discovery fails; remote MCP retains its existing discovery flow.
  const hasAuthDetails = hasAuthServers && (!mcpAuthMetadata?.providedSettings || (
    typeof mcpAuthMetadata.oauthAuthorizationServer?.['authorization_endpoint'] === 'string'
    && typeof mcpAuthMetadata.oauthAuthorizationServer?.['token_endpoint'] === 'string'
  ));

  const handleAuthorize = useCallback(() => {
    setShowAuthModal(true);
  }, []);

  const handleCloseModal = useCallback(
    (success?: boolean) => {
      setShowAuthModal(false);
      if (success) {
        onAuthSuccess?.();
      }
    },
    [onAuthSuccess],
  );

  const handleCancelModal = useCallback(() => {
    setShowAuthModal(false);
  }, []);

  if (!authRequired && !requiresConfirmation) {
    return null;
  }

  return (
    <>
      {authRequired && !hasAuthDetails && (
        <Typography component="output" variant="body2">
          Authorization details are unavailable. You can skip this tool.
        </Typography>
      )}
      <Stack
        direction="row"
        spacing={1}
        sx={{ mt: 1, mb: 1 }}
      >
        {authRequired ? (
          <>
            <Button
              size="small"
              variant="contained"
              startIcon={<PlayArrowIcon />}
              onClick={handleAuthorize}
              disabled={disabled || !hasAuthDetails || !renderAuthModal}
            >
              Authorize
            </Button>
            <Button
              size="small"
              variant="outlined"
              onClick={onContinueWithoutAuth}
              disabled={disabled}
            >
              Skip Auth
            </Button>
          </>
        ) : (
          <Button
            size="small"
            variant="contained"
            startIcon={<PlayArrowIcon />}
            onClick={onContinue}
            disabled={disabled}
          >
            Continue
          </Button>
        )}
      </Stack>
      {authRequired &&
        showAuthModal &&
        renderAuthModal?.({
          open: showAuthModal,
          mcpAuthMetadata,
          serverUrl: authRequiredAction?.toolMeta?.server_url,
          tokenStorageKey: authRequiredAction?.toolOutputs?.server_url,
          toolkitId: mcpAuthMetadata?.toolkitId,
          toolkitType: authRequiredAction?.toolMeta?.toolkit_type,
          onClose: handleCloseModal,
          onCancel: handleCancelModal,
        })}
    </>
  );
}
