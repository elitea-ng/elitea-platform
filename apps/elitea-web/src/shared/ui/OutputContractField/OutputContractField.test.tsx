import { useState } from 'react';

import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { OutputContract } from '@/shared/api/generated/model';

import { renderWithTheme } from '../lib/testTheme';
import { OutputContractField } from '.';

/** Holds the value the way a page does, so the field sees its own writes come back. */
function Harness({ initial, onChange }: { readonly initial: OutputContract | null; readonly onChange: (next: OutputContract | null) => void }) {
  const [value, setValue] = useState<OutputContract | null>(initial);
  return (
    <OutputContractField
      value={value}
      onChange={(next) => {
        setValue(next);
        onChange(next);
      }}
    />
  );
}

async function chooseType(user: ReturnType<typeof userEvent.setup>, label: string) {
  await user.click(screen.getAllByRole('combobox')[0] as HTMLElement);
  await user.click(screen.getByRole('option', { name: label }));
}

describe('OutputContractField', () => {
  it('shows None and no input when there is no contract', () => {
    renderWithTheme(<Harness initial={null} onChange={vi.fn()} />);
    expect(screen.getByText('None')).toBeInTheDocument();
    expect(screen.queryByTestId('output-contract-schema')).not.toBeInTheDocument();
    expect(screen.queryByTestId('output-contract-pattern')).not.toBeInTheDocument();
  });

  it('writes a regex contract', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithTheme(<Harness initial={null} onChange={onChange} />);

    await chooseType(user, 'Regular expression');
    fireEvent.change(screen.getByTestId('output-contract-pattern'), { target: { value: '^[A-Z]+$' } });

    expect(onChange).toHaveBeenLastCalledWith({ type: 'regex', pattern: '^[A-Z]+$' });
  });

  it('starts a JSON Schema with an object schema and sends it', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithTheme(<Harness initial={null} onChange={onChange} />);

    await chooseType(user, 'JSON Schema');

    expect(onChange).toHaveBeenLastCalledWith({ type: 'json_schema', schema: { type: 'object' } });
  });

  it('shows an inline error and sends nothing while the JSON does not parse', () => {
    const onChange = vi.fn();
    renderWithTheme(<Harness initial={{ type: 'json_schema', schema: { type: 'object' } }} onChange={onChange} />);

    fireEvent.change(screen.getByTestId('output-contract-schema'), { target: { value: '{"type": ' } });

    expect(screen.getByText(/not valid JSON/)).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
    // The person's text stays; a re-seed would have put the stored schema back.
    expect(screen.getByTestId('output-contract-schema')).toHaveValue('{"type": ');

    fireEvent.change(screen.getByTestId('output-contract-schema'), { target: { value: '{"type": "string"}' } });
    expect(onChange).toHaveBeenLastCalledWith({ type: 'json_schema', schema: { type: 'string' } });
    expect(screen.queryByText(/not valid JSON/)).not.toBeInTheDocument();
  });

  it('writes a format contract', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithTheme(<Harness initial={{ type: 'format', format: 'json' }} onChange={onChange} />);

    await user.click(screen.getAllByRole('combobox')[1] as HTMLElement);
    await user.click(screen.getByRole('option', { name: 'CSV' }));

    expect(onChange).toHaveBeenLastCalledWith({ type: 'format', format: 'csv' });
  });

  it('writes null when the type goes back to None', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithTheme(<Harness initial={{ type: 'regex', pattern: 'a' }} onChange={onChange} />);

    await chooseType(user, 'None');

    expect(onChange).toHaveBeenLastCalledWith(null);
  });

  it('re-seeds when the value changes from outside', () => {
    const { rerender } = renderWithTheme(
      <OutputContractField value={{ type: 'regex', pattern: 'a' }} onChange={vi.fn()} />,
    );
    expect(screen.getByTestId('output-contract-pattern')).toHaveValue('a');

    rerender(<OutputContractField value={{ type: 'regex', pattern: 'b' }} onChange={vi.fn()} />);

    expect(screen.getByTestId('output-contract-pattern')).toHaveValue('b');
  });
});
