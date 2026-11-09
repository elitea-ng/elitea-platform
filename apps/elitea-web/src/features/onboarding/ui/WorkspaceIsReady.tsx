/**
 * WorkspaceIsReady component for the onboarding flow.
 * Port of `apps/elitea-ui/src/[fsd]/features/onboarding/ui/WorkspaceIsReady.jsx`
 * (Wave-2 unit A13).
 */

import { memo } from 'react';

import { Box, Typography } from '@mui/material';

import { BaseBtn } from '@/shared/ui/BaseBtn';

import ChatWelcomeImage from '@/assets/onboarding/welcome/chat-welcome.png';
import { productName } from '@/shared/brand';

/** Props for {@link WorkspaceIsReady}. */
interface WorkspaceIsReadyProps {
  onJumpIn: () => void;
}

/**
 * Banner displayed after the private project is ready.
 * Provides a "Jump in now!" CTA that navigates to Chat.
 */
const WorkspaceIsReady = memo<WorkspaceIsReadyProps>(({ onJumpIn }) => {
  return (
    <Box
      sx={theme => ({
        height: '4rem',
        width: '30rem',
        padding: '1px',
        borderRadius: '2.75rem',
        background: (theme.palette.background.welcome as Record<string, string>)?.outside ?? 'rgba(0,0,0,0.1)',
      })}
    >
      <Box
        sx={{
          width: '100%',
          height: '100%',
          backgroundColor: 'background.default',
          borderRadius: 'calc(2.75rem - 1px)',
          boxSizing: 'border-box' as const,
          display: 'flex',
          flexDirection: 'column' as const,
          alignItems: 'center',
        }}
      >
        <Box
          sx={{
            width: '100%',
            height: '100%',
            borderRadius: 'calc(1.5rem - 1px)',
            padding: '1rem 1.25rem',
            boxSizing: 'border-box' as const,
            display: 'flex',
            flexDirection: 'row' as const,
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: '0.625rem',
          }}
        >
          <Box sx={styles.leftPart}>
            <Box
              component="img"
              src={ChatWelcomeImage}
              alt={productName()}
              sx={styles.image}
            />
            <Typography
              component="div"
              variant="bodyMedium"
              sx={styles.title}
            >
              Your {productName()} workspace is ready!
            </Typography>
          </Box>
          <BaseBtn
            variant="elitea"
            color="primary"
            sx={styles.button}
            onClick={onJumpIn}
          >
            Jump in now!
          </BaseBtn>
        </Box>
      </Box>
    </Box>
  );
});

WorkspaceIsReady.displayName = 'WorkspaceIsReady';

const styles = {
  image: {
    width: '2rem',
  },
  title: {
    color: 'text.secondary',
  },
  leftPart: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
    gap: '0.75rem',
  },
  button: {
    marginTop: 'auto',
  },
};

export default WorkspaceIsReady;
