import type { MouseEvent, ReactNode } from 'react';
import { useState } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ListItemIcon from '@mui/material/ListItemIcon';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { CheckedIcon } from '@/shared/ui/icons/checked-icon';
import { DatasetIcon } from '@/shared/ui/icons/dataset-icon';
import { t } from '@/shared/i18n';

import type { ArtifactStorageConfiguration } from '../model/types';

interface BucketStorageSelectorProps {
  readonly configurations: readonly ArtifactStorageConfiguration[];
  readonly selected?: string;
  readonly onChange: (id: string) => void;
}

/**
 * The storage row directly under the panel header — a bordered, full-width
 * strip carrying the current S3 configuration's title and a caret, and a menu
 * of the alternatives.
 *
 * Ported from `apps/elitea-ui/src/pages/Artifacts/Components/
 * BucketStorageSelector.jsx`. It renders even when the project has NO S3
 * configuration (the fallback label, exactly as the baseline does): the row is
 * part of the panel's geometry, and dropping it moved every element below it
 * up by its own height plus its border.
 */
export function BucketStorageSelector(props: BucketStorageSelectorProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement>();
  const platformStorage: ArtifactStorageConfiguration = {
    id: PLATFORM_STORAGE_ID,
    title: t('artifacts.buckets.platformStorage', 'Platform storage'),
    shared: false,
  };
  // The platform's own store always exists, so the menu is never empty (#2094) and, with
  // nothing chosen, names where the buckets actually are rather than "Select Storage".
  const entries = [platformStorage, ...props.configurations];
  const selectedId = props.selected ?? PLATFORM_STORAGE_ID;
  const current = entries.find((configuration) => configuration.id === selectedId);
  const label = current?.title ?? t('artifacts.buckets.selectStorage', 'Select Storage');

  return (
    <>
      <Box
        component="button"
        type="button"
        aria-haspopup="menu"
        sx={rowSx}
        onClick={(event: MouseEvent<HTMLElement>) => setAnchor(event.currentTarget)}
      >
        <DatasetIcon style={iconStyle} />
        {/*
          * #6638: "Storage:" above the name, the same two-line shape as the
          * sidebar's "Project:" selector (`widgets/sidebar/ui/ProjectSwitcher`).
          * The button's name is this visible text ("Storage: <name>"), so a
          * voice user can say what they see (WCAG 2.5.3).
          */}
        <Box sx={labelBoxSx}>
          <Typography
            variant="labelSmall"
            sx={captionSx}
            data-testid="bucket-storage-caption"
          >
            {t('artifacts.buckets.storageCaption', 'Storage:')}
          </Typography>
          <Typography
            variant="labelSmall"
            sx={titleSx}
          >
            {label}
          </Typography>
        </Box>
        <ExpandMoreIcon
          fontSize="small"
          sx={caretSx}
        />
      </Box>
      <Menu
        anchorEl={anchor}
        open={anchor !== undefined}
        onClose={() => setAnchor(undefined)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        transformOrigin={{ vertical: 'top', horizontal: 'left' }}
        slotProps={{ paper: { sx: { minWidth: anchor?.offsetWidth } } }}
      >
        {entries.map((configuration) => (
          <MenuItem
            key={configuration.id}
            selected={configuration.id === selectedId}
            onClick={() => {
              props.onChange(configuration.id);
              setAnchor(undefined);
            }}
          >
            <Box sx={menuTextSx}>
              <Typography
                variant="bodyMedium"
                sx={titleSx}
              >
                {configuration.title}
              </Typography>
              <Typography
                variant="bodySmall"
                sx={subtitleSx}
              >
                {storageKind(configuration)}
              </Typography>
            </Box>
            <ListItemIcon sx={checkSx}>
              {configuration.id === selectedId && <CheckedIcon style={iconStyle} />}
            </ListItemIcon>
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}

const PLATFORM_STORAGE_ID = '__platform__';

function storageKind(configuration: ArtifactStorageConfiguration): string {
  if (configuration.id === PLATFORM_STORAGE_ID) return t('artifacts.buckets.platformStorageKind', 'Built-in storage');
  return configuration.shared
    ? t('artifacts.buckets.sharedStorage', 'Shared S3 storage')
    : t('artifacts.buckets.s3Storage', 'S3 storage');
}

const iconStyle = { width: '1.25rem', height: '1.25rem' };
const rowSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  padding: theme.spacing(0.9, 3),
  // Two lines now ("Storage:" over the name), as tall as the project selector.
  minHeight: '3.25rem',
  width: '100%',
  border: 0,
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.artifactDivider}`,
  // Full UA reset instead of `borderRadius: 0`, which R-T10 bans outright:
  // `appearance: none` drops the platform button chrome (its radius included).
  appearance: 'none',
  cursor: 'pointer',
  textAlign: 'start',
  color: theme.vars.palette.text.secondary,
  backgroundColor: theme.vars.palette.background.tabPanel,
  transition: 'background-color 0.2s ease',
  '&:hover': { backgroundColor: theme.vars.palette.background.button.secondary.hover },
});
const labelBoxSx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' };
const captionSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.metrics });
const titleSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
const caretSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.icon.fill.default });
const subtitleSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.default });
const menuTextSx: SxProps<Theme> = { flex: 1, display: 'flex', flexDirection: 'column' };
const checkSx: SxProps<Theme> = {
  minWidth: '1.5rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
};
