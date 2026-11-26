/**
 * The frame and header of a page that draws its own title (ADR-0062 decision 4,
 * the page frame and header family). The admin console's pages wrote this frame
 * by hand on every page: `DrawerPage` with `padding: '1rem 1.5rem'` and
 * `gap: '0.75rem'`, an `h1` in `headingLarge`, and on list pages a row that
 * puts the actions on the right.
 *
 * Spacing is on the brand scale in numbers (ADR-0062 decision 2), so in
 * comfortable density it is the pixels the copies had (16, 24, 12) and in
 * compact density it shrinks with the rest of the theme — a visible change
 * there, by decision 5.
 *
 * Settings pages are not this family: their title is the shell's crumb
 * (`DrawerPageHeader`).
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { DrawerPage } from '../settings/DrawerPage';

const frameSx = { paddingY: 2, paddingX: 3, gap: 1.5 } as const;

const rowSx = {
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  gap: 2,
  flexWrap: 'wrap',
} as const;

export interface PageFrameProps {
  children: React.ReactNode;
}

/**
 * A full-height scrolling page with the frame's padding and the gap between
 * its blocks. It takes no `sx`: a page that needs another frame extends this
 * one. `DrawerPage` spreads its `sx` as one object, so an `sx` array would
 * lose the frame's padding without an error.
 */
export function PageFrame({ children }: PageFrameProps) {
  return <DrawerPage sx={frameSx}>{children}</DrawerPage>;
}

export interface PageHeaderProps {
  /** The page's one `h1`. */
  title: React.ReactNode;
  /** One line under the title, in the strong text role (`role.text`). */
  subtitle?: React.ReactNode;
  /** Controls on the right of the title (search, buttons); they wrap under it when narrow. */
  actions?: React.ReactNode;
}

/**
 * The title, an optional actions row and an optional subtitle. It renders
 * them as children of the frame, so the frame's gap spaces them.
 */
export function PageHeader({ title, subtitle, actions }: PageHeaderProps) {
  const heading = (
    <Typography variant="headingLarge" component="h1">
      {title}
    </Typography>
  );
  return (
    <>
      {actions === undefined ? heading : (
        <Box sx={rowSx}>
          {heading}
          {actions}
        </Box>
      )}
      {subtitle === undefined ? null : (
        <Typography variant="bodySmall" color="role.text">
          {subtitle}
        </Typography>
      )}
    </>
  );
}
