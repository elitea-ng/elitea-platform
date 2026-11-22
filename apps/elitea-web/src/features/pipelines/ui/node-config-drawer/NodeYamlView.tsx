import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { YamlCodeEditor } from '../YamlCodeEditor';

export interface NodeYamlViewProps {
  readonly code: string;
  readonly onChangeCode: (nextCode: string) => void;
  readonly error?: string | null | undefined;
  readonly disabled?: boolean | undefined;
}

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  flex: 1,
  minHeight: '20rem',
};

const editorSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  minHeight: 0,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusMd,
  overflow: 'hidden',
});

const errorSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.error.main,
});

export const NodeYamlView = memo(function NodeYamlView({
  code,
  onChangeCode,
  error,
  disabled = false,
}: NodeYamlViewProps): ReactNode {
  return (
    <Box sx={rootSx} data-testid="pipeline-node-panel-yaml">
      <Box sx={editorSx}>
        <YamlCodeEditor code={code} onChangeCode={onChangeCode} disabled={disabled} />
      </Box>
      {error ? (
        <Typography variant="bodySmall" sx={errorSx} role="alert" data-testid="pipeline-node-panel-yaml-error">
          {error}
        </Typography>
      ) : null}
    </Box>
  );
});
