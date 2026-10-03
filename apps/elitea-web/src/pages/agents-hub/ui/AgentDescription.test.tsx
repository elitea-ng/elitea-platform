/**
 * #6861: the catalog cut long agent descriptions and welcome messages with
 * "..." and gave no way to read the rest. These tests fake the layout (jsdom
 * has none): a text whose `scrollHeight` is larger than its `clientHeight` is
 * cut by its clamp.
 */
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AgentDescription } from './AgentDescription';
import { AgentWelcomeMessage } from './AgentWelcomeMessage';

const CLAMPED_TEST_IDS = new Set(['agent-modal-description', 'agent-welcome-message-text']);
let cutText = true;

const scrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollHeight');
const clientHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight');

beforeEach(() => {
  cutText = true;
  Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
    configurable: true,
    get(this: HTMLElement): number {
      return cutText && CLAMPED_TEST_IDS.has(this.dataset['testid'] ?? '') ? 200 : 20;
    },
  });
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
    configurable: true,
    get(): number {
      return 20;
    },
  });
});

afterEach(() => {
  if (scrollHeight) Object.defineProperty(HTMLElement.prototype, 'scrollHeight', scrollHeight);
  if (clientHeight) Object.defineProperty(HTMLElement.prototype, 'clientHeight', clientHeight);
});

describe('AgentDescription', () => {
  it('offers "Show more" when the clamp cuts the text, and opens the full text', async () => {
    renderWithTheme(
      <AgentDescription
        description={'A long description. '.repeat(40)}
        isSmallHeight={false}
      />,
    );
    const button = await screen.findByRole('button', { name: 'Show more' });
    const text = screen.getByTestId('agent-modal-description');
    expect(text).toHaveStyle({ WebkitLineClamp: '2' });
    expect(button).toHaveAttribute('aria-controls', text.id);

    fireEvent.click(button);
    expect(screen.getByRole('button', { name: 'Show less' })).toHaveAttribute('aria-expanded', 'true');
    expect(text).not.toHaveStyle({ WebkitLineClamp: '2' });

    fireEvent.click(screen.getByRole('button', { name: 'Show less' }));
    expect(await screen.findByRole('button', { name: 'Show more' })).toBeInTheDocument();
  });

  it('scrolls an opened description inside a bounded box so "Show less" stays reachable', async () => {
    // The modal content clips (`overflow: hidden`) on a normal-height window:
    // an unbounded opened description pushed its own tail and the "Show less"
    // button past the clipped edge, where nothing scrolled them back.
    renderWithTheme(
      <AgentDescription
        description={'A long description. '.repeat(115)}
        isSmallHeight={false}
      />,
    );
    const text = screen.getByTestId('agent-modal-description');
    fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
    expect(text).toHaveStyle({ maxHeight: '10rem', overflowY: 'auto' });
    const showLess = screen.getByRole('button', { name: 'Show less' });
    // The button is a sibling of the scroll box, never inside it.
    expect(text.contains(showLess)).toBe(false);

    fireEvent.click(showLess);
    await screen.findByRole('button', { name: 'Show more' });
    expect(text).not.toHaveStyle({ maxHeight: '10rem' });
  });

  it('leaves the description unbounded on a short window, whose content box already scrolls', () => {
    renderWithTheme(
      <AgentDescription
        description={'A long description. '.repeat(115)}
        isSmallHeight
      />,
    );
    expect(screen.getByTestId('agent-modal-description')).not.toHaveStyle({ maxHeight: '10rem' });
  });

  it('offers no button when the text fits', async () => {
    cutText = false;
    renderWithTheme(
      <AgentDescription
        description="Short."
        isSmallHeight={false}
      />,
    );
    // Wait past the hook's settle delays (50 and 200 ms).
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(screen.queryByRole('button', { name: 'Show more' })).not.toBeInTheDocument();
  });

  it('offers no button on a short window, where the text is never clamped', async () => {
    renderWithTheme(
      <AgentDescription
        description={'A long description. '.repeat(40)}
        isSmallHeight
      />,
    );
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(screen.queryByRole('button', { name: 'Show more' })).not.toBeInTheDocument();
  });
});

describe('AgentWelcomeMessage', () => {
  it('offers "Show more" for a clamped welcome message and opens it', async () => {
    renderWithTheme(<AgentWelcomeMessage welcome_message={'Welcome to the agent. '.repeat(60)} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Show less' })).toBeInTheDocument());
    expect(screen.getByTestId('agent-welcome-message-text')).not.toHaveStyle({ WebkitLineClamp: '8' });
  });

  it('offers no button for the empty state', async () => {
    renderWithTheme(<AgentWelcomeMessage welcome_message="  " />);
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(screen.getByText(/No welcome message set/)).toBeInTheDocument();
  });
});
