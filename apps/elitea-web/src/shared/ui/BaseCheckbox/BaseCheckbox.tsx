import { forwardRef, useCallback } from 'react';

import MuiCheckbox, { type CheckboxProps } from '@mui/material/Checkbox';
import MuiRadio, { type RadioProps } from '@mui/material/Radio';
import SvgIcon from '@mui/material/SvgIcon';

import { CheckboxCheckedIcon } from '../icons/checkbox-checked-icon';
import { CheckboxEmptyIcon } from '../icons/checkbox-empty-icon';
import { CheckboxIndeterminateIcon } from '../icons/checkbox-indeterminate-icon';

/** @public Exported for consumers that want the mode vocabulary by name (this unit's own call sites just pass `'checkbox'`/`'radio'` literals). */
export const CHECKBOX_MODES = {
  checkbox: 'checkbox',
  radio: 'radio',
} as const;

/** @public */
export type CheckboxMode = (typeof CHECKBOX_MODES)[keyof typeof CHECKBOX_MODES];

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface BaseCheckboxProps extends Omit<CheckboxProps, 'icon' | 'checkedIcon' | 'indeterminateIcon'> {
  /** `'checkbox'` (default) renders a real `<Checkbox>`; `'radio'` renders a `<Radio>`. */
  mode?: CheckboxMode;
  'aria-label'?: string;
  'aria-labelledby'?: string;
}

/**
 * A checkbox/radio wrapper with the app's outline-style checked/empty/
 * indeterminate icons. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/checkbox/BaseCheckbox.jsx`. Styling
 * (colours, size ladder) lives in `shared/brand/mui-overrides/MuiCheckbox.ts`
 * / `MuiRadio.ts` (R-T12).
 *
 * Accessibility fix, not in the baseline (which had the same gap): MUI 9's
 * `Checkbox`/`Radio` render `<span role=undefined class="MuiButtonBase-root">
 * <input type="checkbox"></span>` — passing `aria-label` as a bare prop (the
 * natural call shape, and what the baseline's own call sites do) lands it on
 * the outer, roleless `<span>`, where axe's `aria-prohibited-attr` rule
 * correctly rejects it, AND the actual form control is left with no
 * accessible name at all (`form elements must have labels`). Both were
 * caught by Storybook's a11y addon (`a11y.test: 'error'`) on this unit's
 * own stories — exactly the class of defect that gate exists to catch,
 * since it runs against real components, not code review. Fixed by routing
 * `aria-label`/`aria-labelledby` to the `input` slot, which is the element
 * that actually needs the accessible name.
 */
export const BaseCheckbox = forwardRef<HTMLButtonElement, BaseCheckboxProps>(function BaseCheckbox(
  { mode = CHECKBOX_MODES.checkbox, 'aria-label': ariaLabel, 'aria-labelledby': ariaLabelledBy, ...rest },
  ref,
) {
  const inputSlotProps = {
    ...(ariaLabel !== undefined ? { 'aria-label': ariaLabel } : {}),
    ...(ariaLabelledBy !== undefined ? { 'aria-labelledby': ariaLabelledBy } : {}),
  };

  // Second accessibility fix, same shape as the `aria-label` one above and
  // found the same way — by a real component being rendered, not by review.
  //
  // MUI's `Checkbox` sets `aria-checked="mixed"` on the input when
  // `indeterminate`, and deliberately does NOT set the native `indeterminate`
  // DOM property (its own prop docs say so; it emits `data-indeterminate`
  // instead). axe's `aria-conditional-attr` then correctly rejects the pair: on
  // a native checkbox `aria-checked` must agree with the element's own state,
  // and that state is `false` while `aria-checked` claims `mixed`.
  //
  // Setting the native property is the fix rather than stripping the ARIA:
  // `input.indeterminate = true` is what screen readers actually announce as
  // "mixed", and with it set, MUI's `aria-checked="mixed"` becomes true rather
  // than contradictory. So the control gains the state it was only ever
  // claiming to have.
  //
  // This surfaced when issue #246's migration put the first rows into the
  // admin permission matrix's `default` mode: its group checkboxes render
  // indeterminate for a partially-granted group, and until there was a single
  // default-mode grant, that branch had no data to render and the defect had
  // never been reachable. See PermissionMatrixRows.tsx.
  // The ref goes on the `input` SLOT rather than through an `inputRef` prop:
  // MUI 9 moved that plumbing into slotProps, and the input is the element
  // whose DOM property has to be set.
  const indeterminate = rest.indeterminate === true;
  const attachInput = useCallback(
    (node: HTMLInputElement | null) => {
      if (node !== null) {
        node.indeterminate = indeterminate;
      }
    },
    [indeterminate],
  );

  if (mode === CHECKBOX_MODES.radio) {
    return (
      <MuiRadio
        ref={ref}
        disableRipple
        slotProps={{ input: inputSlotProps }}
        {...(rest as RadioProps)}
      />
    );
  }

  // A caller's own `slotProps.input` (an `aria-label`, an `aria-describedby`)
  // is MERGED into the slot, not spread over it: spread last, it replaced the
  // whole slot and silently dropped the native-indeterminate ref above.
  const { slotProps: callerSlotProps, ...checkboxRest } = rest;
  const callerInput = callerSlotProps?.input;
  const inputSlot =
    typeof callerInput === 'function'
      ? callerInput
      : { ...inputSlotProps, ...callerInput, ref: attachInput };

  return (
    <MuiCheckbox
      ref={ref}
      disableRipple
      icon={
        <SvgIcon
          component={CheckboxEmptyIcon}
          inheritViewBox
        />
      }
      checkedIcon={
        <SvgIcon
          component={CheckboxCheckedIcon}
          inheritViewBox
        />
      }
      indeterminateIcon={
        <SvgIcon
          component={CheckboxIndeterminateIcon}
          inheritViewBox
        />
      }
      {...checkboxRest}
      slotProps={{ ...callerSlotProps, input: inputSlot }}
    />
  );
});
