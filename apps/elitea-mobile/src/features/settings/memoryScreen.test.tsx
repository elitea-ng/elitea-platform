import { fireEvent, render, screen } from '@testing-library/react-native';
import { Alert } from 'react-native';

import { memoriesList } from '@/api/__fixtures__/settings13';
import MemoryScreen from '@/app/(app)/memory';
import { memoriesOf } from '@/api/queries/memories';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

let mockRows = memoriesOf(memoriesList);
const mockForget = jest.fn(async (ids: readonly string[] | 'all') => {
  mockRows = ids === 'all' ? [] : mockRows.filter((m) => !ids.includes(m.id));
});
jest.mock('@/api/queries/memories', () => {
  const actual = jest.requireActual('@/api/queries/memories');
  return {
    ...actual,
    useMemories: () => ({ isLoading: false, error: null, data: mockRows, refetch: jest.fn(), isRefetching: false }),
    useForgetMemories: () => mockForget,
    deleteMemory: jest.fn(async () => {}),
    clearMemories: jest.fn(async () => 2),
  };
});
let mockContract = '1.3';
jest.mock('@/state/workspaceInfo', () => ({
  ...jest.requireActual('@/state/workspaceInfo'),
  useWorkspaceInfo: () => ({ origin: 'https://ai.example.com', discovery: { client_contract: mockContract } }),
}));

const api = jest.requireMock<{ deleteMemory: jest.Mock; clearMemories: jest.Mock }>('@/api/queries/memories');

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: 'aaaaaaaaaaaa', projectId: 90503 });
  mockRows = memoriesOf(memoriesList);
  mockContract = '1.3';
  jest.clearAllMocks();
});

it('lists the memories with their tags', async () => {
  await render(<MemoryScreen />);
  expect(screen.getByText('My team ships on Thursdays.')).toBeTruthy();
  expect(screen.getByText('#style')).toBeTruthy();
});

it('deletes one through its row action (the same path as the swipe)', async () => {
  const view = await render(<MemoryScreen />);
  await fireEvent(screen.getByLabelText('I prefer answers in British English.'), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
  expect(api.deleteMemory).toHaveBeenCalledWith(90503, '1');
  expect(mockForget).toHaveBeenCalledWith(['1']);
  await view.rerender(<MemoryScreen />);
  expect(screen.queryByTestId('memory-1')).toBeNull();
});

it('the Delete action sits on the element a screen reader focuses', async () => {
  await render(<MemoryScreen />);
  const row = screen.getByLabelText('My team ships on Thursdays.');
  expect(row.props.accessible).toBe(true);
  expect(row.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Delete' }]);
  await fireEvent(row, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
  expect(api.deleteMemory).toHaveBeenCalledWith(90503, '2');
});

it('clears all only after the confirmation', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  await render(<MemoryScreen />);
  await fireEvent.press(screen.getByText('Clear all'));
  expect(api.clearMemories).not.toHaveBeenCalled();
  const buttons = alert.mock.calls[0][2]!;
  buttons.find((b) => b.text === 'Clear all')!.onPress!();
  await new Promise<void>((resolve) => setImmediate(() => resolve()));
  expect(api.clearMemories).toHaveBeenCalledWith(90503);
  expect(mockForget).toHaveBeenCalledWith('all');
});

// QA-3: the native Alert outlives a workspace switch (a connect link for
// another workspace confirmed on top of it). Its "Clear all" must not send the
// old screen's project id to whichever workspace is active by then.
it.each([
  ['another workspace', { serverOrigin: 'https://other.example.com', workspaceId: 'bbbbbbbbbbbb', projectId: 90503 }],
  ['another project', { projectId: 12 }],
])('a confirmation answered after a switch to %s clears nothing', async (_, next) => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  await render(<MemoryScreen />);
  await fireEvent.press(screen.getByText('Clear all'));
  const buttons = alert.mock.calls[0][2]!;
  setSession(next);
  buttons.find((b) => b.text === 'Clear all')!.onPress!();
  await new Promise<void>((resolve) => setImmediate(() => resolve()));
  expect(api.clearMemories).not.toHaveBeenCalled();
  expect(mockForget).not.toHaveBeenCalled();
});

it('a server before client contract 1.3 shows no memory', async () => {
  mockContract = '1.2';
  await render(<MemoryScreen />);
  expect(screen.getByText(/does not offer memory/)).toBeTruthy();
  expect(screen.queryByText('My team ships on Thursdays.')).toBeNull();
});
