import { fireEvent, render, screen } from '@testing-library/react-native';

import ProjectsScreen from '@/app/(app)/projects';
import { __resetSessionForTests, setSession } from '@/state/session';

jest.mock('expo-router', () => ({ router: { back: jest.fn(), canGoBack: () => true } }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// listProjects rows as fetchProjects keeps them (ProjectWithGroups → id, name, suspended).
jest.mock('@/api/queries/projects', () => {
  const actual = jest.requireActual('@/api/queries/projects');
  return {
    ...actual,
    useProjects: () => ({
      isLoading: false,
      error: null,
      data: [
        { id: 1, name: 'Platform team', suspended: false },
        { id: 7, name: 'project_user_42', suspended: false },
        { id: 12, name: 'Support', suspended: false },
        { id: 13, name: 'Archived', suspended: true },
      ],
    }),
  };
});
jest.mock('@/state/selfIdentity', () => ({ usePersonalProjectId: () => 7 }));
jest.mock('@/state/projectSelection', () => ({ selectProject: jest.fn(async () => {}) }));

const { selectProject } = jest.requireMock<{ selectProject: jest.Mock }>('@/state/projectSelection');

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: 'https://ai.example.com', workspaceId: 'aaaaaaaaaaaa', projectId: 12 });
  jest.clearAllMocks();
});

it('lists the personal project first, then the team projects, without suspended ones', async () => {
  await render(<ProjectsScreen />);
  // Section headers are the iOS inset-grouped style (uppercase footnote); the workspace comes first.
  const headers = screen.getAllByRole('header').map((h) => h.props.children);
  expect(headers).toEqual(['WORKSPACE', 'PERSONAL', 'TEAM PROJECTS']);
  expect(screen.getByText('ai.example.com')).toBeTruthy();
  expect(screen.getByText('My project')).toBeTruthy();
  expect(screen.getByText('Platform team')).toBeTruthy();
  expect(screen.getByText('Support')).toBeTruthy();
  expect(screen.queryByText('Archived')).toBeNull();
  expect(screen.getByTestId('project-12').props.accessibilityState).toMatchObject({ selected: true });
});

it('selects a project for the active workspace', async () => {
  await render(<ProjectsScreen />);
  fireEvent.press(screen.getByTestId('project-7'));
  expect(selectProject).toHaveBeenCalledWith(7);
});
