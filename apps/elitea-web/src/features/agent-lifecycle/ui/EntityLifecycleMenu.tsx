import type { ReactNode } from 'react';
import { useState } from 'react';

import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
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
 */
export interface EntityLifecycleMenuProps {
  /** Disables every item — used while the editor is still loading its entity. */
  readonly disabled?: boolean;
  /** `true` when the open version already has `status: published`. */
  readonly isPublished: boolean;
  readonly onShareVersion?: () => void;
  readonly onShareEntity: () => void;
  readonly onFork: () => void;
  readonly onPublish?: () => void;
  readonly onUnpublish?: () => void;
  /** Distinguishes the agent and pipeline copies of this menu in the DOM. */
  readonly testIdPrefix: string;
}

export function EntityLifecycleMenu({
  disabled = false,
  isPublished,
  onShareVersion,
  onShareEntity,
  onFork,
  onPublish,
  onUnpublish,
  testIdPrefix,
}: EntityLifecycleMenuProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchorEl(null);
  const run = (action: (() => void) | undefined) => (): void => {
    close();
    action?.();
  };

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
        {onShareVersion !== undefined && (
          <MenuItem
            data-testid={`${testIdPrefix}-share-version-menuitem`}
            onClick={run(onShareVersion)}
          >
            <LinkOutlinedIcon sx={iconSx} />
            {t('features.agentLifecycle.menu.shareVersion', 'Share')}
          </MenuItem>
        )}
        <MenuItem
          data-testid={`${testIdPrefix}-fork-menuitem`}
          onClick={run(onFork)}
        >
          <CallSplitOutlinedIcon sx={iconSx} />
          {t('features.agentLifecycle.menu.fork', 'Fork')}
        </MenuItem>
        {onPublish !== undefined && !isPublished && (
          <MenuItem
            data-testid={`${testIdPrefix}-publish-menuitem`}
            onClick={run(onPublish)}
          >
            <PublicOutlinedIcon sx={iconSx} />
            {t('features.agentLifecycle.menu.publish', 'Publish')}
          </MenuItem>
        )}
        {onUnpublish !== undefined && isPublished && (
          <MenuItem
            data-testid={`${testIdPrefix}-unpublish-menuitem`}
            onClick={run(onUnpublish)}
          >
            <PublicOffOutlinedIcon sx={iconSx} />
            {t('features.agentLifecycle.menu.unpublish', 'Unpublish')}
          </MenuItem>
        )}
        <Divider />
        <Typography
          variant="labelSmall"
          sx={groupSx}
        >
          {t('features.agentLifecycle.menu.entityGroup', 'ENTITY')}
        </Typography>
        <MenuItem
          data-testid={`${testIdPrefix}-share-entity-menuitem`}
          onClick={run(onShareEntity)}
        >
          <LinkOutlinedIcon sx={iconSx} />
          {t('features.agentLifecycle.menu.shareEntity', 'Share')}
        </MenuItem>
      </Menu>
    </>
  );
}

const groupSx: SxProps<Theme> = { display: 'block', padding: '0.5rem 1rem 0.25rem', color: 'text.secondary' };
const iconSx: SxProps<Theme> = ({ typography }) => ({
  fontSize: typography.headingMedium.fontSize,
  marginRight: '0.75rem',
});
