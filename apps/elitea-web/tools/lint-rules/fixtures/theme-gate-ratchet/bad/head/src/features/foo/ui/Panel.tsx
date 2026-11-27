import { Box, Chip, Dialog, Table, Typography } from '@mui/material';

export const Panel = () => (
  <Dialog open>
    <Typography component="h1">t</Typography>
    <Box sx={{ p: '1rem', gap: 0.75 }} />
    <Typography color="text.secondary">x</Typography>
    <Table />
    <Chip color="success" label="Active" />
  </Dialog>
);
