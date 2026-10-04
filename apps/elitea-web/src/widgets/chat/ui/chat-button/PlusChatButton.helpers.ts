/**
 * Pure (non-JSX) helpers for `PlusChatButton.tsx` — split out purely to
 * keep that file under the §3.5 file-length-400 budget, same rationale as
 * `AgentEditorPanel.derive.ts`/`NewChatInput.types.ts` elsewhere in this
 * codebase.
 */
import { t } from '@/shared/i18n';
import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import type { SvgIconComponent } from '@/shared/ui/icons/svg-icon.types';
import { ToolIcon } from '@/shared/ui/icons/tool-icon';
import { ValueIcon } from '@/shared/ui/icons/value-icon';

export interface PlusChatButtonEntitySubmenus {
  readonly agents?: readonly unknown[];
  readonly pipelines?: readonly unknown[];
  readonly toolkits?: readonly unknown[];
  readonly mcps?: readonly unknown[];
  readonly onSelectParticipant?: (participant: unknown) => void;
  readonly getParticipantMenuState?: (participant: unknown) => {
    readonly checked?: boolean;
    readonly pending?: boolean;
  };
  /**
   * Fired the first time the menu is opened, so the composition root can
   * defer its four participant queries until someone actually looks. Carried
   * in this bundle rather than as its own `PlusChatButton` prop because the
   * bundle is already threaded through every layer between `processes/chat`
   * and this widget, and the two travel together by definition.
   */
  readonly onOpen?: () => void;
}

/**
 * `attachments` is NOT a submenu key any more — see `MENU_ITEMS`. It stays in
 * the union because `resolveActiveSubmenuView`'s exhaustive switch and the
 * component's `activeSubmenu` state are both still typed by it, and a caller
 * may still open the attachments panel directly.
 */
export type SubmenuKey = 'agents' | 'pipelines' | 'toolkits' | 'mcps' | 'attachments' | 'tools';

export interface MenuItemDef {
  key: string;
  label: string;
  /** The ported icon component rendered at the start of the row. */
  Icon: SvgIconComponent;
  submenu?: SubmenuKey;
}

/**
 * The expandable rows, in the baseline's own order and wording
 * (`PlusChatButton.jsx:42-48`'s `EXPANDABLE_ITEMS`).
 *
 * Three corrections against what this file used to state:
 *
 *  - **"Modules", not "Tools".** The internal-tools toggles (Image creation,
 *    Data Analysis, Planner, Python Sandbox, Swarm Mode, …) are labelled
 *    `Modules` in the product. "Tools" reads as the toolkits below it.
 *  - **Modules comes FIRST**, before the four entity categories.
 *  - **Attachments is not in this list.** The baseline renders the
 *    attachment control as its own row above these — a direct action with a
 *    remaining-file count, not a category that opens a submenu. Burying it
 *    one level down made attaching a file a two-click operation that the
 *    reference does in one.
 *
 * `icon` was an emoji string. Emoji render in the OS's own colour and metrics
 * and sit outside the icon set entirely; these are the ported assets the rest
 * of the app uses.
 */
export const MENU_ITEMS: MenuItemDef[] = [
  { key: 'tools', label: t('widgets.chat.plusChatButton.menuModules', 'Modules'), Icon: ValueIcon, submenu: 'tools' },
  { key: 'agents', label: t('widgets.chat.plusChatButton.menuAgents', 'Agents'), Icon: ApplicationsIcon, submenu: 'agents' },
  { key: 'pipelines', label: t('widgets.chat.plusChatButton.menuPipelines', 'Pipelines'), Icon: FlowIcon, submenu: 'pipelines' },
  { key: 'toolkits', label: t('widgets.chat.plusChatButton.menuToolkits', 'Toolkits'), Icon: ToolIcon, submenu: 'toolkits' },
  { key: 'mcps', label: t('widgets.chat.plusChatButton.menuMcps', 'MCPs'), Icon: McpIcon, submenu: 'mcps' },
];

interface SubmenuItem {
  key: string;
  label: string;
  onClick?: () => void;
  checked?: boolean;
  pending?: boolean;
}

/** `agents`/`pipelines`/`toolkits`/`mcps` share the same "id/name -> selectable row" shape. */
function toEntityItems(
  list: readonly unknown[],
  kind: string,
  onSelect: (participant: unknown) => void,
  getState: PlusChatButtonEntitySubmenus['getParticipantMenuState'],
  showToggle = false,
): SubmenuItem[] {
  return list.map((entity, i) => {
    const item = entity as { id?: string | number; name?: string };
    const fallbackLabel = `${kind.charAt(0).toUpperCase()}${kind.slice(1)} ${i + 1}`;
    const state = getState?.(entity);
    return {
      key: `${kind}-${item.id ?? i}`,
      label: item.name ?? fallbackLabel,
      onClick: () => onSelect(entity),
      ...(showToggle ? { checked: state?.checked ?? false } : {}),
      ...(state?.pending === true ? { pending: true } : {}),
    };
  });
}

/** Only called with a truthy `activeSubmenu` (the caller narrows `SubmenuKey | null` first), so it takes the non-null member directly — `switch-exhaustiveness-check` requires every `SubmenuKey` literal handled explicitly (a `default` alone isn't accepted by this project's config). Not exported — only `resolveActiveSubmenuView` below calls it; that's the one this file's own consumers (`PlusChatButton.tsx`) actually need. */
function buildSubmenuItems(input: {
  readonly activeSubmenu: SubmenuKey;
  readonly participants: readonly unknown[];
  readonly entities: PlusChatButtonEntitySubmenus;
  readonly availableTools: readonly { readonly name: string; readonly title: string }[];
  readonly enabledToolNames: readonly string[];
  readonly onInternalToolsConfigChange?: ((config: { key: string; value: boolean }) => void) | undefined;
  readonly onSelect: (participant: unknown) => void;
  readonly onToggle: (participant: unknown) => void;
}): SubmenuItem[] {
  const { activeSubmenu, participants, entities, availableTools, enabledToolNames, onInternalToolsConfigChange, onSelect, onToggle } = input;
  switch (activeSubmenu) {
    case 'agents':
      return toEntityItems(entities.agents ?? participants, 'agent', onSelect, entities.getParticipantMenuState);
    case 'pipelines':
      return toEntityItems(entities.pipelines ?? [], 'pipeline', onSelect, entities.getParticipantMenuState);
    case 'toolkits':
      return toEntityItems(entities.toolkits ?? [], 'toolkit', onToggle, entities.getParticipantMenuState, true);
    case 'mcps':
      return toEntityItems(entities.mcps ?? [], 'mcp', onToggle, entities.getParticipantMenuState, true);
    case 'tools':
      return availableTools.map((tool) => {
        const isEnabled = enabledToolNames.includes(tool.name);
        return {
          key: tool.name,
          label: tool.title,
          checked: isEnabled,
          onClick: () => onInternalToolsConfigChange?.({ key: tool.name, value: !isEnabled }),
        };
      });
    case 'attachments':
      return [];
  }
}

interface SubmenuCreateConfig {
  readonly showCreateNew: boolean;
  readonly onCreateNew: (() => void) | undefined;
  /** "Create Agent", "Create Pipeline", … — the row names what it creates (#6629). */
  readonly createLabel: string;
}

/**
 * The create row's label per category (#6629). The design names the entity
 * ("Create Agent") instead of the generic "Create new".
 */
function submenuCreateLabel(activeSubmenu: SubmenuKey): string {
  switch (activeSubmenu) {
    case 'agents':
      return t('widgets.chat.plusChatButton.createAgent', 'Create Agent');
    case 'pipelines':
      return t('widgets.chat.plusChatButton.createPipeline', 'Create Pipeline');
    case 'toolkits':
      return t('widgets.chat.plusChatButton.createToolkit', 'Create Toolkit');
    case 'mcps':
      return t('widgets.chat.plusChatButton.createMcp', 'Create MCP');
    case 'tools':
    case 'attachments':
      return t('widgets.chat.plusChatButton.createNewLabel', 'Create new');
  }
}

const CREATABLE_SUBMENUS = new Set<SubmenuKey>(['agents', 'pipelines', 'toolkits', 'mcps']);

/**
 * One lookup instead of an OR-chain plus a 4-way ternary inlined in JSX.
 *
 * `showCreateNew` requires an actual handler, not just a creatable category.
 * It used to be `CREATABLE_SUBMENUS.has(...)` alone, so a composition root
 * that supplied entity lists but no `onCreateAgent`/`onCreatePipeline`/
 * `onCreateToolkit` still rendered a "Create new" row that did nothing when
 * clicked. A row that cannot act is worse than an absent one.
 */
function resolveSubmenuCreateConfig(
  activeSubmenu: SubmenuKey,
  onCreate: Readonly<Partial<Record<'agents' | 'pipelines' | 'toolkits' | 'mcps', () => void>>>,
): SubmenuCreateConfig {
  const onCreateNew = activeSubmenu === 'attachments' || activeSubmenu === 'tools' ? undefined : onCreate[activeSubmenu];
  return {
    showCreateNew: CREATABLE_SUBMENUS.has(activeSubmenu) && onCreateNew !== undefined,
    onCreateNew,
    createLabel: submenuCreateLabel(activeSubmenu),
  };
}

/** The create row's label, or the generic one when no submenu is open. Kept out of the component for its complexity budget. */
export function createRowLabel(createConfig: SubmenuCreateConfig | undefined): string {
  return createConfig?.createLabel ?? t('widgets.chat.plusChatButton.createNewLabel', 'Create new');
}

interface ActiveSubmenuView {
  readonly items: SubmenuItem[];
  readonly createConfig: SubmenuCreateConfig | undefined;
}

/**
 * Combines `buildSubmenuItems`/`resolveSubmenuCreateConfig` behind the one
 * "is a submenu even open" null-check — split out purely to keep the
 * component itself under the §3.5 cyclomatic-complexity-12 budget (moves
 * both `activeSubmenu ? … : …` ternaries, plus every `??` default for the
 * caller-supplied lists, out of the component's own function scope).
 */
export function resolveActiveSubmenuView(
  activeSubmenu: SubmenuKey | null,
  params: {
    readonly participants: readonly unknown[] | undefined;
    readonly entities: PlusChatButtonEntitySubmenus | undefined;
    readonly availableTools: readonly { readonly name: string; readonly title: string }[];
    readonly enabledToolNames: readonly string[] | undefined;
    readonly onInternalToolsConfigChange?: ((config: { key: string; value: boolean }) => void) | undefined;
    readonly onSelect: (participant: unknown) => void;
    readonly onToggle: (participant: unknown) => void;
    readonly onCreate: Readonly<Partial<Record<'agents' | 'pipelines' | 'toolkits' | 'mcps', () => void>>>;
  },
): ActiveSubmenuView {
  if (!activeSubmenu) return { items: [], createConfig: undefined };
  const items = buildSubmenuItems({
    activeSubmenu,
    participants: params.participants ?? [],
    entities: params.entities ?? {},
    availableTools: params.availableTools,
    enabledToolNames: params.enabledToolNames ?? [],
    onInternalToolsConfigChange: params.onInternalToolsConfigChange,
    onSelect: params.onSelect,
    onToggle: params.onToggle,
  });
  return { items, createConfig: resolveSubmenuCreateConfig(activeSubmenu, params.onCreate) };
}
