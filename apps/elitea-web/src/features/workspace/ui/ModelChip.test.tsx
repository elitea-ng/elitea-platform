import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ModelChip, modelChipLabel, type ModelOption } from './ModelChip';

const MODELS: ModelOption[] = [
  { name: 'gpt-5', description: 'Reasons', supportsReasoning: true },
  { name: 'gpt-4o-mini', supportsReasoning: false },
];

describe('ModelChip', () => {
  it('names the current model and its effort, or the agent’s before it is known', () => {
    expect(modelChipLabel('gpt-5', 'high')).toBe('gpt-5 · High');
    expect(modelChipLabel('gpt-5', undefined)).toBe('gpt-5');
    expect(modelChipLabel(null, undefined)).toBe('Agent’s model');
  });

  it('offers the project’s chat models and hands back the one picked', async () => {
    const onChoose = vi.fn();
    renderWithTheme(<ModelChip current="gpt-5" models={MODELS} onChoose={onChoose} />);
    await userEvent.click(screen.getByTestId('model-chip'));
    const menu = screen.getByRole('menu');
    expect(within(menu).getByRole('menuitem', { name: /gpt-4o-mini/ })).toBeInTheDocument();
    await userEvent.click(within(menu).getByRole('menuitem', { name: /gpt-4o-mini/ }));
    expect(onChoose).toHaveBeenCalledWith({ name: 'gpt-4o-mini' });
  });

  it('offers a reasoning effort only for a model that supports reasoning', async () => {
    const onChoose = vi.fn();
    const { unmount } = renderWithTheme(<ModelChip current="gpt-5" effort="low" models={MODELS} onChoose={onChoose} />);
    await userEvent.click(screen.getByTestId('model-chip'));
    expect(screen.getByText('Reasoning effort')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('model-effort-high'));
    expect(onChoose).toHaveBeenCalledWith({ name: 'gpt-5', reasoning_effort: 'high' });
    unmount();

    renderWithTheme(<ModelChip current="gpt-4o-mini" models={MODELS} onChoose={vi.fn()} />);
    await userEvent.click(screen.getByTestId('model-chip'));
    expect(screen.queryByText('Reasoning effort')).toBeNull();
    expect(screen.queryByTestId('model-effort-high')).toBeNull();
  });

  it('says so when the project lists no models', async () => {
    renderWithTheme(<ModelChip current={null} models={[]} onChoose={vi.fn()} />);
    await userEvent.click(screen.getByTestId('model-chip'));
    expect(screen.getByRole('menuitem', { name: 'No other models in this project' })).toHaveAttribute('aria-disabled', 'true');
  });
});
