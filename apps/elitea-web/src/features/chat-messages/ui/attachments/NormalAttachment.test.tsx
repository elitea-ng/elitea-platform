/**
 * DEFECT: two `/* eslint-disable-next-line i18next/no-literal-string *​/` comments
 * sat in JSX children position (between `)}` and the next `<IconButtonAny>`),
 * not inside a `{…}` expression container. JSX treats such text as a child
 * node, so React rendered the raw comment source as visible text in the
 * attachment card's hover action row, twice, beside the download and remove
 * icons.
 *
 * EVIDENCE: a jsdom render produced the textContent
 * `report.pdf👁/* eslint-disable… *​/↓/* eslint-disable… *​/✕`.
 * Neither oxlint nor typecheck reports this class of defect, so the card's
 * text content is pinned here.
 */
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { Attachment } from '@/entities/attachment/model/types';

import { NormalAttachment } from './NormalAttachment';

const ATTACHMENT = { name: 'report.pdf' } as unknown as Attachment;

describe('NormalAttachment', () => {
  it('renders only the file name and the icon glyphs, with no source comments', () => {
    render(<NormalAttachment attachment={ATTACHMENT} preview />);

    const card = screen.getByTestId('chat-artifact-file-card');
    expect(card.textContent).not.toContain('eslint-disable');
    expect(card.textContent).toBe('report.pdf👁↓✕');
  });
});

/**
 * "Open in canvas" (issue #878) — offered only for a TEXT-LIKE attachment
 * that actually resolves to an artifact-storage bucket/key pair; a legacy
 * base64/File attachment (no `item_details.filepath`) has neither, so the
 * control must not appear even when the caller supplies the callback.
 */
describe('NormalAttachment: "Open in canvas"', () => {
  const STORAGE_BACKED = {
    item_details: { filepath: '/docs/notes.md', bucket: 'docs', name: 'notes.md', attachment_type: 'document' },
  } as unknown as Attachment;
  const IMAGE_BACKED = {
    item_details: { filepath: '/docs/photo.png', bucket: 'docs', name: 'photo.png', attachment_type: 'document' },
  } as unknown as Attachment;
  const LEGACY_BASE64 = { name: 'inline.txt' } as unknown as Attachment;

  it('opens the (bucket, name) it resolves from item_details.filepath', () => {
    const onOpenFileInCanvas = vi.fn();
    render(<NormalAttachment attachment={STORAGE_BACKED} onOpenFileInCanvas={onOpenFileInCanvas} />);

    const button = screen.getByTestId('attachment-open-in-canvas');
    fireEvent.click(button);
    expect(onOpenFileInCanvas).toHaveBeenCalledWith({ bucket: 'docs', name: 'notes.md' });
  });

  it('offers no control for a kind canvas cannot open (an image)', () => {
    render(<NormalAttachment attachment={IMAGE_BACKED} onOpenFileInCanvas={vi.fn()} />);
    expect(screen.queryByTestId('attachment-open-in-canvas')).not.toBeInTheDocument();
  });

  it('offers no control for a legacy base64 attachment (no bucket/key at all)', () => {
    render(<NormalAttachment attachment={LEGACY_BASE64} onOpenFileInCanvas={vi.fn()} />);
    expect(screen.queryByTestId('attachment-open-in-canvas')).not.toBeInTheDocument();
  });

  it('offers no control when the caller supplies no callback, even for an eligible file', () => {
    render(<NormalAttachment attachment={STORAGE_BACKED} />);
    expect(screen.queryByTestId('attachment-open-in-canvas')).not.toBeInTheDocument();
  });
});
