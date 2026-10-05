import { render, screen } from '@testing-library/react-native';

import { MessageBubble, initialOf } from '@/features/chat/MessageBubble';

describe('MessageBubble authorship', () => {
  it("shows another person's message with their name and initial", async () => {
    await render(
      <MessageBubble role="user" mine={false} author="Bob">
        Hi Alice
      </MessageBubble>,
    );
    expect(screen.getByText('Bob')).toBeTruthy();
    // The avatar is decoration: hidden from screen readers (the name is read).
    expect(screen.getByText('B', { includeHiddenElements: true })).toBeTruthy();
    expect(screen.queryByText('B')).toBeNull();
    expect(screen.getByText('Hi Alice')).toBeTruthy();
  });

  it('shows my own message without a name or avatar', async () => {
    await render(<MessageBubble role="user">Hello</MessageBubble>);
    expect(screen.queryByText('B')).toBeNull();
    expect(screen.getByText('Hello')).toBeTruthy();
  });

  it('takes the first letter of any script', () => {
    expect(initialOf('ann')).toBe('A');
    expect(initialOf('  Élodie')).toBe('É');
    expect(initialOf('李雷')).toBe('李');
    expect(initialOf('')).toBe('?');
  });
});

describe('MessageBubble highlighting', () => {
  it('sets off a known person, an agent and a tool token', async () => {
    await render(
      <MessageBubble role="user" highlight={{ people: ['Anna'], agents: ['Research'] }}>
        @Anna ask #Research to run /github/list_issues
      </MessageBubble>,
    );
    expect(screen.getByText('@Anna').props.style).toMatchObject({ fontWeight: '700' });
    expect(screen.getByText('#Research').props.style).toMatchObject({ fontWeight: '700' });
    expect(screen.getByText('/github/list_issues').props.style).toMatchObject({ fontFamily: expect.any(String) });
  });
});
