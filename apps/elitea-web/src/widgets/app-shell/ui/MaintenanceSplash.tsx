/**
 * The maintenance splash — what a user without administration access sees while
 * a maintenance window is open.
 *
 * ## Why this is a rendered page and not a served HTML document
 *
 * pylon served a stored `splash_template` VERBATIM, with no sanitisation,
 * because its hook sat in front of everything including the SPA's own assets —
 * there was no app left to render anything. Here the API is what closes; the
 * SPA still loads. That buys two things the served template could not have: the
 * splash is themed and translated like the rest of the product, and the
 * operator's markup is a BODY inside this page rather than the whole document.
 *
 * ## Two fields, because an operator needs both registers
 *
 * `message` is markdown with raw HTML disabled, for the same reason
 * `PlatformBanner`'s message is — and it always resolves to something, because
 * the server fills a default.
 *
 * `html` is the port of `splash_template`: markup for the things a sentence
 * cannot carry, a status-page link or a table of windows. It WINS when it is
 * not empty. It is a stored-XSS surface and is treated as one at both ends —
 * elitea-main refuses executable markup on the way in
 * (`internal/api/v2/admin/config_values.go`'s `validateSplashHTML`) and
 * `sanitizeSplashHtml` runs here on the way out. The earlier decision recorded
 * in this comment was to have no such field at all; the risk that motivated it
 * is real and is why both checks exist, but declining the capability left
 * operators unable to say things the reference let them say.
 *
 * ## What it replaces on screen
 *
 * `AppShell` renders this INSTEAD of the page and the sidebar. Rendering the
 * product around it would leave every control looking usable over an API that
 * answers 503 to all of them — which is the state this screen exists to explain.
 *
 * An administrator never reaches here: the server resolves `bypass` for the
 * calling user, and `AppShell` honours it. See `usePlatformAnnouncements`.
 *
 * ## Not to be confused with `src/entries/maintenance`
 *
 * That is a SEPARATE artefact and a separate situation: a self-contained,
 * single-file splash (`vite build --mode maintenance` → `dist/maintenance`) that
 * an ingress serves INSTEAD of the application when the platform is fully down —
 * elitea-main included. It cannot ask the API anything, which is why it still
 * takes its copy from build-time `VITE_MAINTENANCE_*` variables.
 *
 * This component covers the other case, the one an operator can actually
 * schedule: the platform is up, an administrator has closed it to users from the
 * admin panel, and can re-open it from the same screen. Sharing an
 * implementation between the two is not possible — that entry has its own theme,
 * its own root and no query client — and sharing the wording would be wrong,
 * since only one of them can say anything the operator typed today.
 */
import type { ReactNode } from 'react';

import ConstructionIcon from '@mui/icons-material/Construction';
import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';

import { DefaultMarkdown } from '@/shared/ui/DefaultMarkdown';
import { sanitizeSplashHtml } from '@/shared/ui/lib/sanitizeSplashHtml';
import type { PlatformMaintenance } from '@/shared/lib/hooks/usePlatformAnnouncements';

export interface MaintenanceSplashProps {
  readonly maintenance: PlatformMaintenance;
}

export function MaintenanceSplash({ maintenance }: MaintenanceSplashProps): ReactNode {
  return (
    <Box
      data-testid="maintenance-splash"
      sx={{
        minHeight: '100vh',
        width: '100%',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: '1.5rem',
        boxSizing: 'border-box',
      }}
    >
      <Paper
        elevation={0}
        variant="outlined"
        // `output` (implicit role `status`) with `aria-live="polite"`: the
        // splash replaces the page rather than interrupting a task, so it is
        // announced when the reader arrives at it, not by stealing focus.
        component="output"
        aria-live="polite"
        sx={{
          maxWidth: '32rem',
          padding: '2rem',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: '1rem',
          textAlign: 'center',
        }}
      >
        <ConstructionIcon color="warning" fontSize="large" />
        <Typography variant="h5" component="h1" sx={{ fontWeight: 600 }}>
          {maintenance.title}
        </Typography>
        <Typography variant="bodyMedium" color="text.secondary" component="div">
          {maintenance.html.trim() === '' ? (
            <DefaultMarkdown markdown={maintenance.message} renderHtml={false} />
          ) : (
            <div
              data-testid="maintenance-splash-html"
              // Sanitised on this line by the same function the admin editor
              // previews through, so what an operator saw is what ships.
              dangerouslySetInnerHTML={{ __html: sanitizeSplashHtml(maintenance.html) }}
            />
          )}
        </Typography>
      </Paper>
    </Box>
  );
}
