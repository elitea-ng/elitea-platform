import type { ReactNode } from 'react';
import { useCallback, useMemo, useRef, useState } from 'react';

import { useNavigate, useRouteContext, useRouterState } from '@tanstack/react-router';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { useChatSessionStore } from '@/entities/conversation';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';
import { t } from '@/shared/i18n';
import { getConfig } from '@/shared/config';

import { createEntityOptions, type CreateEntityKind, type CreateEntityOption } from '../lib/constants';
import {
  currentEntityFromPathname,
  defaultEntityKind,
  hasCreatePermission,
  hasMainButtonCreatePermission,
  resolveCreateCommand,
} from '../lib/command';

export interface CreateEntityButtonProps {
  /** SHELL-010-style permission gating, computed by the caller (Sidebar) from `usePermissionList`. */
  permissions: ReadonlySet<string>;
  /** Sidebar-collapsed layout mode — icon-only button, no dropdown label. */
  collapsed?: boolean;
  /** The project the "own LLMs" config gate (§7.1 C7 `allow_project_own_llms`) compares against `vite_public_project_id`. */
  projectId?: string | undefined;
}

/** `allow_project_own_llms` reads `unknown` (schema.ts) — old app compares with a strict `=== false`, never coerced. */
function ownLlmsDisabled(): boolean {
  const result = getConfig();
  if (result.status !== 'ok') return false;
  return result.config.allow_project_own_llms === false;
}

/**
 * `projectId === undefined` (no project resolved yet) must NOT early-return
 * `false` here: the old app's own check is a loose `projectId !=
 * PUBLIC_PROJECT_ID` (`CreateEntityButton.jsx:79-85`), and `undefined !=
 * <a defined id>` is `true` in JS — an unresolved project counts as "not
 * the public project" and therefore still contributes to disabling the
 * button, the OPPOSITE polarity from returning early with `false`.
 */
function isOwnLlmsBlocked(activeKind: CreateEntityKind, projectId: string | undefined): boolean {
  if (activeKind !== 'configuration') return false;
  if (!ownLlmsDisabled()) return false;
  const configResult = getConfig();
  const publicProjectId = configResult.status === 'ok' ? configResult.config.vite_public_project_id : undefined;
  return projectId !== publicProjectId;
}

/**
 * `personal_project_id` from the TanStack Router root context's
 * `auth.getUser()` (`src/app/router-context.ts`'s `AuthUser.
 * personal_project_id` — outside this cluster's file scope, read
 * structurally rather than imported, per `no-upward-from-features`). Same
 * seam `features/settings/ui/personal-tokens/TokensTable.tsx` and
 * `features/toolkits/lib/hooks/useSelectedProjectId.ts` already use, so
 * this self-resolves once R2 replaces the stub context — no change needed
 * here or in the `Sidebar`/`SidebarBody` caller.
 */
interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function isPersonalProjectIdContext(value: unknown): value is PersonalProjectIdContext {
  return typeof value === 'object' && value !== null;
}

/** Pure extraction, mirrors `TokensTable.tsx`'s `selectPersonalProjectId`. */
function selectPersonalProjectId(context: unknown): string | undefined {
  if (!isPersonalProjectIdContext(context)) return undefined;
  return context.auth?.getUser?.()?.personal_project_id;
}

/**
 * SHELL "no personal project / viewing public project" gate (old app:
 * `useDisablePersonalSpace` — `!privateProjectId && selectedProjectId ==
 * PUBLIC_PROJECT_ID`, `CreateEntityButton.jsx:77` +
 * `useDisablePersonalSpace.hooks.js`). Unlike {@link isOwnLlmsBlocked},
 * `projectId === undefined` here DOES early-return `false`: the old app's
 * loose `selectedProjectId == PUBLIC_PROJECT_ID` is `false` when
 * `selectedProjectId` is `undefined` (loose `==` against a defined,
 * non-null value), so an unresolved project does not trip this gate.
 */
function isPersonalSpaceBlocked(projectId: string | undefined, personalProjectId: string | undefined): boolean {
  if (projectId === undefined || personalProjectId) return false;
  const configResult = getConfig();
  const publicProjectId = configResult.status === 'ok' ? configResult.config.vite_public_project_id : undefined;
  return projectId === publicProjectId;
}

/**
 * The merged-pill corners of the split button
 * (`CreateEntityButton.jsx`'s `mainButton`/`chevronButton`: `0.875rem
 * 0.125rem 0.125rem 0.875rem` and its mirror). Composed from the radius
 * TOKENS rather than those literals (R-T10): on a 28px-tall control the
 * browser clamps `radiusPill` to 14px, which is the baseline's `0.875rem`
 * exactly, and `radiusSm` stands in for the 2px interior join.
 */
function splitRadius(theme: Theme, side: 'start' | 'end'): string {
  const pill = theme.vars.shape.radiusPill;
  const flat = theme.vars.shape.radiusSm;
  return side === 'start' ? `${pill} ${flat} ${flat} ${pill}` : `${flat} ${pill} ${pill} ${flat}`;
}

interface TriggerProps {
  isSimple: boolean;
  collapsed: boolean;
  disabled: boolean;
  currentLabel: string | undefined;
  menuOpen: boolean;
  onMainClick: () => void;
  onToggleMenu: () => void;
}

/** The main button — a plain icon+label trigger on simple/collapsed routes, or a split button with a dropdown chevron otherwise. */
function CreateEntityTrigger({
  isSimple,
  collapsed,
  disabled,
  currentLabel,
  menuOpen,
  onMainClick,
  onToggleMenu,
}: TriggerProps): ReactNode {
  if (isSimple) {
    // [R1 fix] The old app's `showSimpleButton` branch renders exactly ONE
    // button, and its `onClick` is `handleOpenMenu` (`CreateEntityButton.
    // jsx:308`) — it ONLY ever opens the 13-item dropdown, never navigates
    // directly. There is no separate chevron in this branch (that only
    // exists in the split-button branch below), so `onToggleMenu` here is
    // the ONLY way to reach the dropdown while simple/collapsed — wiring
    // this to `onMainClick` instead (as a prior version of this file did)
    // makes the entity picker completely unreachable whenever the sidebar
    // is collapsed, and fires a direct navigation against a possibly-stale
    // `activeKind` on every simple/unrecognised route.
    return (
      <BaseBtn
        variant="special"
        disabled={disabled}
        startIcon={<PlusIcon />}
        onClick={onToggleMenu}
        data-testid="sidebar-create-button"
        sx={{ width: '100%', ...(collapsed ? { minWidth: '1.75rem', width: '1.75rem' } : {}) }}
      >
        {!collapsed ? t('widgets.createButton.label', 'Create') : null}
      </BaseBtn>
    );
  }

  return (
    <>
      <BaseBtn
        variant="special"
        disabled={disabled}
        startIcon={<PlusIcon />}
        onClick={onMainClick}
        data-testid="sidebar-create-button"
        // The two halves are one MERGED pill: rounded at the group's outer
        // ends, near-square at the interior join
        // (`CreateEntityButton.jsx`'s `mainButton`/`chevronButton`). The port
        // had left both halves on the uniform `radiusPill`, which rendered
        // the split button as two separate lozenges with a visible seam —
        // the shape is not expressible as a single radius token, so the
        // baseline's own four-corner literals are used verbatim.
        sx={(theme: Theme) => ({ flex: '1 1 auto', borderRadius: splitRadius(theme, 'start') })}
      >
        {currentLabel}
      </BaseBtn>
      <BaseBtn
        variant="special"
        disabled={disabled}
        onClick={onToggleMenu}
        aria-label={t('widgets.createButton.chooseEntity', 'Choose what to create')}
        sx={(theme: Theme) => ({
          padding: '0 0.5rem',
          minWidth: 'unset',
          width: 'fit-content',
          borderRadius: splitRadius(theme, 'end'),
        })}
      >
        {/* `ArrowDownIcon` is a 1rem glyph in the baseline (measured 16x16 in
            production); MUI's default `medium` SvgIcon is 24px, which is what
            made this half of the split button 6px wider than production's. */}
        <ExpandMoreIcon style={{ width: '1rem', height: '1rem', transform: menuOpen ? 'rotate(180deg)' : 'none' }} />
      </BaseBtn>
    </>
  );
}

interface DropdownProps {
  open: boolean;
  anchorEl: HTMLElement | null;
  collapsed: boolean;
  options: readonly CreateEntityOption[];
  activeKind: CreateEntityKind;
  permissions: ReadonlySet<string>;
  onSelect: (kind: CreateEntityKind) => void;
}

function CreateEntityDropdown({ open, anchorEl, collapsed, options, activeKind, permissions, onSelect }: DropdownProps): ReactNode {
  return (
    <Popper
      open={open}
      anchorEl={anchorEl}
      placement={collapsed ? 'right-start' : 'bottom-start'}
      sx={{ zIndex: 1300 }}
    >
      <Paper
        sx={(theme: Theme) => ({
          background: theme.vars.palette.background.secondary,
          borderRadius: theme.vars.shape.radiusMd,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          padding: '0.5rem 0',
          minWidth: '11.5rem',
          boxShadow: theme.vars.palette.boxShadow.default,
        })}
      >
        {options.map((option) => {
          const optionDisabled = !hasCreatePermission(option.kind, permissions);
          return (
            <Box
              key={option.kind}
              role="menuitem"
              tabIndex={optionDisabled ? -1 : 0}
              aria-disabled={optionDisabled}
              onClick={() => onSelect(option.kind)}
              onKeyDown={(event) => {
                if (event.key === 'Enter' || event.key === ' ') {
                  event.preventDefault();
                  onSelect(option.kind);
                }
              }}
              sx={(theme: Theme) => ({
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                padding: theme.spacing(1, 2),
                cursor: optionDisabled ? 'not-allowed' : 'pointer',
                color: optionDisabled ? theme.vars.palette.text.default : theme.vars.palette.text.secondary,
                ...(activeKind === option.kind && !optionDisabled
                  ? { backgroundColor: theme.vars.palette.split.pressed }
                  : {}),
              })}
            >
              <Typography variant="labelSmall">{option.label}</Typography>
            </Box>
          );
        })}
      </Paper>
    </Popper>
  );
}

/**
 * SHELL-013..026 — the sidebar's global "Create" button: a 13-entity
 * dropdown (or, on simple/collapsed routes, a plain icon button) that
 * navigates to the right create surface and permission-gates each option.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/widgets/sidebar-root/ui/button/
 * CreateEntityButton.jsx`; the route-detection/permission/navigation
 * decisions live in `../lib/command.ts` (pure, independently tested), and
 * the trigger/dropdown rendering is split into `CreateEntityTrigger`/
 * `CreateEntityDropdown` above (§3.5 complexity budget) — this component's
 * own job is just wiring state to those two. Reduced-scope notes are in
 * `command.ts`'s header and in `../index.ts`.
 */
export function CreateEntityButton({ permissions, collapsed = false, projectId }: CreateEntityButtonProps): ReactNode {
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);
  const [selectedKind, setSelectedKind] = useState<CreateEntityKind>(() => defaultEntityKind(pathname));
  const [menuOpen, setMenuOpen] = useState(false);
  const anchorRef = useRef<HTMLElement | null>(null);

  const options = useMemo(() => createEntityOptions(), []);
  const currentLabel = useMemo(() => {
    const kind = currentEntityFromPathname(pathname);
    return kind ? options.find((option) => option.kind === kind)?.label : undefined;
  }, [pathname, options]);

  const isSimple = collapsed || currentLabel === undefined;
  const activeKind = currentEntityFromPathname(pathname) ?? selectedKind;
  const isSystemPromptsPage = pathname.includes('/settings/prompts');
  // [R2 fix] Old app: `shouldDisableCreatingChat = selectedOption === 'Chat'
  // && isCreatingNewConversation` (`CreateEntityButton.jsx:52-55`) — guards
  // against a duplicate/concurrent create-chat click while one is already
  // in flight. `isCreatingNewConversation` now has a real, queryable source
  // (`entities/conversation`'s `useChatSessionStore`, written by
  // `useSelectConversation.js`'s port) since this unit's original "no chat
  // feature slice exists yet" disclaimer was written.
  const isCreatingNewConversation = useChatSessionStore((state) => state.isCreatingNewConversation);
  const shouldDisableCreatingChat = activeKind === 'chat' && isCreatingNewConversation;

  const runCommand = useCallback(
    (kind: CreateEntityKind) => {
      const target = resolveCreateCommand(kind, pathname);
      void navigate({ to: target.to, search: target.search, replace: target.replace });
    },
    [navigate, pathname],
  );

  const handleMainClick = useCallback(() => {
    runCommand(activeKind);
  }, [runCommand, activeKind]);

  const handleOptionClick = useCallback(
    (kind: CreateEntityKind) => {
      if (!hasCreatePermission(kind, permissions)) return;
      setSelectedKind(kind);
      setMenuOpen(false);
      runCommand(kind);
    },
    [permissions, runCommand],
  );

  const toggleMenu = useCallback(() => setMenuOpen((open) => !open), []);
  const closeMenu = useCallback(() => setMenuOpen(false), []);

  // [R5 fix] `hasMainButtonCreatePermission` (not the plain
  // `hasCreatePermission` the dropdown items below still use) — the old
  // app's own two gates diverge for Bucket by design, see that function's
  // doc comment in `lib/command.ts`.
  const disabled =
    !hasMainButtonCreatePermission(activeKind, permissions) ||
    isSystemPromptsPage ||
    isOwnLlmsBlocked(activeKind, projectId) ||
    isPersonalSpaceBlocked(projectId, personalProjectId) ||
    shouldDisableCreatingChat;

  return (
    <ClickAwayListener onClickAway={closeMenu}>
      <Box
        component="span"
        ref={anchorRef}
        sx={{ position: 'relative', display: 'flex', justifyContent: 'center', width: '100%', boxSizing: 'border-box' }}
      >
        <Box sx={{ display: 'flex', width: '100%', gap: '0.0625rem' }}>
          <CreateEntityTrigger
            isSimple={isSimple}
            collapsed={collapsed}
            disabled={disabled}
            currentLabel={currentLabel}
            menuOpen={menuOpen}
            onMainClick={handleMainClick}
            onToggleMenu={toggleMenu}
          />
        </Box>

        <CreateEntityDropdown
          open={menuOpen}
          anchorEl={anchorRef.current}
          collapsed={collapsed}
          options={options}
          activeKind={activeKind}
          permissions={permissions}
          onSelect={handleOptionClick}
        />
      </Box>
    </ClickAwayListener>
  );
}
