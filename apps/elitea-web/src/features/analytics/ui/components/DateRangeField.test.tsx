import { useState } from 'react';
import type { ReactElement } from 'react';

import { AdapterDateFns } from '@mui/x-date-pickers/AdapterDateFns';
import { LocalizationProvider } from '@mui/x-date-pickers/LocalizationProvider';
import type { RenderResult } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { DateRangeField } from './DateRangeField';

/**
 * `DateRangeField` requires a `LocalizationProvider` ancestor (it does not
 * supply its own — `AnalyticsContainer`, its only real caller, provides
 * exactly one for both `From:`/`To:` fields, per that component's own
 * header comment on why this app self-provisions the provider locally).
 */
function renderField(ui: ReactElement): RenderResult {
  return renderWithTheme(<LocalizationProvider dateAdapter={AdapterDateFns}>{ui}</LocalizationProvider>);
}

describe('DateRangeField', () => {
  it('renders the label', () => {
    const { getByText } = renderField(
      <DateRangeField
        label="From:"
        value={new Date('2026-07-20T00:00:00.000Z')}
        onChange={() => {}}
        open={false}
        onOpen={() => {}}
        onClose={() => {}}
      />,
    );
    expect(getByText('From:')).toBeInTheDocument();
  });

  it('renders the picker input with the formatted value', () => {
    const { container } = renderField(
      <DateRangeField
        label="From:"
        value={new Date('2026-07-20T10:30:00.000Z')}
        onChange={() => {}}
        open={false}
        onOpen={() => {}}
        onClose={() => {}}
      />,
    );
    const input = container.querySelector('input');
    expect(input).not.toBeNull();
    expect(input?.value).toContain('2026');
  });

  it('does not crash without minDateTime/maxDateTime (both optional)', () => {
    expect(() =>
      renderField(
        <DateRangeField
          label="To:"
          value={new Date('2026-07-27T00:00:00.000Z')}
          onChange={vi.fn()}
          open={false}
          onOpen={() => {}}
          onClose={() => {}}
        />,
      ),
    ).not.toThrow();
  });

  it('accepts min/maxDateTime constraints without crashing', () => {
    expect(() =>
      renderField(
        <DateRangeField
          label="From:"
          value={new Date('2026-07-20T00:00:00.000Z')}
          onChange={vi.fn()}
          open={false}
          onOpen={() => {}}
          onClose={() => {}}
          maxDateTime={new Date('2026-07-27T00:00:00.000Z')}
        />,
      ),
    ).not.toThrow();
  });

  /**
   * Regression coverage for the "Clear button silently no-ops" finding: the
   * old `onChange={(next) => next !== null && onChange(next)}` handler
   * discarded the `null` MUI's action bar sends on Clear, so the button was
   * visible but dead. These two cases pin the fix: the button only appears
   * when a caller opts in via `onClear`, and clicking it calls `onClear`
   * (not `onChange`, which has no way to represent "cleared").
   */
  it('routes a Clear click to onClear, not onChange, when onClear is supplied', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const onClear = vi.fn();
    const { getByRole } = renderField(
      <DateRangeField
        label="From:"
        value={new Date('2026-07-20T00:00:00.000Z')}
        onChange={onChange}
        open
        onOpen={() => {}}
        onClose={() => {}}
        onClear={onClear}
      />,
    );

    await user.click(getByRole('button', { name: /clear/i }));

    expect(onClear).toHaveBeenCalledTimes(1);
    expect(onChange).not.toHaveBeenCalled();
  });

  it('hides the Clear action entirely when no onClear is supplied', () => {
    const { queryByRole } = renderField(
      <DateRangeField
        label="From:"
        value={new Date('2026-07-20T00:00:00.000Z')}
        onChange={vi.fn()}
        open
        onOpen={() => {}}
        onClose={() => {}}
      />,
    );

    expect(queryByRole('button', { name: /clear/i })).toBeNull();
    // The rest of the action bar (Apply/"OK") is still there — only the
    // dead Clear affordance is removed.
    expect(queryByRole('button', { name: /apply/i })).not.toBeNull();
  });

  /**
   * #6738: the bounds used to constrain only the popup. A value typed into the
   * field past `maxDateTime` reached `onChange`, so From could be set later
   * than To. The field now keeps its last valid value and says why.
   */
  it('shows the From-after-To error and does not propagate a value past maxDateTime', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    // Stateful like the real caller, so the section keeps both typed digits.
    function Controlled() {
      const [value, setValue] = useState(new Date(2026, 6, 27, 10, 30));
      return (
        <>
          <DateRangeField
            label="From:"
            value={value}
            onChange={(next) => {
              onChange(next);
              setValue(next);
            }}
            open={false}
            onOpen={() => {}}
            onClose={() => {}}
            maxDateTime={new Date(2026, 6, 27, 12, 0)}
          />
          {/* Stands in for a preset button replacing the value from outside. */}
          <button type="button" onClick={() => setValue(new Date(2026, 6, 27, 0, 0))}>
            preset
          </button>
        </>
      );
    }
    const { getByRole, findByRole, queryByRole } = renderField(<Controlled />);

    // 23:30 is past the 12:00 bound on the same day.
    await user.click(getByRole('spinbutton', { name: 'Hours' }));
    await user.keyboard('23');

    expect(await findByRole('alert')).toHaveTextContent('From must not be later than To.');
    // "2" (02:30) was a valid intermediate value; 23:30 never got through.
    const hours = onChange.mock.calls.map((call) => (call[0] as Date).getHours());
    expect(hours).not.toContain(23);

    // A new value from outside (a preset) drops the stale message.
    await user.click(getByRole('button', { name: 'preset' }));
    expect(queryByRole('alert')).toBeNull();
  });

  it('propagates a typed value inside the bounds and shows no error', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole, queryByRole } = renderField(
      <DateRangeField
        label="From:"
        value={new Date(2026, 6, 20, 10, 30)}
        onChange={onChange}
        open={false}
        onOpen={() => {}}
        onClose={() => {}}
        maxDateTime={new Date(2026, 6, 27, 0, 0)}
      />,
    );

    await user.click(getByRole('spinbutton', { name: 'Day' }));
    await user.keyboard('22');

    expect(onChange).toHaveBeenCalled();
    const last = onChange.mock.lastCall?.[0] as Date;
    expect(last.getDate()).toBe(22);
    expect(queryByRole('alert')).toBeNull();
  });

  it('reports an already-backwards pair on render (From later than To)', async () => {
    const { findByTestId } = renderField(
      <DateRangeField
        label="From:"
        value={new Date(2026, 6, 28, 0, 0)}
        onChange={vi.fn()}
        open={false}
        onOpen={() => {}}
        onClose={() => {}}
        maxDateTime={new Date(2026, 6, 27, 23, 59)}
      />,
    );
    expect(await findByTestId('analytics-date-range-error')).toHaveTextContent('From must not be later than To.');
  });
});
