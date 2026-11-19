/**
 * The Microsoft app dialog's pure form logic. The cases that matter are the ones a
 * form gets wrong silently: an untouched secret field must send NOTHING (an empty
 * string would clear the sealed secret), an edit must never start with the secret
 * filled in, and the ceiling of each type has its own shape.
 */
import { describe, expect, it } from 'vitest';

import type { MicrosoftApp, MicrosoftAppTypePermissions } from '@/shared/api/generated/model';

import {
  emptyMicrosoftAppForm,
  isFormRefusal,
  microsoftAppFormOf,
  microsoftAppRequest,
  parseProjectIds,
  toggleName,
} from './adminMicrosoftAppForm';

const TYPES: MicrosoftAppTypePermissions[] = [
  {
    type: 'outlook',
    api: 'Microsoft Graph',
    offered: ['Mail.Read', 'Mail.ReadWrite', 'Mail.Send'].map((name) => ({ name, admin_consent: false })),
    default: ['Mail.Read'],
  },
  { type: 'ms_graph', api: 'Microsoft Graph', offered: [], default: ['User.Read'] },
  { type: 'dataverse', api: 'Dynamics CRM', offered: [], default: ['user_impersonation'] },
];

const APP: MicrosoftApp = {
  id: '11111111-1111-4111-8111-111111111111',
  name: 'Corp',
  description: 'Company app',
  provider: 'entra',
  client_id: '7f8d0c1e-0000-4000-8000-000000000001',
  authority: 'https://login.microsoftonline.com/organizations',
  secret_set: true,
  secret_expires_at: '2027-06-30T00:00:00Z',
  credential_types: ['outlook', 'ms_graph', 'dataverse'],
  scope_ceiling: { outlook: ['Mail.Read', 'Mail.Send'], ms_graph: ['Files.Read', 'Sites.Read.All'] },
  effective_ceiling: { outlook: ['Mail.Read', 'Mail.Send'], ms_graph: ['Files.Read', 'Sites.Read.All'] },
  auto_refresh_token: false,
  project_scope: 'selected',
  project_ids: [3, 9],
  state: 'active',
  state_reason: '',
  created_at: '2026-11-18T00:00:00Z',
  updated_at: '2026-11-18T00:00:00Z',
  revision: 2,
  checklist: {
    redirect_uri: 'https://elitea.example.com/api/v2/microsoft_login:callback',
    account_types: 'multi',
    permissions: [],
    admin_consent_url: '',
    needs_admin_consent: false,
    secret_expiry: 'ok',
    secret_days_left: 100,
  },
};

function bodyOf(form: ReturnType<typeof emptyMicrosoftAppForm>) {
  const request = microsoftAppRequest(form);
  if (isFormRefusal(request)) throw new Error('unexpected refusal');
  return { ...request, scope_ceiling: request.scope_ceiling ?? {} };
}

describe('microsoftAppRequest', () => {
  it('sends no secret key at all when the secret field was not touched', () => {
    const form = { ...microsoftAppFormOf(APP, TYPES) };
    expect('secret' in bodyOf(form)).toBe(false);
  });

  it('sends the typed secret, and an empty string only when the administrator asked to clear it', () => {
    const typed = bodyOf({ ...microsoftAppFormOf(APP, TYPES), secret: 'new-secret' });
    expect(typed.secret).toBe('new-secret');
    const cleared = bodyOf({ ...microsoftAppFormOf(APP, TYPES), clearSecret: true, secret: 'ignored' });
    expect(cleared.secret).toBe('');
  });

  it('shapes the ceiling per type: ticked names, typed names, and none for Dataverse', () => {
    const form = microsoftAppFormOf(APP, TYPES);
    expect(bodyOf(form).scope_ceiling).toEqual({
      outlook: ['Mail.Read', 'Mail.Send'],
      ms_graph: ['Files.Read', 'Sites.Read.All'],
    });
    const free = { ...form, typed: { ms_graph: ' Files.Read,  Sites.Read.All\nMail.Read ' } };
    expect(bodyOf(free).scope_ceiling.ms_graph).toEqual(['Files.Read', 'Sites.Read.All', 'Mail.Read']);
    const none = { ...form, typed: { ms_graph: '' } };
    expect('ms_graph' in bodyOf(none).scope_ceiling).toBe(false);
  });

  it('leaves out the ceiling of a type the app no longer serves', () => {
    const form = { ...microsoftAppFormOf(APP, TYPES), types: ['ms_graph'] };
    expect(Object.keys(bodyOf(form).scope_ceiling)).toEqual(['ms_graph']);
  });

  it('carries the project scope, the expiry and trimmed text', () => {
    const body = bodyOf({
      ...microsoftAppFormOf(APP, TYPES),
      name: '  Corp  ',
      clientId: ' 7f8d0c1e-0000-4000-8000-000000000001 ',
      secretExpiresAt: '',
    });
    expect(body).toMatchObject({
      name: 'Corp',
      client_id: '7f8d0c1e-0000-4000-8000-000000000001',
      project_scope: 'selected',
      project_ids: [3, 9],
      secret_expires_at: null,
      auto_refresh_token: false,
    });
  });

  it('refuses a project id that is not a positive integer before any request is sent', () => {
    const refusal = microsoftAppRequest({ ...microsoftAppFormOf(APP, TYPES), projectIds: '3, abc' });
    expect(isFormRefusal(refusal)).toBe(true);
    // With the scope on all, the ids are not read at all.
    const all = bodyOf({ ...microsoftAppFormOf(APP, TYPES), projectScope: 'all', projectIds: 'abc' });
    expect(all.project_ids).toEqual([]);
  });
});

describe('microsoftAppFormOf', () => {
  it('starts an edit with an empty secret field: the secret is never read back', () => {
    const form = microsoftAppFormOf(APP, TYPES);
    expect(form.secret).toBe('');
    expect(form.clearSecret).toBe(false);
    expect(form.secretExpiresAt).toBe('2027-06-30');
    expect(form.projectIds).toBe('3, 9');
    expect(form.typed.ms_graph).toBe('Files.Read Sites.Read.All');
  });

  it('starts a new app with the least-privilege defaults ticked and nothing typed', () => {
    const form = emptyMicrosoftAppForm(TYPES);
    expect(form.ticked.outlook).toEqual(['Mail.Read']);
    expect(form.types).toEqual([]);
    expect(form.secret).toBe('');
    expect(form.authority).toBe('https://login.microsoftonline.com/organizations');
  });
});

describe('helpers', () => {
  it('parses project ids strictly', () => {
    expect(parseProjectIds('1, 2 3')).toEqual([1, 2, 3]);
    expect(parseProjectIds('')).toEqual([]);
    expect(parseProjectIds('0')).toBeUndefined();
    expect(parseProjectIds('1.5')).toBeUndefined();
    expect(parseProjectIds('-2')).toBeUndefined();
  });

  it('toggles a name and keeps the offered order', () => {
    const offered = ['Mail.Read', 'Mail.ReadWrite', 'Mail.Send'];
    expect(toggleName(['Mail.Read'], offered, 'Mail.Send')).toEqual(['Mail.Read', 'Mail.Send']);
    expect(toggleName(['Mail.Send', 'Mail.Read'], offered, 'Mail.Send')).toEqual(['Mail.Read']);
  });
});
