import { memo, useCallback, useRef, useState } from 'react';
import type { RefObject } from 'react';

import AddIcon from '@mui/icons-material/Add';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import IconButton from '@mui/material/IconButton';
import Popper from '@mui/material/Popper';
import Tooltip from '@mui/material/Tooltip';

import { agentEditorHooks } from '@/features/agents';
import { t } from '@/shared/i18n';

import { AttachmentButton } from './AttachmentButton';
import type { AttachmentButtonHandle } from './AttachmentButton';
import { AttachmentsPanel, MainMenuList, MenuPaper } from './PlusChatButton.parts';
import {
  MENU_ITEMS,
  resolveActiveSubmenuView,
  type PlusChatButtonEntitySubmenus,
  type SubmenuKey,
} from './PlusChatButton.helpers';
import { PlusChatSubmenu } from './PlusChatSubmenu';

/**
 * Chat button primitive: PlusChatButton
 *
 * A "+" icon button that opens a dropdown menu for adding agents, pipelines,
 * toolkits, MCPs, attachments and internal-tool toggles. Every entity
 * category (agents/pipelines/toolkits/mcps) renders whatever real items the
 * composition root supplies via `entitySubmenus` and calls
 * `entitySubmenus.onSelectParticipant` on click — this widget cannot fetch
 * that data itself: the equivalent of baseline's `useApplicationSubmenu`
 * lives at `processes/chat/model/useChatEntityBrowser.ts`, a layer ABOVE
 * `widgets/` (R-L1 forbids the upward import), so wiring real data in is a
 * later composition-root stage's job, same as `participants` already was.
 * Until that lands, an omitted category just renders its own empty state
 * (`PlusChatSubmenu`'s `emptyMessage`), not fake placeholder rows.
 *
 * "Tools" renders the real internal-tools catalog
 * (`agentEditorHooks.useAvailableInternalTools()`, feature/agents — the
 * same source baseline's `PlusChatButton.jsx` reads directly) with working
 * checkboxes that call `onInternalToolsConfigChange`. "MCPs" is gated by
 * `agentEditorHooks.useIsMcpVisible()`, matching baseline's `isMcpVisible`
 * gate. "Attachments" embeds the real `AttachmentButton` (via
 * `PlusChatButton.parts.tsx`'s `AttachmentsPanel`) wired to the real
 * `onAttachFiles`/`disableAttachments`/`attachments`/`limits` props,
 * instead of discarding them.
 *
 * Split across `PlusChatButton.helpers.ts` (pure submenu-item/create-config
 * logic) and `PlusChatButton.parts.tsx` (the attachments panel + main menu
 * list JSX) purely to keep this file under the §3.5 file-length-400 and
 * cyclomatic-complexity-12 budgets.
 *
 * Prop contract (injected by the composition root through `slots.attachmentButton` / the footer row):
 *   - `onAttachFiles`                — real file-picker callback (also used by the embedded AttachmentButton)
 *   - `disableAttachments`           — disables the attachments row
 *   - `attachments`                  — current attachment list (shown + counted)
 *   - `limits`                       — optional overrides merged onto `ATTACHMENT_LIMITS` (see `AttachmentButton`'s own doc comment)
 *   - `onInternalToolsConfigChange`  — toggles a tool on/off
 *   - `internal_tools`               — currently-enabled tool names
 *   - `onCreateAgent`                — navigate/create an agent
 *   - `onCreatePipeline`             — navigate/create a pipeline
 *   - `onCreateToolkit`              — navigate/create a toolkit (`isMcp` for the MCPs category's "create new")
 *   - `participants`                 — list of participants for agent selection
 *   - `entitySubmenus`               — real pipelines/toolkits/mcps lists + the shared select callback (grouped to stay under the §3.5 12-prop budget)
 *   - `attachmentButtonRef`          — imperative drop/paste handle, attached to a hidden always-mounted
 *     `AttachmentButton` (baseline `PlusChatButton.jsx:313-320`'s `hiddenAttachment` Box). The visible
 *     attachment rows live inside a `Popper` and only exist while the menu is open, so without this
 *     hidden mount `useNewChatInputAttachmentBridge` (features/chat-input) finds `ref.current === null`
 *     and silently discards every file dropped or pasted onto the chat surface.
 */
export type { PlusChatButtonEntitySubmenus } from './PlusChatButton.helpers';

export interface PlusChatButtonProps {
  onAttachFiles?: (files: readonly File[]) => void;
  disableAttachments?: boolean;
  attachments?: readonly File[];
  limits?: Record<string, number>;
  onInternalToolsConfigChange?: (config: { key: string; value: boolean }) => void;
  internal_tools?: string[];
  onCreateAgent?: () => void;
  onCreatePipeline?: () => void;
  onCreateToolkit?: (isMcp?: boolean) => void;
  participants?: unknown[];
  entitySubmenus?: PlusChatButtonEntitySubmenus;
  attachmentButtonRef?: RefObject<AttachmentButtonHandle | null>;
}

export const PlusChatButton = memo(
  ({
    onAttachFiles = () => {},
    disableAttachments = false,
    attachments = [],
    limits = {},
    onInternalToolsConfigChange,
    internal_tools,
    onCreateAgent,
    onCreatePipeline,
    onCreateToolkit,
    participants,
    entitySubmenus,
    attachmentButtonRef,
  }: PlusChatButtonProps) => {
    const [menuOpen, setMenuOpen] = useState(false);
    const [activeSubmenu, setActiveSubmenu] = useState<SubmenuKey | null>(null);
    // The ROW the open submenu is anchored to, so it sits beside that row
    // rather than replacing the whole menu (baseline `hoveredAnchorEl`).
    const [submenuAnchor, setSubmenuAnchor] = useState<HTMLElement | null>(null);
    const [searchValue, setSearchValue] = useState('');
    const anchorRef = useRef<HTMLButtonElement>(null);

    const availableTools = agentEditorHooks.useAvailableInternalTools();
    const isMcpVisible = agentEditorHooks.useIsMcpVisible();

    const onOpen = entitySubmenus?.onOpen;
    // `onOpen` is called OUTSIDE the `setMenuOpen` updater on purpose: React
    // may invoke an updater twice (StrictMode double-render), and this one
    // starts network requests.
    const toggleMenu = useCallback(() => {
      if (!menuOpen) onOpen?.();
      setMenuOpen(!menuOpen);
      if (activeSubmenu) {
        setActiveSubmenu(null);
        setSubmenuAnchor(null);
      }
    }, [menuOpen, activeSubmenu, onOpen]);

    const closeMenu = useCallback(() => {
      setMenuOpen(false);
      setActiveSubmenu(null);
      setSubmenuAnchor(null);
      setSearchValue('');
    }, []);

    const handleSubmenuOpen = useCallback((key: SubmenuKey, anchor: HTMLElement) => {
      setActiveSubmenu(key);
      setSubmenuAnchor(anchor);
      // Each category has its own search; carrying the previous one over
      // would silently filter the newly-opened list by an unrelated string.
      setSearchValue('');
    }, []);

    const handleCreateAgent = useCallback(() => {
      onCreateAgent?.();
      closeMenu();
    }, [onCreateAgent, closeMenu]);

    const handleCreatePipeline = useCallback(() => {
      onCreatePipeline?.();
      closeMenu();
    }, [onCreatePipeline, closeMenu]);

    const handleCreateToolkit = useCallback(() => {
      onCreateToolkit?.();
      closeMenu();
    }, [onCreateToolkit, closeMenu]);

    const handleCreateMcp = useCallback(() => {
      onCreateToolkit?.(true);
      closeMenu();
    }, [onCreateToolkit, closeMenu]);

    const handleSelectParticipant = useCallback(
      (participant: unknown) => {
        entitySubmenus?.onSelectParticipant?.(participant);
        closeMenu();
      },
      [entitySubmenus, closeMenu],
    );

    const handleToggleParticipant = useCallback(
      (participant: unknown) => {
        entitySubmenus?.onSelectParticipant?.(participant);
      },
      [entitySubmenus],
    );

    const { items: submenuItems, createConfig } = resolveActiveSubmenuView(activeSubmenu, {
      participants,
      entities: entitySubmenus,
      availableTools,
      enabledToolNames: internal_tools,
      onInternalToolsConfigChange,
      onSelect: handleSelectParticipant,
      onToggle: handleToggleParticipant,
      onCreate: {
        agents: handleCreateAgent,
        pipelines: handleCreatePipeline,
        toolkits: handleCreateToolkit,
        mcps: handleCreateMcp,
      },
    });

    const visibleMenuItems = MENU_ITEMS.filter((item) => item.key !== 'mcps' || isMcpVisible);

    return (
      <>
        {/*
          * The drag-and-drop / paste target. The visible AttachmentButton rows
          * below are inside a Popper and unmount whenever the menu is closed,
          * so the injected imperative handle must live on this always-mounted
          * instance instead (baseline PlusChatButton.jsx:313-320's
          * `hiddenAttachment` Box). `dropTargetOnly` renders no DOM for it at
          * all — see that prop's own doc for why a hidden CONTROL was worse
          * than none.
          */}
        <AttachmentButton
          dropTargetOnly
          ref={attachmentButtonRef}
          onAttachFiles={onAttachFiles}
          disableAttachments={disableAttachments}
          attachments={attachments}
          limits={limits}
        />

        <Tooltip title={t('widgets.chat.plusChatButton.tooltip', 'Add files, agents, toolkits and more...')} placement="top">
          <IconButton
            ref={anchorRef}
            color="secondary"
            aria-label={t('widgets.chat.plusChatButton.ariaLabel', 'plus menu')}
            data-testid="plus-menu-button"
            onClick={toggleMenu}
            sx={{ marginLeft: 0 }}
          >
            <AddIcon fontSize="small" />
          </IconButton>
        </Tooltip>

        <Popper
          open={menuOpen}
          anchorEl={anchorRef.current}
          placement="bottom-start"
          sx={{ zIndex: 9998 }}
        >
          {/*
            * ONE ClickAwayListener around BOTH papers. The submenu renders in
            * its own portal, so a listener that only wrapped the main paper
            * would treat every click inside the submenu — selecting an agent,
            * ticking a module — as a click away and close the menu underneath
            * the pointer.
            */}
          <ClickAwayListener onClickAway={closeMenu}>
            <Box>
              <MenuPaper>
                <MainMenuList
                  items={visibleMenuItems}
                  onSelectSubmenu={handleSubmenuOpen}
                  attachRow={
                    <AttachmentButton
                      showLabel
                      disableAttachments={disableAttachments}
                      attachments={attachments}
                      onAttachFiles={onAttachFiles}
                      limits={limits}
                    />
                  }
                />
              </MenuPaper>

              <Popper
                open={activeSubmenu !== null && submenuAnchor !== null}
                anchorEl={submenuAnchor}
                placement="right-start"
                sx={{ zIndex: 9999 }}
              >
                <MenuPaper>
                  {activeSubmenu === 'attachments' ? (
                    <AttachmentsPanel
                      disableAttachments={disableAttachments}
                      attachments={attachments}
                      onAttachFiles={onAttachFiles}
                      limits={limits}
                    />
                  ) : (
                    <PlusChatSubmenu
                      items={submenuItems}
                      searchValue={searchValue}
                      onSearchChange={(e) => setSearchValue(e.target.value)}
                      searchPlaceholder={t('widgets.chat.plusChatButton.searchPlaceholder', 'Search...')}
                      showCreateNew={createConfig?.showCreateNew ?? false}
                      onCreateNew={createConfig?.onCreateNew}
                      createNewLabel={t('widgets.chat.plusChatButton.createNewLabel', 'Create new')}
                      emptyMessage={t('widgets.chat.plusChatButton.noItemsAvailable', 'Nothing available')}
                      noResultsMessage={t('widgets.chat.plusChatButton.noItemsFound', 'No items found')}
                      isLoading={false}
                    />
                  )}
                </MenuPaper>
              </Popper>
            </Box>
          </ClickAwayListener>
        </Popper>
      </>
    );
  },
);

PlusChatButton.displayName = 'PlusChatButton';
