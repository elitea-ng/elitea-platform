import type { ReactNode } from 'react';
import { useState } from 'react';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ArtifactTreeItem } from '../model/types';

interface BucketTreeItemProps {
  readonly item: ArtifactTreeItem;
  readonly depth: number;
  readonly expandedPaths: readonly string[];
  readonly selectedKey?: string;
  readonly onSelectFile: (item: ArtifactTreeItem) => void;
  readonly onSelectFolder: (key: string) => void;
}

/**
 * One row of the in-panel file tree under a selected bucket, and its children.
 *
 * Ported from `apps/elitea-ui/src/pages/Artifacts/Components/FileTreeItem.jsx`:
 * a 2rem row, folder icon for directories only, name at `bodySmall`, and the
 * indent ladder the baseline computes (1.25rem per level to depth 3, 0.75rem
 * after that, so a deep path still fits a 300px panel).
 */
export function BucketTreeItem(props: BucketTreeItemProps): ReactNode {
  const isFolder = props.item.kind === 'folder';
  const [expanded, setExpanded] = useState(isFolder && props.expandedPaths.includes(props.item.key));
  const active = props.selectedKey === props.item.key;
  const children = props.item.children ?? [];

  return (
    <Box sx={wrapperSx}>
      <Box
        component="button"
        type="button"
        aria-label={props.item.name}
        sx={rowSx(props.depth, active)}
        onClick={() => {
          if (isFolder) {
            setExpanded((current) => !current);
            props.onSelectFolder(props.item.key);
            return;
          }
          props.onSelectFile(props.item);
        }}
      >
        {isFolder && (
          <FolderOutlinedIcon
            fontSize="small"
            sx={folderIconSx}
          />
        )}
        <Typography sx={nameSx}>
          {props.item.name}
        </Typography>
      </Box>
      {children.length > 0 && (
        <Collapse
          in={expanded}
          unmountOnExit
        >
          {children.map((child) => (
            <BucketTreeItem
              key={child.key}
              item={child}
              depth={props.depth + 1}
              expandedPaths={props.expandedPaths}
              {...(props.selectedKey === undefined ? {} : { selectedKey: props.selectedKey })}
              onSelectFile={props.onSelectFile}
              onSelectFolder={props.onSelectFolder}
            />
          ))}
        </Collapse>
      )}
    </Box>
  );
}

/** `FileTreeItem.jsx:calculateIndent` — 1.25rem a level to depth 3, 0.75rem after. */
function indentRem(depth: number): number {
  const base = 0.15;
  if (depth <= 3) return base + depth * 1.25;
  return base + 3 * 1.25 + (depth - 3) * 0.75;
}

const wrapperSx: SxProps<Theme> = { width: '100%' };
const rowSx = (depth: number, active: boolean): SxProps<Theme> => (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(1),
  width: '100%',
  minHeight: '2rem',
  paddingLeft: `${indentRem(depth) + 0.5}rem`,
  paddingRight: theme.spacing(1),
  paddingBlock: theme.spacing(0.75),
  border: 0,
  textAlign: 'start',
  cursor: 'pointer',
  overflow: 'hidden',
  background: active
    ? theme.vars.palette.background.conversation.selected
    : theme.vars.palette.background.conversation.normal,
  borderRadius: active ? theme.vars.shape.radiusSm : 0,
  borderBottom: active ? 'none' : `0.0625rem solid ${theme.vars.palette.border.conversationItemDivider}`,
  '&:hover': {
    background: active
      ? theme.vars.palette.background.conversation.selected
      : theme.vars.palette.background.conversation.hover,
    borderRadius: theme.vars.shape.radiusSm,
    borderBottomColor: 'transparent',
  },
});
const folderIconSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.icon.fill.secondary,
  flexShrink: 0,
});
/** Same 12px/24px as the bucket row above it — see `BucketList.tsx`'s `nameSx`. */
const nameSx: SxProps<Theme> = (theme) => ({
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.secondary,
  lineHeight: '1.5rem',
  flex: 1,
  minWidth: 0,
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
});
