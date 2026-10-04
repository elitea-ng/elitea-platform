import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { remToPx, renderWithTheme } from '@/shared/ui/lib/testTheme';

import { BucketStorageSelector } from './BucketStorageSelector';

const configurations = [
  { id: 'a', title: 'Team S3', shared: true },
  { id: 'b', title: 'Own S3', shared: false },
] as const;

describe('BucketStorageSelector (#6638)', () => {
  it('labels the control "Storage:" above the selected storage, like the project selector', () => {
    renderWithTheme(
      <BucketStorageSelector
        configurations={configurations}
        selected="b"
        onChange={vi.fn()}
      />,
    );
    const trigger = screen.getByRole('button', { name: 'Storage integration' });
    expect(screen.getByTestId('bucket-storage-caption')).toHaveTextContent('Storage:');
    expect(trigger).toHaveTextContent('Storage:Own S3');
    expect(trigger).toHaveStyle({ minHeight: remToPx('3.25rem') });
  });

  it('keeps the caption with the fallback label and still opens the menu', () => {
    const onChange = vi.fn();
    renderWithTheme(
      <BucketStorageSelector
        configurations={configurations}
        onChange={onChange}
      />,
    );
    const trigger = screen.getByRole('button', { name: 'Storage integration' });
    expect(trigger).toHaveTextContent('Storage:Select Storage');
    fireEvent.click(trigger);
    fireEvent.click(screen.getByText('Team S3'));
    expect(onChange).toHaveBeenCalledWith('a');
  });
});
