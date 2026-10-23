import { act, fireEvent, render, screen } from '@testing-library/react-native';
import { AccessibilityInfo, Pressable, Text } from 'react-native';

import { Toast, useToast } from '@/features/chat/ThreadLayout';

function Harness() {
  const { toast, show, hide } = useToast();
  return (
    <>
      <Pressable accessibilityRole="button" accessibilityLabel="copy" onPress={() => show('Copied')}>
        <Text>copy</Text>
      </Pressable>
      <Toast toast={toast} onHide={hide} />
    </>
  );
}

describe('the thread toast (§4.13)', () => {
  beforeEach(() => jest.useFakeTimers());

  it('is announced once, by the explicit announcement only (no live region as well)', async () => {
    const announce = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    await render(<Harness />);
    await fireEvent.press(screen.getByRole('button', { name: 'copy' }));
    expect(announce).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(screen.toJSON())).not.toContain('accessibilityLiveRegion');
    await screen.unmount();
    announce.mockRestore();
  });

  it('the same note again is announced again and its timer restarts', async () => {
    const announce = jest.spyOn(AccessibilityInfo, 'announceForAccessibility').mockImplementation(() => {});
    await render(<Harness />);
    await fireEvent.press(screen.getByRole('button', { name: 'copy' }));
    await act(async () => jest.advanceTimersByTime(1500));
    await fireEvent.press(screen.getByRole('button', { name: 'copy' }));
    expect(announce).toHaveBeenCalledTimes(2);
    // 1.5 s after the second copy the toast is still up (the first timer would have hidden it).
    await act(async () => jest.advanceTimersByTime(1500));
    expect(screen.getByTestId('thread-toast')).toBeTruthy();
    await act(async () => jest.advanceTimersByTime(400));
    expect(screen.queryByTestId('thread-toast')).toBeNull();
    await screen.unmount();
    announce.mockRestore();
  });
});
