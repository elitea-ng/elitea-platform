/**
 * The sidebar's folder glyphs, closed and open, for Local work's folders and
 * the Chats folders and date groups. Material's outlined "folder open" differs
 * from "folder" by one inner line, which a 16px row cannot show; the open one
 * here tips its front flap forward. Stroked in `currentColor` like the
 * sidebar's other glyphs.
 */
import SvgIcon, { type SvgIconProps } from '@mui/material/SvgIcon';

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
