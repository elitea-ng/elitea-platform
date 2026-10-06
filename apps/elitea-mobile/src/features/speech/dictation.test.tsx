import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import { ExpoSpeechRecognitionModule } from 'expo-speech-recognition';

import { Composer } from '@/features/chat/Composer';
import { canDictateOnDevice, dictationProblemText, joinDictation, problemOf } from '@/features/speech/dictation';

// The root __mocks__/expo-speech-recognition.ts stands in for the native module.
const recogniser = ExpoSpeechRecognitionModule as unknown as typeof ExpoSpeechRecognitionModule & {
  __emit: (name: string, event: unknown) => void;
  __reset: () => void;
  isRecognitionAvailable: jest.Mock;
  supportsOnDeviceRecognition: jest.Mock;
  requestPermissionsAsync: jest.Mock;
  start: jest.Mock;
  stop: jest.Mock;
  abort: jest.Mock;
};

beforeEach(() => recogniser.__reset());

describe('dictation helpers', () => {
  it('puts the words after what was typed, with one space', () => {
    expect(joinDictation('', 'hello there ')).toBe('hello there');
    expect(joinDictation('Note:', 'buy milk')).toBe('Note: buy milk');
    expect(joinDictation('Note: ', 'buy milk')).toBe('Note: buy milk');
    expect(joinDictation('typed', '   ')).toBe('typed');
  });

  it("maps the recogniser's errors; the user's own abort is no error", () => {
    expect(problemOf('aborted')).toBeNull();
    expect(problemOf('not-allowed')).toBe('permission');
    expect(problemOf('language-not-supported')).toBe('unsupported');
    expect(problemOf('no-speech')).toBe('no_speech');
    expect(problemOf('network')).toBe('failed');
    expect(dictationProblemText('unsupported')).toMatch(/never sent off the device/);
  });
});

describe('canDictateOnDevice', () => {
  it('Android below 13 (API 33) never dictates: the library has no on-device recogniser there, only a PREFER_OFFLINE hint', () => {
    // isOnDeviceRecognitionAvailable() is true from API 31, but expo-speech-recognition
    // creates the on-device recogniser only from API 33 and otherwise uses the default
    // (cloud-capable) service.
    expect(canDictateOnDevice({ OS: 'android', Version: 31 })).toBe(false);
    expect(canDictateOnDevice({ OS: 'android', Version: 32 })).toBe(false);
    expect(canDictateOnDevice({ OS: 'android', Version: 33 })).toBe(true);
    expect(canDictateOnDevice({ OS: 'android', Version: 35 })).toBe(true);
  });

  it('iOS asks the recogniser; web never dictates', () => {
    expect(canDictateOnDevice({ OS: 'ios', Version: '18.0' })).toBe(true);
    recogniser.supportsOnDeviceRecognition.mockImplementation(() => false);
    expect(canDictateOnDevice({ OS: 'ios', Version: '18.0' })).toBe(false);
    expect(canDictateOnDevice({ OS: 'web', Version: '' })).toBe(false);
  });
});

describe('Composer dictation (on-device only)', () => {
  it('starts the recogniser ON THE DEVICE, writes what it hears after the typed text, and stops on tap', async () => {
    await render(<Composer busy={false} onSend={() => null} dictation />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'Draft:');
    await fireEvent.press(screen.getByRole('button', { name: 'Dictate' }));
    await waitFor(() => expect(recogniser.start).toHaveBeenCalledTimes(1));
    expect(recogniser.start.mock.calls[0][0]).toMatchObject({ requiresOnDeviceRecognition: true, interimResults: true });
    expect(screen.getByRole('button', { name: 'Stop dictation' })).toBeTruthy();

    await act(async () => recogniser.__emit('result', { isFinal: false, results: [{ transcript: 'book a', confidence: -1, segments: [] }] }));
    expect(screen.getByLabelText('Message').props.value).toBe('Draft: book a');
    await act(async () => recogniser.__emit('result', { isFinal: true, results: [{ transcript: 'book a room', confidence: 0.9, segments: [] }] }));
    expect(screen.getByLabelText('Message').props.value).toBe('Draft: book a room');

    await fireEvent.press(screen.getByRole('button', { name: 'Stop dictation' }));
    expect(recogniser.stop).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: 'Dictate' })).toBeTruthy();
  });

  it('a device without on-device recognition gets no dictation, never a server fallback', async () => {
    recogniser.supportsOnDeviceRecognition.mockImplementation(() => false);
    await render(<Composer busy={false} onSend={() => null} dictation />);
    await fireEvent.press(screen.getByRole('button', { name: 'Dictate' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/isn't available/);
    expect(recogniser.requestPermissionsAsync).not.toHaveBeenCalled();
    expect(recogniser.start).not.toHaveBeenCalled();
  });

  it('a refused permission says where to allow it', async () => {
    recogniser.requestPermissionsAsync.mockImplementation(async () => ({ granted: false, status: 'denied', canAskAgain: false, expires: 'never' }));
    await render(<Composer busy={false} onSend={() => null} dictation />);
    await fireEvent.press(screen.getByRole('button', { name: 'Dictate' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/in Settings/);
    expect(recogniser.start).not.toHaveBeenCalled();
  });

  it('a recogniser error ends listening with a message; no microphone without the prop', async () => {
    const view = await render(<Composer busy={false} onSend={() => null} dictation />);
    await fireEvent.press(screen.getByRole('button', { name: 'Dictate' }));
    await waitFor(() => expect(recogniser.start).toHaveBeenCalled());
    await act(async () => recogniser.__emit('error', { error: 'no-speech', message: '' }));
    expect(screen.getByRole('alert')).toHaveTextContent(/Didn't catch that/);
    expect(screen.getByRole('button', { name: 'Dictate' })).toBeTruthy();
    await view.unmount();
    await render(<Composer busy={false} onSend={() => null} />);
    expect(screen.queryByRole('button', { name: 'Dictate' })).toBeNull();
  });
});
