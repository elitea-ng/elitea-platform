import { waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { StyledInputEnhancer } from '.';

describe('StyledInputEnhancer', () => {
  it('renders the inline field and no modal by default', () => {
    const { getByLabelText, queryByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="hello"
        onChange={() => {}}
      />,
    );
    expect(getByLabelText('Prompt')).toBeInTheDocument();
    expect(queryByRole('dialog')).toBeNull();
  });

  it('opens the full-screen modal when the toolbar full-screen action is clicked', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="hello"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    expect(getByRole('dialog')).toBeInTheDocument();
  });

  it('titles the modal from a string label by default', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="hello"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    const dialog = getByRole('dialog');
    expect(within(dialog).getByText('Prompt')).toBeInTheDocument();
  });

  it('falls back to a generic modal title when there is no string label', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        value="hello"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    const dialog = getByRole('dialog');
    expect(within(dialog).getByText('Edit content')).toBeInTheDocument();
  });

  it('prefers an explicit fullScreenTitle over the label', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        fullScreenTitle="Custom title"
        value="hello"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    const dialog = getByRole('dialog');
    expect(within(dialog).getByText('Custom title')).toBeInTheDocument();
  });

  it('shows the same value inside the modal editor', async () => {
    const user = userEvent.setup();
    const { getByRole, getAllByDisplayValue } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="shared value"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    // Both the inline field and the modal editor render the same value.
    expect(getAllByDisplayValue('shared value')).toHaveLength(2);
  });

  it('fills the modal instead of pinning the editor to a fixed row count (#2010)', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="shared value"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    const editor = within(getByRole('dialog')).getByRole('textbox');
    // A fixed `rows`/`minRows` height stopped the text partway down and drew a rule across the modal.
    expect(editor).toHaveStyle({ height: '100%' });
    expect(editor).not.toHaveAttribute('rows', '15');
    // A plain <textarea>, not TextareaAutosize (which adds a hidden shadow textarea and overwrites the height inline).
    expect(getByRole('dialog').querySelectorAll('textarea')).toHaveLength(1);
  });

  it('closes the modal via the close button', async () => {
    const user = userEvent.setup();
    const { getByRole, queryByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value="hello"
        onChange={() => {}}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    expect(getByRole('dialog')).toBeInTheDocument();
    await user.click(getByRole('button', { name: 'Close' }));
    // MUI's Dialog exit transition keeps the element mounted briefly after
    // `open` flips false.
    await waitFor(() => {
      expect(queryByRole('dialog')).toBeNull();
    });
  });

  it('propagates edits made in the modal editor via onChange', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <StyledInputEnhancer
        label="Prompt"
        value=""
        onChange={onChange}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    const dialog = getByRole('dialog');
    await user.type(within(dialog).getByLabelText('Prompt'), 'x');
    expect(onChange).toHaveBeenCalled();
  });
});
