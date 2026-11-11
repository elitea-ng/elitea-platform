/**
 * features/notifications/ui/NotificationIcon.tsx — port of
 * `apps/elitea-ui/src/[fsd]/entities/notifications/lib/helpers/
 * getIcon.helpers.jsx` (unit A11).
 *
 * `getIcon.helpers.jsx`'s `switch` also has cases for `NotificationType`
 * members that do NOT exist in the current enum (`PromptModeratorApproval`,
 * `PromptOfSomeProjectWasPublished`, `NewPromptVersionOfSomeProjectWas
 * Published/Created`, `PromptHasAddedToSomeProject`, `PromptModeratorReject`,
 * `PromptOfSomeProjectWasRejected`, `NewPromptVersionOfSomeProjectWas
 * Rejected`, `RewardBadgeToPrompt`) — `entities/notification`'s own doc
 * comment (unit E1) already flags these as dead code and excludes them from
 * `NotificationEventType`; this port does the same (a case for a value
 * outside the type union would not compile). The dead
 * `RewardBadgeToPrompt` -> `TrophyOutlinedIcon` branch is why
 * `TrophyOutlinedIcon` is not needed below despite being in the baseline
 * switch.
 *
 * **Local duplicates, not a silent drop:** three of the baseline's icons —
 * `CommentIcon` (`Comments`), `HeartIcon` (`Rates`), `MedalIcon`
 * (`RewardNewLevel`) — were NOT ported to `shared/ui/icons/**` by unit S2
 * (verified: `find src/shared/ui/icons -iname '*comment*' -o -iname
 * '*heart*' -o -iname '*medal*'` -> no match, while their old-app sources DO
 * exist: `apps/elitea-ui/src/components/Icons/{CommentIcon,HeartIcon,
 * MedalIcon}.jsx`). Per this unit's ownership fence (`src/features/
 * notifications/` only — `shared/ui/icons/**` belongs to S2, out of this
 * unit's file scope), {@link HeartIcon}, {@link CommentIcon} and
 * {@link MedalIcon} below are minimal local re-implementations (same path
 * data as the old-app sources, `fill="currentColor"` so `style={{ color }}`
 * behaves identically to every `shared/ui/icons/svg/*.svg` asset this file
 * also renders) rather than importing a nonexistent module or reaching
 * outside this unit's owned path to add one. **Follow-up for whoever owns
 * `shared/ui/icons/**` (S2 or its successor):** promote these three to real
 * `shared/ui/icons/svg/{heart,comment,medal}-icon.svg` assets (same
 * `?react` + `svg-icon.types.ts` convention as the others) and delete the
 * local copies here. `attention-icon.tsx`, `error-icon.tsx`,
 * `remove-icon.tsx`, `success-icon.tsx`, `mark-read-icon.tsx`,
 * `mark-unread-icon.tsx` all exist and are used below/by
 * `NotificationListItem`.
 */
import type { CSSProperties, ReactElement } from 'react';

import type { Theme } from '@mui/material/styles';

import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { ErrorIcon } from '@/shared/ui/icons/error-icon';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';
import { SuccessIcon } from '@/shared/ui/icons/success-icon';

import type { NotificationEventType } from '@/entities/notification';

import type { FullNotificationMeta } from '../api/normalize';

export interface NotificationIconProps {
  readonly eventType: NotificationEventType;
  readonly meta: FullNotificationMeta | undefined;
  readonly theme: Theme;
}

const ICON_SIZE = 16;

// theme.vars.palette must be read through a static `theme.vars.palette.X.Y`
// dotted path with no aliasing (R-T7, §4.6 check 7's reference scan) --
// these renderers take the full `theme`, not a `palette` value handed off
// from the caller, so every colour token stays visible to that scan.
type IconRenderer = (theme: Theme, meta: FullNotificationMeta | undefined) => ReactElement | null;

function successPublished(theme: Theme): ReactElement | null {
  return <SuccessIcon style={{ color: theme.vars.palette.status.published }} />;
}

function successTips(theme: Theme): ReactElement | null {
  return <SuccessIcon style={{ color: theme.vars.palette.icon.fill.tips }} />;
}

function removeRejected(theme: Theme): ReactElement | null {
  return <RemoveIcon style={{ color: theme.vars.palette.status.rejected }} />;
}

function errorRejected(theme: Theme): ReactElement | null {
  return (
    <ErrorIcon
      width={ICON_SIZE}
      height={ICON_SIZE}
      style={{ color: theme.vars.palette.status.rejected }}
    />
  );
}

function attentionOnModeration(theme: Theme): ReactElement | null {
  return (
    <AttentionIcon
      width={ICON_SIZE}
      height={ICON_SIZE}
      style={{ color: theme.vars.palette.status.onModeration }}
    />
  );
}

/**
 * `index_data_changed` alone branches on `meta` (`getIcon.helpers.jsx:24-32`:
 * a failed reindex reports the same as a hard error).
 */
function indexDataChangedIcon(theme: Theme, meta: FullNotificationMeta | undefined): ReactElement | null {
  const error = meta?.error;
  const hasError = typeof error === 'string' && error.trim() !== '';
  return hasError ? errorRejected(theme) : successPublished(theme);
}

/** `run_finished` (ADR-0045): by outcome — done, failed, or waiting / stopped at a limit. */
function runFinishedIcon(theme: Theme, meta: FullNotificationMeta | undefined): ReactElement | null {
  if (meta?.outcome === 'failed') return errorRejected(theme);
  if (meta?.outcome === 'waiting' || meta?.outcome === 'limit_reached') return attentionOnModeration(theme);
  return successPublished(theme);
}

function noIcon(): null {
  return null;
}

interface LocalIconProps {
  readonly style: CSSProperties;
}

/** Local duplicate of `apps/elitea-ui/src/components/Icons/HeartIcon.jsx` — see module doc comment. */
function HeartIcon(props: LocalIconProps): ReactElement {
  return (
    <svg
      width={ICON_SIZE}
      height={ICON_SIZE}
      viewBox="0 0 16 16"
      fill="currentColor"
      style={props.style}
      xmlns="http://www.w3.org/2000/svg"
    >
      <path d="M13.9391 3.06626C13.2599 2.38908 12.34 2.00827 11.3807 2.0071C10.4213 2.00593 9.50048 2.38449 8.81961 3.06001L8.00218 3.81892L7.18414 3.05751C6.50342 2.37905 5.58093 1.99866 4.61961 2C3.65828 2.00135 2.73686 2.38433 2.05806 3.06469C1.37925 3.74506 0.998656 4.66707 1 5.6279C1.00135 6.58873 1.38453 7.50967 2.06524 8.18813L7.64909 13.8509C7.69561 13.8981 7.75106 13.9356 7.81222 13.9612C7.87337 13.9868 7.93901 14 8.00531 14C8.07161 14 8.13724 13.9868 8.1984 13.9612C8.25955 13.9356 8.315 13.8981 8.36152 13.8509L13.9391 8.18813C14.6184 7.50879 15 6.58764 15 5.62719C15 4.66674 14.6184 3.7456 13.9391 3.06626ZM13.2298 7.48606L8.00218 12.7853L2.77143 7.48106C2.27916 6.98905 2.00261 6.32175 2.00261 5.62594C2.00261 4.93014 2.27916 4.26283 2.77143 3.77083C3.26369 3.27882 3.93134 3.00241 4.6275 3.00241C5.32366 3.00241 5.99131 3.27882 6.48358 3.77083L6.49608 3.78332L7.66159 4.86703C7.75411 4.95309 7.8758 5.00093 8.00218 5.00093C8.12857 5.00093 8.25026 4.95309 8.34278 4.86703L9.50829 3.78332L9.52079 3.77083C10.0134 3.27915 10.6812 3.00319 11.3774 3.00366C12.0735 3.00413 12.741 3.28098 13.2329 3.77332C13.7249 4.26566 14.001 4.93316 14.0005 5.62896C14 6.32476 13.723 6.99188 13.2304 7.48356L13.2298 7.48606Z" />
    </svg>
  );
}

/** Local duplicate of `apps/elitea-ui/src/components/Icons/CommentIcon.jsx` — see module doc comment. */
function CommentIcon(props: LocalIconProps): ReactElement {
  return (
    <svg
      width={ICON_SIZE}
      height={ICON_SIZE}
      viewBox="0 0 17 16"
      fill="currentColor"
      style={props.style}
      xmlns="http://www.w3.org/2000/svg"
    >
      <path d="M5.64107 6.33326C5.64107 6.1896 5.6978 6.05183 5.79878 5.95025C5.89976 5.84867 6.03672 5.7916 6.17953 5.7916H10.4872C10.63 5.7916 10.767 5.84867 10.868 5.95025C10.969 6.05183 11.0257 6.1896 11.0257 6.33326C11.0257 6.47691 10.969 6.61469 10.868 6.71627C10.767 6.81785 10.63 6.87491 10.4872 6.87491H6.17953C6.03672 6.87491 5.89976 6.81785 5.79878 6.71627C5.6978 6.61469 5.64107 6.47691 5.64107 6.33326ZM6.17953 9.04154H10.4872C10.63 9.04154 10.767 8.98448 10.868 8.88289C10.969 8.78131 11.0257 8.64354 11.0257 8.49989C11.0257 8.35623 10.969 8.21846 10.868 8.11688C10.767 8.0153 10.63 7.95823 10.4872 7.95823H6.17953C6.03672 7.95823 5.89976 8.0153 5.79878 8.11688C5.6978 8.21846 5.64107 8.35623 5.64107 8.49989C5.64107 8.64354 5.6978 8.78131 5.79878 8.88289C5.89976 8.98448 6.03672 9.04154 6.17953 9.04154ZM15.3334 3.08331V11.7498C15.3334 12.0371 15.2199 12.3127 15.018 12.5158C14.816 12.719 14.5421 12.8331 14.2565 12.8331H10.2537L9.25482 14.4777C9.15867 14.6375 9.02311 14.7695 8.86128 14.861C8.69946 14.9525 8.51686 15.0004 8.33122 15C8.14558 14.9996 7.9632 14.9509 7.80178 14.8587C7.64035 14.7665 7.50536 14.6339 7.40991 14.4737L6.41309 12.8331H2.4103C2.12468 12.8331 1.85076 12.719 1.6488 12.5158C1.44684 12.3127 1.33337 12.0371 1.33337 11.7498V3.08331C1.33337 2.796 1.44684 2.52046 1.6488 2.3173C1.85076 2.11413 2.12468 2 2.4103 2H14.2565C14.5421 2 14.816 2.11413 15.018 2.3173C15.2199 2.52046 15.3334 2.796 15.3334 3.08331ZM14.2565 3.08331H2.4103V11.7498H6.41309C6.59848 11.7503 6.78064 11.7987 6.94211 11.8903C7.10357 11.982 7.23893 12.1138 7.3352 12.2732L8.33337 13.9165L9.33087 12.2698C9.42757 12.1109 9.5632 11.9797 9.72477 11.8886C9.88635 11.7976 10.0685 11.7498 10.2537 11.7498H14.2565V3.08331Z" />
    </svg>
  );
}

/** Local duplicate of `apps/elitea-ui/src/components/Icons/MedalIcon.jsx` — see module doc comment. */
function MedalIcon(props: LocalIconProps): ReactElement {
  return (
    <svg
      width={ICON_SIZE}
      height={ICON_SIZE}
      viewBox="0 0 16 16"
      fill="currentColor"
      style={props.style}
      xmlns="http://www.w3.org/2000/svg"
    >
      <path d="M14 6.50194C14.0006 5.57529 13.7457 4.66353 13.2588 3.85134C12.772 3.03916 12.0691 2.35289 11.2155 1.85629C10.3618 1.35969 9.38503 1.06885 8.37588 1.0108C7.36673 0.952751 6.35794 1.12937 5.4432 1.52424C4.52847 1.91912 3.73746 2.51945 3.14365 3.26947C2.54985 4.01949 2.17249 4.89488 2.04665 5.81432C1.92081 6.73377 2.05056 7.66745 2.42384 8.52864C2.79712 9.38983 3.40184 10.1506 4.18182 10.7403V15.4998C4.18175 15.585 4.20548 15.6689 4.25074 15.7433C4.296 15.8178 4.3613 15.8805 4.44042 15.9253C4.51954 15.9701 4.60986 15.9957 4.70279 15.9995C4.79572 16.0033 4.88817 15.9853 4.97136 15.9472L8 14.5625L11.0293 15.9503C11.1053 15.9836 11.1885 16.0005 11.2727 15.9996C11.4174 15.9996 11.5561 15.947 11.6584 15.8532C11.7607 15.7595 11.8182 15.6323 11.8182 15.4998V10.7403C12.5006 10.2252 13.0502 9.578 13.4274 8.84521C13.8046 8.11242 14.0002 7.31218 14 6.50194ZM3.09091 6.50194C3.09091 5.61214 3.37882 4.74232 3.91824 4.00247C4.45766 3.26263 5.22435 2.686 6.12137 2.34548C7.01839 2.00497 8.00545 1.91588 8.95772 2.08947C9.90999 2.26306 10.7847 2.69154 11.4713 3.32072C12.1578 3.94991 12.6253 4.75154 12.8148 5.62424C13.0042 6.49695 12.907 7.40153 12.5354 8.2236C12.1639 9.04566 11.5346 9.7483 10.7273 10.2426C9.92005 10.737 8.97093 11.0009 8 11.0009C6.69847 10.9995 5.45066 10.5251 4.53034 9.68169C3.61002 8.83826 3.09235 7.69472 3.09091 6.50194ZM10.7273 14.6912L8.24341 13.5534C8.16763 13.5186 8.08406 13.5005 7.99932 13.5005C7.91458 13.5005 7.83101 13.5186 7.75523 13.5534L5.27273 14.6912V11.3989C6.11707 11.7944 7.05176 12.0006 8 12.0006C8.94823 12.0006 9.88293 11.7944 10.7273 11.3989V14.6912ZM8 10.0011C8.75516 10.0011 9.49337 9.79587 10.1213 9.41138C10.7492 9.02689 11.2386 8.48039 11.5275 7.84101C11.8165 7.20162 11.8921 6.49805 11.7448 5.81929C11.5975 5.14052 11.2338 4.51703 10.6999 4.02766C10.1659 3.5383 9.48554 3.20503 8.74489 3.07002C8.00424 2.935 7.23653 3.0043 6.53884 3.26914C5.84116 3.53398 5.24484 3.98248 4.8253 4.55791C4.40575 5.13334 4.18182 5.80987 4.18182 6.50194C4.1829 7.42967 4.58552 8.31911 5.30133 8.97512C6.01715 9.63112 6.98769 10.0001 8 10.0011ZM8 4.00254C8.5394 4.00254 9.06669 4.14913 9.51519 4.42376C9.96369 4.6984 10.3132 5.08875 10.5197 5.54546C10.7261 6.00216 10.7801 6.50471 10.6749 6.98954C10.5696 7.47438 10.3099 7.91973 9.92847 8.26928C9.54706 8.61882 9.0611 8.85687 8.53206 8.95331C8.00303 9.04975 7.45466 9.00025 6.95632 8.81108C6.45797 8.6219 6.03203 8.30155 5.73236 7.89053C5.43268 7.4795 5.27273 6.99627 5.27273 6.50194C5.27273 5.83906 5.56006 5.20332 6.07153 4.7346C6.58299 4.26587 7.27668 4.00254 8 4.00254Z" />
    </svg>
  );
}

/**
 * `getIcon.helpers.jsx`'s `Rates` case: `<HeartIcon fill={theme.palette.icon.fill.tips} size={16} />`.
 */
function ratesIcon(theme: Theme): ReactElement {
  return <HeartIcon style={{ color: theme.vars.palette.icon.fill.tips }} />;
}

/**
 * `getIcon.helpers.jsx`'s `Comments` case: `<CommentIcon fill={theme.palette.icon.fill.tips} size={16} />`.
 */
function commentsIcon(theme: Theme): ReactElement {
  return <CommentIcon style={{ color: theme.vars.palette.icon.fill.tips }} />;
}

/**
 * `getIcon.helpers.jsx`'s `RewardNewLevel` case: `<MedalIcon fill={theme.palette.status.published} size={16} />`.
 */
function rewardNewLevelIcon(theme: Theme): ReactElement {
  return <MedalIcon style={{ color: theme.vars.palette.status.published }} />;
}

/**
 * One entry per {@link NotificationEventType} member — see `../lib/
 * routes.ts`'s `HREF_RESOLVERS` doc comment for why a `Record` literal over
 * the full union (rather than a `switch`) is this file's exhaustiveness
 * mechanism. `contributor_request_for_publish_approve` has no baseline
 * `getIcon.helpers.jsx` case at all — {@link noIcon}, same as the baseline's
 * implicit `default: return null`.
 */
const ICON_RENDERERS: Record<NotificationEventType, IconRenderer> = {
  author_approval: successPublished,
  moderator_approval_of_version: successPublished,
  chat_user_added: successPublished,
  chat_user_mentioned: successPublished,
  private_project_created: successPublished,
  moderation_approved: successPublished,
  index_data_changed: indexDataChangedIcon,
  user_was_added_to_some_project_as_teammate: successTips,
  moderator_unpublish: removeRejected,
  agent_unpublished: removeRejected,
  author_reject: removeRejected,
  moderator_reject_of_version: removeRejected,
  moderation_rejected: removeRejected,
  token_is_expired: errorRejected,
  spending_limit_is_expired: errorRejected,
  token_expiring: attentionOnModeration,
  spending_limit_expiring: attentionOnModeration,
  bucket_expiration_warning: attentionOnModeration,
  personal_access_token_expiring: attentionOnModeration,
  rates: ratesIcon,
  comments: commentsIcon,
  reward_new_level: rewardNewLevelIcon,
  contributor_request_for_publish_approve: noIcon,
  run_finished: runFinishedIcon,
  agent_heartbeat: successTips,
};

/**
 * Port of `getIcon.helpers.jsx:11-104`'s `getIcon(type, theme, notification)`.
 * `theme` is passed explicitly (not read via `useTheme()` inside this
 * component) so it stays a plain render function usable from both
 * `NotificationListItem` (which already holds a `theme` from its own
 * `useTheme()` call, R-T4) and any future consumer without a second theme
 * subscription. Falls back to {@link noIcon} for an `eventType` outside the
 * known union (`../api/normalize.ts` preserves an unrecognised wire
 * `event_type` verbatim), same reasoning as `../lib/routes.ts`'s resolver.
 */
export function NotificationIcon(props: NotificationIconProps): ReactElement | null {
  const { eventType, meta, theme } = props;
  const render = ICON_RENDERERS[eventType] ?? noIcon;
  return render(theme, meta);
}
