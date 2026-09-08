import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { combineSx } from '../lib/combineSx';

/** @public shared/ui component API. */
export interface CharacterCounterProps {
  value: string;
  maxLength: number;
  textVariant?: 'bodySmall' | 'bodySmall2' | 'bodyMedium' | 'labelSmall' | 'labelMedium' | 'labelTiny';
  /**
   * `false` keeps the line in flow but invisible (#848).
   *
   * A counter that UNMOUNTS on blur shrinks the row and shifts every control
   * under it up by its own height, so a real click already in flight lands on
   * nothing. Every in-flow caller passes this instead of a conditional mount.
   *
   * @default true
   */
  visible?: boolean;
  /** Layout only — the caller's own placement. The colour stays this component's. */
  sx?: SxProps<Theme>;
  'data-testid'?: string;
}

/**
 * A "N characters left" counter that turns to the error colour at the
 * limit. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/text/CharacterCounter.jsx`.
 *
 * Deviation from the baseline: the baseline used `palette.error.main` /
 * `palette.secondary.main` (roles tuned for filled surfaces, not small
 * text-on-background contrast — `error.main` is `#D71616` in both schemes,
 * 3.55:1 against the dark scheme's background at this text size, short of
 * WCAG AA's 4.5:1). Storybook's a11y addon (`a11y.test: 'error'`) caught
 * this — exactly the defect class that gate exists to catch, since the
 * baseline's own `a11y: { test: 'todo' }` could never fail on it.
 * `text.warningText` is the same role the baseline's own `BannerMessage`
 * uses for its error-variant text (`rgba(255,223,223,1)` in dark scheme —
 * still red-adjacent, but tuned for AA text contrast rather than for a
 * filled/bordered surface). Also fixes a WCAG 1.4.1 (use-of-color) gap: the
 * limit-reached state was signalled by colour alone; the appended "reached
 * the MAXIMUM" text was already there, so this is belt-and-suspenders, not
 * a new requirement.
 *
 * WIRED, at last. This component was ported, tested and had ZERO callers —
 * its own header said so ("consumed once a features/widgets/pages caller
 * exists (none does yet in this pass)"). Five files meanwhile rendered their
 * own bare `Typography` counter: none of them turned red, and none of them
 * said anything at the limit, which is what the legacy suite's
 * `agents/test_agent_character_limits.py` asserts. All five call this now —
 * `WelcomeMessageInput`, `ConversationStartersEditor`, `CreateAgentForm`,
 * `ApplicationEditForm` and toolkits' `NameDescriptionInput`.
 *
 * `remaining` is clamped at zero. A caller whose stored value is longer than
 * its own `maxLength` — a row saved under an older, larger limit — would
 * otherwise read "-12 characters left", which is not a state a reader can act
 * on, and would never show the limit message either.
 */
export function CharacterCounter({
  value,
  maxLength,
  textVariant = 'bodySmall',
  visible = true,
  sx,
  'data-testid': dataTestId,
}: CharacterCounterProps): ReactNode {
  const remaining = Math.max(maxLength - value.length, 0);
  const isAtLimit = remaining === 0;

  return (
    <Box
      data-testid={dataTestId}
      sx={combineSx(
        (theme: Theme) => ({
          color: isAtLimit ? theme.vars.palette.text.warningText : theme.vars.palette.text.secondary,
          visibility: visible ? 'visible' : 'hidden',
        }),
        sx,
      )}
    >
      <Typography variant={textVariant}>
        {remaining} {t('shared.ui.characterCounter.remaining', 'characters left')}
        {isAtLimit &&
          t('shared.ui.characterCounter.atLimit', '. You have reached the MAXIMUM character limit')}
      </Typography>
    </Box>
  );
}
