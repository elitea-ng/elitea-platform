/**
 * Onboarding page — the first-screen experience for new users.
 * Port of `apps/elitea-ui/src/pages/Onboarding/Onboarding.jsx` (Wave-2 unit A13).
 *
 * ── What this screen IS ─────────────────────────────────────────────────────
 *
 * A wait. `routes/-guards/indexRoute.ts` sends a user with no
 * `personal_project_id` here, the server provisions that project in the
 * background, and this page polls `GET /social/author` until the id appears —
 * then offers "Jump in". Everything else on it (the tips carousel, the
 * progress bar, "about 5 min") is decoration over that one poll.
 *
 * ── The defect this file used to BE ─────────────────────────────────────────
 *
 * It polled nothing. The page read a module-level `MOCK_USER` with a hardcoded
 * empty `personal_project_id`, and its "poll" was a stub that declared the
 * workspace ready once the COSMETIC progress bar reached its cap — about 150
 * seconds, whatever the server said. So:
 *
 *   - a user whose personal project genuinely existed still sat through the
 *     fake wait, because nothing ever asked;
 *   - a user whose personal project did NOT exist was told it was ready
 *     anyway, sent to `/chat` with no project selected, and bounced straight
 *     back here by the index guard on the next navigation. That was the
 *     "stuck in onboarding" loop, and the server half of it (nothing in
 *     elitea-main ever created a `project_user_<uid>` project — see
 *     internal/application/personalproject) is fixed in the same change.
 *
 * The baseline polls `useLazyAuthorDetailsQuery` every 5 s and finishes on
 * `result.personal_project_id`. This does the same through the generated
 * `useGetCurrentAuthor` query — the SAME query `widgets/app-shell` reads to
 * auto-select the personal project once it exists, so one poll feeds both and
 * the shell selects the project the moment this page sees it.
 *
 * ── Other deviations from the baseline, stated rather than dropped ──────────
 *
 * 1. **GA event tracking (useTrackEvent).**  The old app fires Google
 *    Analytics events through `useTrackEvent` (imported from `GA.js`). The new
 *    app has no analytics infra yet. Disclosed, not silently dropped — the
 *    calls are omitted entirely and noted here.
 *
 * 2. **No Suspense for OnboardingTour.**  The old app wraps the tour in
 *    `Suspense` + `lazyWithRetry`. Here we import it directly (inlined).
 *
 * 3. **`getProjectList()` on ready.**  The baseline refetches the project list
 *    so the switcher knows the new project. `widgets/app-shell` reads that list
 *    itself and auto-selects on the author response, so a refetch from this
 *    page would be a second fetcher for a decision this page does not make.
 *
 * 4. **Route wiring.**  "Jump in" navigates through the router. A root-relative
 *    `window.location.href = '/chat'` ignored the router `basepath` (`/app/` in
 *    every real deployment), so a new account left the SPA and hit a 404 on its
 *    first action. Back uses the router's own history for the same reason, and
 *    is shown only when there is somewhere to go back TO — `useCanGoBack()` is
 *    this router's equivalent of the baseline's `location.state?.from` gate.
 *
 * 5. **Theme tokens (R-T7).**  Every colour reads `theme.vars.palette.*` (live
 *    CSS var, repaints on scheme change), not `theme.palette.*` (frozen to the
 *    default scheme).
 *
 * @public Wave-2 unit A13 surface: consumers mount this page behind a route.
 */

import { memo, useCallback, useEffect, useRef, useState } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import { Box, CircularProgress, IconButton, LinearProgress, Typography } from '@mui/material';
import { useCanGoBack, useNavigate, useRouteContext, useRouter } from '@tanstack/react-router';

import { FIRST_ELITEA_TOUR_ID, markTourPending } from '@/features/interactive-tours';
import { OnboardingTour, Welcome, WorkspaceIsReady } from '@/features/onboarding';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { t } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';
import { BrandLogoFull } from '@/shared/ui/brand-logo';

import { AuthorUnavailable } from './AuthorUnavailable';
import { authorOf, displayName, selectRefreshSession } from './Onboarding.selectors';
import { styles } from './Onboarding.styles';

const ONBOARDING_STORAGE_KEY = 'onboarding_state';
const sessionStore = createStorage('session');

/** Baseline poll cadence for `authorDetails` while the project is provisioning. */
const AUTHOR_POLL_INTERVAL_MS = 5_000;

/** Progress bar cap/step — cosmetic only; readiness comes from the server. */
const PROGRESS_CAP = 95;
const PROGRESS_STEP = 95 / 150;

const Onboarding = memo(() => {
  // Disclosed, not silently dropped: useTrackEvent is unavailable in the new
  // app — no analytics infra yet.
  const navigate = useNavigate();
  const router = useRouter();
  const canGoBack = useCanGoBack();
  const routeContext: unknown = useRouteContext({ strict: false });
  const progressIntervalIdRef = useRef<ReturnType<typeof setInterval> | null>(null);
  /**
   * Latches the "project arrived" transition, synchronously.
   *
   * The effect below cannot guard on `thePrivateProjectIsReady` alone: it SETS
   * that state, and state set in an effect is not visible to a re-run queued
   * before it commits. `useRouteContext` is in that effect's dependency list,
   * so any render giving the context a fresh identity re-runs it — and two runs
   * would each fire a full session refresh (three requests apiece) and a
   * `router.invalidate()`.
   */
  const readyHandledRef = useRef(false);
  const [thePrivateProjectIsReady, setThePrivateProjectIsReady] = useState(false);

  // Check if user has clicked "Get Started" before (survives a refresh).
  const hasClickedGetStarted = sessionStore.get(ONBOARDING_STORAGE_KEY) === 'true';
  const [showTour, setShowTour] = useState(hasClickedGetStarted);
  const [progress, setProgress] = useState(5);

  /**
   * The poll. It runs only while this page is actually waiting — the tour is
   * on screen and the project has not arrived — which is exactly the window
   * the baseline's `setInterval(getUserDetails, 5000)` covers. `false` stops
   * it; the query itself stays mounted so the answer is still read.
   */
  const [waiting, setWaiting] = useState(hasClickedGetStarted);
  const authorQuery = useGetCurrentAuthor({
    query: { refetchInterval: waiting ? AUTHOR_POLL_INTERVAL_MS : false },
  });
  const author = authorOf(authorQuery.data);
  const personalProjectId = author?.personal_project_id ?? '';
  const welcomeName = author === undefined ? undefined : displayName(author);

  const onClearIntervals = useCallback(() => {
    setWaiting(false);
    if (progressIntervalIdRef.current !== null) {
      clearInterval(progressIntervalIdRef.current);
      progressIntervalIdRef.current = null;
    }
  }, []);

  const handlePersonalProjectReady = useCallback(() => {
    // Disclosed: markTourPending may be unavailable if interactive-tours is not
    // fully wired. A failure here (e.g. a storage write throwing — Safari
    // private browsing, quota exceeded) must not abort the state transition
    // below: this is a best-effort UI hint, not a precondition for "workspace
    // is ready" actually being ready.
    try {
      if (typeof markTourPending === 'function') {
        markTourPending(FIRST_ELITEA_TOUR_ID);
      }
    } catch {
      // Handled (§3.6): see comment above — best-effort only.
    }
    onClearIntervals();
    setShowTour(true);
    setThePrivateProjectIsReady(true);
    sessionStore.remove(ONBOARDING_STORAGE_KEY);
  }, [onClearIntervals]);

  const startProgress = useCallback(() => {
    setWaiting(true);
    if (progressIntervalIdRef.current !== null) return;
    progressIntervalIdRef.current = setInterval(() => {
      setProgress(prev => (prev < PROGRESS_CAP ? prev + PROGRESS_STEP : prev));
    }, 1000);
  }, []);

  const handleShowTour = useCallback(() => {
    // Disclosed, not silently dropped: useTrackEvent('onboarding_click_get_started')
    // is called by the old page here — analytics gap.
    sessionStore.set(ONBOARDING_STORAGE_KEY, 'true');
    if (!personalProjectId) {
      startProgress();
    }
    setShowTour(true);
  }, [personalProjectId, startProgress]);

  // Stable identity, so `AuthorUnavailable`'s memo can actually compare: an
  // inline arrow here would be a new prop on every render and the memo would
  // never hit. Same shape as the other handlers on this page.
  const handleRetryAuthor = useCallback(() => {
    void authorQuery.refetch();
  }, [authorQuery]);

  const handleJumpIn = () => {
    // Disclosed, not silently dropped: useTrackEvent('onboarding_jump_in') is
    // called by the old page here — analytics gap.
    sessionStore.remove(ONBOARDING_STORAGE_KEY);
    // Router navigation, so the app's `basepath` (`/app/`) is applied. A raw
    // `window.location.href = '/chat'` left the SPA and hit a 404.
    void navigate({ to: '/chat' });
    onClearIntervals();
  };

  useEffect(() => {
    return () => {
      onClearIntervals();
    };
  }, [onClearIntervals]);

  // Resume the progress bar after a refresh mid-onboarding: sessionStorage
  // still holds ONBOARDING_STORAGE_KEY, so `showTour` starts `true` on this
  // mount, but `handleShowTour` never ran in THIS mount to start the interval.
  // `startProgress` no-ops when it is already running, so this cannot
  // double-start it right after `handleShowTour` itself did.
  useEffect(() => {
    if (!personalProjectId && showTour && !thePrivateProjectIsReady) {
      startProgress();
    }
  }, [personalProjectId, showTour, thePrivateProjectIsReady, startProgress]);

  /**
   * THE TRANSITION THIS PAGE EXISTS FOR: the server named a personal project.
   *
   * It fires for the user who was waiting AND for the one who arrived here
   * already holding a project (a deep link, or the index guard racing a stale
   * session) — the baseline has the same two entry points into
   * `handlePersonalProjectReady`.
   *
   * The session refresh is what stops the loop: the guards judge on the
   * router's session, not on this query, so a session still carrying no
   * personal project would send this user back here on the next visit to `/`.
   * `router.invalidate()` then re-runs every active `beforeLoad` against the
   * refreshed session.
   */
  useEffect(() => {
    if (!personalProjectId || readyHandledRef.current) return;
    readyHandledRef.current = true;
    handlePersonalProjectReady();
    const refreshSession = selectRefreshSession(routeContext);
    if (refreshSession === undefined) return;
    void refreshSession().then(() => router.invalidate());
  }, [personalProjectId, handlePersonalProjectReady, routeContext, router]);

  return (
    <Box
      sx={theme => ({
        width: '100%',
        minWidth: '64rem',
        height: '100vh',
        minHeight: '48rem',
        display: 'flex',
        flexDirection: 'column' as const,
        alignItems: 'center',
        justifyContent: 'center',
        overflow: 'auto',
        background: theme.vars.palette.background.default,
        position: 'relative',
      })}
    >
      {!!personalProjectId && canGoBack && (
        <IconButton
          onClick={() => router.history.back()}
          sx={styles.backButton}
          aria-label="Go back"
        >
          <ArrowBackIcon />
        </IconButton>
      )}
      <Box sx={styles.body}>
        <Box sx={styles.logo}>
          <BrandLogoFull />
        </Box>
        <Box
          sx={theme => ({
            height: '32.5rem',
            minHeight: '32.5rem',
            width: '100%',
            padding: '1px',
            borderRadius: '1.5rem',
            background: theme.vars.palette.background.onboarding,
            boxShadow: theme.vars.palette.boxShadow.onboarding,
          })}
        >
          <Box
            sx={theme => ({
              width: '100%',
              height: '100%',
              borderRadius: 'calc(1.5rem - 1px)',
              padding: '2rem 2rem 1.25rem 2rem',
              boxSizing: 'border-box' as const,
              display: 'flex',
              flexDirection: 'column' as const,
              alignItems: 'center',
              background: theme.vars.palette.background.onboardingBody,
            })}
          >
            {!showTour && !personalProjectId && author?.id !== undefined && (
              <Welcome
                // Spread rather than `name={welcomeName}`: under
                // `exactOptionalPropertyTypes` an optional prop does not accept
                // an explicit `undefined`, and omitting it is what lets
                // `Welcome`'s own "there" default apply.
                {...(welcomeName !== undefined ? { name: welcomeName } : {})}
                onShowTour={handleShowTour}
              />
            )}
            {showTour && <OnboardingTour />}
            {/*
              Exclusive with the tour, which it used to render ON TOP OF: a
              refresh mid-onboarding starts `showTour` true from sessionStorage
              while the author query has not answered, and both branches were
              live at once inside the same flex column.

              The error arm exists because the query can also END without an
              answer. `/social/author` answering 500, or the network dropping,
              exhausts TanStack Query's retries and leaves `author` undefined
              for good — which rendered a bare spinner forever, indistinguishable
              from "still provisioning".
            */}
            {!showTour && author?.id === undefined && (
              <Box sx={styles.loadingContainer}>
                {authorQuery.isError ? (
                  <AuthorUnavailable onRetry={handleRetryAuthor} />
                ) : (
                  <CircularProgress aria-label={t('pages.onboarding.loadingAriaLabel', 'Loading…')} />
                )}
              </Box>
            )}
          </Box>
        </Box>
        {showTour && !thePrivateProjectIsReady && (
          <Box sx={styles.footer}>
            <Box sx={styles.footerHead}>
              <Typography
                sx={theme => {
                  const baseColor = theme.vars.palette.text.secondary;
                  return {
                    color: baseColor,
                    fontWeight: 600,
                    // A flat opacity pulse, not a gradient shimmer (brand
                    // guide v3: flat UI); off under prefers-reduced-motion.
                    animation: 'shimmer 4s infinite ease-in-out',
                    '@keyframes shimmer': {
                      '0%, 100%': { opacity: 1 },
                      '50%': { opacity: 0.55 },
                    },
                    '@media (prefers-reduced-motion: reduce)': { animation: 'none' },
                  };
                }}
                variant="headingSmall"
              >
                Configuring Personal project...
              </Typography>
              <Typography
                sx={theme => ({
                  color: theme.vars.palette.text.secondary,
                })}
                variant="bodySmall"
              >
                about 5 min
              </Typography>
            </Box>
            <Box sx={styles.progressContainer}>
              <LinearProgress
                variant="determinate"
                value={progress}
                sx={theme => ({
                  height: '0.375rem',
                  borderRadius: '0.1875rem',
                  backgroundColor: theme.vars.palette.border.lines,
                  '& .MuiLinearProgress-bar': {
                    borderRadius: '0.1875rem',
                  },
                })}
              />
            </Box>
          </Box>
        )}
        {thePrivateProjectIsReady && <WorkspaceIsReady onJumpIn={handleJumpIn} />}
      </Box>
    </Box>
  );
});

Onboarding.displayName = 'Onboarding';

export default Onboarding;
