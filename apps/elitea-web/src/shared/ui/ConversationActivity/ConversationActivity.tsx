/**
 * The conversation activity status of a list row (client contract 1.12), as
 * the chat lists of the web and the desktop show it — one component, so the
 * glyphs and the bold rule are the same everywhere:
 *
 * - `running`: an animated partial ring (static when the person asks for
 *   reduced motion);
 * - `needs_you`: an amber ring with a dot;
 * - `unread`: a filled circle with a check — the title is bold;
 * - `done`: a light grey check.
 *
 * `activityGlyph` (shared/lib/conversationActivity) picks the kind. Each
 * glyph has an accessible name. Colours are theme tokens, so a brand pack and
 * the dark scheme re-theme them.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import useMediaQuery from '@mui/material/useMediaQuery';

import { t } from '@/shared/i18n';
import { useNeedsYouCount, type ActivityGlyphKind, type ActivityRowFields } from '@/shared/lib/conversationActivity';

export interface ConversationActivityGlyphProps {
  readonly kind: ActivityGlyphKind | null;
  /** Pixel size of the square glyph. */
  readonly size?: number;
}

const REDUCED_MOTION = '(prefers-reduced-motion: reduce)';

const visuallyHidden = { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' } as const;

/** The accessible name of each kind. */
export function activityGlyphLabel(kind: ActivityGlyphKind): string {
  switch (kind) {
    case 'running':
      return t('conversationActivity.running', 'Running');
    case 'needs_you':
      return t('conversationActivity.needsYou', 'Needs your input');
    case 'unread':
      return t('conversationActivity.unread', 'Finished, unread');
    case 'done':
      return t('conversationActivity.done', 'Finished');
  }
}

const spinSx = {
  '@keyframes elitea-activity-spin': { from: { transform: 'rotate(0deg)' }, to: { transform: 'rotate(360deg)' } },
  transformOrigin: '50% 50%',
  animation: 'elitea-activity-spin 0.9s linear infinite',
  [`@media ${REDUCED_MOTION}`]: { animation: 'none' },
} as const;

function Running({ animate }: { readonly animate: boolean }): ReactNode {
  return (
    <Box component="svg" viewBox="0 0 16 16" sx={(theme: Theme) => ({ width: '100%', height: '100%', color: theme.vars.palette.text.secondary, ...(animate ? spinSx : {}) })}>
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeOpacity="0.2" strokeWidth="2" />
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeDasharray="12 26" />
    </Box>
  );
}

function NeedsYou(): ReactNode {
  return (
    <Box component="svg" viewBox="0 0 16 16" sx={(theme: Theme) => ({ width: '100%', height: '100%', color: theme.vars.palette.warning.main })}>
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="2" />
      <circle cx="8" cy="8" r="2.25" fill="currentColor" />
    </Box>
  );
}

function Unread(): ReactNode {
  return (
    <Box component="svg" viewBox="0 0 16 16" sx={{ width: '100%', height: '100%' }}>
      <Box component="circle" cx="8" cy="8" r="7" sx={(theme: Theme) => ({ fill: theme.vars.palette.text.primary })} />
      <Box
        component="path"
        d="M5 8.2l2 2 4-4.4"
        sx={(theme: Theme) => ({ fill: 'none', stroke: theme.vars.palette.background.default, strokeWidth: 1.75, strokeLinecap: 'round', strokeLinejoin: 'round' })}
      />
    </Box>
  );
}

function Done(): ReactNode {
  return (
    <Box component="svg" viewBox="0 0 16 16" sx={(theme: Theme) => ({ width: '100%', height: '100%', color: theme.vars.palette.text.disabled })}>
      <path d="M4 8.4l2.6 2.6 5.4-5.8" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" />
    </Box>
  );
}

export function ConversationActivityGlyph({ kind, size = 14 }: ConversationActivityGlyphProps): ReactNode {
  const reduced = useMediaQuery(REDUCED_MOTION);
  if (kind === null) return null;
  return (
    <Box
      component="span"
      data-testid="conversation-activity-glyph"
      data-kind={kind}
      data-motion={kind === 'running' && !reduced ? 'animated' : 'static'}
      title={activityGlyphLabel(kind)}
      sx={{ position: 'relative', display: 'inline-flex', flexShrink: 0, width: size, height: size }}
    >
      <Box component="span" aria-hidden sx={{ display: 'inline-flex', width: '100%', height: '100%' }}>
        {kind === 'running' && <Running animate={!reduced} />}
        {kind === 'needs_you' && <NeedsYou />}
        {kind === 'unread' && <Unread />}
        {kind === 'done' && <Done />}
      </Box>
      {/* The accessible name: read with the row's title, never shown. */}
      <Box component="span" sx={visuallyHidden}>
        {activityGlyphLabel(kind)}
      </Box>
    </Box>
  );
}

export interface ActivityGroupSummaryProps {
  /** The rows of the group, shown as quiet text; absent hides it. */
  readonly count?: number | undefined;
  readonly needsYou: number;
}

export interface GroupActivitySummaryProps {
  /** The rows of the group the list holds. */
  readonly rows: readonly ActivityRowFields[];
  /** The listing's count for the whole group (`needs_you`), when it sent one. */
  readonly serverNeedsYou?: number | undefined;
  /** The group's row count to show; absent shows none. */
  readonly count?: number | undefined;
}

/** `ActivityGroupSummary` over a group's rows, following their live state. */
export function GroupActivitySummary({ rows, serverNeedsYou, count }: GroupActivitySummaryProps): ReactNode {
  const needsYou = useNeedsYouCount(rows, serverNeedsYou);
  return <ActivityGroupSummary count={count} needsYou={needsYou} />;
}

/** A group header's trailing summary: its row count, and "N needs you" in the warning colour when N > 0. */
export function ActivityGroupSummary({ count, needsYou }: ActivityGroupSummaryProps): ReactNode {
  if (count === undefined && needsYou <= 0) return null;
  return (
    <Box component="span" data-testid="activity-group-summary" sx={{ display: 'inline-flex', alignItems: 'baseline', gap: 0.75, marginLeft: 'auto', flexShrink: 0 }}>
      {needsYou > 0 && (
        <Typography component="span" variant="bodySmall" data-testid="activity-needs-you" sx={(theme: Theme) => ({ color: theme.vars.palette.warning.main, fontWeight: 600 })}>
          {t('conversationActivity.needsYouCount', '{{count}} needs you', { count: needsYou })}
        </Typography>
      )}
      {count !== undefined && (
        // The count is a visual aid; the header keeps its name, and the needs-you line is read out.
        <Typography component="span" variant="bodySmall" data-testid="activity-row-count" aria-hidden="true" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
          {count}
        </Typography>
      )}
    </Box>
  );
}
