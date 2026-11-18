/**
 * features/notifications/ui/NotificationMessage.tsx — port of
 * `apps/elitea-ui/src/[fsd]/entities/notifications/ui/
 * NotificationListItemMessage.jsx` (unit A11).
 */
import { Fragment, useMemo } from 'react';
import type { ReactElement } from 'react';

import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { TypographyProps } from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';

import type { Notification } from '@/entities/notification';

import type { FullNotificationMeta } from '../api/normalize';
import { eventTypeLabel, hasLegacyText } from '../lib/legacyText';
import { parseNotificationMessage, resolveNotificationHref } from '../lib/routes';
import { LegacyNotificationMessage } from './LegacyNotificationMessage';
import { RunFinishedMessage } from './RunFinishedMessage';

export interface NotificationMessageProps {
  readonly notification: Omit<Notification, 'meta'> & { readonly meta?: FullNotificationMeta };
  readonly onCloseNotificationList?: (() => void) | undefined;
  readonly textVariant?: TypographyProps['variant'];
}

/**
 * `message ? <...> : <LegacyNotificationMessage .../>` — the `!message`
 * fallback (`NotificationListItemMessage.jsx:17,43`). `message` present
 * means the backend stored a pre-formatted `[text]()`-link-syntax string
 * (`meta.message`); its absence means a pre-backfill notification, handled
 * by `LegacyNotificationMessage`.
 */
export function NotificationMessage(props: NotificationMessageProps): ReactElement {
  const { notification, onCloseNotificationList, textVariant = 'bodySmall' } = props;
  const theme = useTheme();
  const textColor = notification.isSeen ? theme.vars.palette.text.primary : theme.vars.palette.text.secondary;
  const message = notification.meta?.message;
  const segments = useMemo(() => parseNotificationMessage(message), [message]);
  const resolvedHref = resolveNotificationHref(notification.eventType, notification.meta, notification.projectId);
  const linkColor = notification.isSeen ? theme.vars.palette.text.linkSeen : theme.vars.palette.text.link;

  if (notification.eventType === 'run_finished') {
    return (
      <RunFinishedMessage
        meta={notification.meta}
        projectId={notification.projectId}
        textVariant={textVariant}
        textColor={textColor}
        linkColor={linkColor}
        onCloseNotificationList={onCloseNotificationList}
      />
    );
  }

  if (message !== undefined && message !== '') {
    return (
      <Typography
        variant={textVariant}
        sx={{ color: textColor }}
      >
        {segments.map((segment, index) =>
          segment.isLink === true && resolvedHref !== null ? (
            <Link
              key={index}
              variant={textVariant}
              sx={{ textDecoration: 'underline', cursor: 'pointer', color: linkColor }}
              href={resolvedHref}
              target="_blank"
              rel="noopener noreferrer"
            >
              {segment.text}
            </Link>
          ) : (
            <Fragment key={index}>{segment.text}</Fragment>
          ),
        )}
      </Typography>
    );
  }

  // An event type the legacy renderer has no sentence for (every one the new
  // platform added: approvals, credential reviews, hand-offs...) would render
  // an empty cell. Name the event instead (#1992).
  if (!hasLegacyText(notification.eventType)) {
    return (
      <Typography
        variant={textVariant}
        sx={{ color: textColor }}
      >
        {eventTypeLabel(notification.eventType)}
      </Typography>
    );
  }

  return (
    <LegacyNotificationMessage
      eventType={notification.eventType}
      meta={notification.meta}
      projectId={notification.projectId}
      onCloseNotificationList={onCloseNotificationList}
      textVariant={textVariant}
      textColor={textColor}
    />
  );
}
