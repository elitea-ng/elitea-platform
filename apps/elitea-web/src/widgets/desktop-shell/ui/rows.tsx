/**
 * The sidebar's row primitives: one dense, single-line, native-feeling list
 * row (icon, label, optional quiet trailing text, optional trailing actions),
 * and a section caption. Selection and hover use the same drawer-menu tokens
 * as the web sidebar, so a brand pack re-themes both alike.
 */
import type { MouseEvent, ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

/**
 * The indent (spacing units) of one outline level: an icon (1rem) plus the
 * icon-label gap (one unit). A child row's icon column starts at its
 * parent's text edge, and a disclosure chevron sits one level to the left,
 * in the parent's icon column (`ShellRowProps.disclosure`), as in a Finder
 * outline. A row without an icon of its own keeps the icon column empty
 * (`reserveIcon`), so every row of a level shares one text edge.
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
  /** The room the hover actions take at the right edge (the label truncates before it); two buttons by default. */
  trailingRoom?: string;
  /** Quiet text after the label, on the same line (a folder's project); gives way to `trailing` on hover. */
  meta?: string;
  /** A secondary row (an empty state, "Show all"): the label in the muted colour. */
  muted?: boolean;
  /** The native tooltip: the full text a truncated row cannot show. */
  title?: string;
  testId?: string;
  ariaExpanded?: boolean;
  /** Right-click: the row's native context menu. */
  onContextMenu?: (event: MouseEvent) => void;
  /** A disclosure chevron in the leading column, one level left of the icon: `true` expanded, `false` collapsed. */
  disclosure?: boolean;
  /** Keep the icon column empty when there is no icon, so the label sits on the level's text edge. */
  reserveIcon?: boolean;
  /** A drop target that the dragged row is over. */
  dropActive?: boolean;
  /** A class on the label (a date group's caption: `shell-group-caption`). */
  labelClassName?: string;
  /** A test id on the meta text (a closed group's count). */
  metaTestId?: string;
  /** The label in bold: the row has something the person did not open yet (client contract 1.12). */
  bold?: boolean;
  /** An always-shown status at the label's end (the activity glyph); gives way to `trailing` on hover, as `meta` does. */
  status?: ReactNode;
}

const ONE_LINE = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

/** `text.secondary` is this palette's strong label colour; `text.metrics` the quiet one. */
function labelColor(theme: Theme, selected: boolean, muted: boolean): string {
  return muted && !selected ? theme.vars.palette.text.metrics : theme.vars.palette.text.secondary;
}

interface RowLook {
  readonly selected: boolean;
  readonly dropActive: boolean;
  readonly swaps: boolean;
  readonly trailingRoom: string;
}

function rowSx(theme: Theme, { selected, dropActive, swaps, trailingRoom }: RowLook) {
  return {
    position: 'relative',
    display: 'flex',
    alignItems: 'center',
    borderRadius: theme.vars.shape.radiusSm,
    background: selected ? theme.vars.palette.background.button.drawerMenu.selected : undefined,
    outline: dropActive ? `1px solid ${theme.vars.palette.primary.main}` : undefined,
    outlineOffset: -1,
    '&:hover': { background: selected ? undefined : theme.vars.palette.background.button.drawerMenu.hover },
    // The actions overlay the row's right edge on hover, where the meta text was.
    '& .shell-row-trailing': swaps ? { position: 'absolute', right: 0, top: 0, bottom: 0, visibility: 'hidden' } : {},
    '&:hover .shell-row-trailing, &:focus-within .shell-row-trailing': { visibility: 'visible' },
    ...(swaps
      ? {
          '&:hover .shell-row-meta, &:focus-within .shell-row-meta': { display: 'none' },
          // Room for the actions, so the label truncates before them.
          '&:hover .shell-row-main, &:focus-within .shell-row-main': { paddingRight: trailingRoom },
        }
      : {}),
  } as const;
}

/** Quiet text after the label (a folder's project, a count). */
function RowMeta({ meta, testId }: { readonly meta: string | undefined; readonly testId?: string | undefined }): React.JSX.Element | null {
  if (meta === undefined || meta === '') return null;
  return (
    <Typography
      className="shell-row-meta"
      data-testid={testId}
      variant="bodySmall"
      component="span"
      sx={(theme: Theme) => ({ ...ONE_LINE, flexShrink: 1, maxWidth: '45%', color: theme.vars.palette.text.metrics })}
    >
      {meta}
    </Typography>
  );
}

interface RowLabelProps {
  readonly label: string;
  readonly className: string | undefined;
  /** Bold while the row has something the person did not open yet (client contract 1.12). */
  readonly bold: boolean | undefined;
  readonly selected: boolean;
  readonly muted: boolean;
}

/** The row's label, one line. */
function RowLabel({ label, className, bold = false, selected, muted }: RowLabelProps): React.JSX.Element {
  return (
    <Typography
      className={className}
      variant="labelSmall"
      component="span"
      data-unread={bold ? 'true' : undefined}
      sx={(theme: Theme) => ({ ...ONE_LINE, flex: 1, minWidth: 0, color: labelColor(theme, selected, muted), ...(bold ? { fontWeight: 700 } : {}) })}
    >
      {label}
    </Typography>
  );
}

/** An always-shown status after the label (the activity glyph). */
function RowStatus({ status }: { readonly status: ReactNode }): React.JSX.Element | null {
  if (status === undefined || status === null) return null;
  return (
    <Box component="span" className="shell-row-meta" sx={{ display: 'inline-flex', alignItems: 'center', flexShrink: 0 }}>
      {status}
    </Box>
  );
}

/** The row's leading columns: the disclosure chevron (one level left) and the icon column. */
function RowLeading({ disclosure, icon, reserveIcon, indent }: { readonly disclosure: boolean | undefined; readonly icon: ReactNode; readonly reserveIcon: boolean; readonly indent: number }): React.JSX.Element {
  return (
    <>
      {disclosure !== undefined && (
        <Box
          aria-hidden
          data-testid="shell-row-disclosure"
          sx={(theme: Theme) => ({
            position: 'absolute',
            // One level left of the icon column: the parent's icon column.
            left: theme.spacing(1 + indent - CHILD_INDENT),
            width: '1rem',
            display: 'flex',
            justifyContent: 'center',
            color: theme.vars.palette.text.metrics,
            '& svg': { width: '0.875rem', height: '0.875rem', transition: 'transform 120ms', transform: disclosure ? 'rotate(90deg)' : 'none' },
          })}
        >
          <ChevronRightIcon />
        </Box>
      )}
      {(icon !== undefined || reserveIcon) && (
        <Box
          aria-hidden
          sx={(theme: Theme) => ({
            display: 'flex',
            flexShrink: 0,
            width: '1rem',
            color: theme.vars.palette.text.metrics,
            '& svg': { width: '1rem', height: '1rem' },
          })}
        >
          {icon}
        </Box>
      )}
    </>
  );
}

export function ShellRow(props: ShellRowProps): React.JSX.Element {
  const { label, icon, selected = false, indent = 0, onClick, trailing, trailingOnHover = false, trailingRoom = '3.75rem', meta, muted = false, title, testId, ariaExpanded, onContextMenu } = props;
  const { disclosure, reserveIcon = false, dropActive = false, labelClassName, metaTestId, bold, status } = props;
  const swaps = trailing !== undefined && trailingOnHover;
  return (
    <Box
      data-testid={testId}
      onContextMenu={onContextMenu}
      sx={(theme: Theme) => rowSx(theme, { selected: selected || dropActive, dropActive, swaps, trailingRoom })}
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
        <RowLeading disclosure={disclosure} icon={icon} reserveIcon={reserveIcon} indent={indent} />
        <RowLabel label={label} className={labelClassName} bold={bold} selected={selected} muted={muted} />
        <RowMeta meta={meta} testId={metaTestId} />
        <RowStatus status={status} />
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
 * right. With `onToggle` the name is a disclosure button (chevron after it);
 * with `onOpen` it is a button to the section's own page (Local work's list,
 * a new chat).
 */
export function SectionCaption({
  children,
  action,
  expanded,
  onToggle,
  onOpen,
  testId,
}: {
  children: string;
  action?: ReactNode;
  expanded?: boolean;
  onToggle?: () => void;
  onOpen?: () => void;
  testId?: string;
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
        {onOpen !== undefined && (
          <ButtonBase
            onClick={onOpen}
            data-testid={testId}
            sx={(theme: Theme) => ({ borderRadius: theme.vars.shape.radiusSm, '&:hover span': { color: theme.vars.palette.text.secondary } })}
          >
            {name}
          </ButtonBase>
        )}
        {onOpen === undefined && onToggle === undefined && name}
        {onOpen === undefined && onToggle !== undefined && (
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
