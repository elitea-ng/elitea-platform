import type { ReactNode } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import GroupsOutlinedIcon from '@mui/icons-material/GroupsOutlined';
import PushPinIcon from '@mui/icons-material/PushPin';
import PushPinOutlinedIcon from '@mui/icons-material/PushPinOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { Bucket } from '@/entities/bucket';
import { t } from '@/shared/i18n';
import { BucketIcon } from '@/shared/ui/icons/bucket-icon';

import type { ArtifactTreeItem } from '../model/types';
import { BucketTreeItem } from './BucketTreeItem';

interface BucketListProps {
  readonly buckets: readonly Bucket[];
  readonly selectedBucket?: string;
  /** File tree of the SELECTED bucket — the only one the page has contents for. */
  readonly tree: readonly ArtifactTreeItem[];
  readonly expandedPaths: readonly string[];
  readonly selectedKey?: string;
  readonly onSelect: (bucket: Bucket) => void;
  readonly onEdit: (bucket: Bucket) => void;
  /**
   * "Manage access" — the per-bucket exception list. The reference puts it in
   * the row's kebab menu with a `GroupsIcon`
   * (`pages/Artifacts/Components/BucketItem.jsx:195-199`); this row keeps its
   * actions as icon buttons, so it takes the same icon and the same label.
   */
  readonly onManageAccess: (bucket: Bucket) => void;
  readonly onPin: (bucket: Bucket) => void;
  readonly onDelete: (bucket: Bucket) => void;
  readonly onSelectFile: (item: ArtifactTreeItem) => void;
  readonly onSelectFolder: (key: string) => void;
}

/**
 * The bucket rows and, under the selected bucket, its file tree.
 *
 * Ported from `SimpleBucketList.jsx` + `BucketItem.jsx` + `BucketContent.jsx`.
 * The row's three actions sit in the trailing slot and are revealed on hover
 * or keyboard focus, which is where the baseline keeps them (a hover-only pin
 * plus a dot menu); they stay in the DOM so their `aria-label`s remain the
 * stable handle J20c addresses them by.
 */
export function BucketList(props: BucketListProps): ReactNode {
  return (
    <Box>
      {props.buckets.map((bucket) => {
        const selected = bucket.name === props.selectedBucket;
        return (
          <Box key={bucket.id}>
            <Box sx={rowSx(selected)}>
              <Box
                component="button"
                type="button"
                sx={selectSx}
                onClick={() => props.onSelect(bucket)}
              >
                <BucketIcon style={bucketIconStyle} />
                <Typography sx={nameSx}>
                  {bucket.name}
                </Typography>
              </Box>
              <Box
                className="artifact-bucket-actions"
                sx={actionsSx}
              >
                <Tooltip title={bucket.isPinned
                  ? t('artifacts.buckets.unpin', 'Unpin bucket')
                  : t('artifacts.buckets.pin', 'Pin bucket')}
                >
                  <IconButton
                    size="small"
                    color="tertiary"
                    aria-label={bucket.isPinned ? `Unpin ${bucket.name}` : `Pin ${bucket.name}`}
                    onClick={() => props.onPin(bucket)}
                  >
                    {bucket.isPinned ? <PushPinIcon fontSize="small" /> : <PushPinOutlinedIcon fontSize="small" />}
                  </IconButton>
                </Tooltip>
                <Tooltip title={t('artifacts.buckets.edit', 'Edit bucket')}>
                  <IconButton
                    size="small"
                    color="tertiary"
                    aria-label={`Edit ${bucket.name}`}
                    onClick={() => props.onEdit(bucket)}
                  >
                    <EditOutlinedIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
                <Tooltip title={t('artifacts.buckets.manageAccess', 'Manage access')}>
                  <IconButton
                    size="small"
                    color="tertiary"
                    aria-label={`Manage access to ${bucket.name}`}
                    onClick={() => props.onManageAccess(bucket)}
                  >
                    <GroupsOutlinedIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
                <Tooltip title={t('artifacts.buckets.delete', 'Delete bucket')}>
                  <IconButton
                    size="small"
                    color="tertiary"
                    aria-label={`Delete ${bucket.name}`}
                    onClick={() => props.onDelete(bucket)}
                  >
                    <DeleteOutlinedIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
              </Box>
            </Box>
            {selected && props.tree.length > 0 && (
              <Box sx={treeSx}>
                {props.tree.map((item) => (
                  <BucketTreeItem
                    key={item.key}
                    item={item}
                    depth={0}
                    expandedPaths={props.expandedPaths}
                    {...(props.selectedKey === undefined ? {} : { selectedKey: props.selectedKey })}
                    onSelectFile={props.onSelectFile}
                    onSelectFolder={props.onSelectFolder}
                  />
                ))}
              </Box>
            )}
          </Box>
        );
      })}
    </Box>
  );
}

const bucketIconStyle = { width: '1rem', height: '1rem', minWidth: '1rem' };
const rowSx = (selected: boolean): SxProps<Theme> => (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(1),
  width: '100%',
  height: '2.5rem',
  boxSizing: 'border-box',
  padding: theme.spacing(1),
  background: selected
    ? theme.vars.palette.background.conversation.selected
    : theme.vars.palette.background.conversation.normal,
  borderRadius: selected ? theme.vars.shape.radiusSm : 0,
  borderBottom: selected ? 'none' : `0.0625rem solid ${theme.vars.palette.border.conversationItemDivider}`,
  '&:hover': {
    background: selected
      ? theme.vars.palette.background.conversation.selected
      : theme.vars.palette.background.conversation.hover,
    borderRadius: theme.vars.shape.radiusSm,
    borderBottomColor: 'transparent',
  },
  // The trailing actions are hover/focus-revealed, matching the baseline's
  // hover-only pin and dot menu. Opacity rather than `display: none` so the
  // controls keep their box (no reflow when a row is hovered) and stay
  // reachable by keyboard through `:focus-within`.
  '&:hover .artifact-bucket-actions, &:focus-within .artifact-bucket-actions': { opacity: 1 },
});
const selectSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  minWidth: 0,
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.25),
  padding: 0,
  border: 0,
  background: 'transparent',
  color: 'inherit',
  cursor: 'pointer',
  overflow: 'hidden',
  textAlign: 'start',
});
// 12px on a 24px line box — the baseline's `bodyMedium` variant with its own
// `fontSize: '0.75rem'` override (`BucketItem.jsx:bucketName`), measured as
// 12px/24px on next.elitea.ai. Spelled as a variant SPREAD rather than an
// `sx.fontSize` literal, which R-T11 bans.
const nameSx: SxProps<Theme> = (theme) => ({
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.secondary,
  lineHeight: '1.5rem',
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
});
const actionsSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  opacity: 0,
  transition: 'opacity 0.15s ease',
};
const treeSx: SxProps<Theme> = (theme) => ({ paddingLeft: theme.spacing(1.5) });
