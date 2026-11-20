import { memo, useCallback, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

import type { PickerItem } from '../../lib/tool-picker/toolPicker.types';

export interface PickerItemRowProps<TItem extends PickerItem = PickerItem> {
  readonly item: TItem;
  readonly icon: ReactNode;
  readonly selected?: boolean | undefined;
  readonly onSelect?: ((item: TItem) => void) | undefined;
  readonly status?: ReactNode | undefined;
  readonly 'data-testid'?: string | undefined;
}

const iconBoxSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  width: '1.25rem',
  height: '1.25rem',
  flexShrink: 0,
};

const textBoxSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minWidth: 0,
};

const inUseSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  paddingBlock: theme.spacing(0.25),
  paddingInline: theme.spacing(0.75),
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  color: theme.vars.palette.text.metrics,
});

const checkSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  flexShrink: 0,
  width: '1rem',
  height: '1rem',
  minWidth: '1rem',
  marginLeft: theme.spacing(2),
  color: theme.vars.palette.text.secondary,
  '& svg': { width: '1rem', height: '1rem' },
});

const disabledWrapperSx: SxProps<Theme> = {
  display: 'block',
  width: '100%',
};

function buildRowSx(isDisabled: boolean, selected: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(1.5),
    paddingBlock: theme.spacing(1),
    paddingInline: theme.spacing(2),
    minHeight: '2.5rem',
    ...(isDisabled ? { opacity: 0.6 } : {}),
    ...(selected ? { backgroundColor: theme.vars.palette.background.tagChip.default } : {}),
  });
}

function PickerItemRowInner<TItem extends PickerItem = PickerItem>({
  item,
  icon,
  selected = false,
  onSelect,
  status,
  'data-testid': testId,
}: PickerItemRowProps<TItem>): ReactNode {
  const handleClick = useCallback(() => {
    onSelect?.(item);
  }, [item, onSelect]);

  const isDisabled = Boolean(item.disabledReason);

  const row = (
    <ListItemButton
      aria-selected={selected}
      selected={selected}
      disabled={isDisabled}
      onClick={handleClick}
      sx={buildRowSx(isDisabled, selected)}
      data-testid={testId}
    >
      <Box sx={iconBoxSx}>{icon}</Box>
      <Box sx={textBoxSx}>
        <Typography variant="bodyMedium" color="text.secondary" noWrap>
          {item.label}
        </Typography>
        {item.description ? (
          <Typography variant="bodySmall" color="text.primary" noWrap>
            {item.description}
          </Typography>
        ) : null}
      </Box>
      {item.inUse ? (
        <Typography
          variant="labelSmall"
          sx={inUseSx}
          data-testid={testId ? `${testId}-in-use` : undefined}
        >
          {t('pipelines.toolPicker.inUse', 'In use')}
        </Typography>
      ) : null}
      {status}
      {selected ? (
        <ListItemIcon sx={checkSx}>
          <CheckedIcon />
        </ListItemIcon>
      ) : null}
    </ListItemButton>
  );

  if (!isDisabled) return row;

  return (
    <Tooltip title={item.disabledReason} placement="left">
      <Box component="span" sx={disabledWrapperSx}>
        {row}
      </Box>
    </Tooltip>
  );
}

/** One picker row: icon, name, short description, "In use" tag, optional status, and EL-7085 disabled tooltip. */
export const PickerItemRow = memo(PickerItemRowInner) as typeof PickerItemRowInner;
