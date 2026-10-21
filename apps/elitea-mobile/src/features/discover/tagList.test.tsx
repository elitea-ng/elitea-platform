import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { DesignProvider } from '@/design/DesignProvider';
import { colors } from '@/design/tokens';
import { TagList } from '@/features/discover/TagList';

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

const TAGS = [
  { id: '1', name: 'travel' },
  { id: '2', name: 'planning' },
];

describe('tag capsules keep a visible edge on every ground, in both schemes (S6)', () => {
  it.each([
    ['light', 'surface'],
    ['dark', 'surface'],
    ['light', 'elevated'],
    ['dark', 'elevated'],
  ] as const)('%s, on %s', async (scheme, on) => {
    await render(
      <DesignProvider scheme={scheme}>
        <TagList category="Project Management" tags={TAGS} on={on} />
      </DesignProvider>,
    );
    const ground = colors[scheme][on];
    for (const capsule of screen.getAllByTestId('tag-capsule')) {
      const style = StyleSheet.flatten(capsule.props.style);
      const bordered = (style.borderWidth ?? 0) > 0 && style.borderColor !== ground;
      expect(bordered || style.backgroundColor !== ground).toBe(true);
    }
  });

  it('on the dark agent sheet the fill itself differs from the sheet', async () => {
    await render(
      <DesignProvider scheme="dark">
        <TagList category={null} tags={TAGS} on="elevated" />
      </DesignProvider>,
    );
    for (const capsule of screen.getAllByTestId('tag-capsule')) {
      expect(StyleSheet.flatten(capsule.props.style).backgroundColor).not.toBe(colors.dark.elevated);
    }
  });
});
