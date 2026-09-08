/**
 * Ported from `apps/elitea-ui/src/components/Chat/CreatedTimeInfo.jsx` —
 * renders a message's creation time as a relative label.
 *
 * The label is `date-fns`' `formatDistanceToNow(...) + ' ago'`, exactly as the
 * baseline computes it. The hand-rolled ladder that used to live here
 * ("just now" / "5m ago" / "3h ago" / "2d ago" / a locale date past a week)
 * produced a different string for EVERY bucket than the production UI, which
 * says "about 1 minute ago", "about 3 hours ago", "11 days ago", "about 1
 * month ago" — measured live on next.elitea.ai. `date-fns` is already a
 * direct dependency of this app, so there was nothing to hand-roll around.
 *
 * Port of `apps/elitea-ui/src/components/Chat/CreatedTimeInfo.jsx`.
 */
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import { formatDistanceToNow } from 'date-fns';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/** @public Props for `CreatedTimeInfo`. */
export interface CreatedTimeInfoProps {
  /** The creation time string (ISO format). */
  readonly createdAt: string;
  /** Optional updated time. */
  readonly updatedAt?: string;
}

/**
 * The relative label for `time`, or the raw value when it is not a date this
 * runtime can parse — `formatDistanceToNow` THROWS on an invalid date, and an
 * unparseable timestamp must not take the whole transcript row down with it.
 */
function computeDisplayTime(time: string): string {
  try {
    const parsed = new Date(time.replace('Z', '+00:00'));
    if (Number.isNaN(parsed.getTime())) return time;
    return t('features.chatMessages.timeAgo', '{{distance}} ago', { distance: formatDistanceToNow(parsed) });
  } catch {
    return time;
  }
}

/**
 * `CreatedTimeInfo` — the right-hand end of a message's caption line.
 * Recomputed every 30s while mounted (baseline: the same `setInterval`), so
 * "less than a minute ago" advances without a remount.
 *
 * No colour of its own: the production row leaves it at the surrounding
 * default (`text.default`, `#A9B7C1` in dark). It used to force
 * `text.secondary`, which this pack defines as `#FFFFFF` — the timestamp
 * rendered as bright as the author's own name.
 */
export function CreatedTimeInfo({ createdAt, updatedAt }: CreatedTimeInfoProps): ReactNode {
  const time = updatedAt || createdAt;
  const [displayTime, setDisplayTime] = useState(() => (time ? computeDisplayTime(time) : ''));

  useEffect(() => {
    if (!time) return;
    setDisplayTime(computeDisplayTime(time));

    const intervalId = setInterval(() => {
      setDisplayTime(computeDisplayTime(time));
    }, 30000);

    return () => {
      clearInterval(intervalId);
    };
  }, [time]);

  if (!time) return null;

  return (
    <Typography
      variant="bodySmall"
      data-testid="chat-message-time"
      sx={{ marginLeft: '0.15rem', whiteSpace: 'nowrap' }}
    >
      {displayTime}
    </Typography>
  );
}
