import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ImageStrip } from './ImageStrip';

const IMAGES = [
  { name: 'shot.png', thumbnail: 'data:image/jpeg;base64,AAAA' },
  { name: 'diagram.webp', thumbnail: null },
];

describe('ImageStrip', () => {
  it('shows each thumbnail, and the name of an image the host could not preview', () => {
    renderWithTheme(<ImageStrip images={IMAGES} />);
    expect(screen.getAllByTestId('image-thumb')).toHaveLength(2);
    expect(screen.getByAltText('shot.png')).toHaveAttribute('src', 'data:image/jpeg;base64,AAAA');
    expect(screen.getByText('diagram.webp')).toBeInTheDocument();
  });

  it('is read only without onRemove', () => {
    renderWithTheme(<ImageStrip images={IMAGES} />);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('removes the image whose button is pressed', () => {
    const onRemove = vi.fn();
    renderWithTheme(<ImageStrip images={IMAGES} onRemove={onRemove} />);
    fireEvent.click(screen.getByRole('button', { name: /diagram\.webp/ }));
    expect(onRemove).toHaveBeenCalledWith(1);
  });

  it('renders nothing for no images', () => {
    const { container } = renderWithTheme(<ImageStrip images={[]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
