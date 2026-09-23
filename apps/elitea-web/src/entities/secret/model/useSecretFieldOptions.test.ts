/**
 * The pure halves of `useSecretFieldOptions` (#441). The hook itself is
 * covered end-to-end, against a real permission list and a real secret list,
 * by `features/toolkits/ui/form/ToolBase/SecretFieldInput.permission.test.tsx`
 * — the surface where the defect actually showed.
 */
import { describe, expect, it } from 'vitest';

import { PERMISSIONS } from '@/shared/lib/permissions';

import { SECRETS_SETTINGS_PATH, buildSecretsSettingsHref, readSecretFieldGrants, secretCreateLabel, secretReference, toSecretOptions } from './useSecretFieldOptions';

describe('secretReference', () => {
  it('wraps a name in the reference syntax the backend expands', () => {
    expect(secretReference('prod_api_key')).toBe('{{secret.prod_api_key}}');
  });
});

describe('toSecretOptions', () => {
  it('labels each option with the name and values it with the reference', () => {
    expect(toSecretOptions([{ name: 'alpha', secretName: 'alpha', isDefault: false }])).toEqual([{ label: 'alpha', value: '{{secret.alpha}}' }]);
  });

  it('gives an empty list for no secrets', () => {
    expect(toSecretOptions([])).toEqual([]);
  });
});

describe('buildSecretsSettingsHref', () => {
  it('is root-relative when no base path is configured', () => {
    expect(buildSecretsSettingsHref('')).toBe(SECRETS_SETTINGS_PATH);
  });

  it('joins a base path without doubling the separator', () => {
    // `vite_base_uri` defaults to `/app/`, with the trailing slash — the exact
    // shape `normalizeBasename`'s own header exists for.
    expect(buildSecretsSettingsHref('/app/')).toBe(`/app${SECRETS_SETTINGS_PATH}`);
  });

  it('carries the createSecret flag the settings route validates', () => {
    expect(buildSecretsSettingsHref('')).toContain('createSecret=1');
  });
});

describe('secretCreateLabel', () => {
  // #925/ELITEA-1069,1074: scope-aware "Create new secret" wording.
  it('reads "New Project Secret" for a team project', () => {
    expect(secretCreateLabel(true)).toBe('New Project Secret');
  });

  it('reads "New Private Secret" for a non-team (personal) project', () => {
    expect(secretCreateLabel(false)).toBe('New Private Secret');
  });

  it('is undefined when the caller has no notion of project scope, so the generic fallback renders', () => {
    expect(secretCreateLabel(undefined)).toBeUndefined();
  });
});

describe('readSecretFieldGrants', () => {
  it('reports both grants when both are enabled', () => {
    const grants = readSecretFieldGrants([
      { name: PERMISSIONS.secrets.list, enabled: true },
      { name: PERMISSIONS.secrets.create, enabled: true },
    ]);
    expect(grants).toEqual({ canList: true, canCreate: true });
  });

  it('withholds a permission that is present but disabled', () => {
    const grants = readSecretFieldGrants([
      { name: PERMISSIONS.secrets.list, enabled: true },
      { name: PERMISSIONS.secrets.create, enabled: false },
    ]);
    expect(grants).toEqual({ canList: true, canCreate: false });
  });

  it('withholds both while the permission list is still absent', () => {
    expect(readSecretFieldGrants(undefined)).toEqual({ canList: false, canCreate: false });
  });
});
