import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { Composer } from '@/features/chat/Composer';

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
    expect(onSend).toHaveBeenCalledWith('hello');
  });
});
