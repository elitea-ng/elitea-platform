import { Box, Dialog, Typography } from '@mui/material';

export const Panel = () => (
  <Dialog open>
    <Typography component="h1">t</Typography>
    <Box sx={{ p: '1rem', gap: 0.75 }} />
    <Typography color="text.secondary">x</Typography>
  </Dialog>
);
