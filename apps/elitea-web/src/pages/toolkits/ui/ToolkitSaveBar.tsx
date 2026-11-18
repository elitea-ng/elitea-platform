import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { DiscardButton } from '@/shared/ui/DiscardButton';

/**
 * The toolkit edit page's Save/Cancel control.
 *
 * SHAPE: the agent editor's, deliberately. `pages/agents/ui/
 * EditApplicationSaveBar.tsx` puts a dirty-gated Save next to a
 * confirm-then-discard Cancel in the page header, over `shared/ui`'s
 * `BaseBtn`/`DiscardButton`; this is the same pair in the same place, so the
 * two editors behave alike. It is NOT `entities/application-form`'s
 * `CreateApplicationTabBar` (which the agent page reaches that pair
 * through): that slice models the AGENT form, a toolkit is not one, and the
 * one thing this bar adds — a stated reason for a refused Save — belongs to
 * the toolkit's credential check rather than to any application form.
 *
 * A CREDENTIAL THE PROVIDER REFUSED WARNS; IT NEVER BLOCKS (#2143). Save is
 * gated on the dirty flag and the in-flight save only. The warning is visible
 * text with an `<output>` live region, and Save points at it with `aria-describedby`.
 */
export interface ToolkitSaveBarProps {
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  /** The dirty gate: false when there is nothing to save. */
  readonly canSave: boolean;
  readonly isSaving: boolean;
  /** Present only when the provider explicitly refused the selected credential. Shown beside Save; it never disables it. */
  readonly credentialWarning: string | undefined;
}

/** The element `aria-describedby` points at. One per page — this bar is rendered once. */
const REASON_ID = 'toolkit-save-credential-warning';

export function ToolkitSaveBar({ onSave, onDiscard, canSave, isSaving, credentialWarning }: ToolkitSaveBarProps): ReactNode {
  const hasWarning = credentialWarning !== undefined;
  return (
    <Box sx={containerSx}>
      {hasWarning && (
        <Typography
          component="output"
          id={REASON_ID}
          data-testid="toolkit-save-credential-warning"
          variant="bodySmall"
          sx={warningSx}
        >
          {credentialWarning}
        </Typography>
      )}
      <BaseBtn
        data-testid="toolkit-save-button"
        variant="elitea"
        color="primary"
        disabled={!canSave || isSaving}
        onClick={onSave}
        // `exactOptionalPropertyTypes`: the key must be ABSENT, not
        // present-and-undefined, or the attribute is rendered empty and
        // points at nothing.
        {...(hasWarning ? { 'aria-describedby': REASON_ID } : {})}
      >
        {t('pages.toolkits.editToolkit.save', 'Save')}
        {isSaving && (
          <CircularProgress
            size={20}
            sx={spinnerSx}
          />
        )}
      </BaseBtn>
      <DiscardButton
        data-testid="toolkit-cancel-button"
        title={t('pages.toolkits.editToolkit.cancel', 'Cancel')}
        disabled={!canSave}
        isSaving={isSaving}
        onDiscard={onDiscard}
      />
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1) });

const warningSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.warning.main, maxWidth: theme.spacing(40) });

const spinnerSx: SxProps<Theme> = (theme: Theme) => ({ marginLeft: theme.spacing(1) });
