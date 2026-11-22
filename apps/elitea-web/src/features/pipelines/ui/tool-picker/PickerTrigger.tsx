import { memo, useCallback, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';

export interface PickerTriggerProps {
  readonly label?: string | undefined;
  readonly tooltip?: ReactNode | undefined;
  readonly placeholder?: string | undefined;
  readonly hasValue?: boolean | undefined;
  readonly isOpen?: boolean | undefined;
  readonly disabled?: boolean | undefined;
  readonly onOpen?: ((anchor: HTMLElement) => void) | undefined;
  readonly onClear?: (() => void) | undefined;
  readonly children?: ReactNode | undefined;
  readonly 'data-testid'?: string | undefined;
}

const rootSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.5),
  width: '100%',
  minWidth: 0,
});

const labelRowSx: SxProps<Theme> = (theme: Theme) => ({
  paddingLeft: theme.spacing(1.5),
});

const placeholderSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.disabled,
});

const clearIconSx: SxProps<Theme> = {
  width: '0.875rem',
  height: '0.875rem',
};

const fieldRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  width: '100%',
  minWidth: 0,
};

function buildFieldSx(isOpen: boolean, disabled: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(1),
    flex: 1,
    minHeight: '2rem',
    minWidth: 0,
    textAlign: 'left',
    paddingBlock: theme.spacing(0.5),
    paddingInline: theme.spacing(1.5),
    borderBottom: `0.0625rem solid ${isOpen ? theme.vars.palette.primary.main : theme.vars.palette.border.lines}`,
    cursor: disabled ? 'default' : 'pointer',
    outline: 'none',
    '&:hover': disabled ? {} : { borderBottomColor: theme.vars.palette.border.hover },
    '&:focus-visible': { borderBottomColor: theme.vars.palette.primary.main },
  });
}

function buildValueSx(disabled: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    flexWrap: 'wrap',
    gap: theme.spacing(0.75),
    flex: 1,
    minWidth: 0,
    color: disabled
      ? theme.vars.palette.text.primary
      : theme.vars.palette.text.select.selected.primary,
  });
}

function buildArrowSx(isOpen: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    color: theme.vars.palette.icon.fill.default,
    transform: isOpen ? 'rotate(180deg)' : 'none',
    transition: 'transform 0.2s',
    '& svg': { width: '1rem', height: '1rem' },
  });
}

function PickerTriggerClearButton({
  visible,
  onClear,
  ariaLabel,
  testId,
}: {
  readonly visible: boolean;
  readonly onClear: (event: MouseEvent<HTMLButtonElement>) => void;
  readonly ariaLabel: string;
  readonly testId: string | undefined;
}): ReactNode {
  if (!visible) return null;
  return (
    <BaseBtn
      variant={BUTTON_VARIANTS.tertiary}
      startIcon={<CloseIcon sx={clearIconSx} />}
      onClick={onClear}
      aria-label={ariaLabel}
      data-testid={testId ? `${testId}-clear` : undefined}
    />
  );
}

/** Field that opens a picker menu; matches the shared node select appearance. */
export const PickerTrigger = memo(function PickerTrigger({
  label,
  tooltip,
  placeholder,
  hasValue = false,
  isOpen = false,
  disabled = false,
  onOpen,
  onClear,
  children,
  'data-testid': testId,
}: PickerTriggerProps): ReactNode {
  const resolvedPlaceholder = placeholder ?? t('pipelines.toolPicker.selectPlaceholder', 'Select');

  const handleClick = useCallback(
    (event: MouseEvent<HTMLButtonElement>) => {
      if (!disabled) onOpen?.(event.currentTarget);
    },
    [disabled, onOpen],
  );

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>) => {
      if (disabled || event.key !== 'ArrowDown') return;
      event.preventDefault();
      onOpen?.(event.currentTarget);
    },
    [disabled, onOpen],
  );

  const handleClear = useCallback(
    (event: MouseEvent<HTMLButtonElement>) => {
      event.stopPropagation();
      onClear?.();
    },
    [onClear],
  );

  const showClear = hasValue && Boolean(onClear) && !disabled;
  const clearAriaLabel = t('pipelines.toolPicker.clearSelection', 'Clear {{label}}', {
    label: label || t('pipelines.toolPicker.selectionFallback', 'selection'),
  });

  return (
    <Box sx={rootSx} className="nopan nodrag nowheel">
      {label ? <InfoLabelWithTooltip label={label} tooltip={tooltip} sx={labelRowSx} /> : null}
      <Box sx={fieldRowSx}>
        <ButtonBase
          type="button"
          disabled={disabled}
          aria-expanded={isOpen}
          aria-haspopup="menu"
          aria-label={label ?? resolvedPlaceholder}
          sx={buildFieldSx(isOpen, disabled)}
          onClick={handleClick}
          onKeyDown={handleKeyDown}
          data-testid={testId}
        >
          <Box sx={buildValueSx(disabled)}>
            {hasValue ? (
              children
            ) : (
              <Typography variant="labelMedium" sx={placeholderSx}>
                {resolvedPlaceholder}
              </Typography>
            )}
          </Box>
          <Box sx={buildArrowSx(isOpen)}>
            <ExpandMoreIcon />
          </Box>
        </ButtonBase>
        <PickerTriggerClearButton
          visible={showClear}
          onClear={handleClear}
          ariaLabel={clearAriaLabel}
          testId={testId}
        />
      </Box>
    </Box>
  );
});
