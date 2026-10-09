import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

/** Answer progress presentation. The caller owns visibility and recovery guards. */
export function ApplicationAnswerLoading({ isStreaming }: { readonly isStreaming: boolean }): ReactNode {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <Box
        component="span"
        sx={{
          display: 'inline-block',
          width: '6px',
          height: '6px',
          borderRadius: '50%',
          backgroundColor: 'primary.main',
          animation: 'pulse 1.5s infinite',
        }}
      />
      <Typography variant="bodyMedium" component="p" sx={{ color: 'text.secondary' }}>
        {isStreaming ? 'Streaming...' : 'Loading...'}
      </Typography>
    </Box>
  );
}
