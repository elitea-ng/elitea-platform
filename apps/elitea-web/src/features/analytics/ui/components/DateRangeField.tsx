import { memo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { DateTimePicker } from '@mui/x-date-pickers/DateTimePicker';
import type { DateTimeValidationError } from '@mui/x-date-pickers/models';

import { t } from '@/shared/i18n';
import { ArrowLeftIcon } from '@/shared/ui/icons/arrow-left-icon';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';
import { CalendarIcon } from '@/shared/ui/icons/calendar-icon';

/**
 * One `From:`/`To:` field of `AnalyticsContainer`'s date-range filter bar.
 * Extracted from the baseline's two near-identical `DateTimePicker` blocks
 * (`AnalyticsContainer.jsx`'s `datePickerCommonProps` spread over both) —
 * also what brought `AnalyticsContainer`'s own cyclomatic complexity under
 * the `eslint(complexity)` budget (12).
 *
 * FIX (A10-overview-pages cluster, confirmed finding #1): the
 * `DateTimePicker`'s action bar used to unconditionally render a "Clear"
 * button (`actionBar: { actions: ['clear', 'accept'] }`), but the old
 * handler — `(next) => next !== null && onChange(next)` — dropped the
 * `null` that MUI passes to `onChange` on Clear (or on manually blanking
 * the text field), so the button was visible but silently did nothing.
 * Fixed here by adding an optional `onClear` callback: the action bar now
 * only advertises `'clear'` when a caller actually supplies one, and a
 * `null` value is routed to it instead of being swallowed — no more
 * rendered-but-dead button.
 *
 * `onClear` is a separate optional callback rather than widening `onChange`
 * itself to `(value: Date | null) => void`, because this component's only
 * real caller — `AnalyticsContainer` (`src/features/analytics/ui/
 * AnalyticsContainer.tsx`, OUT OF SCOPE for this cluster) — wires
 * `onChange` directly to `setDateFrom`/`setDateTo`
 * (`Dispatch<SetStateAction<Date>>` from `useState<Date>`), and its
 * `toIsoRange`/`presetToDateRange` model (`../model/dateRange.ts`) has no
 * representation of an unbounded/cleared bound: `toIsoRange` always emits a
 * concrete `dateFrom`/`dateTo` pair, consumed as a mandatory range by
 * `useProjectAnalyticsQuery`. Widening `onChange` to accept `null` would
 * both fail to type-check there (`Dispatch<SetStateAction<Date>>` does not
 * accept `null` under `strictFunctionTypes`) and, even if it did, still be
 * a silent no-op one layer up since nothing would consume the `null`.
 * TODO for whoever owns `AnalyticsContainer.tsx`: decide what "clear"
 * should mean for a mandatory date-range filter (e.g. resetting the
 * cleared bound to the active preset's default from `DATE_FILTER_PRESETS`,
 * `../lib/constants.ts`) and pass that decision in as `onClear={...}` on
 * each `DateRangeField`. Until that lands, this component intentionally
 * hides the Clear button instead of repeating the original silently-broken
 * affordance.
 *
 * ## An invalid value is never propagated (#6738)
 *
 * `minDateTime`/`maxDateTime` only constrain the calendar popup. A value TYPED
 * into the field is validated but was still handed to `onChange`, so From could
 * be set later than To and the screen queried a backwards window. MUI reports
 * the outcome as `context.validationError`; a non-null error now keeps the
 * last valid value and shows why under the field instead.
 *
 * ## Minimum width (#6818)
 *
 * The field sized itself to its content, so a From of `00:00` could render as
 * `00:…`. The input now reserves room for a full `dd/MM/yyyy HH:mm`.
 */
export interface DateRangeFieldProps {
  readonly label: string;
  readonly value: Date;
  readonly onChange: (value: Date) => void;
  readonly open: boolean;
  readonly onOpen: () => void;
  readonly onClose: () => void;
  readonly minDateTime?: Date;
  readonly maxDateTime?: Date;
  /**
   * Optional: when supplied, the picker's action bar shows a working
   * "Clear" button that calls this (instead of dropping the value). Omitted
   * (the current `AnalyticsContainer` caller) → no Clear button is shown at
   * all, see this file's header doc comment.
   */
  readonly onClear?: () => void;
}

const fieldSx = (theme: Theme, active: boolean) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  borderBottom: `0.0625rem solid ${active ? theme.vars.palette.primary.main : theme.vars.palette.border.lines}`,
  padding: `${theme.spacing(0.75)} ${theme.spacing(1.5)}`,
  height: '1.75rem',
  boxSizing: 'border-box' as const,
});

const rootSx = { position: 'relative' as const };

/** Enough for `dd/MM/yyyy HH:mm` plus the calendar button at the field's font size. */
const textFieldSx = { minWidth: '10.5rem' };

// Helper/error text is the `bodySmall` role (#1023); only placement here.
const errorSx = (theme: Theme) => ({
  position: 'absolute' as const,
  top: '100%',
  left: theme.spacing(1.5),
  marginTop: theme.spacing(0.25),
  whiteSpace: 'nowrap' as const,
});

const MIN_BOUND_ERRORS: ReadonlySet<DateTimeValidationError> = new Set(['minDate', 'minTime']);
const MAX_BOUND_ERRORS: ReadonlySet<DateTimeValidationError> = new Set(['maxDate', 'maxTime']);

/**
 * The message for a validation error, or `null` when the value is usable.
 *
 * A min/max error means "From later than To" only when this field was given
 * that bound — the other field's value. Without it the error can only come from
 * the picker's default 1900/2099 limits, which is an invalid date.
 */
export function dateRangeFieldErrorMessage(
  error: DateTimeValidationError,
  bounds: { readonly hasMin: boolean; readonly hasMax: boolean },
): string | null {
  if (error === null) return null;
  if ((bounds.hasMin && MIN_BOUND_ERRORS.has(error)) || (bounds.hasMax && MAX_BOUND_ERRORS.has(error))) {
    return t('analytics.dateRange.fromAfterTo', 'From must not be later than To.');
  }
  return t('analytics.dateRange.invalid', 'Enter a valid date and time.');
}

interface RejectedEdit {
  readonly error: DateTimeValidationError;
  /** The value shown after the rejection (the restored one). */
  readonly against: number;
  /** The bounds the edit was checked against. */
  readonly min: number | undefined;
  readonly max: number | undefined;
}

const labelSx = (theme: Theme) => ({
  color: theme.vars.palette.text.default,
  fontSize: theme.typography.labelSmall.fontSize,
  fontWeight: 500,
  lineHeight: '1rem',
  whiteSpace: 'nowrap' as const,
});

function DateRangeFieldImpl({
  label,
  value,
  onChange,
  open,
  onOpen,
  onClose,
  minDateTime,
  maxDateTime,
  onClear,
}: DateRangeFieldProps): ReactNode {
  // Two sources of error. `onError` reports the CURRENT value against the
  // bounds (an already-backwards pair). A rejected edit never becomes the
  // value, so it is remembered here, tagged with the value and bounds it was
  // rejected against: once any of them changes the stale message goes.
  const [validationError, setValidationError] = useState<DateTimeValidationError>(null);
  const [rejected, setRejected] = useState<RejectedEdit | null>(null);
  // The value as it was when the current edit began; `null` between edits.
  const editStart = useRef<Date | null>(null);
  const minTime = minDateTime?.getTime();
  const maxTime = maxDateTime?.getTime();
  const rejectedError =
    rejected !== null && rejected.against === value.getTime() && rejected.min === minTime && rejected.max === maxTime
      ? rejected.error
      : null;
  const errorMessage = dateRangeFieldErrorMessage(rejectedError ?? validationError, {
    hasMin: minDateTime !== undefined,
    hasMax: maxDateTime !== undefined,
  });
  return (
    // The testid is what the @visual suite masks. The value is a wall-clock
    // range derived from `Date.now()` (`Today` → today 00:00 .. 23:59), so the
    // rendered text changes from day to day; it is volatile in exactly the
    // sense `volatileRegions()` exists for, and masking it is what lets the
    // rest of this screen be asserted at all (issue #159).
    <Box
      sx={rootSx}
      onBlur={(event) => {
        // Focus leaving the field (not moving between its sections) ends the edit.
        if (!event.currentTarget.contains(event.relatedTarget)) editStart.current = null;
      }}
    >
      <Box sx={(theme: Theme) => fieldSx(theme, open)} data-testid="analytics-date-range">
        <Typography sx={labelSx}>{label}</Typography>
        <DateTimePicker
          value={value}
          onChange={(next, context) => {
            if (next === null) {
              onClear?.();
              return;
            }
            editStart.current ??= value;
            // #6738: never query a backwards or half-typed window. Put back
            // the value the edit started from (not a keystroke intermediate)
            // and say why.
            if (context.validationError !== null) {
              const restored = editStart.current;
              setRejected({ error: context.validationError, against: restored.getTime(), min: minTime, max: maxTime });
              if (restored.getTime() !== value.getTime()) onChange(restored);
              return;
            }
            setRejected(null);
            onChange(next);
          }}
          onError={setValidationError}
          open={open}
          onOpen={onOpen}
          onClose={onClose}
          {...(minDateTime !== undefined ? { minDateTime } : {})}
          {...(maxDateTime !== undefined ? { maxDateTime } : {})}
          ampm={false}
          format="dd/MM/yyyy HH:mm"
          localeText={{ okButtonLabel: t('analytics.dateRange.apply', 'Apply') }}
          slots={{
            openPickerIcon: CalendarIcon,
            leftArrowIcon: ArrowLeftIcon,
            rightArrowIcon: ArrowRightIcon,
          }}
          slotProps={{
            textField: {
              size: 'small',
              variant: 'standard',
              sx: textFieldSx,
            },
            actionBar: { actions: onClear !== undefined ? ['clear', 'accept'] : ['accept'] },
          }}
        />
      </Box>
      {errorMessage !== null ? (
        <Typography variant="bodySmall" color="error" sx={errorSx} role="alert" data-testid="analytics-date-range-error">
          {errorMessage}
        </Typography>
      ) : null}
    </Box>
  );
}

export const DateRangeField = memo(DateRangeFieldImpl);

