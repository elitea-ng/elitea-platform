import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { DataTypeValueField } from './DataTypeValueField';

describe('DataTypeValueField', () => {
  it('renders a checkbox for a fixed boolean value', () => {
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="boolean"
        type="fixed"
        value={true}
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    expect(getByRole('checkbox')).toBeChecked();
  });

  it('reports boolean toggles via onBooleanChange', async () => {
    const user = userEvent.setup();
    const onBooleanChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="boolean"
        type="fixed"
        value={false}
        onInput={vi.fn()}
        onBooleanChange={onBooleanChange}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    await user.click(getByRole('checkbox'));
    expect(onBooleanChange).toHaveBeenCalledTimes(1);
  });

  it('renders a number spinbutton for an integer value and reports edits via onNumberInput', async () => {
    const user = userEvent.setup();
    const onNumberInput = vi.fn();
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="integer"
        type="fixed"
        value={1}
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={onNumberInput}
        stateVariableOptions={[]}
      />,
    );
    const input = getByRole('spinbutton');
    expect(input).toHaveValue(1);
    await user.type(input, '2');
    expect(onNumberInput).toHaveBeenCalled();
  });

  it('renders the JSON-stringified value for an object dataType', () => {
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="object"
        type="fixed"
        value={{ a: 1 }}
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    expect(getByRole('textbox')).toHaveValue('{"a":1}');
  });

  it('renders a plain text field for a fixed string value and reports edits via onInput', async () => {
    const user = userEvent.setup();
    const onInput = vi.fn();
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="string"
        type="fixed"
        value="hi"
        onInput={onInput}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    await user.type(getByRole('textbox'), '!');
    expect(onInput).toHaveBeenCalled();
  });

  it('shows the f-string autocomplete popper when the value contains an open "{" and options are available', async () => {
    const user = userEvent.setup();
    const { getByRole, findByText } = renderWithTheme(
      <DataTypeValueField
        dataType="string"
        type="fstring"
        value="hello {"
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[{ value: 'input', label: 'input' }]}
      />,
    );
    const input = getByRole('textbox');
    if (!(input instanceof HTMLInputElement)) throw new Error('expected a real <input> element');
    await user.click(input);
    input.setSelectionRange(input.value.length, input.value.length);
    await user.keyboard(' ');
    await user.keyboard('{Backspace}');
    expect(await findByText('input')).toBeInTheDocument();
  });

  it('respects the disabled prop for a fixed string value', () => {
    const { getByRole } = renderWithTheme(
      <DataTypeValueField
        dataType="string"
        type="fixed"
        value="hi"
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
        disabled
      />,
    );
    expect(getByRole('textbox')).toBeDisabled();
  });

  /*
   * elitea_issues: #1930 — REPRODUCED, cosmetic-only (the reporter's own
   * words: "it is only a visual issue, the pipeline works as expected, and
   * the input is treated as a string"). A fixed 'string' value and a fixed
   * 'integer' value render through the SAME plain-text control
   * (`getDisplayValue`/`TextInputField`) with no quote marks or other visual
   * identifier — `123` and "123" are indistinguishable at a glance, and the
   * only disclosure of the field's actual data type is a tooltip
   * (`dataTypeExpectedTooltip`) that requires a hover, not something visible
   * at rest. Fixing this (e.g. a quote-mark `InputAdornment` for string-typed
   * fixed values) needs a rebuild+visual check this pass' remaining budget
   * does not have room for; test.fail pins the gap instead of silently
   * passing on it.
   */
  it.fails('issue 1930: a fixed string value is visually distinguishable at rest from a fixed integer value (e.g. quoted)', () => {
    const { getByRole: getStringRole } = renderWithTheme(
      <DataTypeValueField
        dataType="string"
        type="fixed"
        value="123"
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    const stringField = getStringRole('textbox') as HTMLInputElement;

    const { getByRole: getIntegerRole } = renderWithTheme(
      <DataTypeValueField
        dataType="integer"
        type="fixed"
        value={123}
        onInput={vi.fn()}
        onBooleanChange={vi.fn()}
        onNumberInput={vi.fn()}
        stateVariableOptions={[]}
      />,
    );
    const integerField = getIntegerRole('spinbutton') as HTMLInputElement;

    // Wished-for behaviour: the DOM text a sighted user reads at rest differs
    // between the two (e.g. `"123"` vs `123`). Today it does not.
    expect(stringField.value).not.toBe(integerField.value);
  });
});
