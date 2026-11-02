import { describe, expect, it } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import type { MessageGroupWire } from '@/entities/message/lib/wire';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { convertMessagesToChatHistory } from '../../lib/convertMessagesToChatHistory';
import { parseGrounding } from '../../lib/grounding';
import { ApplicationAnswer } from './ApplicationAnswer';

const content = 'Le café ouvre à 8 h [1]. Le menu change 🎉 chaque jour. Il ferme à 22 h.';
const grounding = {
  version: 1, sources_available: 3, sources_cited: 1, claim_spans: 3, uncited_spans: 2,
  uncited: [{ start: 25, end: 55 }, { start: 56, end: 72 }], invalid_markers: 0,
};
const base: ChatMessage = { id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee', role: 'assistant', name: 'Agent', content, createdAt: '' };

describe('grounding indicator', () => {
  it('shows the counts, and lists the uncited statements on click', () => {
    renderWithTheme(<ApplicationAnswer answer={{ ...base, responseMetadata: { grounding } }} messageId={base.id} />);
    const indicator = screen.getByTestId('grounding-indicator');
    expect(indicator).toHaveTextContent('1 of 3 sources cited');
    expect(indicator).not.toHaveTextContent(/%|confiden|score/i);
    expect(screen.queryByTestId('grounding-uncited-list')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '2 statements without a citation' }));
    const marks = screen.getByTestId('grounding-uncited-list').querySelectorAll('mark');
    expect([...marks].map((mark) => mark.textContent)).toEqual(['Le menu change 🎉 chaque jour.', 'Il ferme à 22 h.']);
  });

  it('is restored from the persisted message after a reload', () => {
    const [restored] = convertMessagesToChatHistory([{
      id: 1, uuid: base.id, role: 'assistant', content, is_streaming: false, created_at: '',
      meta: { grounding: { ...grounding, uncited_spans: 0, uncited: [] } },
    } as MessageGroupWire]);
    renderWithTheme(<ApplicationAnswer answer={restored!} messageId={base.id} />);
    expect(screen.getByTestId('grounding-indicator')).toHaveTextContent('1 of 3 sources cited');
    expect(screen.queryByRole('button', { name: /without a citation/ })).not.toBeInTheDocument();
  });

  it('renders nothing without an indicator, or while the answer is still running', () => {
    const { unmount } = renderWithTheme(<ApplicationAnswer answer={base} messageId={base.id} />);
    expect(screen.queryByTestId('grounding-indicator')).not.toBeInTheDocument();
    unmount();
    renderWithTheme(<ApplicationAnswer answer={{ ...base, responseMetadata: { grounding } }} messageId={base.id} status={{ isStreaming: true }} />);
    expect(screen.queryByTestId('grounding-indicator')).not.toBeInTheDocument();
  });

  it('drops a malformed indicator', () => {
    expect(parseGrounding({ ...grounding, version: 2 })).toBeNull();
    expect(parseGrounding({ ...grounding, sources_available: 0 })).toBeNull();
    expect(parseGrounding({ ...grounding, uncited: [{ start: 5, end: 5 }] })).toBeNull();
    expect(parseGrounding('yes')).toBeNull();
  });
});
