import type { ReactNode } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import MenuItem from '@mui/material/MenuItem';
import PushPinOutlinedIcon from '@mui/icons-material/PushPinOutlined';
import Typography from '@mui/material/Typography';

import { isSetDefaultDisabled } from '@/entities/version';
import { t } from '@/shared/i18n';

import type { AgentPipelineVersionOption } from '../lib/types';
import { isDeleteVersionDisabled } from '../lib/versionDeletion';

import {
  deleteIconSx,
  deleteItemSx,
  setDefaultIconSx,
  setDefaultItemSx,
} from './AgentPipelineVersionSelector.styles';

/**
 * The two COMMAND items at the foot of the agent version menu, "Set as
 * default" and "Delete version" (#147).
 *
 * Split out of `AgentPipelineVersionSelector.tsx` purely to keep that file
 * under the §3.5 400-line budget, the same split its `.styles.ts` sibling
 * already documents. They are plain render functions, not components: each is
 * its own scope for the `oxlint` complexity budget without also creating a new
 * capitalised component the 12-prop budget would apply to.
 *
 * They render nothing when the caller offers no callback. That is how the
 * read-only viewer and the tool card (`ToolCardBody`) keep the plain version
 * list they had.
 */

/** What both items need to decide their subject and their eligibility. */
interface VersionCommandParams {
  /** The version the menu currently marks as selected — the subject of both commands. */
  readonly selectedVersion: AgentPipelineVersionOption | undefined;
  readonly defaultVersionId: number | undefined;
}

/**
 * Eligibility is `entities/version`'s promoted `isSetDefaultDisabled`, whose
 * own doc comment cites the baseline's `disableSetAsADefault` (already the
 * default, no default recorded yet and this is the "base" fallback, or the
 * version is published). That selector was written for `VersionSummary`
 * (string ids, from the normalised entity layer) and this menu's option
 * carries a numeric id, so the row is ADAPTED rather than the rule copied —
 * a second, drifting definition of "which versions may be pinned" is
 * exactly the cost this app has already paid elsewhere.
 */
export function renderSetDefaultItem(
  params: VersionCommandParams & {
    readonly onSetDefaultVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
  },
): ReactNode {
  const { selectedVersion, defaultVersionId, onSetDefaultVersion } = params;
  if (onSetDefaultVersion === undefined || selectedVersion === undefined) return null;
  const disabled = isSetDefaultDisabled(
    {
      id: String(selectedVersion.id),
      name: selectedVersion.name,
      status: selectedVersion.status ?? '',
      agentType: '',
      createdAt: selectedVersion.created_at ?? '',
    },
    defaultVersionId === undefined ? undefined : String(defaultVersionId),
  );
  return (
    <MenuItem
      data-testid="agent-version-set-default"
      disabled={disabled}
      onClick={() => onSetDefaultVersion(selectedVersion)}
      sx={setDefaultItemSx}
    >
      <PushPinOutlinedIcon sx={setDefaultIconSx} />
      <Typography variant="bodyMedium">{t('agents.versionSelector.setDefault', 'Set as default')}</Typography>
    </MenuItem>
  );
}

/**
 * Eligibility is `../lib/versionDeletion`'s `isDeleteVersionDisabled`, which
 * carries the baseline's `disableDelete` rule and states why the
 * published/embedded case stays with the server.
 */
export function renderDeleteItem(
  params: VersionCommandParams & {
    readonly onDeleteVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
  },
): ReactNode {
  const { selectedVersion, defaultVersionId, onDeleteVersion } = params;
  if (onDeleteVersion === undefined || selectedVersion === undefined) return null;
  return (
    <MenuItem
      data-testid="agent-version-delete"
      disabled={isDeleteVersionDisabled(selectedVersion, defaultVersionId)}
      onClick={() => onDeleteVersion(selectedVersion)}
      sx={deleteItemSx}
    >
      <DeleteOutlinedIcon sx={deleteIconSx} />
      <Typography variant="bodyMedium">{t('agents.versionSelector.deleteVersion', 'Delete version')}</Typography>
    </MenuItem>
  );
}
