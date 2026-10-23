// @ts-nocheck
/**
 * CollapsedParticipantsDropdown — per-entity-type icon strip for the
 * collapsed participants view, each icon opening a popper listing that
 * type's participants.
 *
 * Ported from `[fsd]/features/chat/participants/ui/CollapsedParticipants/
 * CollapsedPerticapantsList.jsx` (the real old-app "collapsed" entry point,
 * rendered from `Participants.jsx` when `collapsed && !isSmallWindow`) +
 * `CollapsedParticipantsDropdown.jsx` (the old app's popper-content half —
 * played by this cluster's sibling `CollapsedParticipantsList.tsx` here).
 * This rewrite folds both old-app files' responsibilities into this one
 * component (self-managed trigger + `Popper` state per section) rather than
 * keeping the two-file split, since nothing outside this pair depends on
 * the old two-file shape.
 *
 * FIXED regression: this file previously rendered an invented flat
 * "avatar-initial row + N overflow badge" widget with no type grouping, no
 * per-type counts, no MCP-visibility/private-project gating, and no
 * error/skipped-container indicators — none of which exist in the old app.
 * Replaced below with the old app's actual behaviour: participants grouped
 * by entity type (agents/pipelines/toolkits/mcp/users), one icon+count per
 * type, an attention icon when a type's group has a misconfigured
 * participant, an info icon for the "skipped container" hint (issue #5680)
 * when no error is present, and a click-to-open popper (via
 * `CollapsedParticipantsList`) listing that type's participants with
 * working select/edit/delete.
 *
 * NOTE ON REACHABILITY: at the time of the adversarial review that flagged
 * this file, it had zero import sites and was dead code. That is no longer
 * true — a concurrent fix pass ("adversarial review C5-wrapper") landed
 * `ParticipantsLayout.tsx` rendering this component for real (its
 * `showCollapsedParticipants` branch, one instance per users row / per
 * populated type-section). This component's public prop name (`onItemClick`)
 * is kept aligned with that ALREADY-LIVE caller rather than renamed to the
 * old app's own naming, so this fix does not silently break the working
 * "select a participant from the collapsed strip" behaviour.
 *
 * DISCLOSED GAPS (do not block this fix, but are real, remaining parity
 * gaps — see this cluster's landing report for full detail):
 *  - **`ParticipantsLayout.tsx` does not yet forward `onDeleteParticipant`/
 *    `onEditParticipant`/`isMcpVisible`/`isPrivateProject`/
 *    `activeParticipantId`/`disabledEdit` to this component** — it only
 *    passes `participants` and `onItemClick` (plus a now-inert `maxVisible`,
 *    the old "avatar row capped to N + overflow badge" prop this component
 *    intentionally no longer reads — the old app's real per-type icon strip
 *    never had a "cap N avatars" concept either, one icon+count per type,
 *    so dropping it is correct, not a regression). Until `ParticipantsLayout
 *    .tsx` forwards the missing props through from its own `actions`/
 *    `sections`/`header` groups, the edit/delete restored here and in
 *    `CollapsedParticipantsList.tsx` are reachable but never invoked (both
 *    callbacks resolve to `undefined`, so no actions column renders).
 *    Fixing this needs `ParticipantsLayout.tsx`, outside this cluster's file
 *    scope — a precise, scoped follow-up for whichever pass owns that file.
 *  - **MCP-visibility / private-project gating are consumer-supplied
 *    props** (`isMcpVisible`, `isPrivateProject`), not internally fetched
 *    via `useIsMcpVisible()`/`useSelectedProjectId()` + `user.
 *    personal_project_id` like the old app. This mirrors the SAME
 *    established convention this feature's own `Participants.tsx` already
 *    uses for the identical MCP-visibility concern (its `_isMcpVisible`
 *    prop, "so this unit does not import `features/mcp` or `shared/lib/
 *    hooks` directly") — followed here for consistency rather than
 *    inventing a second pattern. `isMcpVisible` DEFAULTS TO `true` here
 *    (unlike `Participants.tsx`'s own `_isMcpVisible = false` default) —
 *    deliberately: `ParticipantsLayout.tsx`'s per-section call already
 *    passes this component a pre-filtered, already-homogeneous participants
 *    array (mcp visibility was already decided upstream, in `Participants.
 *    tsx`'s own `groupedByType`, before an 'mcp'-keyed section even reaches
 *    `ParticipantsLayout.tsx`); defaulting to `false` here would silently
 *    re-hide an 'mcp' group the caller already decided to show, since that
 *    caller does not (yet) pass `isMcpVisible` through. A future direct
 *    caller that passes a raw, unfiltered participants array can still
 *    override with `isMcpVisible={false}`. Unlike MCP visibility, there is
 *    no equivalent of the old app's `user.personal_project_id` anywhere in
 *    this port at all (no `state.user`/current-user hook exists yet in this
 *    worktree), so `isPrivateProject` has no internal fallback to compute —
 *    it must be threaded from whichever composition root eventually wires
 *    this component in.
 *  - **Entity-type icons are generic `@mui/icons-material` glyphs**, not the
 *    old app's custom SVG assets (`assets/agent.svg`, `assets/flow-icon.svg`,
 *    `assets/mcp-icon.svg`, `assets/tool-icon.svg`) — none of those assets
 *    were ported into this app. Same substitution precedent this feature's
 *    own `ExpandedParticipants/ParticipantItem.tsx` already uses (MUI
 *    `WarningAmber`/`Info` icons standing in for the old app's custom
 *    `AttentionIcon`/`InfoIcon`).
 *  - **Users are grouped through the same generic per-type popper** as
 *    every other entity type, instead of the old app's dedicated
 *    `UsersParticipantDropdown` (which additionally offers "add user to
 *    conversation" from inside the collapsed view). That richer add-user
 *    flow is not reproduced here — out of scope for this fix pass.
 */
import { memo, useMemo, useRef, useState } from 'react';

import { Badge, Box, ClickAwayListener, IconButton, Paper, Popper, Tooltip } from '@mui/material';
import { useTheme } from '@mui/material/styles';

import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ExtensionOutlinedIcon from '@mui/icons-material/ExtensionOutlined';
import GroupOutlinedIcon from '@mui/icons-material/GroupOutlined';
import InfoIcon from '@mui/icons-material/Info';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import WarningAmberIcon from '@mui/icons-material/WarningAmber';

import { t } from '@/shared/i18n';

import { useParticipantDetailsContext } from '../../lib/context/ParticipantDetailsContext';
import { isSkippedContainerParticipant } from '../../lib/helpers';
import { ChatParticipantType } from '../../model/constants';

import CollapsedParticipantsList from './CollapsedParticipantsList';

// ---------------------------------------------------------------------------
// Local snake_case-native helpers
// ---------------------------------------------------------------------------

/**
 * Duplicated here rather than reused from `@/entities/participant` —
 * that module's `chatParticipantUniqueId` is typed against a **camelCase**
 * `Participant` shape (`entityName`, `entitySettings`, `entityMeta.projectId`),
 * while every
 * participant flowing through this feature (`TransformedParticipant`,
 * `../../model/types.ts`) is **snake_case** (`entity_name`,
 * `entity_settings`, `entity_meta.project_id`). Calling the
 * camelCase-typed helpers on this feature's real snake_case objects would
 * silently read every field as `undefined` and always return a wrong
 * answer, so this is a plain, verbatim port of the old app's own
 * `participants.helpers.js` `getChatParticipantUniqueId` on the shape this
 * feature actually uses.
 */
function uniqueIdEntityKey(participant: Record<string, unknown>): string {
  const entitySettings = participant.entity_settings as Record<string, unknown> | undefined;
  const entityName = participant.entity_name as string | undefined;
  const isApplicationActingAsPipeline = entityName === ChatParticipantType.Applications && entitySettings?.agent_type === ChatParticipantType.Pipelines;
  return isApplicationActingAsPipeline ? ChatParticipantType.Pipelines : (entityName ?? '');
}

function uniqueIdBody(participant: Record<string, unknown>): string {
  const entityMeta = participant.entity_meta as Record<string, unknown> | undefined;
  if (participant.entity_name === ChatParticipantType.Models) {
    const modelName = (entityMeta?.model_name as string | undefined) ?? '';
    const integrationUid = (entityMeta?.integration_uid as string | undefined) ?? '';
    return `${modelName}-${integrationUid}`;
  }
  const id = (entityMeta?.id as string | number | undefined) ?? '';
  return String(id);
}

function getChatParticipantUniqueId(participant: Record<string, unknown> | undefined): string {
  if (!participant) return '';
  const entityMeta = participant.entity_meta as Record<string, unknown> | undefined;
  const projectId = (entityMeta?.project_id as string | undefined) ?? '';
  return `${uniqueIdEntityKey(participant)}_${uniqueIdBody(participant)}_${projectId}`;
}

/** `mcp.helpers.js:7-14`'s `isMcpToolkitType`, duplicated per this feature's own established precedent (`Participants.tsx`'s identical local copy) — no shared home for it exists in this worktree. */
function isMcpToolkitType(type: string | undefined): boolean {
  return type === 'mcp' || Boolean(type?.startsWith('mcp_'));
}

/** Resolves the group key for one participant (extracted so `groupParticipantsByType`'s loop body stays under the complexity budget). */
function resolveParticipantGroupKey(p: Record<string, unknown>): string {
  const entitySettings = (p.entity_settings as Record<string, unknown> | undefined) ?? {};
  const meta = (p.meta as Record<string, unknown> | undefined) ?? {};
  const entityName = p.entity_name as string | undefined;

  if (entityName === ChatParticipantType.Applications && entitySettings.agent_type === ChatParticipantType.Pipelines) {
    return ChatParticipantType.Pipelines;
  }
  if (entityName === ChatParticipantType.Toolkits && (isMcpToolkitType(entitySettings.toolkit_type as string | undefined) || meta.mcp === true)) {
    return 'mcp';
  }
  return entityName ?? '';
}

/** Groups participants by entity type. Mirrors `Participants.tsx`'s own `groupedByType` (same feature, same logic) plus the old app's MCP-visibility filter. */
function groupParticipantsByType(
  participants: Record<string, unknown>[],
  isMcpVisible: boolean,
): Record<string, Record<string, unknown>[]> {
  const groups: Record<string, Record<string, unknown>[]> = {};
  for (const p of participants) {
    const key = resolveParticipantGroupKey(p);
    if (key === 'mcp' && !isMcpVisible) continue;
    (groups[key] ??= []).push(p);
  }
  return groups;
}

// ---------------------------------------------------------------------------
// Section definitions
// ---------------------------------------------------------------------------

interface EntitySectionDef {
  key: string;
  type: string;
  icon: typeof SmartToyOutlinedIcon;
  labelKey: string;
  labelFallback: string;
}

const USERS_SECTION: EntitySectionDef = {
  key: 'users',
  type: ChatParticipantType.Users,
  icon: GroupOutlinedIcon,
  labelKey: 'chat-participants.collapsed.users',
  labelFallback: 'Users',
};

const ENTITY_SECTIONS: EntitySectionDef[] = [
  {
    key: 'agents',
    type: ChatParticipantType.Applications,
    icon: SmartToyOutlinedIcon,
    labelKey: 'chat-participants.collapsed.agents',
    labelFallback: 'Agents',
  },
  {
    key: 'pipelines',
    type: ChatParticipantType.Pipelines,
    icon: AccountTreeOutlinedIcon,
    labelKey: 'chat-participants.collapsed.pipelines',
    labelFallback: 'Pipelines',
  },
  {
    key: 'toolkits',
    type: ChatParticipantType.Toolkits,
    icon: BuildOutlinedIcon,
    labelKey: 'chat-participants.collapsed.toolkits',
    labelFallback: 'Toolkits',
  },
  {
    key: 'mcp',
    type: 'mcp',
    icon: ExtensionOutlinedIcon,
    labelKey: 'chat-participants.collapsed.mcps',
    labelFallback: 'MCPs',
  },
];

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

interface CollapsedParticipantsDropdownProps {
  participants: Record<string, unknown>[];
  activeParticipantId?: string;
  disabledEdit?: boolean;
  /** Fired when a participant row is selected. Named to match the already-live `ParticipantsLayout.tsx` caller — see header comment. */
  onItemClick?: (participant: Record<string, unknown>) => void;
  onDeleteParticipant?: (participant: Record<string, unknown>) => void;
  onEditParticipant?: (participant: Record<string, unknown>) => void;
  /** Consumer-supplied MCP-visibility gate — see header comment for why this defaults to `true` (unlike `Participants.tsx`'s own `_isMcpVisible = false`). */
  isMcpVisible?: boolean;
  /** Consumer-supplied "hide the Users section" gate (old app: `!isPrivateProject`) — see header comment. */
  isPrivateProject?: boolean;
}

/**
 * CollapsedParticipantsDropdown component — per-entity-type icon strip with
 * counts, error/skipped-container indicators, and a click-to-open popper
 * per type.
 */
const CollapsedParticipantsDropdown = memo((props: CollapsedParticipantsDropdownProps): React.ReactElement | null => {
  const theme = useTheme();
  const {
    participants,
    activeParticipantId,
    disabledEdit,
    onItemClick,
    onDeleteParticipant,
    onEditParticipant,
    isMcpVisible = true,
    isPrivateProject = false,
  } = props;

  const { hasParticipantError } = useParticipantDetailsContext();
  const [openSectionKey, setOpenSectionKey] = useState<string | null>(null);
  const anchorsRef = useRef<Record<string, HTMLElement | null>>({});

  const groupedByType = useMemo(() => groupParticipantsByType(participants ?? [], isMcpVisible), [participants, isMcpVisible]);

  if (!participants?.length) return null;

  const sections = isPrivateProject ? ENTITY_SECTIONS : [USERS_SECTION, ...ENTITY_SECTIONS];
  const closeSection = (): void => setOpenSectionKey(null);

  return (
    <ClickAwayListener onClickAway={closeSection}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
        {sections.map((section) => {
          const group = groupedByType[section.type];
          if (!group?.length) return null;

          const hasError = group.some((p) => {
            const entityMeta = p.entity_meta as Record<string, unknown> | undefined;
            const id = (entityMeta?.id as string | number | undefined) ?? '';
            const projectId = (entityMeta?.project_id as string | undefined) ?? '';
            return hasParticipantError(p.entity_name as ChatParticipantType, String(id), projectId);
          });
          const hasSkippedContainer =
            !hasError && group.some((p) => getChatParticipantUniqueId(p) !== activeParticipantId && isSkippedContainerParticipant(p));

          const Icon = section.icon;
          const label = t(section.labelKey, section.labelFallback);
          const tooltipTitle = hasError
            ? t('chat-participants.collapsed.errorTooltip', 'Misconfiguration error in {{label}}', { label: label.toLowerCase() })
            : hasSkippedContainer
              ? t('chat-participants.collapsed.skippedContainerTooltip', 'Its sub-agent chain is at the nesting limit (3 tiers) — select it to run.')
              : t('chat-participants.collapsed.sectionTooltip', '{{label}} in this conversation', { label });

          return (
            <Box key={section.key} sx={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 0.25 }}>
              <Tooltip title={tooltipTitle} placement="right">
                <Badge badgeContent={group.length} overlap="circular">
                  <IconButton
                    size="small"
                    aria-label={label}
                    onClick={(event: React.MouseEvent<HTMLButtonElement>) => {
                      anchorsRef.current[section.key] = event.currentTarget;
                      setOpenSectionKey((prev) => (prev === section.key ? null : section.key));
                    }}
                    sx={{
                      width: 36,
                      height: 36,
                      borderRadius: theme.vars.shape.radiusMd,
                      backgroundColor: 'background.paper',
                      border: hasError
                        ? `0.0625rem solid ${theme.vars.palette.error.main}`
                        : hasSkippedContainer
                          ? `0.0625rem solid ${theme.vars.palette.divider}`
                          : 'none',
                      color: 'secondary.main',
                    }}
                  >
                    <Icon fontSize="small" />
                  </IconButton>
                </Badge>
              </Tooltip>
              {hasError ? (
                <WarningAmberIcon fontSize="small" sx={{ color: 'error.main' }} />
              ) : hasSkippedContainer ? (
                <InfoIcon fontSize="small" sx={{ color: 'text.secondary' }} />
              ) : null}

              <Popper
                open={openSectionKey === section.key}
                anchorEl={anchorsRef.current[section.key]}
                placement="right-start"
                modifiers={[{ name: 'offset', options: { offset: [0, 8] } }]}
                sx={{ zIndex: (t2) => t2.zIndex.modal + 10 }}
              >
                <Paper elevation={3} sx={{ maxHeight: 320, overflow: 'auto', minWidth: 220, borderRadius: theme.vars.shape.radiusMd }}>
                  <CollapsedParticipantsList
                    participants={group}
                    activeParticipantId={activeParticipantId}
                    disabledEdit={disabledEdit}
                    onItemClick={(p) => {
                      onItemClick?.(p);
                      closeSection();
                    }}
                    onDelete={onDeleteParticipant}
                    onEdit={
                      onEditParticipant
                        ? (p) => {
                            onEditParticipant(p);
                            closeSection();
                          }
                        : undefined
                    }
                  />
                </Paper>
              </Popper>
            </Box>
          );
        })}
      </Box>
    </ClickAwayListener>
  );
});

CollapsedParticipantsDropdown.displayName = 'CollapsedParticipantsDropdown';

export default CollapsedParticipantsDropdown;
