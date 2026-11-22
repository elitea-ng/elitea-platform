import { cleanup, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { renderExtensionCard, splitNodeYaml, authoredExtensionYaml } from '../../../__tests__/graphExtensionTestUtils';
import { ExtensionChoice, ExtensionField } from './ExtensionFields';
import { ExtensionLimits } from './ExtensionLimits';
import { StateChannelSelect } from './StateChannelSelect';

afterEach(cleanup);

describe('ExtensionChoice (design-system SingleSelect & FU-286)', () => {
  it('renders None instead of a raw i18n key when an empty choice is present (FU-286)', async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const screen = renderWithTheme(
      <ExtensionChoice
        label="Owned worker"
        value=""
        choices={['', 'worker', 'drafter']}
        disabled={false}
        change={onChange}
        help="Pick a worker node."
      />,
    );

    const combobox = screen.getByRole('combobox', { name: 'Owned worker' });
    expect(combobox).toHaveTextContent('None');
    expect(screen.queryByText(/pipelines\.graphExtensions\.values\./u)).toBeNull();
    expect(screen.getByText('Pick a worker node.')).toBeInTheDocument();
    expect(combobox.closest('.nodrag.nopan.nowheel')).not.toBeNull();

    await user.click(combobox);
    const listbox = await screen.findByRole('listbox');
    const optionTexts = within(listbox).getAllByRole('option').map((option) => option.textContent);
    expect(optionTexts).toEqual(['None', 'worker', 'drafter']);
    expect(optionTexts).not.toContain('pipelines.graphExtensions.values.');

    await user.click(within(listbox).getByRole('option', { name: 'drafter' }));
    expect(onChange).toHaveBeenCalledWith('drafter');
  });

  it('allows clearing a selected value back to empty string via the None option', async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const screen = renderWithTheme(
      <ExtensionChoice
        label="Join transition"
        value="END"
        choices={['', 'END', 'next_node']}
        disabled={false}
        change={onChange}
      />,
    );

    const combobox = screen.getByRole('combobox', { name: 'Join transition' });
    expect(combobox).toHaveTextContent('END');
    await user.click(combobox);
    const listbox = await screen.findByRole('listbox');
    await user.click(within(listbox).getByRole('option', { name: 'None' }));
    expect(onChange).toHaveBeenCalledWith('');
  });

  it('keeps an unknown authored value visible and marks combobox aria-disabled when disabled', () => {
    const screen = renderWithTheme(
      <ExtensionChoice
        label="Reduction"
        value="authored_unknown"
        choices={['ordered_collection']}
        disabled
        change={vi.fn()}
      />,
    );

    const combobox = screen.getByRole('combobox', { name: 'Reduction' });
    expect(combobox).toHaveTextContent('authored_unknown');
    expect(combobox).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('ExtensionField (design-system InputBase)', () => {
  it('renders single-line, numeric, and multiline fields with drag guards and helper text', async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const screen = renderWithTheme(
      <>
        <ExtensionField label="Child item channel" value="entity" disabled={false} change={onChange} help="Opaque item" />
        <ExtensionField label="Maximum items" value="8" disabled={false} number change={onChange} />
        <ExtensionField label="Worker task value" value="hello" disabled multiline change={onChange} />
      </>,
    );

    const textInput = screen.getByLabelText('Child item channel');
    expect(textInput).toHaveValue('entity');
    expect(textInput.closest('.nodrag.nopan.nowheel')).not.toBeNull();
    expect(screen.getByText('Opaque item')).toBeInTheDocument();

    const numberInput = screen.getByLabelText('Maximum items');
    expect(numberInput).toHaveAttribute('type', 'number');
    expect(numberInput).toHaveValue(8);

    const multilineInput = screen.getByLabelText('Worker task value');
    expect(multilineInput.tagName).toBe('TEXTAREA');
    expect(multilineInput).toBeDisabled();

    await user.type(textInput, 'x');
    expect(onChange).toHaveBeenCalledWith('entityx');
  });
});

describe('StateChannelSelect (design-system SingleSelect)', () => {
  it('renders declared matching state channels via SingleSelect with empty placeholder and preview help', async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    const Wrapper = () => (
      <StateChannelSelect
        label="Source state variable"
        value=""
        types={['list']}
        disabled={false}
        change={onChange}
      />
    );
    const screen = renderExtensionCard(Wrapper, authoredExtensionYaml(splitNodeYaml));

    const combobox = await screen.findByRole('combobox', { name: 'Source state variable' });
    expect(combobox).toHaveTextContent('Select a state variable');
    expect(combobox.closest('.nodrag.nopan.nowheel')).not.toBeNull();

    await user.click(combobox);
    const listbox = await screen.findByRole('listbox');
    const optionTexts = within(listbox).getAllByRole('option').map((option) => option.textContent);
    expect(optionTexts).toContain('Select a state variable');
    expect(optionTexts).toContain('source_rows');
    expect(optionTexts).not.toContain('source_object');

    await user.click(within(listbox).getByRole('option', { name: 'source_rows' }));
    expect(onChange).toHaveBeenCalledWith('source_rows');
  });
});

describe('ExtensionLimits (design-system BasicAccordion)', () => {
  it('expands Processing limits and updates numeric limit fields', async () => {
    const change = vi.fn();
    const remove = vi.fn();
    const user = userEvent.setup();
    const screen = renderWithTheme(
      <ExtensionLimits
        node={{ id: 'extension', type: 'split_out' }}
        disabled={false}
        change={change}
        remove={remove}
      />,
    );

    const summary = screen.getByRole('button', { name: 'Processing limits' });
    expect(summary).toHaveAttribute('aria-expanded', 'false');
    await user.click(summary);
    expect(summary).toHaveAttribute('aria-expanded', 'true');
    const valuesInput = screen.getByLabelText('values');
    await user.type(valuesInput, '5');
    expect(change).toHaveBeenCalledWith('limits', { values: 5 });
  });
});
