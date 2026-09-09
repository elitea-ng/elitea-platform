/**
 * `DocsLink` (the MDX `a` override) — asserts the rendered `href`/`target`/
 * `rel` for each case its header documents, plus `DocsImage`'s equivalent
 * for `src`, and `Card`'s own copy of the same rule (it renders its `<a>`
 * outside MDX — see `Card.tsx`'s header). Uses the module's own
 * `import.meta.env.BASE_URL` rather than a hardcoded `/docs/…`, matching
 * `router.test.ts` (under `vitest.config.ts` BASE_URL is `/`, not the
 * `docs`-mode value).
 */
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { Card } from './Card';
import { DocsImage } from './DocsImage';
import { DocsLink } from './DocsLink';

const BASE = import.meta.env.BASE_URL;

describe('DocsLink', () => {
  it('prefixes a root-relative slug with BASE', () => {
    render(<DocsLink href="/menus/chat">Chat</DocsLink>);
    expect(screen.getByRole('link', { name: 'Chat' })).toHaveAttribute('href', `${BASE}menus/chat`);
  });

  it('prefixes a root-relative slug carrying a heading anchor', () => {
    render(<DocsLink href="/menus/chat#voice">Voice</DocsLink>);
    expect(screen.getByRole('link', { name: 'Voice' })).toHaveAttribute('href', `${BASE}menus/chat#voice`);
  });

  it('leaves a bare #anchor untouched, with no target/rel added', () => {
    render(<DocsLink href="#voice">Voice</DocsLink>);
    const link = screen.getByRole('link', { name: 'Voice' });
    expect(link).toHaveAttribute('href', '#voice');
    expect(link).not.toHaveAttribute('target');
  });

  it('renders an https:// link as external: untouched href, new tab, noopener', () => {
    render(<DocsLink href="https://github.com/elitea-ng/elitea-platform/issues/1">issue</DocsLink>);
    const link = screen.getByRole('link', { name: 'issue' });
    expect(link).toHaveAttribute('href', 'https://github.com/elitea-ng/elitea-platform/issues/1');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener');
  });

  it('renders a mailto: link as external too', () => {
    render(<DocsLink href="mailto:support@elitea.ai">support</DocsLink>);
    const link = screen.getByRole('link', { name: 'support' });
    expect(link).toHaveAttribute('href', 'mailto:support@elitea.ai');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener');
  });

  it('passes an href-less anchor (e.g. a rehype-slug heading anchor target) through unchanged', () => {
    render(<DocsLink id="voice">Voice</DocsLink>);
    const link = document.getElementById('voice');
    expect(link).not.toBeNull();
    expect(link).not.toHaveAttribute('href');
  });
});

describe('DocsImage', () => {
  it('prefixes a root-relative src with BASE', () => {
    render(<DocsImage src="/hand-written.png" alt="an image" />);
    expect(screen.getByAltText('an image')).toHaveAttribute('src', `${BASE}hand-written.png`);
  });

  it('leaves an external src untouched', () => {
    render(<DocsImage src="https://example.com/x.png" alt="an image" />);
    expect(screen.getByAltText('an image')).toHaveAttribute('src', 'https://example.com/x.png');
  });

  it('leaves a data: src untouched (not root-relative)', () => {
    render(<DocsImage src="data:image/png;base64,AA==" alt="an image" />);
    expect(screen.getByAltText('an image')).toHaveAttribute('src', 'data:image/png;base64,AA==');
  });
});

describe('Card href (bypasses the MDX `a` override entirely)', () => {
  it('prefixes an internal href with BASE', () => {
    render(<Card title="Chat" href="/menus/chat" />);
    expect(screen.getByRole('link', { name: 'Chat' })).toHaveAttribute('href', `${BASE}menus/chat`);
  });

  it('treats an external href the same way DocsLink does', () => {
    render(<Card title="Issue" href="https://github.com/elitea-ng/elitea-platform/issues/1" />);
    const link = screen.getByRole('link', { name: 'Issue' });
    expect(link).toHaveAttribute('href', 'https://github.com/elitea-ng/elitea-platform/issues/1');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener');
  });

  it('renders a plain tile (no link) when href is omitted', () => {
    render(<Card title="Plain" />);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByText('Plain')).toBeInTheDocument();
  });
});
