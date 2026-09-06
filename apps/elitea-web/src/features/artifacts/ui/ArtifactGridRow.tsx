import type { ReactNode } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { ViewFileIcon } from '@/shared/ui/icons/view-file-icon';

import type { ArtifactColumn } from '../model/columns';
import type { ArtifactListItem } from '../model/types';

interface ArtifactGridRowProps {
  readonly item: ArtifactListItem;
  readonly columns: readonly ArtifactColumn[];
  readonly gridTemplateColumns: string;
  readonly values: Readonly<Record<string, string>>;
  readonly selected: boolean;
  readonly onToggle: (id: string) => void;
  readonly onOpen: (item: ArtifactListItem) => void;
  readonly onPreview: (item: ArtifactListItem) => void;
  readonly onDownload: (item: ArtifactListItem) => void;
  readonly onDelete: (item: ArtifactListItem) => void;
}

/**
 * One data row, on the header's grid.
 *
 * Ported from `GridTableRow.jsx`: a 2.5rem-minimum row, `0.5rem 1rem` cells,
 * a hairline bottom border from `border.table`, and the selected/hovered
 * background from `background.userInputBackground`. Files carry no leading
 * icon (only folders do) and the name is plain text, not a link — clicking a
 * FOLDER row navigates, and a file is opened from its own action button.
 */
export function ArtifactGridRow(props: ArtifactGridRowProps): ReactNode {
  const isFolder = props.item.kind === 'folder';
  const dataColumns = props.columns.filter(
    (column) => column.field !== 'name' && column.field !== 'actions',
  );

  return (
    <Box
      component="tr"
      sx={rowSx(props.selected, isFolder, props.gridTemplateColumns)}
      onClick={isFolder ? () => props.onOpen(props.item) : undefined}
    >
      <Box
        component="td"
        sx={checkboxCellSx}
      >
        <BaseCheckbox
          checked={props.selected}
          aria-label={`Select ${props.item.name}`}
          onClick={(event) => event.stopPropagation()}
          onChange={() => props.onToggle(props.item.id)}
        />
      </Box>
      <Box
        component="td"
        sx={nameCellSx}
      >
        {isFolder && (
          <FolderOutlinedIcon
            fontSize="small"
            sx={folderIconSx}
          />
        )}
        <Typography
          variant="bodyMedium"
          sx={cellTextSx}
        >
          {props.item.name}
        </Typography>
      </Box>
      {dataColumns.map((column) => (
        <Box
          key={column.field}
          component="td"
          sx={dataCellSx}
        >
          <Typography
            variant="bodyMedium"
            sx={cellTextSx}
          >
            {props.values[column.field] ?? '-'}
          </Typography>
        </Box>
      ))}
      <Box
        component="td"
        sx={actionsCellSx}
      >
        {!isFolder && (
          <Tooltip title={t('artifacts.table.viewFile', 'View/Edit file')}>
            <IconButton
              size="small"
              color="tertiary"
              aria-label={`Preview ${props.item.name}`}
              onClick={(event) => {
                event.stopPropagation();
                props.onPreview(props.item);
              }}
            >
              <ViewFileIcon style={actionIconStyle} />
            </IconButton>
          </Tooltip>
        )}
        <Tooltip title={t('common.download', 'Download')}>
          <IconButton
            size="small"
            color="tertiary"
            aria-label={`Download ${props.item.name}`}
            onClick={(event) => {
              event.stopPropagation();
              props.onDownload(props.item);
            }}
          >
            <FileDownloadOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
        <Tooltip title={t('common.delete', 'Delete')}>
          <IconButton
            size="small"
            color="tertiary"
            aria-label={`Delete ${props.item.name}`}
            onClick={(event) => {
              event.stopPropagation();
              props.onDelete(props.item);
            }}
          >
            <DeleteOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
    </Box>
  );
}

const actionIconStyle = { width: '1rem', height: '1rem' };
const rowSx = (selected: boolean, isFolder: boolean, gridTemplateColumns: string): SxProps<Theme> => (theme) => ({
  display: 'grid',
  gridTemplateColumns,
  alignItems: 'center',
  width: '100%',
  flexShrink: 0,
  minHeight: '2.5rem',
  cursor: isFolder ? 'pointer' : 'default',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
  backgroundColor: selected ? theme.vars.palette.background.userInputBackground : 'transparent',
  transition: 'background-color 0.2s ease',
  '&:hover': { backgroundColor: theme.vars.palette.background.userInputBackground },
  '&:first-of-type': {
    borderTopLeftRadius: theme.vars.shape.radiusMd,
    borderTopRightRadius: theme.vars.shape.radiusMd,
  },
  '&:last-of-type': {
    borderBottom: 'none',
    borderBottomLeftRadius: theme.vars.shape.radiusMd,
    borderBottomRightRadius: theme.vars.shape.radiusMd,
  },
});
const checkboxCellSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  minWidth: 0,
};
const nameCellSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  padding: theme.spacing(1, 2),
  minWidth: 0,
  overflow: 'hidden',
});
const dataCellSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  textAlign: 'start',
  padding: theme.spacing(1, 2),
  minWidth: 0,
  overflow: 'hidden',
});
const actionsCellSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-end',
  gap: theme.spacing(0.5),
  padding: theme.spacing(1, 2),
  minWidth: 0,
  overflow: 'hidden',
});
const folderIconSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.icon.fill.secondary,
  flexShrink: 0,
});
// `color="text.secondary"` on Typography emits no CSS rule in this MUI setup
// (verified in the running app: the generated class carries no `color` at
// all), so the text silently inherited `text.primary` — a greyer table than
// the baseline's white. Colours are set through `sx`, which does emit.
const cellTextSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
