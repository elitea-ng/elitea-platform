import type { MouseEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import KeyboardArrowDownIcon from '@mui/icons-material/KeyboardArrowDown';
import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import type { AgentPipelineVersionOption } from '../lib/types';

import { renderMenu } from './AgentPipelineVersionSelector.menu';
import { LATEST_VERSION_NAME, filterVersionsBySearch, formatVersionDisplayText, toDisplayVersions } from './AgentPipelineVersionSelector.search';
import type { DisplayVersion } from './AgentPipelineVersionSelector.search';
import {
  contentWrapperSx,
  dropdownIconInvalidSx,
  dropdownIconSx,
  selectorSx,
  versionTextInvalidSx,
  versionTextSx,
  warningIconSx,
} from './AgentPipelineVersionSelector.styles';

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/Tools/AgentPipelineVersionSelector.jsx`.
 *
 * `LATEST_VERSION_NAME` ('base') is duplicated here rather than imported
 * from `entities/version` — same reason `entities/application-form/model/
 * initialValues.ts` already documents for its own copy: `no-sideways-*`
 * boundaries make a fresh small-constant duplication cheaper than a new
 * import edge for one string.
 *
 * MAJOR DISCLOSED REDESIGN — the version-SWITCH mutation is entirely
 * caller-owned. The baseline hook-mixes-with-component version of this file
 * calls, inline: `useApplicationDetailsQuery`/`useLazyGetApplicationVersionDetailQuery`/
 * `useUpdateApplicationRelationMutation` (old-app `@/api/applications`,
 * RTK-Query), `useSetRefetchDetails` (`features/agent/lib/hooks`, a
 * DIFFERENT A1 sub-unit's ownership, same slice but not this one's owned
 * file), `useSelectedProjectId`/`useToast` (generic infra with no
 * `features/`-importable equivalent yet — see `src/app/router-context.ts`'s
 * own doc comment: "this almost certainly blocks every OTHER Wave-2 A* unit
 * the same way"), and `useFormikContext` for `setFieldValue`/`dirty`/
 * `resetForm` (this app has no ambient form context at all in this
 * cluster — see `ToolCard.tsx`'s header for why props, not
 * `useFormContext()`, is this cluster's consistent choice). None of that
 * is "version selector" domain logic — it is application-form mutation
 * orchestration that belongs to whichever sub-unit owns the top-level
 * create/edit form (`AgentEditor.jsx`, per this batch's cross-domain
 * export requirement).
 *
 * What stays here, faithfully: the dropdown's pure presentation
 * (`formatVersionDisplayText`/`displayText`/`isInvalidVersionReference`/
 * `selectedVersion` — all pure functions of `versions` + the tool's own
 * `settings.application_version_id`) and the menu interaction. The caller
 * supplies the resolved `versions` list, the refresh trigger, and a single
 * `onSelectVersion` callback that performs the whole switch (mutation +
 * form sync + toast) — same "imperative trigger, no Formik/Redux/router
 * coupling" shape `entities/application-form/model/mutations.ts` already
 * established for the sibling create/save mutations.
 *
 * `renderTrigger`/`renderMenu` below are plain (lowercase, non-component)
 * render functions, not `<Trigger/>`/`<Menu/>` sub-components — each is its
 * own function scope for the `oxlint` `complexity` budget (≤12; this
 * component measured 17 with everything inline), without also creating a
 * NEW component the §3.5 12-props budget would apply to (its checker keys
 * on a capitalised function name). The two COMMAND items the menu ends with
 * are the same kind of function, moved to `AgentVersionMenuCommands.tsx` for
 * the §3.5 400-line file budget.
 */
export interface AgentPipelineVersionSelectorProps {
  readonly applicationVersionId: number | string | undefined;
  readonly disabled?: boolean | undefined;
  readonly versions: readonly AgentPipelineVersionOption[];
  readonly isRefreshingVersions?: boolean | undefined;
  readonly onRefreshVersions?: (() => void) | undefined;
  readonly isSwitchingVersion?: boolean | undefined;
  readonly onSelectVersion: (version: AgentPipelineVersionOption) => void;
  /**
   * #147 — the "set as default" item, baseline `entities/version/lib/
   * helpers/version.helpers.jsx:11-84` + `ApplicationVersionSelect.jsx:36,
   * 239`. It acts on the version the menu currently marks as selected, and
   * the row that IS the default carries a "Default" marker.
   *
   * **Deliberately one command item, not a pin button on every row.** The
   * baseline puts an `onClick` pin INSIDE each option row. Here each row is
   * a `MenuItem` (`role="menuitem"`, a widget role), so a button inside one
   * is axe's `nested-interactive` — impact "serious", and a rule the E2E
   * `checkA11y` fixture does NOT disable. `secondaryAction`, MUI's answer
   * for the same problem in `features/artifacts`' `BucketSidebar`, exists on
   * `ListItem` and not on `MenuItem`. A command item reaches every version
   * in the same two steps (pick the version, then pin it), stays reachable
   * from the keyboard, and needs no ARIA exception. The baseline's pin is
   * also revealed on row HOVER only, which no keyboard user and no journey
   * test can reach — that is a large part of why #147 stayed invisible.
   *
   * Caller-owned, like `onSelectVersion`: this component opens no dialog and
   * sends no request, it only reports which version was picked. Omit
   * `onSetDefaultVersion` and the item is not rendered at all — that is how
   * the read-only viewer and the tool card (`ToolCardBody`) keep the plain
   * version list they had.
   */
  readonly defaultVersionId?: number | undefined;
  readonly onSetDefaultVersion?: ((version: AgentPipelineVersionOption) => void) | undefined;
  /**
   * #147 — the "delete version" item, the other half of JRNY-015's middle
   * step. It acts on the version the menu currently marks as selected, the
   * same subject `onSetDefaultVersion` acts on, and it is the SAME kind of
   * command item for the same `nested-interactive` reason recorded above.
   *
   * The baseline reaches this behaviour from the "..." control's menu
   * (`ApplicationControls.jsx:145-156`), beside "Set as a default". This app
   * has no such control on the agent editor, and the version bar is where
   * every other version-scoped affordance already lives, so both items sit
   * in this one menu.
   *
   * Caller-owned like the other two callbacks: this component opens no
   * dialog and sends no request. Omit `onDeleteVersion` and no delete item
   * is rendered — that is how the read-only viewer and the tool card keep
   * the plain version list they had.
   */
  readonly onDeleteVersion?: ((version: AgentPipelineVersionOption) => void) | undefined;
}

function renderTrigger(params: { displayText: string; isInvalid: boolean; isSwitching: boolean; disabled: boolean | undefined; isOpen: boolean; onClick: (event: MouseEvent<HTMLElement>) => void }): ReactNode {
  const { displayText, isInvalid, isSwitching, disabled, isOpen, onClick } = params;
  return (
    <Box
      data-testid="version-selector-trigger"
      sx={combineSx(selectorSx, disabled ? { cursor: 'default' } : {})}
      onClick={isSwitching || disabled ? undefined : onClick}
    >
      {isInvalid && <WarningAmberIcon sx={warningIconSx} />}
      <Typography
        variant="bodySmall"
        className="agents-version-text"
        sx={isInvalid ? versionTextInvalidSx : versionTextSx}
      >
        {displayText}
      </Typography>
      {isSwitching && (
        <CircularProgress
          size={16}
          data-testid="version-selector-switching"
        />
      )}
      {!disabled && (
        <KeyboardArrowDownIcon
          className="agents-dropdown-icon"
          sx={combineSx(isInvalid ? dropdownIconInvalidSx : dropdownIconSx, { transform: isOpen ? 'rotate(180deg)' : 'rotate(0deg)' })}
        />
      )}
    </Box>
  );
}

export function AgentPipelineVersionSelector({
  applicationVersionId,
  disabled,
  versions,
  isRefreshingVersions = false,
  onRefreshVersions,
  isSwitchingVersion = false,
  onSelectVersion,
  defaultVersionId,
  onSetDefaultVersion,
  onDeleteVersion,
}: AgentPipelineVersionSelectorProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  // Issue 940/A11 — the dropdown's own search text. Local to this component
  // (not lifted to the caller): search is a pure view-filter over the
  // `versions` prop the caller already supplies, with no server round trip.
  const [searchQuery, setSearchQuery] = useState('');

  const displayVersions = useMemo(() => toDisplayVersions(versions), [versions]);

  // ELITEA-3278/3280 — filtered by name+creator, in the SAME order
  // `toDisplayVersions` already sorted (`.filter()` never reorders), so the
  // timestamp sort survives a search untouched. Distinct from
  // `displayVersions`: the TRIGGER's own label and the invalid-reference
  // check below must keep reading the FULL list — a search that hides the
  // selected row must not make the trigger forget what is selected.
  const filteredVersions = useMemo(() => filterVersionsBySearch(displayVersions, searchQuery), [displayVersions, searchQuery]);

  const isInvalidVersionReference = useMemo(
    () => !!applicationVersionId && displayVersions.length > 0 && !displayVersions.some((v) => v.id === applicationVersionId),
    [applicationVersionId, displayVersions],
  );

  const selectedVersion = useMemo(() => displayVersions.find((v) => v.id === applicationVersionId) ?? displayVersions[0], [applicationVersionId, displayVersions]);

  const displayText = useMemo(() => {
    if (isInvalidVersionReference) return t('agents.versionSelector.invalidVersion', 'Invalid version');
    return selectedVersion ? formatVersionDisplayText(selectedVersion) : LATEST_VERSION_NAME;
  }, [isInvalidVersionReference, selectedVersion]);

  const handleClick = useCallback((event: MouseEvent<HTMLElement>) => setAnchorEl(event.currentTarget), []);
  // Reopening always starts from the full, unfiltered list — the same
  // "no stale state carried between sessions" rule the bell popover's own
  // infinite scroll follows (issue 940/A4).
  const handleClose = useCallback(() => {
    setAnchorEl(null);
    setSearchQuery('');
  }, []);

  const handleRefresh = useCallback(
    (event: MouseEvent) => {
      event.stopPropagation();
      onRefreshVersions?.();
    },
    [onRefreshVersions],
  );

  const handleVersionClick = useCallback(
    (version: DisplayVersion) => () => {
      setAnchorEl(null);
      onSelectVersion(version);
    },
    [onSelectVersion],
  );

  /*
   * #147 — the menu closes BEFORE the caller is told. The caller answers this
   * with a confirm dialog, and leaving an open `Menu` behind it stacks two
   * focus traps: the dialog takes focus, `Escape` then closes whichever the
   * browser considers topmost, and a keyboard user can end up inside a menu
   * they cannot see past the modal.
   */
  const handleSetDefaultClick = useCallback(
    (version: AgentPipelineVersionOption) => {
      setAnchorEl(null);
      onSetDefaultVersion?.(version);
    },
    [onSetDefaultVersion],
  );

  /* Same reason as `handleSetDefaultClick`: the caller answers this with a
     confirm dialog, and an open `Menu` behind a modal stacks two focus
     traps. */
  const handleDeleteClick = useCallback(
    (version: AgentPipelineVersionOption) => {
      setAnchorEl(null);
      onDeleteVersion?.(version);
    },
    [onDeleteVersion],
  );

  const content = (
    <Box sx={contentWrapperSx}>
      {renderTrigger({ displayText, isInvalid: isInvalidVersionReference, isSwitching: isSwitchingVersion, disabled, isOpen: !!anchorEl, onClick: handleClick })}
      {renderMenu({
        anchorEl,
        onClose: handleClose,
        isRefreshingVersions,
        onRefresh: handleRefresh,
        allVersionsCount: displayVersions.length,
        displayVersions: filteredVersions,
        searchQuery,
        onSearchChange: setSearchQuery,
        selectedVersion,
        onVersionClick: handleVersionClick,
        defaultVersionId,
        onSetDefaultVersion: onSetDefaultVersion === undefined ? undefined : handleSetDefaultClick,
        onDeleteVersion: onDeleteVersion === undefined ? undefined : handleDeleteClick,
      })}
    </Box>
  );

  if (!isInvalidVersionReference) return content;

  return (
    <Tooltip
      title={t('agents.versionSelector.invalidVersionTooltip', "The selected version no longer exists. Please select a valid version or remove this agent/pipeline.")}
      placement="top"
      arrow
    >
      {content}
    </Tooltip>
  );
}
