/**
 * Welcome component for the onboarding flow.
 * Port of `apps/elitea-ui/src/[fsd]/features/onboarding/ui/Welcome.jsx` (Wave-2 unit A13).
 *
 * Note: The old app used `palette.background.welcome.outside` and
 * `palette.background.welcome.inner` — these are declared in the brand tokens
 * (`default.pack.json`) but may not be fully typed in the MUI theme.  We use
 * a type assertion on `palette.background` to access them safely.
 */

import { memo } from 'react';

import { Box, Typography } from '@mui/material';

import { BaseBtn } from '@/shared/ui/BaseBtn';

import ChatWelcomeImage from '@/assets/onboarding/welcome/chat-welcome.png';
import { productName } from '@/shared/brand';

/** Props for {@link Welcome}. */
interface WelcomeProps {
  name?: string;
  onShowTour: () => void;
}

const welcomePaletteBg = ({ palette }: { palette: NonNullable<unknown> }) => {
  const bg = (palette as Record<string, unknown>).background as Record<string, unknown>;
  const welcome = bg?.welcome as Record<string, unknown> | undefined;
  return {
    width: '100%',
    height: '100%',
    borderRadius: 'calc(1.5rem - 1px)',
    padding: '2rem 2rem 1.25rem 2rem',
    boxSizing: 'border-box' as const,
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'flex-start',
    gap: '0.625rem',
    background: (welcome?.inner as string) ?? 'background.default',
  };
};

/**
 * Welcome screen displayed before the tour starts.
 *
 * Note: `onboarding_state` is stored in `sessionStorage` by the page component.
 */
const Welcome = memo<WelcomeProps>(({ name = 'there', onShowTour }) => {
  return (
    <Box sx={styles.container}>
      <Box
        component="img"
        src={ChatWelcomeImage}
        alt={productName()}
        sx={styles.image}
      />
      <Typography
        component="div"
        variant="headingMedium"
        sx={styles.title}
      >
        Welcome to {productName()}!
      </Typography>
      <Box
        sx={theme => ({
          height: '15.375rem',
          width: '42.5rem',
          padding: '1px',
          borderRadius: '1.5rem',
          background: (theme.palette.background.welcome as Record<string, string>)?.outside ?? 'rgba(0,0,0,0.1)',
        })}
      >
        <Box
          sx={{
            width: '100%',
            height: '100%',
            backgroundColor: 'background.default',
            borderRadius: 'calc(1.5rem - 1px)',
            boxSizing: 'border-box' as const,
            display: 'flex',
            flexDirection: 'column' as const,
            alignItems: 'center',
          }}
        >
          <Box sx={welcomePaletteBg}>
            <Typography
              variant="bodyMedium"
              component="div"
              sx={styles.message}
            >
              {`Hello, ${name}!`}
            </Typography>
            <Typography
              variant="bodyMedium"
              component="div"
              sx={styles.message}
            >
              We are setting up your personal workspace — it will be ready in about 5 minutes.
              While we work our magic, take a quick tour through our onboarding slides!
            </Typography>
            <Typography
              variant="bodyMedium"
              component="div"
              sx={styles.message}
            >
              Ready to explore {productName()}&apos;s smart tools and tips?
            </Typography>
            <BaseBtn
              variant="elitea"
              color="primary"
              sx={styles.button}
              onClick={onShowTour}
            >
              Sure, let&apos;s go!
            </BaseBtn>
          </Box>
        </Box>
      </Box>
    </Box>
  );
});

Welcome.displayName = 'Welcome';

const styles = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    justifyContent: 'flex-start',
    alignItems: 'center',
    width: '100%',
    boxSizing: 'border-box' as const,
    gap: '1.5rem',
    flex: 1,
  },
  image: {
    width: '4.09rem',
  },
  title: {
    color: 'text.secondary',
  },
  message: {
    color: 'text.secondary',
  },
  button: {
    marginTop: 'auto',
  },
};

export default Welcome;
