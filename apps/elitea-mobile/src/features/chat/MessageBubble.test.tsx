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

describe('MessageBubble avatar placement (§4.6)', () => {
  const { StyleSheet } = require('react-native');
  // The row is the outermost view; the avatar its first child.
  const rowAlign = () => {
    const row = screen.toJSON() as unknown as { props: { style: unknown }; children: { props: { testID?: string } }[] };
    expect(row.children[0].props.testID).toBe('message-avatar');
    return StyleSheet.flatten(row.props.style).alignItems;
  };
  const actions = <MessageBubble role="assistant">Row</MessageBubble>;

  it('pins the agent avatar beside the name line, the same while streaming and once the action row appears', async () => {
    const view = await render(
      <MessageBubble role="assistant" meta="Research · 9:41" avatar={<></>}>
        Answer
      </MessageBubble>,
    );
    const streaming = rowAlign();
    await view.rerender(
      <MessageBubble role="assistant" meta="Research · 9:41" avatar={<></>} actions={actions}>
        Answer
      </MessageBubble>,
    );
    expect(streaming).toBe('flex-start');
    expect(rowAlign()).toBe('flex-start');
  });
});

describe('MessageBubble for screen readers', () => {
  const { Pressable, Text: RNText } = require('react-native');

  it('a plain message is one element whose label carries its status and error', async () => {
    await render(
      <MessageBubble role="user" status="Waiting to send" error="Couldn't send">
        Hello
      </MessageBubble>,
    );
    expect(screen.getByLabelText("You: Hello. Waiting to send. Couldn't send")).toBeTruthy();
  });

  it('a message with files keeps its file buttons reachable (not one element)', async () => {
    await render(
      <MessageBubble
        role="user"
        footer={
          <Pressable accessibilityRole="button" accessibilityLabel="File a.pdf">
            <RNText>a.pdf</RNText>
          </Pressable>
        }>
        see attached
      </MessageBubble>,
    );
    expect(screen.getByRole('button', { name: 'File a.pdf' })).toBeTruthy();
    // The bubble itself is not an accessible element that would hide the button on iOS.
    let n = screen.getByRole('button', { name: 'File a.pdf' }).parent;
    for (; n; n = n.parent) expect(n.props.accessible === true && n.props.accessibilityLabel?.startsWith('You')).not.toBe(true);
    expect(screen.getByText('see attached')).toBeTruthy();
  });
});
