import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { projectAvatarColor, projectInitial } from '../lib/projectAvatar';
import { avatarInitialsType, typeScale } from '@/shared/brand/typography';

export interface ProjectAvatarProps {
  projectName: string | undefined;
  size?: string;
}

/**
 * Ported from `[fsd]/widgets/sidebar-root/ui/ProjectAvatar.jsx`, reduced:
 * the old app also renders an uploaded project icon image
 * (`useProjectInfoQuery(projectId).icon_meta`) when one exists —
 * `[fsd]/features/settings/api/projectInfoApi.js` is A9 (settings) territory,
 * not this widget's, so only the letter-avatar fallback path is ported here.
 * Every project therefore renders its initial-letter avatar, never an
 * uploaded icon, until A9's icon query is reachable from a shared layer.
 */
export function ProjectAvatar({ projectName, size = '2rem' }: ProjectAvatarProps): ReactNode {
  const backgroundColor = projectAvatarColor(projectName);

  return (
    <Box
      sx={(theme: Theme) => ({
        width: size,
        height: size,
        minWidth: size,
        minHeight: size,
        borderRadius: theme.vars.shape.radiusPill,
        background: `linear-gradient(135deg, ${backgroundColor}, ${backgroundColor}4D)`,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
      })}
    >
      <Typography
        component="span"
        sx={(theme: Theme) => ({
          // Deliberately theme.vars.palette.common.white, not
          // `text.secondary`/etc.: this label sits on an arbitrary
          // per-project decorative gradient (`projectAvatarColor`, itself a
          // fixed non-brand palette — see that file's header), not a
          // themed surface, so it needs a fixed light colour in both
          // schemes rather than a scheme-aware text token.
          color: theme.vars.palette.common.white,
          // Initials snap to the ladder by avatar size (typography spec §3)
          // instead of scaling continuously at 0.375 × the circle.
          ...typeScale(theme.typography[avatarInitialsType(sizeToPx(size))]),
          fontWeight: 500,
          lineHeight: 1,
          userSelect: 'none',
        })}
      >
        {projectInitial(projectName)}
      </Typography>
    </Box>
  );
}

/** `'2rem'` → 32, `'24px'` → 24, at the 16px root the app's rem values assume. */
function sizeToPx(size: string): number {
  const value = Number.parseFloat(size);
  return size.trim().endsWith('rem') ? value * 16 : value;
}
