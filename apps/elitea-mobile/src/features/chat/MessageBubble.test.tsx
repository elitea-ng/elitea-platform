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

describe('MessageBubble colours (docs/design-system.md §4.6)', () => {
  const { DesignProvider } = require('@/design/DesignProvider');
  const { colors } = require('@/design/tokens');
  const inScheme = (scheme: 'light' | 'dark', ui: React.ReactElement) =>
    render(<DesignProvider scheme={scheme}>{ui}</DesignProvider>);
  const tree = () => JSON.stringify(screen.toJSON());

  it('draws a dark user bubble in bubbleUser with white text, not the bright accent', async () => {
    await inScheme('dark', <MessageBubble role="user">Hello</MessageBubble>);
    expect(screen.getByText('Hello')).toHaveStyle({ color: colors.dark.bubbleUserText });
    expect(tree()).toContain(`"backgroundColor":"${colors.dark.bubbleUser}"`);
    expect(tree()).not.toContain(`"backgroundColor":"${colors.dark.accent}"`);
  });

  it('draws a dark agent bubble as a surface step with no border', async () => {
    await inScheme('dark', <MessageBubble role="assistant">Answer</MessageBubble>);
    expect(screen.getByText('Answer')).toHaveStyle({ color: colors.dark.bubbleAgentText });
    expect(tree()).toContain(`"backgroundColor":"${colors.dark.bubbleAgent}"`);
    expect(tree()).not.toContain(`"borderColor":"${colors.dark.bubbleAgentBorder}"`);
  });

  it('gives a light agent bubble its hairline border', async () => {
    await inScheme('light', <MessageBubble role="assistant">Answer</MessageBubble>);
    expect(tree()).toContain(`"borderColor":"${colors.light.bubbleAgentBorder}"`);
    expect(tree()).toContain(`"backgroundColor":"${colors.light.bubbleAgent}"`);
  });
});
