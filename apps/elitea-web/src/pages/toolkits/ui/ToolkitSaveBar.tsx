import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
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
 * THE REASON IS SAID TWICE, ON PURPOSE. A disabled button explains nothing:
 * pointer users get it from the tooltip, and assistive-technology users get
 * it from `aria-describedby`, which needs a real element in the document to
 * point at. The described text is visually hidden rather than absent, so
 * neither audience is served a control that simply refuses to work.
 *
 * `Tooltip` needs a wrapper element around a DISABLED button — a disabled
 * button fires no pointer events of its own, so MUI never sees the hover.
 */
export interface ToolkitSaveBarProps {
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  /** The dirty gate: false when there is nothing to save. */
  readonly canSave: boolean;
  readonly isSaving: boolean;
  /** Present only when something REFUSES the save; it both disables Save and is the text shown for why. */
  readonly disabledReason: string | undefined;
}

/** The element `aria-describedby` points at. One per page — this bar is rendered once. */
const REASON_ID = 'toolkit-save-disabled-reason';

export function ToolkitSaveBar({ onSave, onDiscard, canSave, isSaving, disabledReason }: ToolkitSaveBarProps): ReactNode {
  const isBlocked = disabledReason !== undefined;
  return (
    <Box sx={containerSx}>
      <Tooltip
        title={disabledReason ?? ''}
        disableHoverListener={!isBlocked}
        disableFocusListener={!isBlocked}
        disableTouchListener={!isBlocked}
      >
        <Box
          component="span"
          sx={tooltipAnchorSx}
        >
          <BaseBtn
            data-testid="toolkit-save-button"
            variant="elitea"
            color="primary"
            disabled={!canSave || isSaving || isBlocked}
            onClick={onSave}
            // `exactOptionalPropertyTypes`: the key must be ABSENT, not
            // present-and-undefined, or the attribute is rendered empty and
            // points at nothing.
            {...(isBlocked ? { 'aria-describedby': REASON_ID } : {})}
          >
            {t('pages.toolkits.editToolkit.save', 'Save')}
            {isSaving && (
              <CircularProgress
                size={20}
                sx={spinnerSx}
              />
            )}
          </BaseBtn>
        </Box>
      </Tooltip>
      {isBlocked && (
        <Box
          component="span"
          id={REASON_ID}
          data-testid="toolkit-save-disabled-reason"
          sx={visuallyHiddenSx}
        >
          {disabledReason}
        </Box>
      )}
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

const tooltipAnchorSx: SxProps<Theme> = { display: 'inline-flex' };

const spinnerSx: SxProps<Theme> = (theme: Theme) => ({ marginLeft: theme.spacing(1) });

/**
 * The standard visually-hidden recipe: read by assistive technology, taking
 * no space and never clipped out of the accessibility tree. The sizes are
 * `rem`, not the recipe's usual raw pixels — R-T9 (`elitea/raw-px-spacing`)
 * refuses raw px in `sx`, and the box is collapsed by `clip` anyway, so the
 * exact sub-pixel value carries no meaning.
 */
const visuallyHiddenSx: SxProps<Theme> = {
  position: 'absolute',
  width: '0.0625rem',
  height: '0.0625rem',
  padding: 0,
  margin: '-0.0625rem',
  overflow: 'hidden',
  clip: 'rect(0 0 0 0)',
  whiteSpace: 'nowrap',
  border: 0,
};
