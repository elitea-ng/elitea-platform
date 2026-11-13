import type { ReactNode } from 'react';
import { useState } from 'react';

import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
import DifferenceOutlinedIcon from '@mui/icons-material/DifferenceOutlined';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import GavelOutlinedIcon from '@mui/icons-material/GavelOutlined';
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import LinkOutlinedIcon from '@mui/icons-material/LinkOutlined';
import MoreVertIcon from '@mui/icons-material/MoreVert';
import PublicOffOutlinedIcon from '@mui/icons-material/PublicOffOutlined';
import PublicOutlinedIcon from '@mui/icons-material/PublicOutlined';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

/**
 * The `⋮` menu production puts beside the version bar, ported to this app's
 * two groups.
 *
 * Measured on `next.elitea.ai` 2026-09-06 (read-only): the menu holds
 * VERSION → *Set as a default, Export, Share, Fork, Publish, Delete* and
 * AGENT → *Share, Pin to top, Delete agent*. Four of those nine already exist
 * in this app as their own toolbar buttons (Set as a default, Export, Delete,
 * Delete agent) and one has no backend at all (Pin to top — no route, no
 * column). This menu therefore carries the four that were MISSING and does not
 * duplicate the four that are present: a second Delete beside the first is a
 * second way to reach the same write, which is how two code paths drift.
 *
 * `onPublish`/`onUnpublish` are optional. A PIPELINE gets neither, because the
 * server refuses one outright — `Publish` answers 400 `pipeline_not_publishable`
 * (internal/api/v2/eliteacore/handler.go). Offering a control whose only
 * possible outcome is a refusal is worse than omitting it, which is the same
 * rule `features/settings`' row menu already follows for a missing grant.
 *
 * `onExport`/`onDelete` are optional for the same "do not duplicate" reason
 * the note above states. The AGENT editor keeps its own Export and Delete
 * toolbar buttons and passes neither. The PIPELINE editor has no such buttons
 * — `pages/pipelines/ui/EditPipelineActions.tsx` disclosed that export and
 * delete had no pipeline-side mount point at all — so it passes both, and this
 * menu is where the reference puts them (Share, Fork, Export, Delete).
 */
export interface EntityLifecycleMenuProps {
  /** Disables every item — used while the editor is still loading its entity. */
  readonly disabled?: boolean;
  /** `true` when the open version already has `status: published`. */
  readonly isPublished: boolean;
  readonly onShareVersion?: () => void;
  /** #1997 — opens the version comparison. Omitted where there is nothing to compare (fewer than two versions). */
  readonly onCompareVersions?: () => void;
  readonly onShareEntity: () => void;
  readonly onFork: () => void;
  readonly onPublish?: () => void;
  readonly onUnpublish?: () => void;
  /**
   * ENTITY-group extras: `onExport` exports the whole entity as markdown, `onDelete` deletes it (the
   * caller owns the confirmation). Each is omitted where the editor carries its own button. One object
   * so the component stays within the §3.5 prop budget.
   */
  readonly entityActions?: { readonly onExport?: () => void; readonly onDelete?: () => void };
  /** Opens the release history (agents only; a pipeline has no releases). */
  readonly onReleaseHistory?: () => void;
  /** Opens the governance dialog: registry, change requests, attestations (agents only). */
  readonly onGovernance?: () => void;
  /** Distinguishes the agent and pipeline copies of this menu in the DOM. */
  readonly testIdPrefix: string;
}

export function EntityLifecycleMenu({
  disabled = false,
  isPublished,
  onShareVersion,
  onCompareVersions,
  onShareEntity,
  onFork,
  onPublish,
  onUnpublish,
  entityActions,
  onReleaseHistory,
  onGovernance,
  testIdPrefix,
}: EntityLifecycleMenuProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchorEl(null);

  return (
    <>
      <IconButton
        aria-label={t('features.agentLifecycle.menu.open', 'More actions')}
        aria-haspopup="menu"
        data-testid={`${testIdPrefix}-lifecycle-menu-button`}
        disabled={disabled}
        onClick={(event) => setAnchorEl(event.currentTarget)}
        size="small"
      >
        <MoreVertIcon fontSize="small" />
      </IconButton>
      <Menu
        anchorEl={anchorEl}
        open={anchorEl !== null}
        onClose={close}
        slotProps={{ list: { 'aria-label': t('features.agentLifecycle.menu.label', 'Lifecycle actions') } }}
      >
        <Typography
          variant="labelSmall"
          sx={groupSx}
        >
          {t('features.agentLifecycle.menu.versionGroup', 'VERSION')}
        </Typography>
        <MenuEntry testId={`${testIdPrefix}-share-version-menuitem`} icon={<LinkOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.shareVersion', 'Share version link')} action={onShareVersion} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-compare-versions-menuitem`} icon={<DifferenceOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.compareVersions', 'Compare versions')} action={onCompareVersions} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-fork-menuitem`} icon={<CallSplitOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.fork', 'Fork')} action={onFork} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-publish-menuitem`} icon={<PublicOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.publish', 'Publish')} action={isPublished ? undefined : onPublish} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-unpublish-menuitem`} icon={<PublicOffOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.unpublish', 'Unpublish')} action={isPublished ? onUnpublish : undefined} onClose={close} />
        <Divider />
        <Typography
          variant="labelSmall"
          sx={groupSx}
        >
          {t('features.agentLifecycle.menu.entityGroup', 'ENTITY')}
        </Typography>
        <MenuEntry testId={`${testIdPrefix}-share-entity-menuitem`} icon={<LinkOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.shareEntity', 'Share agent link')} action={onShareEntity} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-release-history-menuitem`} icon={<HistoryOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.releaseHistory', 'Release history')} action={onReleaseHistory} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-governance-menuitem`} icon={<GavelOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.governance', 'Governance')} action={onGovernance} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-export-menuitem`} icon={<FileDownloadOutlinedIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.export', 'Export')} action={entityActions?.onExport} onClose={close} />
        <MenuEntry testId={`${testIdPrefix}-delete-menuitem`} icon={<DeleteOutlineIcon sx={iconSx} />} label={t('features.agentLifecycle.menu.delete', 'Delete')} action={entityActions?.onDelete} onClose={close} />
      </Menu>
    </>
  );
}

/**
 * One menu row. Renders nothing while `action` is absent, which is how the
 * optional items (a pipeline's missing publish, an editor's own Export) stay
 * out of the DOM. A lookup of rows instead of one `&&` per row keeps the menu
 * within the §3.5 cyclomatic-complexity budget (12).
 */
function MenuEntry({
  testId,
  icon,
  label,
  action,
  onClose,
}: {
  readonly testId: string;
  readonly icon: ReactNode;
  readonly label: string;
  readonly action: (() => void) | undefined;
  readonly onClose: () => void;
}): ReactNode {
  if (action === undefined) return null;
  return (
    <MenuItem
      data-testid={testId}
      onClick={() => {
        onClose();
        action();
      }}
    >
      {icon}
      {label}
    </MenuItem>
  );
}

const groupSx: SxProps<Theme> = { display: 'block', padding: '0.5rem 1rem 0.25rem', color: 'text.secondary' };
const iconSx: SxProps<Theme> = ({ typography }) => ({
  fontSize: typography.headingMedium.fontSize,
  marginRight: '0.75rem',
});
