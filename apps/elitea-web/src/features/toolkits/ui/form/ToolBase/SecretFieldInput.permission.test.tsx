/**
 * #441 regression: the "Create new secret" entry in the secret field.
 *
 * WHY THE POSITIVE DIRECTION IS THE ONE THAT DISCRIMINATES. The entry was
 * off for every user, an administrator included, because no renderer of
 * `SecretField` supplied the `secrets` configuration at all — no option
 * list, no `canCreate`, no `onCreate`. A "hidden without the permission"
 * assertion passes against that bug for the wrong reason. So the first test
 * below GRANTS `configuration.secrets.secret.create` and asserts the entry
 * appears and works; the second withholds the grant and asserts the entry is
 * gone while the saved-secret option stays, which proves the picker itself
 * still renders and the read still ran.
 *
 * Both tests exercise `SecretFieldInput`, the toolkit form's own secret
 * renderer, not `SecretField` in isolation: the defect was in the caller
 * half, so a `shared/ui` test cannot see it.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { writePersistedProject } from '@/shared/lib/selectedProjectPersistence';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { SecretFieldInput } from './SecretFieldInput';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const SECRETS_PATH = `${BASE}/secrets/secrets/default/:projectId`;

const CREATE_ENTRY = 'Create new secret';
const SAVED_SECRET = 'prod_api_key';

/** Both queries this field runs, answered with the given grants. */
function serveSecrets(grants: readonly string[]): void {
  server.use(
    http.get(PERMISSIONS_PATH, () =>
      HttpResponse.json(grants.map((name) => ({ name, enabled: true }))),
    ),
    http.get(SECRETS_PATH, () =>
      HttpResponse.json([{ name: SAVED_SECRET, secret_name: SAVED_SECRET, is_default: false }]),
    ),
  );
}

function renderSecretFieldInput(isTeamProject?: boolean) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <SecretFieldInput
        // A `{{secret.NAME}}` value starts the field in "secret" mode, which
        // is the mode that renders the picker the entry lives in.
        value={`{{secret.${SAVED_SECRET}}}`}
        onChange={vi.fn()}
        label="API Key"
        required
        error={false}
        helperText={undefined}
        {...(isTeamProject === undefined ? {} : { isTeamProject })}
      />
    </QueryClientProvider>,
  );
}

/** Opens the saved-secret picker and returns its option labels. */
async function openPicker(): Promise<string[]> {
  const user = userEvent.setup();
  await waitFor(() => {
    expect(screen.getByRole('combobox')).toBeInTheDocument();
  });
  await user.click(screen.getByRole('combobox'));
  await waitFor(() => {
    expect(screen.getAllByRole('option').length).toBeGreaterThan(0);
  });
  return screen.getAllByRole('option').map((option) => option.textContent ?? '');
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  writePersistedProject({ id: 'proj-1', name: 'Acme' });
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('SecretFieldInput — configuration.secrets.secret.create gating (#441)', () => {
  it('shows the "Create new secret" entry, and opens the secrets page, for a caller who holds the permission', async () => {
    const openSpy = vi.spyOn(window, 'open').mockReturnValue(null);
    serveSecrets(['configuration.secrets.secret.list', 'configuration.secrets.secret.create']);

    renderSecretFieldInput();

    await waitFor(async () => {
      expect(await openPicker()).toContain(CREATE_ENTRY);
    });

    await userEvent.setup().click(screen.getByRole('option', { name: CREATE_ENTRY }));
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy.mock.calls[0]?.[0]).toContain('/settings/secrets?createSecret=1');
  });

  it('hides the entry, and keeps the saved secrets, for a caller who holds only the list permission', async () => {
    serveSecrets(['configuration.secrets.secret.list']);

    renderSecretFieldInput();

    await waitFor(async () => {
      expect(await openPicker()).toContain(SAVED_SECRET);
    });
    expect(screen.queryByRole('option', { name: CREATE_ENTRY })).not.toBeInTheDocument();
  });
});

/**
 * #902/ELITEA-0726. The scope-aware wording already existed in
 * `entities/secret`'s `secretCreateLabel`; what was missing was a caller that
 * told it the scope, so every project type got the one generic entry. These
 * assert the caller half from this leaf — the same place the #441 tests above
 * proved the `secrets` bag itself was unsupplied.
 */
describe('SecretFieldInput — the CREATE entry names the project scope (#902)', () => {
  beforeEach(() => {
    serveSecrets(['configuration.secrets.secret.list', 'configuration.secrets.secret.create']);
  });

  it('reads "New Project Secret" in a team project', async () => {
    renderSecretFieldInput(true);
    await waitFor(async () => {
      expect(await openPicker()).toContain('New Project Secret');
    });
    expect(screen.queryByRole('option', { name: CREATE_ENTRY })).not.toBeInTheDocument();
  });

  it('reads "New Private Secret" in a personal project', async () => {
    renderSecretFieldInput(false);
    await waitFor(async () => {
      expect(await openPicker()).toContain('New Private Secret');
    });
  });

  it('keeps the generic entry when the caller does not know the scope', async () => {
    renderSecretFieldInput();
    await waitFor(async () => {
      expect(await openPicker()).toContain(CREATE_ENTRY);
    });
  });
});
