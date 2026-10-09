/**
 * The sidebar's row primitives: one dense, single-line, native-feeling list
 * row (icon, label, optional quiet trailing text, optional trailing actions),
 * and a section caption. Selection and hover use the same drawer-menu tokens
 * as the web sidebar, so a brand pack re-themes both alike.
 */
import type { ReactNode } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

/**
 * The indent (spacing units) that puts a child row's label under its
 * parent's: the parent's icon (1rem) plus the icon-label gap (one unit).
 */
export const CHILD_INDENT = 3;

export interface ShellRowProps {
  label: string;
  icon?: ReactNode;
  selected?: boolean;
  /** Extra left indent, in spacing units (threads sit under their folder: {@link CHILD_INDENT}). */
  indent?: number;
  onClick: () => void;
  /** Shown at the right edge; `trailingOnHover` hides it until the row is hovered or focused. */
  trailing?: ReactNode;
  trailingOnHover?: boolean;
  /** Quiet text after the label, on the same line (a folder's project); gives way to `trailing` on hover. */
  meta?: string;
  /** A secondary row (an empty state, "Show all"): the label in the muted colour. */
  muted?: boolean;
  /** The native tooltip: the full text a truncated row cannot show. */
  title?: string;
  testId?: string;
  ariaExpanded?: boolean;
}

const ONE_LINE = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

/** `text.secondary` is this palette's strong label colour; `text.metrics` the quiet one. */
function labelColor(theme: Theme, selected: boolean, muted: boolean): string {
  return muted && !selected ? theme.vars.palette.text.metrics : theme.vars.palette.text.secondary;
}

export function ShellRow({
  label,
  icon,
  selected = false,
  indent = 0,
  onClick,
  trailing,
  trailingOnHover = false,
  meta,
  muted = false,
  title,
  testId,
  ariaExpanded,
}: ShellRowProps): React.JSX.Element {
  const swaps = trailing !== undefined && trailingOnHover;
  return (
    <Box
      data-testid={testId}
      sx={(theme: Theme) => ({
        position: 'relative',
        display: 'flex',
        alignItems: 'center',
        borderRadius: theme.vars.shape.radiusSm,
        background: selected ? theme.vars.palette.background.button.drawerMenu.selected : undefined,
        '&:hover': { background: selected ? undefined : theme.vars.palette.background.button.drawerMenu.hover },
        // The actions overlay the row's right edge on hover, where the meta text was.
        '& .shell-row-trailing': swaps ? { position: 'absolute', right: 0, top: 0, bottom: 0, visibility: 'hidden' } : {},
        '&:hover .shell-row-trailing, &:focus-within .shell-row-trailing': { visibility: 'visible' },
        ...(swaps
          ? {
              '&:hover .shell-row-meta, &:focus-within .shell-row-meta': { display: 'none' },
              // Room for the actions, so the label truncates before them.
              '&:hover .shell-row-main, &:focus-within .shell-row-main': { paddingRight: '3.75rem' },
            }
          : {}),
      })}
    >
      <ButtonBase
        className="shell-row-main"
        onClick={onClick}
        title={title}
        aria-current={selected ? 'page' : undefined}
        aria-expanded={ariaExpanded}
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          justifyContent: 'flex-start',
          gap: 1,
          paddingY: 0.25,
          paddingLeft: 1 + indent,
          paddingRight: 1,
          minHeight: '1.75rem',
          borderRadius: theme.vars.shape.radiusSm,
          textAlign: 'left',
        })}
      >
        {icon !== undefined && (
          <Box
            aria-hidden
            sx={(theme: Theme) => ({
              display: 'flex',
              flexShrink: 0,
              color: theme.vars.palette.text.metrics,
              '& svg': { width: '1rem', height: '1rem' },
            })}
          >
            {icon}
          </Box>
        )}
        <Typography
          variant="labelSmall"
          component="span"
          sx={(theme: Theme) => ({ ...ONE_LINE, flex: 1, minWidth: 0, color: labelColor(theme, selected, muted) })}
        >
          {label}
        </Typography>
        {meta !== undefined && meta !== '' && (
          <Typography
            className="shell-row-meta"
            variant="bodySmall"
            component="span"
            sx={(theme: Theme) => ({ ...ONE_LINE, flexShrink: 1, maxWidth: '45%', color: theme.vars.palette.text.metrics })}
          >
            {meta}
          </Typography>
        )}
      </ButtonBase>
      {trailing !== undefined && (
        <Box className="shell-row-trailing" sx={{ display: 'flex', alignItems: 'center', paddingRight: 0.5 }}>
          {trailing}
        </Box>
      )}
    </Box>
  );
}

/**
 * A section header: the name, in sentence case, and an optional action at the
 * right. With `onToggle` the name is a disclosure button (chevron after it).
 */
export function SectionCaption({
  children,
  action,
  expanded,
  onToggle,
}: {
  children: string;
  action?: ReactNode;
  expanded?: boolean;
  onToggle?: () => void;
}): React.JSX.Element {
  const name = (
    <Typography
      variant="labelSmall"
      component="span"
      sx={(theme: Theme) => ({ fontWeight: 600, color: theme.vars.palette.text.metrics })}
    >
      {children}
    </Typography>
  );
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', minHeight: '1.75rem', paddingLeft: 1, paddingRight: 0.5, paddingTop: 1.5, paddingBottom: 0.25 }}>
      <Box component="h2" sx={{ flex: 1, margin: 0, display: 'flex', minWidth: 0 }}>
        {onToggle === undefined ? (
          name
        ) : (
          <ButtonBase
            onClick={onToggle}
            aria-expanded={expanded}
            sx={(theme: Theme) => ({
              gap: 0.25,
              borderRadius: theme.vars.shape.radiusSm,
              color: theme.vars.palette.text.metrics,
              '& svg': { width: '0.875rem', height: '0.875rem', transition: 'transform 120ms', transform: expanded === false ? 'rotate(-90deg)' : 'none' },
            })}
          >
            {name}
            <ExpandMoreIcon aria-hidden />
          </ButtonBase>
        )}
      </Box>
      {action}
    </Box>
  );
}
