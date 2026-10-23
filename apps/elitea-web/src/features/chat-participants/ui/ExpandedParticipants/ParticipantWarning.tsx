// @ts-nocheck
/**
 * ParticipantWarning — renders warning messages for various participant error
 * conditions.
 *
 * Ported from `[fsd]/features/chat/participants/ui/ExpandedParticipants/ParticipantWarning.jsx`.
 *
 * Cross-cutting note: the old app imported `McpLogInLink` from `features/mcp/ui`
 * and `SharepointLogInLink` from `features/sharepoint/ui`. New-app port uses
 * slot props (`mcpLoginSlot`, `sharepointLoginSlot`) supplied by the consumer.
 * See `ToolCardDelegatedAuthProps` in `features/agents/ui/ToolCard.types.ts`
 * for the established slot-prop pattern.
 */
import { memo, type ReactNode } from 'react';

import { Typography } from '@mui/material';

import { t } from '@/shared/i18n';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface ParticipantWarningProps {
  isPublishedAgentGone?: boolean;
  isVersionUnavailable?: boolean;
  hasMisconfigurationErrors?: boolean;
  shouldDisableThisItem?: boolean;
  mcpIsDisconnected?: boolean;
  someToolsAreUnavailable?: boolean;
  blockedToolkitNames?: string[];
  remoteMcpLoggedOut?: boolean;
  spOAuthLoggedOut?: boolean;
  isSkippedContainer?: boolean;
  participant?: Record<string, unknown>;
  handleEditClick?: (event: React.MouseEvent) => void;
  isToolkitParticipant?: boolean;
  type?: string;
  originalDetails?: Record<string, unknown>;
  entityMeta?: Record<string, unknown>;
  spConfig?: unknown;
  mcpLoginSlot?: ReactNode;
  sharepointLoginSlot?: ReactNode;
}

// ---------------------------------------------------------------------------
// Warning text resolvers — each handles one condition (complexity ≤ 3)
// ---------------------------------------------------------------------------

/**
 * Old app (`ParticipantWarning.jsx:44-45`, issue #5778): "Its sub-agent chain
 * is at the nesting limit (3 tiers), so it runs only as the active agent here
 * — select it to run." `ParticipantItem.tsx`'s inline `containerInfoRow` copy
 * carries the same key/fallback so the two call sites can't drift apart again.
 */
function resolveSkippedContainer(): ReactNode {
  return t(
    'chat-participants.warning.skippedContainer',
    'Its sub-agent chain is at the nesting limit (3 tiers), so it runs only as the active agent here — select it to run.',
  );
}

function resolvePublishedAgentGone(): ReactNode {
  return t(
    'chat-participants.warning.publishedAgentGone',
    'This published agent has been removed.',
  );
}

function resolveVersionUnavailable(): ReactNode {
  return t(
    'chat-participants.warning.versionUnavailable',
    'The selected version is no longer available.',
  );
}

/**
 * Old app (`ParticipantWarning.jsx`): `` `The ${originalDetails.name} mcp
 * server is disconnected. Reconnect it to use.` `` — unconditional, no
 * login slot. The new-app port previously never referenced any server name
 * here (wave-2 C5 adversarial-review finding #7c); `participant.entity_meta.name`
 * is this component's equivalent of old app's `originalDetails.name` (the
 * richer context-fetched details object has no equivalent prop on this
 * component today).
 */
function resolveMcpDisconnected(participant: Record<string, unknown> | undefined): ReactNode {
  const entityMeta = participant?.entity_meta as Record<string, unknown> | undefined;
  const name = (entityMeta?.name as string | undefined) ?? '';
  return t(
    'chat-participants.warning.mcpDisconnected',
    'The {{name}} mcp server is disconnected. Reconnect it to use.',
    { name },
  );
}

function resolveRemoteMcpExpired(mcpLoginSlot: ReactNode | undefined): ReactNode {
  if (mcpLoginSlot) {
    return (
      <>
        {t('chat-participants.warning.remoteMcpExpiredPrefix', 'Remote MCP session expired. ')}
        <Typography component="span" sx={{ cursor: 'pointer', color: 'primary.main', textDecoration: 'underline' }} onClick={() => {}}>
          {t('chat-participants.warning.login', 'Login')}
        </Typography>{' '}
        {t('chat-participants.warning.reconnect', 'to reconnect.')}
      </>
    );
  }
  return t('chat-participants.warning.remoteMcpExpired', 'Remote MCP session expired.');
}

function resolveSpOAuthExpired(sharepointLoginSlot: ReactNode | undefined): ReactNode {
  if (sharepointLoginSlot) {
    return (
      <>
        {t('chat-participants.warning.sharepointExpiredPrefix', 'SharePoint OAuth session expired. ')}
        <Typography component="span" sx={{ cursor: 'pointer', color: 'primary.main', textDecoration: 'underline' }} onClick={() => {}}>
          {t('chat-participants.warning.login', 'Login')}
        </Typography>{' '}
        {t('chat-participants.warning.reconnect', 'to reconnect.')}
      </>
    );
  }
  return t('chat-participants.warning.sharepointExpired', 'SharePoint OAuth session expired.');
}

/**
 * Old app (`ParticipantWarning.jsx:44-67`): the "Check the {type}" link text
 * is type-specific — 'agent' for a non-pipeline application, 'toolkit' for a
 * toolkit participant, 'pipeline' otherwise. The new-app port previously
 * always showed a generic message with no per-type text at all (wave-2 C5
 * adversarial-review finding #7b).
 *
 * Real wire value for an agent/pipeline participant is `'application'` —
 * this feature's own `ChatParticipantType.Applications` constant now
 * matches that value too (a previous plural/singular mismatch in
 * `model/constants.ts` was fixed), but this file has `@ts-nocheck` so the
 * literal is left as-is rather than adding an import purely for style.
 */
function getParticipantTypeText(participant: Record<string, unknown> | undefined, isToolkitParticipant: boolean | undefined): string {
  const entitySettings = participant?.entity_settings as Record<string, unknown> | undefined;
  const isPipelineAgent = entitySettings?.agent_type === 'pipeline' || participant?.agent_type === 'pipeline';

  if (participant?.entity_name === 'application' && !isPipelineAgent) {
    return t('chat-participants.warning.typeAgent', 'agent');
  }
  return isToolkitParticipant
    ? t('chat-participants.warning.typeToolkit', 'toolkit')
    : t('chat-participants.warning.typePipeline', 'pipeline');
}

function resolveConfigIssues(
  participant: Record<string, unknown> | undefined,
  isToolkitParticipant: boolean | undefined,
  handleEditClick: ((event: React.MouseEvent) => void) | undefined,
): ReactNode {
  if (handleEditClick) {
    const typeText = getParticipantTypeText(participant, isToolkitParticipant);
    return (
      <>
        {t('chat-participants.warning.misconfiguredPrefix', 'Misconfiguration errors found. ')}
        <Typography component="span" sx={{ cursor: 'pointer', color: 'primary.main', textDecoration: 'underline' }} onClick={handleEditClick}>
          {t('chat-participants.warning.checkThe', `Check the ${typeText}`, { type: typeText })}
        </Typography>
        .
      </>
    );
  }
  return t('chat-participants.warning.misconfigured', 'Misconfiguration errors found.');
}

function resolveToolsUnavailable(): ReactNode {
  return t(
    'chat-participants.warning.toolsUnavailable',
    'Some tools available in this toolkit\'s schema are not available on your organization\'s instance.',
  );
}

function resolveBlockedToolkitNames(blockedToolkitNames: string[]): ReactNode {
  return t(
    'chat-participants.warning.blockedToolkitTypes',
    'The following toolkit types are blocked by your organization\'s guardrails: ',
  ).concat(blockedToolkitNames.join(', '), '.');
}

/**
 * Old app (`ParticipantWarning.jsx:69-73`): type-gated — only an
 * `Applications`-type participant shows a message ('Please configure agent
 * chat settings'); every other type shows nothing (`''`). The new-app port
 * previously always returned a non-empty generic message regardless of
 * participant type (wave-2 C5 adversarial-review finding #7d).
 */
function resolveCannotBeAdded(participant: Record<string, unknown> | undefined): ReactNode {
  if (participant?.entity_name === 'application') {
    return t('chat-participants.warning.cannotBeAdded', 'Please configure agent chat settings');
  }
  return '';
}

// ---------------------------------------------------------------------------
// getWarningText — orchestrator (complexity ≤ 8)
// ---------------------------------------------------------------------------

/**
 * Computes the warning text for a participant based on its error conditions.
 * Ported from `ParticipantWarning.jsx` (the entire component's rendering logic).
 *
 * Check order matches old app exactly (wave-2 C5 adversarial-review finding
 * #7a): skippedContainer -> publishedAgentGone -> versionUnavailable ->
 * hasMisconfigurationErrors -> shouldDisableThisItem -> mcpIsDisconnected ->
 * blockedToolkitNames -> someToolsAreUnavailable -> remoteMcpLoggedOut ->
 * spOAuthLoggedOut. The new-app port previously checked these in a
 * different order, so a participant with multiple simultaneous error flags
 * could show a different (wrong-priority) message than old app.
 */
function getWarningText(props: ParticipantWarningProps): ReactNode {
  const {
    isSkippedContainer,
    isPublishedAgentGone,
    isVersionUnavailable,
    mcpIsDisconnected,
    isToolkitParticipant,
    remoteMcpLoggedOut,
    spOAuthLoggedOut,
    spConfig,
    hasMisconfigurationErrors,
    someToolsAreUnavailable,
    blockedToolkitNames,
    shouldDisableThisItem,
    handleEditClick,
    mcpLoginSlot,
    sharepointLoginSlot,
    participant,
  } = props;

  if (isSkippedContainer) return resolveSkippedContainer();
  if (isPublishedAgentGone) return resolvePublishedAgentGone();
  if (isVersionUnavailable) return resolveVersionUnavailable();
  if (hasMisconfigurationErrors) return resolveConfigIssues(participant, isToolkitParticipant, handleEditClick);
  if (shouldDisableThisItem) return resolveCannotBeAdded(participant);

  // MCP checks — extract combined condition to avoid && in if
  if (checkMcpDisconnected(mcpIsDisconnected, isToolkitParticipant)) {
    return resolveMcpDisconnected(participant);
  }
  if (blockedToolkitNames?.length) return resolveBlockedToolkitNames(blockedToolkitNames);
  if (someToolsAreUnavailable) return resolveToolsUnavailable();
  if (checkRemoteMcpLoggedOut(remoteMcpLoggedOut, isToolkitParticipant)) {
    return resolveRemoteMcpExpired(mcpLoginSlot);
  }
  if (checkSpOAuthExpired(spOAuthLoggedOut, spConfig)) {
    return resolveSpOAuthExpired(sharepointLoginSlot);
  }

  return null;
}

// ---------------------------------------------------------------------------
// Helper: combine boolean flags (complexity ≤ 2)
// ---------------------------------------------------------------------------

function checkMcpDisconnected(mcpIsDisconnected: boolean | undefined, isToolkitParticipant: boolean | undefined): boolean {
  return mcpIsDisconnected && isToolkitParticipant;
}

function checkRemoteMcpLoggedOut(remoteMcpLoggedOut: boolean | undefined, isToolkitParticipant: boolean | undefined): boolean {
  return remoteMcpLoggedOut && isToolkitParticipant;
}

function checkSpOAuthExpired(spOAuthLoggedOut: boolean | undefined, spConfig: unknown): boolean {
  return spOAuthLoggedOut && spConfig;
}

/**
 * ParticipantWarning component — renders warning messages for participant errors.
 * Memo'd for performance.
 */
const ParticipantWarning = memo((props: ParticipantWarningProps): React.ReactElement | null => {
  const warningText = getWarningText(props);
  if (!warningText) return null;

  return (
    <Typography variant="bodySmall" color="text.attention" sx={{ wordBreak: 'break-word' }}>
      {warningText}
    </Typography>
  );
});

ParticipantWarning.displayName = 'ParticipantWarning';

export default ParticipantWarning;
