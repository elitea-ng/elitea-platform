import type { MouseEvent, ReactNode } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import type { AgentPipelineVersionOption } from '../lib/types';

import { formatVersionDisplayText, versionMetaLine } from './AgentPipelineVersionSelector.search';
import type { DisplayVersion } from './AgentPipelineVersionSelector.search';
import { renderDeleteItem, renderSetDefaultItem } from './AgentVersionMenuCommands';
import {
  defaultMarkerSx,
  menuItemSx,
  menuListSx,
  menuPaperSx,
  refreshIconStyle,
  rowEndSx,
  searchBoxSx,
  selectedCheckIconSx,
  selectedMenuItemSx,
  versionHeaderSx,
  versionHeaderTitleSx,
  versionMetaLineSx,
} from './AgentPipelineVersionSelector.styles';

/**
 * The dropdown's `Menu` body — split out of `AgentPipelineVersionSelector.tsx`
 * purely to keep that file under the §3.5 400-line budget once issue 940/A11
 * (the search box + creator/timestamp secondary line) grew it past the cap.
 * Same class of split `AgentVersionMenuCommands.tsx`'s own module doc already
 * documents; `renderMenu` stays a plain render function (not a capitalised
 * component) for the identical `complexity`-without-triggering-the-12-props-
 * budget reason that file's doc comment explains.
 */
export function renderMenu(params: {
  anchorEl: HTMLElement | null;
  onClose: () => void;
  isRefreshingVersions: boolean;
  onRefresh: (event: MouseEvent) => void;
  allVersionsCount: number;
  displayVersions: readonly DisplayVersion[];
  searchQuery: string;
  onSearchChange: (value: string) => void;
  selectedVersion: DisplayVersion | undefined;
  onVersionClick: (version: DisplayVersion) => () => void;
  defaultVersionId: number | undefined;
  onSetDefaultVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
  onDeleteVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
}): ReactNode {
  const { anchorEl, onClose, isRefreshingVersions, onRefresh, allVersionsCount, displayVersions, searchQuery, onSearchChange, selectedVersion, onVersionClick } = params;
  const { defaultVersionId, onSetDefaultVersion, onDeleteVersion } = params;
  const selectedVersionId = selectedVersion?.id;
  // ELITEA-3278/step 14 — a search that matches nothing is a DIFFERENT
  // message from "this version list is genuinely empty": the former says
  // try another query, the latter says there is nothing to search.
  const isSearchWithNoMatches = displayVersions.length === 0 && allVersionsCount > 0;
  return (
    <Menu
      anchorEl={anchorEl}
      open={Boolean(anchorEl)}
      onClose={onClose}
      transformOrigin={{ horizontal: 'left', vertical: 'top' }}
      anchorOrigin={{ horizontal: 'left', vertical: 'bottom' }}
      slotProps={{ paper: { sx: menuPaperSx }, list: { sx: menuListSx } }}
    >
      <Box sx={versionHeaderSx}>
        <Typography
          variant="labelSmall"
          sx={versionHeaderTitleSx}
        >
          {t('agents.versionSelector.versionsHeading', 'Versions')}
        </Typography>
        <Tooltip
          title={t('agents.versionSelector.refreshTooltip', 'Refresh versions')}
          placement="top"
        >
          <IconButton
            color="tertiary"
            size="small"
            onClick={onRefresh}
            disabled={isRefreshingVersions}
          >
            {isRefreshingVersions ? <CircularProgress size={12} /> : <RefreshIcon style={refreshIconStyle} />}
          </IconButton>
        </Tooltip>
      </Box>

      {allVersionsCount > 0 && (
        // `onKeyDown` stops `Menu`'s own arrow-key/typeahead handling from
        // eating keystrokes meant for the field — the same guard
        // `ToolMenuDropdown.tsx`'s own search box already established for
        // an identical `SimpleSearchBar`-inside-`Menu` composition.
        <Box
          sx={searchBoxSx}
          onKeyDown={(event) => event.stopPropagation()}
        >
          <SimpleSearchBar
            data-testid="version-selector-search"
            value={searchQuery}
            onChange={onSearchChange}
            debounceMs={0}
            placeholder={t('agents.versionSelector.searchPlaceholder', 'Search versions')}
          />
        </Box>
      )}

      {allVersionsCount === 0 && <MenuItem disabled>{t('agents.versionSelector.noVersions', 'No versions available')}</MenuItem>}
      {isSearchWithNoMatches && <MenuItem disabled>{t('agents.versionSelector.noSearchResults', 'No versions found')}</MenuItem>}

      {displayVersions.map((version) => {
        const isSelected = selectedVersionId === version.id;
        // Never on the `base` row: its accessible NAME must stay the exact
        // literal "base" (`pipelines.version-selector.spec.ts`'s own
        // pre-existing `{ name: 'base', exact: true }` lookups depend on
        // it) — a secondary line's text would otherwise fold into it.
        const metaLine = version.isLatest ? undefined : versionMetaLine(version);
        return (
          <MenuItem
            key={version.id}
            onClick={onVersionClick(version)}
            sx={isSelected ? selectedMenuItemSx : menuItemSx}
          >
            <Box sx={{ minWidth: 0 }}>
              <Typography variant="bodyMedium">{formatVersionDisplayText(version)}</Typography>
              {/* ELITEA-3279 — the row's creator + full timestamp, absent for `base` (which carries neither meaningfully) and for a version whose author/date the wire genuinely does not know. */}
              {metaLine !== undefined && (
                <Typography
                  variant="labelSmall"
                  sx={versionMetaLineSx}
                >
                  {metaLine}
                </Typography>
              )}
            </Box>
            <Box sx={rowEndSx}>
              {version.id === defaultVersionId && (
                <Typography
                  variant="labelSmall"
                  data-testid="agent-version-default-marker"
                  sx={defaultMarkerSx}
                >
                  {t('agents.versionSelector.defaultMarker', 'Default')}
                </Typography>
              )}
              {isSelected && <CheckIcon sx={selectedCheckIconSx} />}
            </Box>
          </MenuItem>
        );
      })}

      {renderSetDefaultItem({ selectedVersion, defaultVersionId, onSetDefaultVersion })}
      {renderDeleteItem({ selectedVersion, defaultVersionId, onDeleteVersion })}
    </Menu>
  );
}
