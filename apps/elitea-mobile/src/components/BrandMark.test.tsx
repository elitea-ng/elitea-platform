import { ELITEA_MARK_ASPECT, ELITEA_MARK_SVG } from '@elitea/client-core/brand';
import { render, screen } from '@testing-library/react-native';
import { StyleSheet } from 'react-native';

import { BRAND_MARK_ASPECT, BRAND_MARK_NAME, BRAND_MARK_SVG, BrandMark } from '@/components/BrandMark';
import { DesignProvider } from '@/design/DesignProvider';
import { colors } from '@/design/tokens';

// The artwork is the built brand's logo-mark.svg (identity.markSvg). For brand/elitea it is the
// canonical SVG of libs/ts/client-core; its own tests and `gen-brand.mjs --check` keep every copy
// equal to it, and scripts/brand/elitea-sync.test.mjs holds brand/elitea's copy to it.

jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

describe('<BrandMark>', () => {
  it('draws brand/elitea\'s mark (the default build): the LE-03 mark', () => {
    expect(BRAND_MARK_SVG.replace(/\s+/g, ' ')).toBe(ELITEA_MARK_SVG.replace(/\s+/g, ' '));
    expect(BRAND_MARK_ASPECT).toBe(ELITEA_MARK_ASPECT);
  });

  it('is an image labelled with the brand\'s short name by default', async () => {
    await render(<BrandMark />);
    expect(BRAND_MARK_NAME).toBe('Elitea');
    expect(screen.getByRole('image', { name: 'Elitea' })).toBeOnTheScreen();
  });

  it('takes the agent name when it stands in for an agent avatar', async () => {
    await render(<BrandMark variant="avatar" size={28} label="Research agent" />);
    expect(screen.getByRole('image', { name: 'Research agent' })).toBeOnTheScreen();
  });

  it('decorative: hidden from assistive technology', async () => {
    await render(<BrandMark decorative testID="m" />);
    expect(screen.queryByRole('image')).toBeNull();
    expect(screen.getByTestId('m', { includeHiddenElements: true }).props.accessible).toBe(false);
  });

  it.each(['mark', 'avatar'] as const)('%s keeps its aspect ratio', async (variant) => {
    await render(<BrandMark variant={variant} size={120} testID="m" />);
    const style = StyleSheet.flatten(screen.getByTestId('m').props.style);
    expect(style.width).toBe(120);
    expect(style.height).toBe(variant === 'avatar' ? 120 : Math.round(120 / BRAND_MARK_ASPECT));
  });

  it('never renders below the 24 pt minimum', async () => {
    await render(<BrandMark variant="avatar" size={12} testID="m" />);
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).width).toBe(24);
  });

  it.each(['light', 'dark'] as const)('the avatar disc is the theme surface in %s, never the workspace accent', async (scheme) => {
    await render(
      <DesignProvider scheme={scheme}>
        <BrandMark variant="avatar" size={40} testID="m" />
      </DesignProvider>,
    );
    expect(StyleSheet.flatten(screen.getByTestId('m').props.style).backgroundColor).toBe(colors[scheme].surface);
  });
});
