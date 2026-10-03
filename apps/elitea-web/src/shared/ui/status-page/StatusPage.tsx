import { useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { resolveBrandPack } from '@/shared/brand';
import { BrandLogoMark } from '@/shared/ui/brand-logo';

export interface StatusPageProps {
  /** `data-testid` for the page root (`<main>`). */
  readonly testId: string;
  /** Big decorative lead (e.g. "404"); omit for pages that carry an icon instead. */
  readonly code?: string;
  readonly icon?: ReactNode;
  readonly title: string;
  readonly children?: ReactNode;
  readonly actions?: ReactNode;
  /** CSS min-height of the page area: a whole viewport when nothing else is on screen. */
  readonly minHeight?: string;
}

/**
 * Centered card for terminal states (access denied, page not found): the
 * brand mark and product name from the served brand pack, a hierarchy of
 * code/icon -> title -> body -> actions, every colour a theme token so the
 * light/dark scheme and tenant re-theming apply (R-T7: no hex here).
 */
export function StatusPage({
  testId,
  code,
  icon,
  title,
  children,
  actions,
  minHeight = '100vh',
}: StatusPageProps): ReactNode {
  // Resolved once: parents re-render (the countdown ticks every second) and a
  // malformed served pack makes resolveBrandPack() console.warn each call.
  const [productName] = useState(() => resolveBrandPack().product.name);
  return (
    <Box
      component="main"
      data-testid={testId}
      sx={(theme: Theme) => ({
        minHeight,
        boxSizing: 'border-box',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '1.5rem 1rem',
        backgroundColor: theme.vars.palette.background.default,
        color: theme.vars.palette.text.secondary,
      })}
    >
      <Box
        sx={(theme: Theme) => ({
          width: '100%',
          maxWidth: '28rem',
          boxSizing: 'border-box',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: '1rem',
          padding: { xs: '2rem 1.5rem', sm: '2.5rem 2.5rem' },
          textAlign: 'center',
          backgroundColor: theme.vars.palette.background.paper,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          borderRadius: theme.vars.shape.radiusLg,
        })}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <BrandLogoMark style={{ width: '1.75rem', height: '1.75rem' }} />
          <Typography
            component="span"
            variant="labelMedium"
            sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}
          >
            {productName}
          </Typography>
        </Box>
        {code ? (
          // An SVG <text> scales with its box, so the display size needs no
          // ad-hoc font-size (R-T11); the glyphs still use the brand font.
          <Box
            component="svg"
            aria-hidden="true"
            focusable="false"
            viewBox="0 0 120 48"
            sx={(theme: Theme) => ({
              width: { xs: '10rem', sm: '13rem' },
              height: 'auto',
              fill: theme.vars.palette.primary.main,
              fontFamily: 'inherit',
            })}
          >
            <text x="60" y="40" textAnchor="middle" fontWeight="700" fontSize="46" letterSpacing="-2">
              {code}
            </text>
          </Box>
        ) : null}
        {icon ? (
          <Box aria-hidden="true" sx={(theme: Theme) => ({ display: 'flex', color: theme.vars.palette.primary.main })}>
            {icon}
          </Box>
        ) : null}
        <Typography
          component="h1"
          variant="headingLarge"
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 })}
        >
          {title}
        </Typography>
        {children}
        {actions ? (
          <Box
            sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.75rem', justifyContent: 'center', marginTop: '0.5rem' }}
          >
            {actions}
          </Box>
        ) : null}
      </Box>
    </Box>
  );
}
