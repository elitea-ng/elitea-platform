import { Box, Typography } from '@mui/material';

type CardProps = { title: string; subtitle?: string };

// Moved from features/foo and rewritten in the same PR: git pairs it with no base file.
export const Card = ({ title, subtitle }: CardProps) => (
  <Box sx={{ p: '1rem', gap: '0.5rem', display: 'flex', flexDirection: 'column' }}>
    <Typography variant="h6">{title}</Typography>
    {subtitle ? <Typography variant="body2">{subtitle}</Typography> : null}
  </Box>
);
