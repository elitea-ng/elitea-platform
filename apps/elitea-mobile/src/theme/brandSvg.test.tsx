import { render, waitFor } from '@testing-library/react-native';

import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import type { BaseFetch } from '@/api/middleware';
import { BrandLogo } from '@/theme/BrandLogo';
import { fetchBrandSvg, sanitizeBrandSvg } from '@/theme/brandSvg';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const mockSvg = { uri: jest.fn(), xml: jest.fn() };
jest.mock('react-native-svg', () => {
  const { View } = jest.requireActual('react-native');
  return {
    SvgUri: (props: { uri: string }) => {
      mockSvg.uri(props.uri);
      return <View testID="svg-uri" />;
    },
    SvgXml: (props: { xml: string }) => {
      mockSvg.xml(props.xml);
      return <View testID="svg-xml" />;
    },
  };
});

const LOGO = `${FIXTURE_ORIGIN}/api/v2/branding/assets/logo.svg`;
const CLEAN =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><defs><linearGradient id="g"/></defs>' +
  '<path d="M0 0h10v10z" fill="url(#g)"/><use href="#p"/><use xlink:href=\'#p\'/></svg>';

beforeEach(() => {
  mockSvg.uri.mockClear();
  mockSvg.xml.mockClear();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('sanitizeBrandSvg', () => {
  it('keeps a document whose references are all local (#id)', () => {
    expect(sanitizeBrandSvg(CLEAN)).toBe(CLEAN);
  });

  it.each([
    ['a remote <image>', '<svg><image href="https://tracker.example/p.png"/></svg>'],
    ['a namespaced remote <image>', '<svg><svg:image xlink:href="https://tracker.example/p.png"/></svg>'],
    ['a remote fill', '<svg><rect fill="url(https://tracker.example/x.svg#a)"/></svg>'],
    ['a quoted remote url()', "<svg><rect style=\"fill:url('//tracker.example/x#a')\"/></svg>"],
    ['an external <use>', '<svg><use href="https://tracker.example/sprite.svg#a"/></svg>'],
    ['a data: <use>', '<svg><use href="data:image/svg+xml,&lt;svg/&gt;"/></svg>'],
    ['an entity-encoded href', '<svg><use href="&#104;ttps://tracker.example/s.svg#a"/></svg>'],
    ['an unquoted href', '<svg><use href=https://tracker.example/s.svg#a /></svg>'],
    ['a <foreignObject>', '<svg><foreignObject><div/></foreignObject></svg>'],
    ['a <script>', '<svg><script>alert(1)</script></svg>'],
    ['a DOCTYPE', '<!DOCTYPE svg [<!ENTITY x SYSTEM "https://tracker.example/">]><svg>&x;</svg>'],
    ['a CSS @import', '<svg><style>@import "https://tracker.example/a.css";</style></svg>'],
    ['not an SVG', '<html><body/></html>'],
  ])('refuses %s', (_label, xml) => {
    expect(sanitizeBrandSvg(xml)).toBeNull();
  });
});

function serve(body: string, status = 200, url?: string): jest.MockedFunction<BaseFetch> {
  return jest.fn(async (input: Request) => {
    const response = new Response(body, { status, headers: { 'Content-Type': 'image/svg+xml' } });
    Object.defineProperty(response, 'url', { value: url ?? input.url });
    return response;
  });
}

describe('fetchBrandSvg', () => {
  it('returns a clean same-origin document', async () => {
    expect(await fetchBrandSvg(LOGO, { fetch: serve(CLEAN) })).toBe(CLEAN);
  });

  it('refuses a logo redirected to another origin', async () => {
    expect(await fetchBrandSvg(LOGO, { fetch: serve(CLEAN, 200, 'https://cdn.tracker.example/logo.svg') })).toBeNull();
  });

  it('refuses a document with an outside reference, and a non-200', async () => {
    expect(
      await fetchBrandSvg(LOGO, { fetch: serve('<svg><image href="https://tracker.example/p.png"/></svg>') }),
    ).toBeNull();
    expect(await fetchBrandSvg(LOGO, { fetch: serve(CLEAN, 404) })).toBeNull();
  });
});

describe('BrandLogo', () => {
  it('never hands an SVG to SvgUri (plain fetch, no reference checks); renders vetted XML only', async () => {
    const fetchSvg = jest.fn(async () => CLEAN);
    const screen = await render(<BrandLogo image={{ uri: LOGO, kind: 'svg' }} fetchSvg={fetchSvg} />);
    await waitFor(() => expect(screen.getByTestId('svg-xml')).toBeTruthy());
    expect(fetchSvg).toHaveBeenCalledWith(LOGO);
    expect(mockSvg.uri).not.toHaveBeenCalled();
    expect(mockSvg.xml).toHaveBeenCalledWith(CLEAN);
  });

  it('renders nothing when the document is refused', async () => {
    const fetchSvg = jest.fn(async () => null);
    const screen = await render(<BrandLogo image={{ uri: LOGO, kind: 'svg' }} fetchSvg={fetchSvg} />);
    await waitFor(() => expect(fetchSvg).toHaveBeenCalled());
    expect(screen.queryByTestId('svg-xml')).toBeNull();
    expect(screen.queryByTestId('svg-uri')).toBeNull();
  });
});
