import { ELITEA_MARK_ASPECT } from '@elitea/client-core/brand';
import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { EliteaMark, ELITEA_MARK_NAME } from '@/components/EliteaMark';
import { DesignProvider } from '@/design/DesignProvider';
import { colors } from '@/design/tokens';

// The artwork itself is the canonical SVG of libs/ts/client-core; its own tests and
// `gen-brand.mjs --check` keep every copy (this app's and the web app's) equal to it.

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

describe('<EliteaMark>', () => {
  it('is an image labelled "Elitea" by default', async () => {
    await render(<EliteaMark />);
    expect(ELITEA_MARK_NAME).toBe('Elitea');
    expect(screen.getByRole('image', { name: 'Elitea' })).toBeOnTheScreen();
  });

  it('takes the agent name when it stands in for an agent avatar', async () => {
    await render(<EliteaMark variant="avatar" size={28} label="Research agent" />);
    expect(screen.getByRole('image', { name: 'Research agent' })).toBeOnTheScreen();
  });

  it('decorative: hidden from assistive technology', async () => {
    await render(<EliteaMark decorative testID="m" />);
    expect(screen.queryByRole('image')).toBeNull();
    expect(screen.getByTestId('m', { includeHiddenElements: true }).props.accessible).toBe(false);
  });

  it.each(['mark', 'avatar'] as const)('%s keeps its aspect ratio', async (variant) => {
    await render(<EliteaMark variant={variant} size={120} testID="m" />);
    const style = StyleSheet.flatten(screen.getByTestId('m').props.style);
    expect(style.width).toBe(120);
    expect(style.height).toBe(variant === 'avatar' ? 120 : Math.round(120 / ELITEA_MARK_ASPECT));
  });

  it('never renders below the 24 pt minimum', async () => {
    await render(<EliteaMark variant="avatar" size={12} testID="m" />);
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).width).toBe(24);
  });

  it.each(['light', 'dark'] as const)('the avatar disc is the theme surface in %s, never the workspace accent', async (scheme) => {
    await render(
      <DesignProvider scheme={scheme}>
        <EliteaMark variant="avatar" size={40} testID="m" />
      </DesignProvider>,
    );
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).backgroundColor).toBe(colors[scheme].surface);
  });
});
