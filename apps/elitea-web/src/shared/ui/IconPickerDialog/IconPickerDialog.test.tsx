import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IconPickerDialog } from './IconPickerDialog';

function renderPicker(overrides: Partial<React.ComponentProps<typeof IconPickerDialog>> = {}) {
  const onUpload = vi.fn().mockResolvedValue(undefined);
  renderWithTheme(
    <IconPickerDialog
      open
      onClose={vi.fn()}
      placeholderName="Skill"
      defaultIcons={[]}
      uploadedIcons={[]}
      onSelectIcon={vi.fn()}
      onUpload={onUpload}
      onDeleteIcon={vi.fn()}
      acceptedExtensions={['.png', '.jpg']}
      maxFileBytes={1024}
      {...overrides}
    />,
  );
  return { onUpload };
}

// applyAccept: false, because the file input's `accept` would make user-event
// drop the wrong format before the component's own check ever runs.
const user = () => userEvent.setup({ applyAccept: false });

describe('IconPickerDialog upload validation (#2107)', () => {
  it('refuses an unsupported format with a message and does not upload', async () => {
    const { onUpload } = renderPicker();
    await user().upload(screen.getByTestId('icon-picker-file-input'), new File(['x'], 'icon.tiff', { type: 'image/tiff' }));
    expect(await screen.findByTestId('icon-picker-upload-error')).toHaveTextContent('Unsupported file format');
    expect(onUpload).not.toHaveBeenCalled();
  });

  it('refuses an oversized file with a message and does not upload', async () => {
    const { onUpload } = renderPicker();
    const big = new File([new Uint8Array(2048)], 'icon.png', { type: 'image/png' });
    await user().upload(screen.getByTestId('icon-picker-file-input'), big);
    expect(await screen.findByTestId('icon-picker-upload-error')).toHaveTextContent('File size exceeds 1 KB');
    expect(onUpload).not.toHaveBeenCalled();
  });

  it('shows the server refusal instead of swallowing it', async () => {
    const refusal = new EliteaApiError({
      kind: 'http',
      status: 400,
      url: '/upload',
      body: { error: 'Unsupported file format. Upload a PNG image.' },
    } as never);
    const onUpload = vi.fn().mockRejectedValue(refusal);
    renderPicker({ onUpload, acceptedExtensions: ['.png'], maxFileBytes: undefined });
    // jsdom cannot decode an image; a TIFF-typed file skips the decode step.
    await user().upload(screen.getByTestId('icon-picker-file-input'), new File(['x'], 'icon.png', { type: 'image/tiff' }));
    await waitFor(() =>
      expect(screen.getByTestId('icon-picker-upload-error')).toHaveTextContent('Unsupported file format. Upload a PNG image.'),
    );
  });

  it('says a non-HTTP failure is a connection problem, not a refusal', async () => {
    const onUpload = vi.fn().mockRejectedValue(new TypeError('Failed to fetch'));
    renderPicker({ onUpload, acceptedExtensions: ['.png'], maxFileBytes: undefined });
    await user().upload(screen.getByTestId('icon-picker-file-input'), new File(['x'], 'icon.png', { type: 'image/tiff' }));
    await waitFor(() => expect(screen.getByTestId('icon-picker-upload-error')).toHaveTextContent('Check your connection'));
  });
});
