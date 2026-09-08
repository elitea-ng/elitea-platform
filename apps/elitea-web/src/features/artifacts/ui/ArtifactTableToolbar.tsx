import type { ChangeEvent, ReactNode, RefObject } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import KeyboardArrowRightIcon from '@mui/icons-material/KeyboardArrowRight';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { FileUploadIcon } from '@/shared/ui/icons/file-upload-icon';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import type { ArtifactBreadcrumb } from '../model/types';

interface ArtifactTableToolbarProps {
  readonly bucket: string;
  readonly breadcrumbs: readonly ArtifactBreadcrumb[];
  readonly fileCount: number;
  readonly retentionDays: number | null;
  readonly query: string;
  readonly hasSelection: boolean;
  readonly fileInputRef: RefObject<HTMLInputElement | null>;
  readonly onQueryChange: (value: string) => void;
  readonly onBreadcrumbClick: (path: string) => void;
  readonly onFilesPicked: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly onDownloadSelected: () => void;
  readonly onDeleteSelected: () => void;
}

function retentionText(days: number | null): string {
  return days === null
    ? t('artifacts.table.retentionNone', 'No expiration')
    : `${days} ${t('artifacts.table.retentionDays', 'days')}`;
}

/**
 * The row above the table: the BUCKET name as the heading (with its folder
 * breadcrumbs and the info tooltip), and on the right a search pill plus three
 * 28px circular actions — upload, download-selected, delete-selected.
 *
 * Ported from `apps/elitea-ui/src/pages/Artifacts/component/
 * ArtifactTableToolbar.jsx` + `BucketInfoTooltip.jsx`. The heading is the
 * bucket, not the word "Files": the breadcrumb trail starts at the bucket name
 * and the baseline makes that first segment the "back to root" affordance.
 */
export function ArtifactTableToolbar(props: ArtifactTableToolbarProps): ReactNode {
  const atRoot = props.breadcrumbs.length === 0;

  return (
    <Box sx={toolbarSx}>
      <Box sx={leftSx}>
        <Typography
          variant="headingSmall"
          component={atRoot ? 'h2' : 'button'}
          sx={atRoot ? bucketNameSx : bucketLinkSx}
          onClick={atRoot ? undefined : () => props.onBreadcrumbClick('')}
        >
          {props.bucket}
        </Typography>
        {props.breadcrumbs.map((breadcrumb, index) => {
          const isLast = index === props.breadcrumbs.length - 1;
          return (
            <Box
              key={breadcrumb.path}
              sx={crumbSx}
            >
              <KeyboardArrowRightIcon
                fontSize="small"
                sx={separatorSx}
              />
              <Typography
                variant="headingSmall"
                component={isLast ? 'span' : 'button'}
                sx={isLast ? bucketNameSx : bucketLinkSx}
                onClick={isLast ? undefined : () => props.onBreadcrumbClick(breadcrumb.path)}
              >
                {breadcrumb.name}
              </Typography>
            </Box>
          );
        })}
        <Tooltip
          arrow
          placement="left"
          title={
            <Box sx={tooltipSx}>
              <Typography variant="labelSmall">
                {t('artifacts.table.retentionPolicy', 'Retention Policy:')} {retentionText(props.retentionDays)}
              </Typography>
              <Typography variant="labelSmall">
                {t('artifacts.table.fileCount', 'Number of files:')} {props.fileCount}
              </Typography>
            </Box>
          }
        >
          <IconButton
            size="small"
            color="tertiary"
            aria-label={t('artifacts.table.bucketInfo', 'Bucket info')}
          >
            <InfoOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
      <Box sx={rightSx}>
        <Box sx={searchSx}>
          <SimpleSearchBar
            value={props.query}
            debounceMs={0}
            onChange={props.onQueryChange}
            placeholder={t('artifacts.table.search', 'Search')}
          />
        </Box>
        <input
          hidden
          multiple
          ref={props.fileInputRef}
          type="file"
          onChange={props.onFilesPicked}
        />
        <Tooltip title={t('artifacts.table.uploadFiles', 'Upload files')}>
          <BaseBtn
            variant="icon"
            aria-label={t('artifacts.table.uploadFiles', 'Upload files')}
            onClick={() => props.fileInputRef.current?.click()}
          >
            <FileUploadIcon style={actionIconStyle} />
          </BaseBtn>
        </Tooltip>
        <Tooltip title={t('artifacts.table.downloadSelected', 'Download selected')}>
          <span>
            <BaseBtn
              variant="icon"
              aria-label={t('artifacts.table.downloadSelected', 'Download selected')}
              disabled={!props.hasSelection}
              onClick={props.onDownloadSelected}
            >
              <FileDownloadOutlinedIcon fontSize="small" />
            </BaseBtn>
          </span>
        </Tooltip>
        <Tooltip title={t('artifacts.table.deleteSelected', 'Delete selected')}>
          <span>
            <BaseBtn
              variant="icon"
              aria-label={t('artifacts.table.deleteSelected', 'Delete selected')}
              disabled={!props.hasSelection}
              onClick={props.onDeleteSelected}
            >
              <DeleteOutlinedIcon fontSize="small" />
            </BaseBtn>
          </span>
        </Tooltip>
      </Box>
    </Box>
  );
}

const actionIconStyle = { width: '1rem', height: '1rem' };
const toolbarSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  justifyContent: 'space-between',
  alignItems: 'center',
  width: '100%',
  gap: theme.spacing(2),
  padding: theme.spacing(1.4, 3),
});
const leftSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(0.8),
  overflow: 'hidden',
});
const bucketNameSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  whiteSpace: 'nowrap',
  margin: 0,
});
const bucketLinkSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  whiteSpace: 'nowrap',
  margin: 0,
  padding: 0,
  border: 0,
  background: 'transparent',
  cursor: 'pointer',
  '&:hover': { color: theme.vars.palette.primary.main, textDecoration: 'underline' },
});
const crumbSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(0.5),
});
const separatorSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  flexShrink: 0,
});
const rightSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(1.2),
});
const searchSx: SxProps<Theme> = { minWidth: '12.5rem' };
const tooltipSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.5),
  padding: theme.spacing(0.5),
});
