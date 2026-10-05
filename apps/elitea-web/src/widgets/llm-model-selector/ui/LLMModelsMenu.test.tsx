/**
 * The model menu row: name, then the admin's description as a second line
 * (legacy issues 6766 and 6727). A row with no description keeps one line,
 * and the list is a listbox whose options say which one is selected.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { LLMModel } from '../lib/types';
import LLMModelsMenu from './LLMModelsMenu';
import LLMModelSelector from './LLMModelSelector';

const DESCRIBED: LLMModel = { id: '1', name: 'gpt-5', display_name: 'GPT-5', description: 'Best for coding and agents' };
const PLAIN: LLMModel = { id: '2', name: 'mini', display_name: 'Mini' };
const FORTY: LLMModel = { id: '3', name: 'long', display_name: 'Long', description: 'x'.repeat(40) };

function renderMenu(onClose = vi.fn(), selected: LLMModel | null = DESCRIBED) {
  const anchor = document.createElement('button');
  document.body.appendChild(anchor);
  renderWithTheme(
    <LLMModelsMenu
      anchorEl={anchor}
      onClose={onClose}
      models={[DESCRIBED, PLAIN, FORTY]}
      selectedModel={selected}
      onSelectModel={vi.fn()}
      labelledBy="trigger-id"
    />,
  );
  return onClose;
}

describe('LLMModelsMenu', () => {
  it('shows the description as a second line only for a model that has one', () => {
    renderMenu();
    const listbox = screen.getByRole('listbox');
    expect(listbox).toHaveAttribute('aria-labelledby', 'trigger-id');

    const described = within(listbox).getByRole('option', { name: /GPT-5/ });
    expect(within(described).getByTestId('model-option-description')).toHaveTextContent('Best for coding and agents');

    const plain = within(listbox).getByRole('option', { name: /Mini/ });
    expect(within(plain).queryByTestId('model-option-description')).toBeNull();

    // A 40-character description is rendered whole; the cut-off is CSS only.
    const long = within(listbox).getByRole('option', { name: /Long/ });
    expect(within(long).getByTestId('model-option-description').textContent).toBe('x'.repeat(40));
  });

  it('marks the selected option with aria-selected', () => {
    renderMenu(vi.fn(), PLAIN);
    expect(screen.getByRole('option', { name: /Mini/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('option', { name: /GPT-5/ })).toHaveAttribute('aria-selected', 'false');
  });

  it('closes on Escape', async () => {
    const onClose = renderMenu();
    await userEvent.setup().keyboard('{Escape}');
    expect(onClose).toHaveBeenCalled();
  });
});

describe('LLMModelSelector trigger', () => {
  it('announces a listbox and shows only the model name, not its description', async () => {
    renderWithTheme(
      <LLMModelSelector
        selectedModel={DESCRIBED}
        models={[DESCRIBED, PLAIN]}
        onSelectModel={vi.fn()}
        showSettingsEntry={false}
      />,
    );
    const trigger = screen.getByTestId('model-selector-name');
    expect(trigger).toHaveAttribute('aria-haspopup', 'listbox');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(trigger).toHaveTextContent('GPT-5');
    expect(trigger).not.toHaveTextContent('Best for coding and agents');

    await userEvent.setup().click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('listbox')).toHaveAttribute('aria-labelledby', trigger.id);
  });
});
