import { Box, Chip } from '@mui/material';
import type { DialogProps } from '@mui/material';

export const Panel = (props: Pick<DialogProps, 'open'>) => (
  <Box sx={{ p: 1.5 }} hidden={!props.open}>
    <Chip label="Lead" />
  </Box>
);
