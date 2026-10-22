import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';

import { useParams } from '@tanstack/react-router';

import { appDetailErrorMessage, useAppDetail } from '@/features/apps';

const loadingContainerSx = {
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  minHeight: '25rem',
};

const errorContainerSx = {
  p: '1.5rem',
};

interface AppDetailRouteParams {
  readonly appId?: string;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/pages/apps/AppDetail.jsx` — covers
 * ROUTE-040 (`/apps/:tab/:appId`). Also the target of ROUTE-046
 * (`/user-public/apps/:appId`, unit A12's `src/pages/user-public/**`
 * domain) — the baseline renders the identical `<AppDetail/>` component
 * for both routes, so this same export is this port's answer for both once
 * both route files are wired to real page content (routing wiring itself
 * is `src/routes/**`, unit R1's exclusive ownership, not this unit's — see
 * the final report).
 *
 * **The baseline's custom-UI iframe branch is NOT ported (ADR-0024 WP8).**
 * The baseline rendered `<iframe src="/ui_host/{provider}/{route}/…">` when
 * an app's version meta carried `custom_ui_route` + `provider`. That path
 * was dead on this stack (no Go handler serves `/ui_host`; the meta keys
 * belong to the retired legacy plugin runtime), its sandbox granted
 * `allow-same-origin` + `allow-scripts` together (which is no sandbox), and
 * it passed context (theme, toolkit id) in the frame URL, which ADR-0013
 * forbids. ADR-0024 decision 8 makes every sub-application a native screen
 * (`pages/deepwiki`, Inventory) inside the brand provider instead. A future
 * ADR-0013 provider frame gets its context by postMessage — see
 * `docs/brand-pack-frame-envelope.md` — never by URL.
 *
 * **Composition gap, not a placeholder:** the baseline's non-custom-UI
 * fallback renders `<EditToolkit/>` (`pages/Toolkits/EditToolkit`,
 * `features/toolkits`'/`pages/toolkits`' ownership, unit A4 — not landed).
 * The loading/error branches above it are fully ported and functional; only
 * the terminal fallback is an intentionally empty composition slot.
 */
export function AppDetail() {
  const params = useParams({ strict: false }) as AppDetailRouteParams;
  const { isFetching, isError, error } = useAppDetail(params.appId);

  if (isFetching) {
    return (
      <Box sx={loadingContainerSx}>
        <CircularProgress />
      </Box>
    );
  }

  if (isError) {
    return (
      <Box sx={errorContainerSx}>
        <Alert severity="error">{appDetailErrorMessage(error)}</Alert>
      </Box>
    );
  }

  // Composition gap: `pages/toolkits`' `EditToolkit` (unit A4) has not
  // landed — see this file's own doc comment.
  return <Box data-testid="app-detail-edit-toolkit-slot" />;
}
