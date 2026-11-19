/**
 * The `oauth_app_select` field (ADR-0057 decision 5): "Default app" or "Custom app".
 *
 * What each case guards:
 *  1. NOTHING SHIPS ENABLED: an empty registry renders no field at all, so the
 *     credential form is exactly what it was before the registry existed.
 *  2. The offers come from the project-scoped read for THIS credential type and
 *     project, and the select writes only the app's id; Custom app clears it.
 *  3. A credential that names an app that is no longer offered keeps showing it, so
 *     the form never silently reads as Custom app for a credential that still resolves
 *     the app.
 *  4. The administrator's `microsoft.custom_apps` switch removes Custom app from the choice.
 *  5. The field needs the credential type: without it, no request and no field.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { resolveFieldKind } from './ToolBaseProperty.kinds';
import { OauthAppSelectField, type OauthAppSelectFieldProps } from './OauthAppSelectField';

const APP = { id: '11111111-1111-4111-8111-111111111111', name: 'Company Microsoft', description: '', auto_refresh_token: true, scope_ceiling: ['Mail.Read'] };

let requests: string[] = [];

function offers(apps: readonly unknown[], customAllowed = true): void {
  server.use(
    http.get('*/microsoft_oauth_apps/:projectId', ({ request }) => {
      requests.push(new URL(request.url).pathname + new URL(request.url).search);
      return HttpResponse.json({ credential_type: 'outlook', apps, custom_app_allowed: customAllowed });
    }),
  );
}

function props(overrides: Partial<OauthAppSelectFieldProps> = {}): OauthAppSelectFieldProps {
  return {
    label: 'App',
    description: undefined,
    required: false,
    credentialType: 'outlook',
    projectId: 7,
    value: undefined,
    disabled: false,
    onChange: () => undefined,
    ...overrides,
  };
}

function renderField(fieldProps: OauthAppSelectFieldProps) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <OauthAppSelectField {...fieldProps} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  requests = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('resolveFieldKind', () => {
  it('classifies the SDK schema field by its ui_component', () => {
    expect(resolveFieldKind({ key: 'oauth_app_id', schema: { type: 'string', ui_component: 'oauth_app_select' }, isSecret: false })).toBe(
      'oauthAppSelect',
    );
  });
});

describe('OauthAppSelectField', () => {
  it('renders nothing while the registry has no app for this type and project', async () => {
    offers([]);
    const { container } = renderField(props());
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(container).toBeEmptyDOMElement();
  });

  it('asks for the offers of this credential type in this project', async () => {
    offers([APP]);
    renderField(props());
    expect(await screen.findByRole('combobox')).toBeVisible();
    expect(requests).toEqual(['/api/v2/microsoft_oauth_apps/7?credential_type=outlook']);
  });

  it('offers Custom app and the apps, and writes only the id of the one picked', async () => {
    offers([APP]);
    const changes: Array<string | undefined> = [];
    const user = userEvent.setup();
    renderField(props({ onChange: (appId) => changes.push(appId) }));

    await user.click(await screen.findByRole('combobox'));
    expect(await screen.findByRole('option', { name: 'Custom app' })).toBeVisible();
    await user.click(screen.getByRole('option', { name: 'Company Microsoft' }));
    expect(changes).toEqual([APP.id]);
  });

  it('clears the id when Custom app is picked', async () => {
    offers([APP]);
    const changes: Array<string | undefined> = [];
    const user = userEvent.setup();
    renderField(props({ value: APP.id, onChange: (appId) => changes.push(appId) }));

    await user.click(await screen.findByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Custom app' }));
    expect(changes).toEqual([undefined]);
  });

  it('keeps showing an app the credential names that is no longer offered', async () => {
    offers([]);
    renderField(props({ value: '99999999-9999-4999-8999-999999999999' }));
    expect(await screen.findByRole('combobox')).toHaveTextContent('Default app (not available)');
  });

  it('removes Custom app from the choice when the administrator turned it off', async () => {
    offers([APP], false);
    const user = userEvent.setup();
    renderField(props());
    await user.click(await screen.findByRole('combobox'));
    expect(await screen.findByRole('option', { name: 'Company Microsoft' })).toBeVisible();
    expect(screen.queryByRole('option', { name: 'Custom app' })).not.toBeInTheDocument();
  });

  it('does not ask without the credential type or the project', async () => {
    offers([APP]);
    const { container } = renderField(props({ credentialType: undefined }));
    renderField(props({ projectId: undefined }));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(requests).toEqual([]);
    expect(container).toBeEmptyDOMElement();
  });
});
