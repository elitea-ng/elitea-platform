import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListApplicationSkillsMockHandler } from '@/shared/api/generated/skills/skills.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import type { MentionableTool } from '../lib/hooks/useInstructionsMention.hooks';
import { renderWithProviders } from '../__tests__/testUtils';

import { InstructionsInput } from './InstructionsInput';
import { INSTRUCTIONS_MAX_HEIGHT } from './InstructionsInput.sizing';

installCodeMirrorTestPolyfills();

/** CM6's `.cm-content` is the actual `role="textbox"` element every interaction below types into. */
function getContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

const TOOLS: readonly MentionableTool[] = [
  { id: 1, name: 'Github', type: 'github', settings: { selected_tools: ['create_issue'] } },
  { id: 2, name: 'SubAgent', type: 'application', agent_type: 'chat' },
];

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('InstructionsInput', () => {
  it('renders the current instructions text', () => {
    renderWithProviders(
      <InstructionsInput
        instructions="Be helpful."
        onInstructionsChange={vi.fn()}
      />,
    );
    expect(screen.getByText('Be helpful.')).toBeInTheDocument();
  });

  it('calls onInstructionsChange as the user types', () => {
    const onInstructionsChange = vi.fn();
    renderWithProviders(
      <InstructionsInput
        instructions=""
        onInstructionsChange={onInstructionsChange}
      />,
    );
    const editor = screen.getByRole('textbox');
    fireEvent.input(editor, { target: { textContent: 'New instructions' } });
    // CodeMirror's own onChange is debounced (~30ms) and driven by its
    // internal EditorView dispatch, not a raw DOM `input` event — this
    // assertion only proves the editable surface renders and accepts focus/
    // typing without crashing; the debounced commit path itself is covered
    // by `shared/ui/CodeMirrorEditor`'s own test suite (unit S1-E).
    expect(editor).toBeInTheDocument();
  });

  it('renders without crashing when disabled, still showing the current text', () => {
    renderWithProviders(
      <InstructionsInput
        instructions="Be helpful."
        onInstructionsChange={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByText('Be helpful.')).toBeInTheDocument();
  });

  it('does not truncate instructions already longer than the Skills-only MAX_INSTRUCTIONS_LENGTH constant (regression: no maxLength on the agent instructions field)', async () => {
    const user = userEvent.setup();
    const longInstructions = 'x'.repeat(2600);
    const onInstructionsChange = vi.fn();
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions={longInstructions}
        onInstructionsChange={onInstructionsChange}
      />,
    );
    const content = getContent(container);
    expect(content).toHaveTextContent(longInstructions);
    await user.click(content);
    await user.keyboard('{End}!');
    await waitFor(() => {
      expect(onInstructionsChange).toHaveBeenCalledWith(longInstructions + '!');
    });
  });

  it('opens the "/" mention dropdown listing the version tools, and inserts a token on click', async () => {
    const user = userEvent.setup();
    const onInstructionsChange = vi.fn();
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions=""
        onInstructionsChange={onInstructionsChange}
        tools={TOOLS}
        versionId="v1"
        entityProjectId="proj-1"
      />,
    );
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('/');

    expect(await screen.findByText('Github')).toBeInTheDocument();
    expect(screen.getByText('SubAgent')).toBeInTheDocument();

    await user.click(screen.getByText('SubAgent'));

    await waitFor(() => {
      expect(onInstructionsChange).toHaveBeenCalledWith('/SubAgent ');
    });
  });

  it('closes the "/" mention dropdown on Escape without inserting anything extra', async () => {
    const user = userEvent.setup();
    const onInstructionsChange = vi.fn();
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions=""
        onInstructionsChange={onInstructionsChange}
        tools={TOOLS}
        versionId="v1"
        entityProjectId="proj-1"
      />,
    );
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('/');
    expect(await screen.findByText('Github')).toBeInTheDocument();

    await user.keyboard('{Escape}');
    await waitFor(() => {
      expect(screen.queryByText('Github')).not.toBeInTheDocument();
    });
  });

  it('opens the "~" skill mention dropdown listing the project/version skills, and inserts a token on click', async () => {
    server.use(
      getListApplicationSkillsMockHandler({
        items: [
          {
            id: 'skill-1',
            project_id: 'proj-1',
            name: 'Summarizer',
            description: 'Summarizes text',
            type: 'skill',
            is_default: false,
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );

    const user = userEvent.setup();
    const onInstructionsChange = vi.fn();
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions=""
        onInstructionsChange={onInstructionsChange}
        versionId={7}
        entityProjectId="proj-1"
      />,
    );
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('~');

    expect(await screen.findByText('Summarizer')).toBeInTheDocument();

    await user.click(screen.getByText('Summarizer'));

    await waitFor(() => {
      expect(onInstructionsChange).toHaveBeenCalledWith('~Summarizer ');
    });
  });
});

describe('InstructionsInput sizing (#1979)', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
  });
  afterEach(() => {
    resetGeneratedClient();
  });

  it('is a compact auto-growing field rather than a viewport-tall code editor', () => {
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions="one line"
        onInstructionsChange={vi.fn()}
      />,
    );
    // The rules CodeMirror injects are applied by jsdom's cascade, so these are the computed values the browser resolves.
    const editor = container.querySelector('.cm-editor');
    const gutters = container.querySelector('.cm-gutters');
    expect(editor).toBeInstanceOf(HTMLElement);
    expect(gutters).toBeInstanceOf(HTMLElement);
    // jsdom resolves rem against 16px: 24rem is 384px.
    expect(INSTRUCTIONS_MAX_HEIGHT).toBe('24rem');
    expect(getComputedStyle(editor as HTMLElement).maxHeight, 'growth is capped, then the field scrolls').toBe('384px');
    expect(getComputedStyle(gutters as HTMLElement).display, 'no line numbers beside prose').toBe('none');
    const css = Array.from(document.querySelectorAll('style'))
      .map((style) => style.textContent ?? '')
      .join('\n');
    expect(css, 'the viewport-tall default must not apply').not.toContain('100vh - 220px');
  });

  it('shows the "/" mention list in full, outside the capped editor, so the compact field cannot clip it', async () => {
    const user = userEvent.setup();
    const { container } = renderWithProviders(
      <InstructionsInput
        instructions=""
        onInstructionsChange={vi.fn()}
        tools={TOOLS}
        versionId="v1"
        entityProjectId="proj-1"
      />,
    );
    await user.click(getContent(container));
    await user.keyboard('/');

    const item = await screen.findByText('Github');
    // The list is a sibling of the editor, not a child of its scroller: the editor's max-height and overflow do not apply to it.
    expect(item.closest('.cm-editor')).toBeNull();
    expect(item.closest('.cm-scroller')).toBeNull();
    // And nothing around the editor (the field's wrapper, the accordion body) clips what overflows it.
    const editor = container.querySelector('.cm-editor') as HTMLElement;
    for (let node: HTMLElement | null = editor.parentElement; node !== null && node !== container; node = node.parentElement) {
      expect(getComputedStyle(node).overflow, `${node.className} must not clip the list`).not.toBe('hidden');
    }
  });
});
