/**
 * Messages from the shell live in the window, under the toolbar, the way a
 * native app shows a notice in the document ("This file changed on disk"):
 * a slim full-width strip with the message, its actions and a close button.
 * Not a floating toast: nothing covers the content, nothing steals the
 * corner.
 */
import type { ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

export interface NoticeBannerProps {
  tone: 'info' | 'warning';
  children: ReactNode;
  /** Buttons after the message (a Take back, a Run Diagnostics). */
  actions?: ReactNode;
  onClose: () => void;
  testId?: string;
}

export function NoticeBanner({ tone, children, actions, onClose, testId }: NoticeBannerProps): React.JSX.Element {
  return (
    <Box
      component="output"
      data-testid={testId}
      sx={(theme: Theme) => {
        const accent = tone === 'warning' ? theme.vars.palette.warning.main : theme.vars.palette.primary.main;
        return {
          pointerEvents: 'auto',
          display: 'flex',
          alignItems: 'center',
          gap: 1,
          minHeight: 32,
          paddingX: 1.5,
          fontSize: '0.8125rem',
          color: theme.vars.palette.text.primary,
          background: `color-mix(in srgb, ${accent} 14%, ${theme.vars.palette.background.paper})`,
          borderBottom: `0.5px solid color-mix(in srgb, ${accent} 40%, transparent)`,
          '@keyframes elitea-banner-in': { from: { transform: 'translateY(-100%)', opacity: 0 }, to: { transform: 'none', opacity: 1 } },
          animation: 'elitea-banner-in 180ms ease-out',
          '@media (prefers-reduced-motion: reduce)': { animation: 'none' },
        };
      }}
    >
      <Box sx={{ flex: 1, minWidth: 0 }}>{children}</Box>
      {actions}
      <IconButton size="small" aria-label={t('desktop.banner.dismiss', 'Dismiss')} onClick={onClose}>
        <CloseIcon sx={{ fontSize: 14 }} />
      </IconButton>
    </Box>
  );
}

/** Where banners stack: the top of the content area, below the toolbar. */
export function BannerStack({ children }: { children: ReactNode }): React.JSX.Element {
  return (
    <Box
      data-testid="banner-stack"
      sx={{ position: 'absolute', top: '3.25rem', left: 0, right: 0, zIndex: (theme: Theme) => theme.zIndex.appBar, pointerEvents: 'none', display: 'flex', flexDirection: 'column' }}
    >
      {children}
    </Box>
  );
}
