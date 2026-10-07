import { render, screen } from '@testing-library/react-native';

import { DesignProvider } from '@/design/DesignProvider';
import { resolveDesign } from '@/design/resolve';

import { DesignGallery, GALLERY_BRANDS, parseGalleryParams, type GalleryBrand } from './DesignGallery';

jest.mock('expo-router', () => ({ router: { push: jest.fn(), back: jest.fn() }, Stack: { Screen: () => null } }));
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

describe('design gallery', () => {
  it('reads the preview from the link, defaulting to the system scheme and Zefir', () => {
    expect(parseGalleryParams({ scheme: 'dark', brand: 'magenta' })).toEqual({ scheme: 'dark', brand: 'magenta' });
    expect(parseGalleryParams({ scheme: 'sepia', brand: 'acme' })).toEqual({ scheme: 'system', brand: 'zefir' });
    expect(parseGalleryParams({})).toEqual({ scheme: 'system', brand: 'zefir' });
  });

  it.each(Object.keys(GALLERY_BRANDS).flatMap((brand) => (['light', 'dark'] as const).map((scheme) => [brand, scheme] as const)))(
    'renders every section with the %s brand in %s',
    async (brand, scheme) => {
      await render(
        <DesignProvider scheme={scheme}>
          <DesignGallery initialScheme={scheme} initialBrand={brand as GalleryBrand} />
        </DesignProvider>,
      );
      for (const title of ['Buttons', 'Lists', 'Cards', 'Empty state', 'Type ramp', 'Icons']) {
        expect(screen.getByRole('header', { name: title })).toBeTruthy();
      }
    },
  );

  it('sample brands are corrected, never dropped (§3.3): each one recolours the accent', () => {
    for (const [name, brand] of Object.entries(GALLERY_BRANDS)) {
      for (const scheme of ['light', 'dark'] as const) {
        const design = resolveDesign({ scheme, platform: 'ios', brand });
        expect([name, scheme, design.branded]).toEqual([name, scheme, brand !== null]);
      }
    }
  });
});
