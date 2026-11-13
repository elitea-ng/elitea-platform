import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { ArtifactListItem } from '../model/types';
import { RenameArtifactDialog } from './RenameArtifactDialog';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const file: ArtifactListItem = { id: 'a/old.txt', key: 'a/old.txt', name: 'old.txt', kind: 'file', size: 1 };

function renderDialog(props: Partial<Parameters<typeof RenameArtifactDialog>[0]> = {}) {
  const onSubmit = vi.fn();
  const onClose = vi.fn();
  render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <RenameArtifactDialog item={file} pending={false} onClose={onClose} onSubmit={onSubmit} {...props} />
    </ThemeProvider>,
  );
  return { onSubmit, onClose };
}

/* #2015 — files in Artifacts had no Rename action */
describe('RenameArtifactDialog', () => {
  it('starts on the current name, keeps Rename disabled until it changes, then submits the new name', async () => {
    const user = userEvent.setup();
    const { onSubmit } = renderDialog();
    const field = screen.getByRole('textbox', { name: 'File name' });
    expect(field).toHaveValue('old.txt');
    expect(screen.getByRole('button', { name: 'Rename' })).toBeDisabled();
    await user.clear(field);
    await user.type(field, 'new.txt');
    await user.click(screen.getByRole('button', { name: 'Rename' }));
    expect(onSubmit).toHaveBeenCalledWith('new.txt');
  });

  it('refuses a name with a forbidden character', async () => {
    const user = userEvent.setup();
    const { onSubmit } = renderDialog();
    const field = screen.getByRole('textbox', { name: 'File name' });
    await user.clear(field);
    await user.type(field, 'a|b.txt');
    expect(screen.getByText('Filenames cannot contain "|".')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Rename' })).toBeDisabled();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("shows the server's refusal under the field", () => {
    renderDialog({ error: 'A file with that name already exists.' });
    expect(screen.getByText('A file with that name already exists.')).toBeInTheDocument();
  });
});
