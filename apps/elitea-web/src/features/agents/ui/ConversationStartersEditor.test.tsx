import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { MAX_CONVERSATION_STARTERS, MAX_CONVERSATION_STARTER_LENGTH } from '@/shared/lib/limits';

import { renderWithProviders } from '../__tests__/testUtils';

import { ConversationStartersEditor } from './ConversationStartersEditor';

/**
 * Every assertion here is on what the component HANDED BACK, not on what it
 * rendered: #307 exists because a control that renders and routes nothing
 * passed for a working field for months.
 */
describe('ConversationStartersEditor', () => {
  it('renders one input per starter, populated', () => {
    renderWithProviders(
      <ConversationStartersEditor
        starters={['first', 'second']}
        onStartersChange={vi.fn()}
      />,
    );

    const inputs = screen.getAllByTestId('agent-conversation-starter-input');
    expect(inputs).toHaveLength(2);
    expect(inputs[0]).toBeVisible();
    expect(inputs[0]).toHaveValue('first');
    expect(inputs[1]).toHaveValue('second');
  });

  it('emits the whole edited list when a row is typed into, leaving the other rows alone', async () => {
    const user = userEvent.setup();
    const onStartersChange = vi.fn();
    renderWithProviders(
      <ConversationStartersEditor
        starters={['keep', 'edit']}
        onStartersChange={onStartersChange}
      />,
    );

    await user.type(screen.getAllByTestId('agent-conversation-starter-input')[1]!, 'X');

    expect(onStartersChange).toHaveBeenCalledWith(['keep', 'editX']);
  });

  it('appends an empty row on add', async () => {
    const user = userEvent.setup();
    const onStartersChange = vi.fn();
    renderWithProviders(
      <ConversationStartersEditor
        starters={['only']}
        onStartersChange={onStartersChange}
      />,
    );

    await user.click(screen.getByTestId('agent-conversation-starter-add'));

    expect(onStartersChange).toHaveBeenCalledWith(['only', '']);
  });

  it('removes exactly the deleted row', async () => {
    const user = userEvent.setup();
    const onStartersChange = vi.fn();
    renderWithProviders(
      <ConversationStartersEditor
        starters={['a', 'b', 'c']}
        onStartersChange={onStartersChange}
      />,
    );

    await user.click(screen.getAllByTestId('agent-conversation-starter-delete')[1]!);

    expect(onStartersChange).toHaveBeenCalledWith(['a', 'c']);
  });

  it('emits an empty list when the last row is deleted, rather than nothing', async () => {
    const user = userEvent.setup();
    const onStartersChange = vi.fn();
    renderWithProviders(
      <ConversationStartersEditor
        starters={['solo']}
        onStartersChange={onStartersChange}
      />,
    );

    await user.click(screen.getByTestId('agent-conversation-starter-delete'));

    expect(onStartersChange).toHaveBeenCalledWith([]);
  });

  it('stops adding at the baseline cap and emits nothing on a click at the cap', async () => {
    // `pointerEventsCheck: 0` — a genuinely disabled MUI button carries
    // `pointer-events: none`, which user-event refuses to click by default.
    // Bypassing that is the point: it proves the handler is inert, not just
    // that the pointer could not reach it.
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onStartersChange = vi.fn();
    const atCap = Array.from({ length: MAX_CONVERSATION_STARTERS }, (_, index) => `starter ${index}`);
    renderWithProviders(
      <ConversationStartersEditor
        starters={atCap}
        onStartersChange={onStartersChange}
      />,
    );

    const add = screen.getByTestId('agent-conversation-starter-add');
    expect(add).toBeDisabled();
    await user.click(add);
    expect(onStartersChange).not.toHaveBeenCalled();
  });

  it('caps each row at the baseline per-starter length', () => {
    renderWithProviders(
      <ConversationStartersEditor
        starters={['x']}
        onStartersChange={vi.fn()}
      />,
    );

    expect(screen.getByTestId('agent-conversation-starter-input')).toHaveAttribute(
      'maxlength',
      String(MAX_CONVERSATION_STARTER_LENGTH),
    );
  });

  // Baseline gate (`ConversationStarters.jsx:132,152`): `disabled` HIDES the
  // add/delete controls. Dropping that would leave the read-only
  // public-agent view of this panel offering structural edits it cannot save.
  it('hides the add and delete controls when disabled, and disables the inputs', () => {
    renderWithProviders(
      <ConversationStartersEditor
        starters={['locked']}
        onStartersChange={vi.fn()}
        disabled
      />,
    );

    expect(screen.getByTestId('agent-conversation-starter-input')).toBeDisabled();
    expect(screen.queryByTestId('agent-conversation-starter-add')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-conversation-starter-delete')).not.toBeInTheDocument();
  });

  it('renders the section with no rows and still offers add when the list is empty', () => {
    renderWithProviders(
      <ConversationStartersEditor
        starters={[]}
        onStartersChange={vi.fn()}
      />,
    );

    expect(screen.queryAllByTestId('agent-conversation-starter-input')).toHaveLength(0);
    expect(screen.getByTestId('agent-conversation-starter-add')).toBeEnabled();
  });

  // #848 — this row's own counter used to be conditionally MOUNTED
  // (`isFocused && value.length > 0 && <Typography>`), so blurring the row
  // unmounted it and shrank the column, moving the next row (or, on the
  // last row, `+ Starter`) up under the pointer mid-click. The counter's
  // line must stay reserved across focus/blur.
  it('keeps a row counter mounted (same node) across focus and blur, only toggling visibility', () => {
    renderWithProviders(
      <ConversationStartersEditor
        starters={['first']}
        onStartersChange={vi.fn()}
      />,
    );

    const input = screen.getByTestId('agent-conversation-starter-input');
    const counterAtRest = screen.getByTestId('agent-conversation-starter-counter');
    expect(getComputedStyle(counterAtRest).visibility).toBe('hidden');

    fireEvent.focus(input);
    const counterFocused = screen.getByTestId('agent-conversation-starter-counter');
    expect(counterFocused).toBe(counterAtRest);
    expect(getComputedStyle(counterFocused).visibility).toBe('visible');

    fireEvent.blur(input);
    const counterBlurred = screen.getByTestId('agent-conversation-starter-counter');
    expect(counterBlurred).toBe(counterAtRest);
    expect(getComputedStyle(counterBlurred).visibility).toBe('hidden');

    expect(screen.getByTestId('agent-conversation-starter-add')).toBeInTheDocument();
  });
});
