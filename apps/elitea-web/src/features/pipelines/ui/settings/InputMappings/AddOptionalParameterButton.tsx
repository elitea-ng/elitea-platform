import type { MouseEvent, ReactNode } from 'react';
import { memo, useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

export interface OptionalParameterOption {
  readonly key: string;
  readonly label: string;
}

export interface AddOptionalParameterButtonProps {
  readonly options: readonly OptionalParameterOption[];
  readonly onAdd: (key: string) => void;
  readonly disabled?: boolean | undefined;
  readonly tooltip?: string | undefined;
  readonly testIdPrefix?: string | undefined;
}

const rowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.5rem',
  alignSelf: 'flex-start',
};

/**
 * Menu button that reveals an optional tool parameter in the Advanced section.
 * Picking a parameter only shows its field; nothing is written to YAML until a value is entered.
 */
export const AddOptionalParameterButton = memo(function AddOptionalParameterButton({
  options,
  onAdd,
  disabled = false,
  tooltip,
  testIdPrefix,
}: AddOptionalParameterButtonProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);

  const handleOpen = useCallback((event: MouseEvent<HTMLElement>) => {
    setAnchorEl(event.currentTarget);
  }, []);

  const handleClose = useCallback(() => {
    setAnchorEl(null);
  }, []);

  const handleSelect = useCallback(
    (key: string) => () => {
      onAdd(key);
      setAnchorEl(null);
    },
    [onAdd],
  );

  if (options.length === 0) return null;

  return (
    <>
      <Box sx={rowSx} className="nopan nodrag">
        <BaseBtn
          variant={BUTTON_VARIANTS.secondary}
          startIcon={<PlusIcon />}
          onClick={handleOpen}
          disabled={disabled}
          data-testid={testIdPrefix ? `${testIdPrefix}-add-optional-parameter` : 'pipeline-add-optional-parameter'}
        >
          {t('pipelines.inputMapping.addOptionalParameter', 'Add optional parameter')}
        </BaseBtn>
        {tooltip ? <InfoTooltip title={tooltip} placement="top" /> : null}
      </Box>
      <Menu anchorEl={anchorEl} open={Boolean(anchorEl)} onClose={handleClose}>
        {options.map(({ key, label }) => (
          <MenuItem
            key={key}
            onClick={handleSelect(key)}
            data-testid={testIdPrefix ? `${testIdPrefix}-optional-parameter-${key}` : `pipeline-optional-parameter-${key}`}
          >
            <Typography variant="bodyMedium" color="text.secondary">
              {label}
            </Typography>
          </MenuItem>
        ))}
      </Menu>
    </>
  );
});
