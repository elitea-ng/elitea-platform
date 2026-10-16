import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import type { MessageGroupWire } from '@/entities/message/lib/wire';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { convertMessagesToChatHistory } from '../../lib/convertMessagesToChatHistory';
import { ApplicationAnswer } from './ApplicationAnswer';

const skipped = [
  { kind: 'toolkit', name: 'wiki', type: 'confluence', reason_code: 'unsupported_toolkit_family', reason: 'this toolkit type is not available on this worker yet, so its tools were left out' },
  { kind: 'internal_tool', name: 'swarm', reason_code: 'internal_tool_unsupported', reason: 'this built-in capability is not available on this worker yet' },
];
const base: ChatMessage = { id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee', role: 'assistant', name: 'Agent', content: 'Done.', createdAt: '' };

describe('skipped capabilities notice', () => {
  it('names each skipped capability once with its reason, from the live final frame', () => {
    renderWithTheme(<ApplicationAnswer answer={{ ...base, responseMetadata: { skipped_capabilities: { items: [...skipped, skipped[0]], more: 4 } } }} messageId={base.id} />);
    const notice = screen.getByTestId('skipped-capabilities-notice');
    expect(notice).toHaveTextContent('Some tools were not available in this reply');
    expect(notice).toHaveTextContent('Toolkit wiki (confluence): this toolkit type is not available on this worker yet, so its tools were left out.');
    expect(notice).toHaveTextContent('Built-in capability swarm');
    expect(notice.querySelectorAll('p')).toHaveLength(3);
    expect(notice).toHaveTextContent('4 more not shown.');
  });

  it('names a left-out tool of a built toolkit', () => {
    const tool = { kind: 'tool', name: 'github.create_file', type: 'github', reason_code: 'unsupported_tool_selection', reason: 'this tool is not available on this worker yet, so it was left out' };
    renderWithTheme(<ApplicationAnswer answer={{ ...base, responseMetadata: { skipped_capabilities: { items: [tool] } } }} messageId={base.id} />);
    expect(screen.getByTestId('skipped-capabilities-notice')).toHaveTextContent('Tool github.create_file: this tool is not available on this worker yet, so it was left out.');
  });

  it('is restored from the persisted message after a reload', () => {
    const [restored] = convertMessagesToChatHistory([{
      id: 1, uuid: base.id, role: 'assistant', content: 'Done.', is_streaming: false, created_at: '',
      meta: { skipped_capabilities: { items: skipped } },
    } as MessageGroupWire]);
    renderWithTheme(<ApplicationAnswer answer={restored!} messageId={base.id} />);
    expect(screen.getByTestId('skipped-capabilities-notice')).toHaveTextContent('Toolkit wiki (confluence)');
  });

  it('renders nothing when the turn served everything, or while it is still running', () => {
    const { unmount } = renderWithTheme(<ApplicationAnswer answer={base} messageId={base.id} />);
    expect(screen.queryByTestId('skipped-capabilities-notice')).not.toBeInTheDocument();
    unmount();
    renderWithTheme(<ApplicationAnswer answer={{ ...base, responseMetadata: { skipped_capabilities: { items: skipped } } }} messageId={base.id} status={{ isStreaming: true }} />);
    expect(screen.queryByTestId('skipped-capabilities-notice')).not.toBeInTheDocument();
  });
});
