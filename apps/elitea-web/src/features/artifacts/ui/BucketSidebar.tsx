import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { filterBucketsByQuery, type Bucket } from '@/entities/bucket';
import { t } from '@/shared/i18n';

import type { ArtifactStorageConfiguration, ArtifactTreeItem } from '../model/types';
import { BucketFooter } from './BucketFooter';
import { BucketList } from './BucketList';
import { BucketPanelHeader } from './BucketPanelHeader';
import { BucketStorageSelector } from './BucketStorageSelector';

interface BucketSidebarProps {
  readonly buckets: readonly Bucket[];
  readonly selectedBucket?: string;
  readonly storageConfigurations: readonly ArtifactStorageConfiguration[];
  readonly selectedStorage?: string;
  readonly loading: boolean;
  readonly collapsed: boolean;
  /** File tree of the selected bucket, rendered under its row. */
  readonly tree: readonly ArtifactTreeItem[];
  readonly expandedPaths: readonly string[];
  readonly selectedKey?: string;
  /** Formatted total size across the project's buckets, for the footer. */
  readonly totalSize: string;
  readonly onToggleCollapsed: () => void;
  readonly onStorageChange: (id: string) => void;
  readonly onSelect: (bucket: Bucket) => void;
  readonly onCreate: () => void;
  /** Bucket EDIT — retention, the one mutable property the API exposes. See `pages/artifacts/CreateBucket.tsx`. */
  readonly onEdit: (bucket: Bucket) => void;
  readonly onPin: (bucket: Bucket) => Promise<unknown>;
  readonly onDelete: (bucket: Bucket) => Promise<unknown>;
  readonly onSelectFile: (item: ArtifactTreeItem) => void;
  readonly onSelectFolder: (key: string) => void;
}

/**
 * The BUCKETS panel: an uppercase title with the create + search actions, the
 * collapse chevron, the storage selector, the bucket list (with the selected
 * bucket's file tree under it) and the count/size footer.
 *
 * Ported from `apps/elitea-ui/src/pages/Artifacts/Components/BucketsPanel.jsx`
 * and its children. Two structural pieces this had been missing entirely: the
 * FOOTER, and the file tree — without them the panel was a search box over a
 * flat list, and the whole lower half of the column was empty.
 *
 * The search field is behind its own button, as in the baseline: it is not a
 * permanent row, so the bucket list starts directly under the storage selector.
 */
export function BucketSidebar(props: BucketSidebarProps): ReactNode {
  const [query, setQuery] = useState('');
  const [searchOpen, setSearchOpen] = useState(false);
  const [deleting, setDeleting] = useState<Bucket>();
  const visibleBuckets = useMemo(() => filterBucketsByQuery(props.buckets, query), [props.buckets, query]);

  return (
    <Box sx={sidebarSx(props.collapsed)}>
      <BucketPanelHeader
        collapsed={props.collapsed}
        searchOpen={searchOpen}
        query={query}
        onQueryChange={setQuery}
        onSearchOpen={() => setSearchOpen(true)}
        onSearchClose={() => {
          setQuery('');
          setSearchOpen(false);
        }}
        onCreate={props.onCreate}
        onToggleCollapsed={props.onToggleCollapsed}
      />
      {!props.collapsed && (
        <BucketStorageSelector
          configurations={props.storageConfigurations}
          {...(props.selectedStorage === undefined ? {} : { selected: props.selectedStorage })}
          onChange={props.onStorageChange}
        />
      )}
      <Box sx={listSx(props.collapsed)}>
        {props.loading ? (
          <Typography variant="bodyMedium">{t('artifacts.buckets.loading', 'Loading buckets…')}</Typography>
        ) : visibleBuckets.length === 0 ? (
          <Typography
            variant="bodyMedium"
            sx={emptySx}
          >
            {t('artifacts.buckets.empty', 'No buckets found.')}
          </Typography>
        ) : (
          <BucketList
            buckets={visibleBuckets}
            {...(props.selectedBucket === undefined ? {} : { selectedBucket: props.selectedBucket })}
            tree={props.tree}
            expandedPaths={props.expandedPaths}
            {...(props.selectedKey === undefined ? {} : { selectedKey: props.selectedKey })}
            onSelect={props.onSelect}
            onEdit={props.onEdit}
            onPin={(bucket) => void props.onPin(bucket).catch(() => undefined)}
            onDelete={setDeleting}
            onSelectFile={props.onSelectFile}
            onSelectFolder={props.onSelectFolder}
          />
        )}
      </Box>
      {!props.collapsed && (
        <BucketFooter
          bucketCount={props.buckets.length}
          totalSize={props.totalSize}
        />
      )}
      <Dialog
        open={deleting !== undefined}
        onClose={() => setDeleting(undefined)}
      >
        <DialogTitle>{t('artifacts.buckets.deleteTitle', 'Delete bucket?')}</DialogTitle>
        <DialogContent>
          {t('artifacts.buckets.deleteDescription', 'This will remove {{name}} and all files inside it.', {
            name: deleting?.name ?? '',
          })}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDeleting(undefined)}>{t('common.cancel', 'Cancel')}</Button>
          <Button
            color="error"
            variant="contained"
            onClick={() => {
              if (deleting === undefined) return;
              void props.onDelete(deleting)
                .then(() => setDeleting(undefined))
                .catch(() => undefined);
            }}
          >
            {t('common.delete', 'Delete')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

const sidebarSx = (collapsed: boolean): SxProps<Theme> => (theme) => ({
  height: '100%',
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: collapsed ? 'center' : 'stretch',
  overflow: 'hidden',
  background: theme.vars.palette.background.eliteaDefault,
});
const listSx = (collapsed: boolean): SxProps<Theme> => (theme) => ({
  display: collapsed ? 'none' : 'flex',
  flexDirection: 'column',
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
  overflowX: 'hidden',
  padding: theme.spacing(2),
});
const emptySx: SxProps<Theme> = (theme) => ({
  textAlign: 'center',
  color: theme.vars.palette.text.button.disabled,
});
