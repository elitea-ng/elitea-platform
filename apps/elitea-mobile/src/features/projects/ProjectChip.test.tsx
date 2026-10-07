import { fireEvent, render, screen } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { projectChipFrame, projectSwitcherOptions } from '@/features/projects/ProjectChip';

jest.mock('expo-router', () => ({ router: { push: jest.fn() } }));
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
jest.mock('@/features/projects/useProjectSelection', () => ({ useProjectSelection: () => ({ project: null, projectId: null }) }));
jest.mock('@/state/selfIdentity', () => ({ usePersonalProjectId: () => null }));

const router = jest.requireMock<{ router: { push: jest.Mock } }>('expo-router').router;

// D6: on iOS 26 the switcher was a native text bar button with no disclosure glyph, its name
// sliced to 17 characters in JS. It is now the same chip everywhere: name + chevron, truncated
// by the system.
describe('project switcher (D6)', () => {
  it('shows the full name with a chevron and opens the project switcher', async () => {
    const options = projectSwitcherOptions('Customer Success EMEA');
    expect(options).not.toHaveProperty('unstable_headerLeftItems');
    await render(options.headerLeft!({ canGoBack: false }) as ReactElement);
    const chip = screen.getByRole('button', { name: 'Project: Customer Success EMEA. Switch project' });
    expect(screen.getByText('Customer Success EMEA').props.numberOfLines).toBe(1);
    expect(JSON.stringify(screen.toJSON())).toContain('"symbolIos":"chevron.down"');
    await fireEvent.press(chip);
    expect(router.push).toHaveBeenCalledWith('/projects');
  });
});

describe('project switcher touch target (F5)', () => {
  it('is 44 pt on iOS and 48 dp on Android', () => {
    expect(projectChipFrame('ios').minHeight).toBe(44);
    expect(projectChipFrame('android').minHeight).toBe(48);
  });
});
