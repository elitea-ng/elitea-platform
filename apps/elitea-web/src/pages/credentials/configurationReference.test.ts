/**
 * configurationReference.test.ts — the value shape a `'configuration'` schema
 * field reads and writes.
 *
 * BLOCKER regression (D2). The project-side create-configuration form
 * rendered `ai_credentials` as a free-text box and stored the bare string
 * `"vllm_creds"`. The gateway's `modelCredentialRef` decodes an OBJECT
 * (`{elitea_title, private}`), elitea-main's create normalizer refuses a
 * non-object, and the admin dialog has always written the object. A model
 * saved through the project form therefore named a credential nothing could
 * resolve.
 */
import { describe, expect, it } from 'vitest';

import {
  buildConfigurationReference,
  configurationReferenceTitle,
  toConfigurationReference,
} from './configurationReference';

describe('toConfigurationReference', () => {
  it('reads the stored object shape', () => {
    expect(toConfigurationReference({ elitea_title: 'vllm_creds', private: false })).toEqual({
      elitea_title: 'vllm_creds',
      private: false,
    });
  });

  it('carries a private link through unchanged', () => {
    expect(toConfigurationReference({ elitea_title: 'personal_creds', private: true })).toEqual({
      elitea_title: 'personal_creds',
      private: true,
    });
  });

  /** `alita_title` is the pre-debranding spelling; a database that has not run the rename task still holds it. */
  it('accepts the pre-debranding title key on read', () => {
    expect(toConfigurationReference({ alita_title: 'team-azure' })).toEqual({
      elitea_title: 'team-azure',
      private: false,
    });
  });

  it('names nothing for a bare string, an empty object, or a non-object', () => {
    expect(toConfigurationReference('vllm_creds')).toBeNull();
    expect(toConfigurationReference({})).toBeNull();
    expect(toConfigurationReference({ elitea_title: '' })).toBeNull();
    expect(toConfigurationReference(null)).toBeNull();
    expect(toConfigurationReference(undefined)).toBeNull();
    expect(toConfigurationReference(['vllm_creds'])).toBeNull();
  });
});

describe('configurationReferenceTitle', () => {
  it('shows the linked title for a well-formed link', () => {
    expect(configurationReferenceTitle({ elitea_title: 'vllm_creds', private: false })).toBe('vllm_creds');
  });

  /**
   * A row already damaged by the defect still shows what it names, so the
   * user can see the link and repair it by re-saving through the picker.
   */
  it('still shows a bare string left behind by the old free-text field', () => {
    expect(configurationReferenceTitle('vllm_creds')).toBe('vllm_creds');
  });

  it('is empty when nothing is linked', () => {
    expect(configurationReferenceTitle(null)).toBe('');
    expect(configurationReferenceTitle(undefined)).toBe('');
    expect(configurationReferenceTitle({})).toBe('');
  });
});

describe('buildConfigurationReference', () => {
  it('always writes the object shape, with the debranded key', () => {
    expect(buildConfigurationReference('vllm_creds', false)).toEqual({
      elitea_title: 'vllm_creds',
      private: false,
    });
    expect(Object.keys(buildConfigurationReference('vllm_creds', true))).toEqual(['elitea_title', 'private']);
  });
});
