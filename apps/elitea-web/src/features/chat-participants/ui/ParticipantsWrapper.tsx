// @ts-nocheck
/**
 * ui/ParticipantsWrapper.tsx — Wraps `Participants` with
 * `ParticipantDetailsProvider` and handles loading / empty states.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/participants/ui/
 * ParticipantsWrapper.jsx` (old-app). The wrapper:
 *  1. Receives the active conversation from the consumer.
 *  2. Derives permission-based flags (e.g., `disabledAdd` from
 *     `checkPermission(PERMISSIONS.users.view)`, via the inlined
 *     `useCheckPermission` below).
 *  3. Feeds `participants` into `ParticipantDetailsProvider` so the detail
 *     cache fetches entity data for non-user participants.
 *  4. Renders a MUI `<Grid>` with responsive sizing.
 *
 * Cross-cutting gaps:
 *  - `ContextBudgetUI` is NOT rendered here — it is a slot prop passed
 *    through to `Participants`'s `renderContextBudget`.
 *  - `ParticipantStatusRunner` children (MCP token change listener,
 *    SharePoint OAuth, tool validation) are rendered by the consumer with
 *    slot injection (see `ParticipantDetailsProvider`'s doc comment).
 */

import React, { useCallback, useEffect, useState, memo, useMemo } from 'react';

import Grid from '@mui/material/Grid';
import { useTheme } from '@mui/material/styles';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { useSelectedProjectId } from '../api/useSelectedProjectId';
import { getChatParticipantUniqueId } from '../lib/helpers';
import { ParticipantDetailsProvider } from '../lib/context/ParticipantDetailsContext';
import { Participants } from './Participants';
import type { ParticipantsProps } from './Participants.types';
import { MIN_LARGE_WINDOW_WIDTH } from '@/shared/lib/layout';
import { derivePaddingLeft, deriveWrapperStyleParams, useWrapperGridSizes, wrapperSx } from './ParticipantsWrapper.styles';

/**
 * Inlined local duplicate of `useCheckPermission` — mirrors the same
 * "local, feature-owned, no shared primitive exists" pattern already
 * established by `features/chat-input/lib/hooks/useCheckPermission.hooks.ts`
 * and `features/agents/lib/useHasPermission.ts` (both carry the identical
 * disclosure). `no-sideways-features` forbids importing the chat-input copy
 * directly, so this feature gets its own byte-for-byte-equivalent copy.
 *
 * Fixes adversarial review C5-wrapper #5: `disabledAdd` was previously
 * derived from playback state alone, dropping the old app's
 * `!checkPermission(PERMISSIONS.users.view)` gate entirely.
 */
function useCheckPermission(): { readonly checkPermission: (permission: string) => boolean } {
  const projectId = useSelectedProjectId();
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });

  const permissions = useMemo(() => {
    const list = query.data?.data as Permission[] | undefined;
    if (!list) return new Set<string>();
    return new Set(list.filter((entry) => entry.enabled).map((entry) => entry.name));
  }, [query.data]);

  const checkPermission = useCallback(
    (permission: string) => (permission ? permissions.has(permission) : true),
    [permissions],
  );

  return { checkPermission };
}

/**
 * Inlined local duplicate of `useIsSmallWindow` — the pipelines and
 * chat-conversation-list features each maintain their own copy because
 * no shared version exists. This one uses the shared `MIN_LARGE_WINDOW_WIDTH`
 * constant from `@/shared/lib/layout` (unit S3).
 */
interface UseIsSmallWindowResult {
  readonly isSmallWindow: boolean;
}

function useIsSmallWindow(): UseIsSmallWindowResult {
  const [isSmallWindow, setIsSmallWindow] = useState(false);

  const onSize = useCallback(() => {
    const windowWidth = window.innerWidth;
    if (windowWidth < MIN_LARGE_WINDOW_WIDTH) {
      setIsSmallWindow(true);
    } else {
      setIsSmallWindow(false);
    }
  }, []);

  useEffect(() => {
    onSize();
    window.addEventListener('resize', onSize);
    return () => window.removeEventListener('resize', onSize);
  }, [onSize]);

  return { isSmallWindow };
}

export interface ParticipantsWrapperProps {
  /**
   * When true, hide the entire participants panel.
   * Ported from the old-app prop that checked `activeConversation` visibility.
   */
  readonly hidden?: boolean;
  /**
   * When true, show the collapsed (icon-only) row.
   */
  readonly collapsed?: boolean;
  /**
   * Width in pixels for the non-collapsed panel on large screens.
   * Defaults to `320` when not provided.
   */
  readonly panelWidth?: number;
  /**
   * The active conversation. Its `participants` array and metadata are
   * propagated to `Participants` and `ParticipantDetailsProvider`.
   *
   * Shape mirrors `ChatConversation` from
   * `features/pipelines/lib/hooks/pipelineChat.types.ts` — the exact shape
   * is consumer-dependent; this wrapper only reads a stable subset.
   */
  readonly activeConversation?: {
    readonly id?: string | number;
    readonly isNew?: boolean;
    readonly isPlayback?: boolean;
    readonly participants?: Record<string, unknown>[];
    readonly instructions?: string;
    readonly meta?: Record<string, unknown>;
    readonly context_strategy?: Record<string, unknown>;
    readonly persona?: unknown;
    [key: string]: unknown;
  } | null;
  /** Called when the user toggles collapsed/expanded state. */
  readonly onCollapsed?: (collapsed: boolean) => void;
  /**
   * The active participant (the one selected as the LLM). Used to compute
   * the highlighted `activeParticipantId`.
   */
  readonly activeParticipant?: Record<string, unknown>;
  /** Called to remove a participant from the chat. */
  readonly onDeleteParticipant?: (participant: Record<string, unknown>) => void;
  /** Called when a participant is selected as the active LLM participant. */
  readonly onSelectParticipant?: (participant: Record<string, unknown>) => void;
  /** Called to update a participant's settings. */
  readonly onUpdateParticipant?: (participant: Record<string, unknown>) => void;
  /** Called to edit a participant (opens edit modal). */
  readonly onEditParticipant?: (participant: Record<string, unknown>) => void;
  /**
   * Slot for rendering the context-budget widget beneath the participants.
   * See `ParticipantsProps.renderContextBudget` for the shape contract.
   */
  readonly renderContextBudget?: ParticipantsProps['renderContextBudget'];
  /**
   * Optional slot to resolve toolkit/MCP icons.
   * @see ParticipantsProps.resolveToolkitIcon
   */
  readonly resolveToolkitIcon?: ParticipantsProps['resolveToolkitIcon'];
  /**
   * When true, MCP toolkits are visible and should be grouped separately.
   * @see ParticipantsProps.isMcpVisible
   */
  readonly isMcpVisible?: boolean;
  /**
   * The toolkit currently being edited (controls which edit button is highlighted).
   */
  readonly editingToolkit?: string;
  /** Maximum visible users in the users row before overflow. */
  readonly maxVisibleUsers?: number;
  /**
   * Id of the toolkit participant currently acting as the active
   * conversation's attachment manager.
   * @see ParticipantsProps.selectedManager
   */
  readonly selectedManager?: string;
  /** Same as `selectedManager`, but for a conversation still being composed (not yet persisted). */
  readonly newConversationSelectedManager?: string;
}

/**
 * Wrapper component that provides the `ParticipantDetailsProvider` context
 * and passes conversation-derived props down to `Participants`.
 *
 * The wrapper:
 *  1. Extracts the participants array from `activeConversation`.
 *  2. Computes `disabledAdd` from the conversation's playback/new flags.
 *  3. Computes `disabledEdit` from the playback flag.
 *  4. Resolves the active participant's unique ID for highlighting.
 *  5. Derives the conversation ID for context-budget rendering.
 *
 * @example
 * ```tsx
 * <ParticipantsWrapper
 *   activeConversation={conversation}
 *   activeParticipant={active}
 *   onCollapsed={setCollapsed}
 *   onSelectParticipant={setActive}
 *   onDeleteParticipant={handleDelete}
 *   onEditParticipant={handleEdit}
 *   onUpdateParticipant={handleUpdate}
 *   renderContextBudget={({ conversationId, contextStrategy, conversationInstructions }) => (
 *     <ContextBudgetUI.ContextBudgetInfo
 *       conversationId={conversationId}
 *       contextStrategy={contextStrategy}
 *       conversationInstructions={conversationInstructions}
 *     />
 *   )}
 * />
 * ```
 */
export const ParticipantsWrapper = memo(
  ({
    hidden = false,
    collapsed = false,
    panelWidth = 320,
    activeConversation,
    onCollapsed,
    activeParticipant,
    onDeleteParticipant,
    onSelectParticipant,
    onUpdateParticipant,
    onEditParticipant,
    renderContextBudget,
    resolveToolkitIcon,
    isMcpVisible,
    editingToolkit,
    maxVisibleUsers = 5,
    selectedManager,
    newConversationSelectedManager,
  }: ParticipantsWrapperProps) => {
    const theme = useTheme();
    const { isSmallWindow } = useIsSmallWindow();
    const { checkPermission } = useCheckPermission();
    const derived = useParticipantsDerived(activeConversation, activeParticipant);
    const responsive = useResponsiveSizes(collapsed, isSmallWindow, panelWidth);
    const contextSlot = useContextBudgetSlot(
      renderContextBudget,
      derived._conversationId,
      derived.contextStrategy,
      derived.conversationInstructions,
      derived.persona,
    );

    // Fixes adversarial review C5-wrapper #5: old-app `disabledAdd` gates on
    // BOTH playback state and `users.view` permission, not playback alone.
    const disabledAdd = useMemo(
      () => derived.isPlayback || !checkPermission(PERMISSIONS.users.view),
      [derived.isPlayback, checkPermission],
    );

    // Fixes adversarial review C5-wrapper #6: `hidden` was previously
    // destructured as `_hidden` (never matching the actual prop name) and
    // never checked at all — the panel could no longer be hidden. All hooks
    // above run unconditionally first; the early return comes after, same as
    // old-app `ParticipantsWrapper.jsx`'s `if (hidden) return null;`.
    if (hidden) return null;

    return (
      <Grid
        size={{ xs: responsive.xsSize, lg: responsive.lgSize }}
        sx={wrapperSx(theme, responsive.styleParams, responsive.paddingLeft)}
        data-testid="participants-wrapper"
      >
        <ParticipantDetailsProvider participants={derived.participants}>
          <Participants
            participants={derived.participants}
            collapsed={collapsed}
            onCollapsed={onCollapsed}
            disabledEdit={derived.isPlayback}
            disabledAdd={disabledAdd}
            activeParticipantId={derived.activeParticipantId}
            onSelectParticipant={onSelectParticipant}
            onDeleteParticipant={onDeleteParticipant}
            onEditParticipant={onEditParticipant}
            onUpdateParticipant={onUpdateParticipant}
            editingToolkit={editingToolkit}
            resolveToolkitIcon={resolveToolkitIcon}
            isMcpVisible={isMcpVisible}
            renderContextBudget={contextSlot}
            {...(derived._conversationId !== undefined ? { conversationId: derived._conversationId } : {})}
            maxVisibleUsers={maxVisibleUsers}
            isSmallWindow={isSmallWindow}
            selectedManager={selectedManager}
            newConversationSelectedManager={newConversationSelectedManager}
          />
        </ParticipantDetailsProvider>
      </Grid>
    );
  },
);

/* ── custom hooks ──────────────────────────────────────────────────────── */

function useParticipantsDerived(
  activeConversation: ParticipantsWrapperProps['activeConversation'],
  activeParticipant: ParticipantsWrapperProps['activeParticipant'],
) {
  return {
    participants: useMemo(() => activeConversation?.participants ?? [], [activeConversation]),
    isPlayback: useMemo(() => !!activeConversation?.isPlayback, [activeConversation]),
    isActive: useMemo(() => !activeConversation?.isNew && !activeConversation?.isPlayback, [activeConversation]),
    // The id the context-budget slot is allowed to query with. A draft
    // (`isNew`) or replayed (`isPlayback`) conversation has no server-side
    // state, so it must NOT be handed down as a real conversation — passing
    // `activeConversation.id` straight through bypasses this and makes the
    // budget query for something that cannot answer.
    _conversationId: useMemo(
      () => {
        const isActive = !activeConversation?.isNew && !activeConversation?.isPlayback;
        return isActive ? activeConversation?.id : undefined;
      },
      [activeConversation],
    ),
    // `getChatParticipantUniqueId` (`../lib/helpers`), not `@/entities/participant`'s
    // camelCase-keyed `chatParticipantUniqueId` — the latter always misses on this
    // feature's snake_case shape and returns the same id for every participant.
    activeParticipantId: useMemo(
      () => activeParticipant ? getChatParticipantUniqueId(activeParticipant) : undefined,
      [activeParticipant],
    ),
    contextStrategy: useMemo(
      () => (activeConversation?.meta?.context_strategy ?? activeConversation?.context_strategy) as Record<string, unknown> | undefined,
      [activeConversation],
    ),
    conversationInstructions: useMemo(
      () => (activeConversation?.instructions ?? activeConversation?.meta?.instructions) as string | undefined,
      [activeConversation],
    ),
    persona: useMemo(
      () => activeConversation?.meta?.persona ?? activeConversation?.persona,
      [activeConversation],
    ),
  };
}

function useResponsiveSizes(collapsed: boolean, isSmallWindow: boolean, panelWidth: number) {
  const { xsSize, lgSize } = useWrapperGridSizes(collapsed);
  const styleParams = deriveWrapperStyleParams(isSmallWindow, panelWidth, collapsed);
  const paddingLeft = derivePaddingLeft(collapsed);
  return { xsSize, lgSize, styleParams, paddingLeft };
}

function useContextBudgetSlot(
  renderContextBudget: ParticipantsWrapperProps['renderContextBudget'],
  _conversationId: string | number | undefined,
  contextStrategy: Record<string, unknown> | undefined,
  conversationInstructions: string | undefined,
  persona: unknown,
) {
  return renderContextBudget
    ? (slotProps: Parameters<Parameters<typeof ParticipantsWrapper>[0]['renderContextBudget']>[0]) =>
        renderContextBudget({
          conversationId: slotProps.conversationId ?? _conversationId,
          collapsed: slotProps.collapsed,
          contextStrategy: slotProps.contextStrategy ?? contextStrategy,
          setActiveConversation: slotProps.setActiveConversation,
          conversationInstructions: slotProps.conversationInstructions ?? conversationInstructions,
          persona: slotProps.persona ?? persona,
        })
    : undefined;
}

ParticipantsWrapper.displayName = 'ParticipantsWrapper';
