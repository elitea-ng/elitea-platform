import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface MentionToolItemProps {
  label: string;
  description?: string;
  icon?: ReactNode;
  onClick?: () => void;
  isHighlighted?: boolean;
  /** Small labels after the name (a trust state, "Pinned"). `title` explains one on hover. */
  badges?: readonly { readonly label: string; readonly title?: string }[];
  'data-testid'?: string;
}

/**
 * One row of an `@`-mention autocomplete list (a single toolkit tool).
 * Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/mention/MentionToolItem.jsx`.
 *
 * R-C1 fix: the baseline's row was a bare `Box onClick={...}` — a
 * mouse-only affordance with no keyboard path and no accessible role. This
 * port uses `ButtonBase` (a native `<button>`), giving `Enter`/`Space`
 * activation, focus, and `tabIndex` from the browser instead of a
 * hand-rolled `role`/`onKeyDown` shim (same fix `BannerMessage` applied for
 * the same reason).
 */
export function MentionToolItem({
  label,
  description,
  icon,
  onClick,
  isHighlighted = false,
  badges = [],
  'data-testid': dataTestId,
}: MentionToolItemProps): ReactNode {
  return (
    <ButtonBase
      onClick={onClick}
      disableRipple
      data-testid={dataTestId}
      data-highlighted={isHighlighted ? 'true' : undefined}
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        width: '100%',
        textAlign: 'left',
        padding: '0.5rem 0.75rem',
        borderRadius: theme.vars.shape.radiusMd,
        cursor: 'pointer',
        background: isHighlighted
          ? theme.vars.palette.background.userInputBackgroundActive
          : theme.vars.palette.background.userInputBackground,
        '&:hover': { background: theme.vars.palette.background.userInputBackgroundActive },
      })}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem', minWidth: 0 }}>
        {icon && (
          <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'center', flexShrink: 0 }}>
            {icon}
          </Box>
        )}
        <Typography
          variant="headingSmall"
          color="text.secondary"
          sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
        >
          {label}
        </Typography>
        {badges.map((badge) => (
          <Typography
            key={badge.label}
            variant="labelSmall"
            color="text.secondary"
            title={badge.title}
            sx={(theme: Theme) => ({ flexShrink: 0, padding: '0 0.375rem', borderRadius: theme.vars.shape.radiusSm, border: `0.0625rem solid ${theme.vars.palette.border.lines}` })}
          >
            {badge.label}
          </Typography>
        ))}
      </Box>
      {description && (
        <Typography
          variant="bodySmall"
          color="text.primary"
          sx={{
            overflow: 'hidden',
            display: '-webkit-box',
            WebkitBoxOrient: 'vertical',
            WebkitLineClamp: 1,
          }}
        >
          {description}
        </Typography>
      )}
    </ButtonBase>
  );
}
