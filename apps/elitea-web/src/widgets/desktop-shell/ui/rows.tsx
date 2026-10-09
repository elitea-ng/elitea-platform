/**
 * The sidebar's row primitives: one dense, native-feeling list row (icon,
 * label, optional trailing slot), and a section caption. Selection and hover
 * use the same drawer-menu tokens as the web sidebar, so a brand pack
 * re-themes both alike.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

export interface ShellRowProps {
  label: string;
  icon?: ReactNode;
  selected?: boolean;
  /** Extra left indent, in spacing units (threads sit under their folder). */
  indent?: number;
  onClick: () => void;
  /** Shown at the right edge; `trailingOnHover` hides it until the row is hovered or focused. */
  trailing?: ReactNode;
  trailingOnHover?: boolean;
  caption?: string | undefined;
  testId?: string;
  ariaExpanded?: boolean;
}

export function ShellRow({
  label,
  icon,
  selected = false,
  indent = 0,
  onClick,
  trailing,
  trailingOnHover = false,
  caption,
  testId,
  ariaExpanded,
}: ShellRowProps): React.JSX.Element {
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
        '& .shell-row-trailing': { visibility: trailingOnHover ? 'hidden' : 'visible' },
        '&:hover .shell-row-trailing, &:focus-within .shell-row-trailing': { visibility: 'visible' },
      })}
    >
      <ButtonBase
        onClick={onClick}
        aria-current={selected ? 'page' : undefined}
        aria-expanded={ariaExpanded}
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          justifyContent: 'flex-start',
          gap: 1,
          paddingY: 0.5,
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
        <Box sx={{ minWidth: 0, flex: 1 }}>
          <Typography
            variant="labelSmall"
            component="span"
            sx={(theme: Theme) => ({
              display: 'block',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              color: selected ? theme.vars.palette.text.primary : theme.vars.palette.text.secondary,
            })}
          >
            {label}
          </Typography>
          {caption !== undefined && (
            <Typography
              variant="bodySmall"
              component="span"
              sx={(theme: Theme) => ({
                display: 'block',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
                color: theme.vars.palette.text.metrics,
              })}
            >
              {caption}
            </Typography>
          )}
        </Box>
      </ButtonBase>
      {trailing !== undefined && (
        <Box className="shell-row-trailing" sx={{ display: 'flex', alignItems: 'center', paddingRight: 0.5 }}>
          {trailing}
        </Box>
      )}
    </Box>
  );
}

export function SectionCaption({ children, action }: { children: string; action?: ReactNode }): React.JSX.Element {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', paddingX: 1, paddingTop: 1.5, paddingBottom: 0.5 }}>
      <Typography
        variant="labelSmall"
        component="h2"
        sx={(theme: Theme) => ({ flex: 1, margin: 0, color: theme.vars.palette.text.metrics, textTransform: 'uppercase', letterSpacing: '0.04em' })}
      >
        {children}
      </Typography>
      {action}
    </Box>
  );
}
