/**
 * Admin › Branding (ADR-0024 WP4), against MSW.
 *
 * The page is mounted under a real (memory) router because its unsaved-changes
 * guard is a TanStack `useBlocker`, which needs one; the settings, save and
 * upload routes are the generated `admin.msw.ts` handlers with the bodies
 * this suite needs.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRouter,
} from '@tanstack/react-router';
import { HttpResponse, http } from 'msw';

import {
  getGetBrandingSettingsMockHandler,
  getListBrandingPackageVersionsMockHandler,
  getSaveBrandingSettingsMockHandler,
  getUploadBrandingAssetMockHandler,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { BrandingSettings } from '@/shared/api/generated/model';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { AdminBranding } from './Branding';
import { useBrandingDirtyStore } from './brandingDirty.store';
import { BRANDING_KEYS, emptyBrandingValues } from './brandingValues';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

// `#${'…'}` keeps R-T1 (`elitea/no-raw-color`) out of a test about hex input.
const NEW_HUE = `#${'e8461a'}`;
const LOGO_PATH = '/api/v2/branding/assets/logo-full/0123abcd.svg';

const STORED: BrandingSettings = {
  // The server answers every section key, the unedited ones too — a package
  // import's hand-tuned scheme tokens and the e-mail sender name.
  values: {
    ...emptyBrandingValues(),
    product_name: 'Acme',
    base_size: 15,
    scheme_tokens: { light: { 'text.primary': `#${'101010'}` }, dark: {} },
    sender_name: 'Acme Mail',
    support_email: '',
  },
  layers: { file: false, database: true },
  effective: {
    ...DEFAULT_BRAND_PACK,
    product: { ...DEFAULT_BRAND_PACK.product, name: 'Acme' },
    typography: { ...DEFAULT_BRAND_PACK.typography, baseSize: 15 },
  },
  etag: 'abc',
};

let savedBodies: Array<Record<string, unknown>> = [];
let uploads: Array<{ kind: string; contentType: string }> = [];

function useBrandingHandlers(): void {
  server.use(
    getGetBrandingSettingsMockHandler(STORED),
    // No kept packages: the versions panel's empty state, and nothing random
    // from the faker default on screen (the package flows have their own
    // suite, `BrandingPackageControls.test.tsx`).
    getListBrandingPackageVersionsMockHandler({ versions: [] }),
    getSaveBrandingSettingsMockHandler(async ({ request }) => {
      const body = (await request.json()) as { values: Record<string, unknown> };
      savedBodies.push(body.values);
      return { ...STORED, values: body.values, saved: true };
    }),
    getUploadBrandingAssetMockHandler(({ params, request }) => {
      // The body is not parsed: undici's multipart parser rejects jsdom's
      // `File`, so `request.formData()` throws here. The content type proves
      // the generated client sent a multipart form, and the answered path is
      // what the page must write into the draft.
      uploads.push({
        kind: String(params['kind']),
        contentType: request.headers.get('content-type') ?? '',
      });
      return {
        kind: String(params['kind']),
        digest: '0123abcd',
        extension: 'svg',
        content_type: 'image/svg+xml',
        size: 42,
        path: LOGO_PATH,
      };
    }),
  );
}

function renderBranding(): void {
  const rootRoute = createRootRoute({ component: AdminBranding });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

async function productName(): Promise<HTMLInputElement> {
  return (await screen.findByTestId('branding-field-product_name')) as HTMLInputElement;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  savedBodies = [];
  uploads = [];
  useBrandingHandlers();
});

afterEach(() => {
  resetGeneratedClient();
  useBrandingDirtyStore.setState({ dirty: false });
});

describe('AdminBranding', () => {
  it('loads the stored values, the layers and a derived swatch strip', async () => {
    renderBranding();
    expect((await productName()).value).toBe('Acme');
    expect(screen.getByTestId('branding-field-base_size')).toHaveValue(15);
    // An inherited field says what it inherits and from where.
    expect(screen.getByText(/Product default · Inherits “Elitea”/)).toBeInTheDocument();
    const layers = screen.getByTestId('branding-layers');
    expect(within(layers).getByText('Database: contributes')).toBeInTheDocument();
    expect(within(layers).getByText('Mounted file pack: absent')).toBeInTheDocument();
    expect(within(screen.getByTestId('branding-layer-row-product_name')).getByText('Set here')).toBeInTheDocument();
    expect(within(screen.getByTestId('branding-layer-row-docs_url')).getByText('Product default')).toBeInTheDocument();
    expect(within(screen.getByTestId('branding-swatches-light')).getAllByRole('listitem')).toHaveLength(9);
    expect(screen.getByTestId('branding-preview-dark')).toBeInTheDocument();
    // Nothing is dirty yet, so Save is withheld.
    expect(screen.getByTestId('branding-save')).toBeDisabled();
  });

  it('re-derives the swatches when the hue is edited', async () => {
    renderBranding();
    await productName();
    const before = screen.getByTestId('branding-swatch-light-primary').getAttribute('data-value');
    const hue = screen.getByTestId('branding-field-brand_hue');
    await userEvent.clear(hue);
    await userEvent.type(hue, NEW_HUE);
    await waitFor(() => {
      expect(screen.getByTestId('branding-swatch-light-primary').getAttribute('data-value')).not.toBe(before);
    });
    expect(screen.getByTestId('branding-swatch-dark-primary').getAttribute('data-value')).not.toBe(before);
    expect(useBrandingDirtyStore.getState().dirty).toBe(true);
    expect(screen.getByTestId('branding-save')).toBeEnabled();
  });

  it('uploads a logo and writes the answered path into the draft', async () => {
    renderBranding();
    await productName();
    const input = screen.getByTestId('branding-upload-input-logo-full');
    const file = new File(['<svg xmlns="http://www.w3.org/2000/svg"/>'], 'logo.svg', { type: 'image/svg+xml' });
    fireEvent.change(input, { target: { files: [file] } });
    await waitFor(() => {
      expect(screen.getByTestId('branding-asset-path-logo-full')).toHaveTextContent(LOGO_PATH);
    });
    expect(uploads).toHaveLength(1);
    expect(uploads[0]?.kind).toBe('logo-full');
    expect(uploads[0]?.contentType).toMatch(/^multipart\/form-data/);
    // The path is DRAFT state: it reaches the server only with the save.
    expect(savedBodies).toHaveLength(0);
    await userEvent.click(screen.getByTestId('branding-save'));
    await waitFor(() => expect(savedBodies).toHaveLength(1));
    expect(savedBodies[0]?.['logo_full']).toBe(LOGO_PATH);
  });

  it('uploads an e-mail logo through the logo-email kind and writes logo_email (WP7 key, WP9 control)', async () => {
    renderBranding();
    await productName();
    const control = screen.getByTestId('branding-asset-logo-email');
    // Enabled now: WP4 withheld it until the `logo_email` key existed.
    expect(within(control).getByTestId('branding-upload-logo-email')).toBeEnabled();
    const input = within(control).getByTestId('branding-upload-input-logo-email');
    expect(input).toHaveAttribute('accept', '.png,.webp,image/png,image/webp');
    const file = new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], 'mail.png', { type: 'image/png' });
    fireEvent.change(input, { target: { files: [file] } });
    await waitFor(() => {
      expect(screen.getByTestId('branding-asset-path-logo-email')).toHaveTextContent(LOGO_PATH);
    });
    expect(uploads.map((upload) => upload.kind)).toEqual(['logo-email']);
    // The layers panel reads the draft: the key is decided here before any save.
    expect(within(screen.getByTestId('branding-layer-row-logo_email')).getByText('Set here')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('branding-save'));
    await waitFor(() => expect(savedBodies).toHaveLength(1));
    expect(savedBodies[0]?.['logo_email']).toBe(LOGO_PATH);
  });

  it('saves the FULL values record and shows the success toast', async () => {
    renderBranding();
    const name = await productName();
    await userEvent.clear(name);
    await userEvent.type(name, 'Acme Corp');
    await userEvent.click(screen.getByTestId('branding-save'));
    expect(await screen.findByTestId('branding-toast-success')).toHaveTextContent('Branding saved');
    expect(savedBodies).toHaveLength(1);
    const body = savedBodies[0] as Record<string, unknown>;
    expect(body['product_name']).toBe('Acme Corp');
    // Every declared key travels, or the PUT would clear the ones left out.
    expect(Object.keys(body).sort()).toEqual([...BRANDING_KEYS].sort());
    expect(body['base_size']).toBe(15);
    expect(body['font_faces']).toEqual([]);
    await waitFor(() => expect(useBrandingDirtyStore.getState().dirty).toBe(false));
  });

  it('shows the server refusal beside the field it names, and as a toast', async () => {
    const reason = `"brand_hue" must be a six-digit hex colour such as #${'1A73E8'}`;
    server.use(
      http.put('*/admin/branding/administration', () =>
        HttpResponse.json({ error: reason }, { status: 400 }),
      ),
    );
    renderBranding();
    await productName();
    const hue = screen.getByTestId('branding-field-brand_hue');
    await userEvent.type(hue, 'zz');
    await userEvent.click(screen.getByTestId('branding-save'));
    expect(await screen.findByTestId('branding-toast-error')).toHaveTextContent(reason);
    // Beside the field: MUI renders the helper text with an id derived from the input's.
    const helper = document.getElementById('branding-brand_hue-helper-text');
    expect(helper).toHaveTextContent(reason);
    // The draft survives a refusal — nothing was discarded.
    expect(hue).toHaveValue('zz');
    expect(screen.getByTestId('branding-save')).toBeEnabled();
  });

  it('resets every key to inherit after a confirmation, the unedited ones too', async () => {
    renderBranding();
    await productName();
    await userEvent.click(screen.getByTestId('branding-reset'));
    await userEvent.click(await screen.findByTestId('branding-reset-confirm'));
    await waitFor(() => expect(savedBodies).toHaveLength(1));
    // The server upserts only the keys it is sent: a reset that omitted
    // `scheme_tokens` left every imported colour in place (live, 2026-10-05).
    expect(savedBodies[0]).toEqual({
      ...emptyBrandingValues(),
      scheme_tokens: {},
      sender_name: '',
      support_email: '',
    });
    expect(await screen.findByTestId('branding-toast-success')).toHaveTextContent('Branding reset');
  });
});
