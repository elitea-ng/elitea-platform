/**
 * The toggle applies a mode at once and then stores it for the user's other
 * clients; a failed save keeps the local choice.
 */
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useColorScheme } from '@mui/material/styles';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import ThemeModeToggle from './ThemeModeToggle';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/theme`;

let mode: string | undefined;
function Probe(): null {
  mode = useColorScheme().mode;
  return null;
}

function renderToggle() {
  render(
    <BrandThemeProvider>
      <ThemeModeToggle />
      <Probe />
    </BrandThemeProvider>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  clearNamespace();
  localStorage.clear();
});

describe('ThemeModeToggle', () => {
  it('applies the mode, caches it, and saves it to the server', async () => {
    const saved: unknown[] = [];
    server.use(http.put(URL, async ({ request }) => {
      saved.push(await request.json());
      return HttpResponse.json({ theme_mode: 'light' });
    }));
    renderToggle();
    fireEvent.click(await screen.findByRole('button', { name: /^light/i }));
    await waitFor(() => expect(mode).toBe('light'));
    await waitFor(() => expect(localStorage.getItem('el-mode')).toBe('light'));
    await waitFor(() => expect(saved).toEqual([{ theme_mode: 'light' }]));
  });

  it('keeps the local choice when the save fails', async () => {
    let attempts = 0;
    server.use(http.put(URL, () => {
      attempts += 1;
      return HttpResponse.json({ error: 'no' }, { status: 500 });
    }));
    renderToggle();
    fireEvent.click(await screen.findByRole('button', { name: /^system/i }));
    await waitFor(() => expect(attempts).toBe(1));
    expect(mode).toBe('system');
  });
});
