import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { Composer, peopleSuggestions } from '@/features/chat/Composer';

// F5: a send that resolves to an error keeps the text; an accepted one clears it.
describe('Composer with an async onSend', () => {
  it('keeps the text and shows the error when the send is not accepted', async () => {
    const onSend = jest.fn(async () => 'This message could not be saved to send later. Try again.');
    await render(<Composer busy={false} onSend={onSend} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'written offline');
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(screen.getByRole('alert')).toBeTruthy());
    expect(screen.getByLabelText('Message').props.value).toBe('written offline');
  });

  it('clears the text once the send is accepted', async () => {
    const onSend = jest.fn(async () => null);
    await render(<Composer busy={false} onSend={onSend} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'hello');
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(screen.getByLabelText('Message').props.value).toBe(''));
    expect(onSend).toHaveBeenCalledWith({ text: 'hello', mentions: null });
  });
});

// A4: "@" over the conversation's other people, mentions derived on send.
describe('Composer mentions', () => {
  const people = [
    { userId: 42, name: 'Anna' },
    { userId: 7, name: 'Bob Stone' },
  ];

  it('filters the strip as "@an" is typed, inserts "@Anna " on a tap, and sends user_ids [42]', async () => {
    const onSend = jest.fn(async () => null);
    await render(<Composer busy={false} onSend={onSend} people={people} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), '@an');
    expect(screen.getByLabelText('Anna')).toBeTruthy();
    expect(screen.queryByLabelText('Bob Stone')).toBeNull();
    await fireEvent.press(screen.getByLabelText('Anna'));
    expect(screen.getByLabelText('Message').props.value).toBe('@Anna ');
    // The strip closes once the token is complete.
    expect(screen.queryByLabelText('People')).toBeNull();
    await fireEvent.changeText(screen.getByLabelText('Message'), '@Anna can you look?');
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend).toHaveBeenCalledWith({ text: '@Anna can you look?', mentions: { userIds: [42], everyone: false } });
  });

  it('offers Everyone first and sends is_mentioning_everyone', async () => {
    const onSend = jest.fn(async () => null);
    await render(<Composer busy={false} onSend={onSend} people={people} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), '@');
    expect(screen.getByLabelText('Everyone, Notify everyone in this project')).toBeTruthy();
    await fireEvent.press(screen.getByLabelText('Everyone, Notify everyone in this project'));
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(onSend).toHaveBeenCalledWith({ text: '@Everyone ', mentions: { userIds: [], everyone: true } }));
  });

  it('shows no strip when nobody else is in the conversation', async () => {
    await render(<Composer busy={false} onSend={jest.fn(() => null)} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), '@an');
    expect(screen.queryByLabelText('People')).toBeNull();
  });

  it('shows the hint under the input', async () => {
    await render(<Composer busy={false} onSend={jest.fn(() => null)} people={people} hint="Mentions notify only in agent chats." />);
    expect(screen.getByText('Mentions notify only in agent chats.')).toBeTruthy();
  });
});

describe('peopleSuggestions', () => {
  it('ranks Everyone, then names starting with the query, then names containing it', () => {
    const rows = peopleSuggestions(
      [
        { userId: 1, name: 'Joanna' },
        { userId: 2, name: 'Anna' },
        { userId: 3, name: 'Bob' },
      ],
      'an',
    );
    expect(rows.map((r) => r.title)).toEqual(['Anna', 'Joanna']);
    expect(peopleSuggestions([{ userId: 2, name: 'Anna' }], '').map((r) => r.title)).toEqual(['Everyone', 'Anna']);
    expect(peopleSuggestions([], '')).toEqual([]);
  });
});

// Client contract 1.3: while an answer streams, Send becomes Stop (server cancel).
describe('Composer Stop', () => {
  it('replaces Send with Stop while busy, and shows why a stop failed', async () => {
    const onStop = jest.fn(async () => ({ problem: "Couldn't stop the answer. Try again.", restoreText: null }));
    await render(<Composer busy onSend={() => null} onStop={onStop} />);
    expect(screen.queryByLabelText('Send')).toBeNull();
    await fireEvent.press(screen.getByRole('button', { name: 'Stop answer' }));
    expect(onStop).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent("Couldn't stop the answer. Try again."));
  });

  it('a stop before any output removed the question on the server: its text comes back to the composer', async () => {
    const onStop = jest.fn(async () => ({ problem: null, restoreText: 'Hello there show me code' }));
    const view = await render(<Composer busy onSend={() => null} onStop={onStop} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(screen.getByLabelText('Message').props.value).toBe('Hello there show me code'));
    expect(screen.getByText(/Stopped before an answer/)).toBeTruthy();
    // The answer has ended: Send is back, with the restored text.
    await view.rerender(<Composer busy={false} onSend={() => null} onStop={onStop} />);
    expect(screen.getByRole('button', { name: 'Send' })).not.toBeDisabled();
  });

  it('files that went with the removed question are named: they must be attached again', async () => {
    const onStop = jest.fn(async () => ({ problem: null, restoreText: 'see attached', lostAttachments: ['report.pdf', 'chart.png'] }));
    await render(<Composer busy onSend={() => null} onStop={onStop} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(screen.getByLabelText('Message').props.value).toBe('see attached'));
    expect(screen.getByRole('alert')).toHaveTextContent('Stopped before an answer. Your message is back here; attach report.pdf and chart.png again to send them.');
  });

  it('an attachment-only message: nothing to put back but the files to attach again', async () => {
    const onStop = jest.fn(async () => ({ problem: null, restoreText: '', lostAttachments: ['scan.jpg'] }));
    await render(<Composer busy onSend={() => null} onStop={onStop} />);
    await fireEvent.press(screen.getByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Stopped before an answer. The message was removed; attach scan.jpg again to send it.'));
    expect(screen.getByLabelText('Message').props.value).toBe('');
  });

  it('never overwrites what the user typed meanwhile', async () => {
    const onStop = jest.fn(async () => ({ problem: null, restoreText: 'old question' }));
    await render(<Composer busy onSend={() => null} onStop={onStop} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'new draft');
    await fireEvent.press(screen.getByRole('button', { name: 'Stop answer' }));
    await waitFor(() => expect(onStop).toHaveBeenCalled());
    expect(screen.getByLabelText('Message').props.value).toBe('new draft');
  });

  it('keeps a disabled Send while busy when stopping is not offered (a server before 1.3)', async () => {
    await render(<Composer busy onSend={() => null} onStop={null} />);
    expect(screen.queryByRole('button', { name: 'Stop answer' })).toBeNull();
    expect(screen.getByLabelText('Send')).toBeDisabled();
  });
});
