import { describe, expect, it } from 'vitest';

import { useCheckStoredConfigurationConnection, useTestConfigurationConnection } from './api/useConfigurations';
import { classifySchemaField, configurationSectionsOf, initialDataForSchema } from './lib/schemaField';
import { useCredentialWarningModal } from './model/useCredentialWarningModal';
import { CredentialsControls } from './ui/CredentialsControls';
import { CredentialsTabBar } from './ui/CredentialsTabBar';
import { CredentialWarningBanner } from './ui/CredentialWarningBanner';
import { CredentialWarningModal } from './ui/CredentialWarningModal';

import * as slice from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is deliberately the value-export subset only. Precedent:
 * `features/toolkits/index.test.ts`.
 *
 * A7-api-model adversarial-review regression test (consolidating an A7-ui
 * pass through the same file): `useCredentialWarningModal` (plus the
 * `lib/credentialWarning.ts` pure helpers it wraps, and the sibling
 * `CredentialWarningModal`/`CredentialWarningBanner` UI it was built for)
 * were fully built and tested but not reachable from `index.ts` at all
 * (finding: "not exported ... and has zero live importers anywhere in the
 * app"). A first pass added `CredentialWarningModal`/
 * `useCredentialWarningModal` as two flat exports, which is what this test
 * originally pinned — but a third flat export for
 * `CredentialWarningBanner` would have breached the §3.5 20-export budget,
 * so this pass grouped all three into one `CredentialWarning` export
 * instead (see `index.ts`'s own doc comment for the full rationale). This
 * test would have caught either regression — the original "not exported at
 * all" state, and a re-ungrouping that silently drops one of the three off
 * the object again.
 *
 * `CredentialsActions` groups `CredentialsTabBar` + `CredentialsControls`
 * the same way, for the budget breach that grouping the warning trio left
 * behind: three flat exports had become one, but the barrel was still at
 * 21/20 and `node scripts/check-budgets.mjs` was failing on its
 * `slice-public-api` row. That gate is not part of this suite, which is why
 * the assertion below only ever sanity-checked the RUNTIME half and never
 * saw the real breach — see `index.ts`'s own doc comment for why this pair
 * is one unit.
 *
 * `CredentialConnectionChecks` is the third grouping, added when the SAVED-row
 * check landed: `useTestConfigurationConnection` (payload form) and
 * `useCheckStoredConfigurationConnection` (no-body form) are the two halves of
 * one control, and the barrel was again at 20/20. The flat
 * `useTestConfigurationConnection` export is gone on purpose — a caller that
 * still reaches for it by name on a saved row is reaching for the form that
 * cannot work there, so making it unavailable is part of the fix.
 */
const PUBLIC_SURFACE = [
  'useAvailableConfigurationsType',
  'useConfigurationDetail',
  'useConfigurationsList',
  'useCreateConfiguration',
  'useDeleteConfiguration',
  'useUpdateConfiguration',
  'CredentialConnectionChecks',
  'SchemaField',
  'extractInformationFromCredentialError',
  'generateCredentialTagList',
  'normalizeCredentialPage',
  'useCredentialValidation',
  'CredentialsSelect',
  'CredentialWarning',
  'CredentialsActions',
] as const;

describe('features/credentials public surface (A7-api-model)', () => {
  it('exports the full documented runtime set', () => {
    const exported = Object.keys(slice);
    for (const name of PUBLIC_SURFACE) {
      expect(exported).toContain(name);
    }
  });

  it('does not exceed the §3.5 20-symbol budget on the runtime half of the surface', () => {
    // Value + type exports both count toward the real budget gate
    // (scripts/check-budgets.mjs counts source-level export statements, not
    // just the runtime namespace) — this test only sanity-checks the
    // RUNTIME half; the real gate is `node scripts/check-budgets.mjs`.
    expect(Object.keys(slice).length).toBeLessThanOrEqual(20);
  });

  it('groups the credential-change-warning hook, modal, and banner under CredentialWarning, wired to their real implementations', () => {
    expect(slice.CredentialWarning.useModal).toBe(useCredentialWarningModal);
    expect(slice.CredentialWarning.Modal).toBe(CredentialWarningModal);
    expect(slice.CredentialWarning.Banner).toBe(CredentialWarningBanner);
  });

  it('groups the credential actions row under CredentialsActions, wired to their real implementations', () => {
    expect(slice.CredentialsActions.TabBar).toBe(CredentialsTabBar);
    expect(slice.CredentialsActions.Controls).toBe(CredentialsControls);
  });

  it('groups the two connection checks under CredentialConnectionChecks, wired to their real implementations', () => {
    expect(slice.CredentialConnectionChecks.useUnsaved).toBe(useTestConfigurationConnection);
    expect(slice.CredentialConnectionChecks.useStored).toBe(useCheckStoredConfigurationConnection);
  });

  it('no longer exposes the two actions-row components as flat exports', () => {
    // The whole point of the grouping is that it costs one budget slot
    // instead of two; a re-added flat alias would silently put the
    // `slice-public-api` row back over budget without failing any test here.
    const exported: readonly string[] = Object.keys(slice);
    expect(exported).not.toContain('CredentialsTabBar');
    expect(exported).not.toContain('CredentialsControls');
    expect(exported).not.toContain('useTestConfigurationConnection');
    expect(exported).not.toContain('classifySchemaField');
    expect(exported).not.toContain('initialDataForSchema');
  });

  it('groups the schema-field classifier, its section reader and the form seeder under SchemaField', () => {
    expect(slice.SchemaField.classify).toBe(classifySchemaField);
    expect(slice.SchemaField.configurationSections).toBe(configurationSectionsOf);
    expect(slice.SchemaField.initialData).toBe(initialDataForSchema);
  });
});
