import type { ReactNode } from 'react';

import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseBtn } from '@/shared/ui/BaseBtn';

/**
 * `ApplicationSaveButton` — ported from TWO baseline files that, once their
 * Formik/Redux/mutation-hook orchestration is stripped per this promotion
 * pass's redesign rule (see `CreateApplicationTabBar.tsx`'s doc comment),
 * collapse into the same presentational shape: a Save button with a
 * spinner, disabled while saving or while the caller says it can't be
 * saved:
 *  - `apps/elitea-ui/src/pages/Applications/Components/Applications/
 *    CreateApplicationSaveButton.jsx` (Part 1 file 4) — its own doc
 *    comment already called it "Reusable save button... Can be used in
 *    both standalone pages and chat context", i.e. it was already meant to
 *    be generic.
 *  - `apps/elitea-ui/src/pages/Applications/Components/Applications/
 *    SaveApplicationButton.jsx` (Part 1 file 5) — the EDIT-mode
 *    equivalent, wired to `useSaveVersion` instead of `useCreateApplication`
 *    but rendering the identical "Save" + spinner button.
 *
 * The two baseline files differed only in WHICH mutation hook backed them
 * and in bespoke `isButtonDisabled` derivations (Formik dirty state, Redux
 * validation errors, pipeline-yaml dirtiness, empty conversation starters).
 * All of that is caller-computed and passed in as `disabled` here — this
 * component owns only the button's own presentation, matching
 * `features/credentials/ui/CredentialsTabBar.tsx`'s established
 * DISCLOSED-REDESIGN precedent for this exact situation.
 */
interface ApplicationSaveButtonProps {
  readonly onSave: () => void;
  readonly disabled?: boolean;
  readonly isSaving?: boolean;
  readonly testId?: string;
  readonly label?: string;
}

export function ApplicationSaveButton({
  onSave,
  disabled = false,
  isSaving = false,
  testId = 'agent-save-button',
  label = 'Save',
}: ApplicationSaveButtonProps): ReactNode {
  return (
    <BaseBtn
      data-testid={testId}
      variant="elitea"
      color="primary"
      disabled={disabled || isSaving}
      onClick={onSave}
    >
      {label}
      {isSaving && (
        <CircularProgress
          size={20}
          sx={spinnerSx}
        />
      )}
    </BaseBtn>
  );
}

const spinnerSx: SxProps<Theme> = (theme: Theme) => ({
  marginLeft: theme.spacing(1),
});
