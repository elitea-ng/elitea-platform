/**
 * The sidebar's folder glyphs, closed and open, for Local work's folders and
 * the Chats folders and date groups. Material's outlined "folder open" differs
 * from "folder" by one inner line, which a 16px row cannot show; the open one
 * here tips its front flap forward. Stroked in `currentColor` like the
 * sidebar's other glyphs.
 */
import SvgIcon, { type SvgIconProps } from '@mui/material/SvgIcon';

import { t } from '@/shared/i18n';

const STROKE = { fill: 'none', stroke: 'currentColor', strokeWidth: 1.75, strokeLinejoin: 'round' } as const;

export function FolderClosedIcon(props: SvgIconProps): React.JSX.Element {
  return (
    <SvgIcon {...props} viewBox="0 0 24 24">
      <path {...STROKE} d="M3 6.5A1.5 1.5 0 0 1 4.5 5H9l2 2h8.5A1.5 1.5 0 0 1 21 8.5v9a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5z" />
    </SvgIcon>
  );
}

export function FolderOpenIcon(props: SvgIconProps): React.JSX.Element {
  return (
    <SvgIcon {...props} viewBox="0 0 24 24">
      <path {...STROKE} d="M3 17.5v-11A1.5 1.5 0 0 1 4.5 5H9l2 2h7.5A1.5 1.5 0 0 1 20 8.5V10" />
      <path {...STROKE} d="M3 17.5l2.6-6.4A1.5 1.5 0 0 1 7 10h14.1a1 1 0 0 1 .94 1.34l-2.4 6.66A1.5 1.5 0 0 1 18.3 19H4.5A1.5 1.5 0 0 1 3 17.5z" />
    </SvgIcon>
  );
}

/** The pin, drawn small after a pinned folder's or conversation's name (issue 2031). Labelled through `aria-label`, not a `<title>`: that would add "Pinned" to the row's text. */
export function PinMark(): React.JSX.Element {
  return (
    <SvgIcon
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- an inline SVG that must take `currentColor`; `<img>` cannot, and a `<title>` would add "Pinned" to the row's text.
      role="img"
      aria-hidden={false}
      aria-label={t('desktop.chats.pinned', 'Pinned')}
      data-testid="shell-pin-mark"
      viewBox="0 0 24 24"
      sx={{ width: '0.75rem', height: '0.75rem', marginRight: '0.25rem', opacity: 0.7 }}
    >
      <path fill="currentColor" d="M16 9V4h1c.55 0 1-.45 1-1s-.45-1-1-1H7c-.55 0-1 .45-1 1s.45 1 1 1h1v5c0 1.66-1.34 3-3 3v2h5.97v7l1 1 1-1v-7H19v-2c-1.66 0-3-1.34-3-3z" />
    </SvgIcon>
  );
}

/** The always-shown status of a row: the pin when pinned, then whatever the row already says. */
export function PinnedStatus({ pinned, children }: { readonly pinned: boolean; readonly children?: React.ReactNode }): React.JSX.Element {
  return (
    <>
      {pinned && <PinMark />}
      {children}
    </>
  );
}
