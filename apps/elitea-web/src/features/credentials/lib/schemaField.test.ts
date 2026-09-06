import { describe, expect, it } from 'vitest';

import {
  classifySchemaField,
  configurationSectionsOf,
  initialDataForSchema,
  initialValueForSchemaField,
  isLikelySecretField,
} from './schemaField';

/**
 * The `data` half of the registry's real `llm_model` descriptor, copied from
 * `services/elitea-main/internal/application/configurations/
 * current_available_snapshot.json` — the exact schema the AI-Configuration
 * form renders. It is the fixture for both blockers this file covers: the
 * integer field whose name contains `token`, and the reference field that
 * must not be free text.
 */
const LLM_MODEL_DATA_SCHEMA = {
  properties: {
    ai_credentials: {
      anyOf: [{ $ref: '#/$defs/AiCredentials' }, { type: 'null' }],
      configuration_sections: ['ai_credentials'],
      default: null,
    },
    context_window: { default: 128000, title: 'Context Window', type: 'integer' },
    max_output_tokens: { default: 16000, title: 'Max Output Tokens', type: 'integer' },
    name: { title: 'Name', type: 'string' },
    supports_vision: { anyOf: [{ type: 'boolean' }, { type: 'null' }], default: true, title: 'Supports Vision' },
  },
  required: ['name', 'ai_credentials'],
} as const;

describe('isLikelySecretField', () => {
  it('matches format: password', () => {
    expect(isLikelySecretField('anything', { format: 'password' })).toBe(true);
  });

  it('matches secret: true', () => {
    expect(isLikelySecretField('anything', { secret: true })).toBe(true);
  });

  it('matches a secret-shaped key name', () => {
    expect(isLikelySecretField('api_key', undefined)).toBe(true);
    expect(isLikelySecretField('access_token', undefined)).toBe(true);
  });

  it('does not match an ordinary field', () => {
    expect(isLikelySecretField('base_url', { type: 'string' })).toBe(false);
  });

  // Regression test for the confirmed [blocker] finding: a field whose ONLY
  // secret marker is nested inside `anyOf` (the standard Pydantic
  // `Optional[SecretStr]` shape) and whose key name does NOT match the
  // crude name heuristic must still be classified as secret — otherwise it
  // renders the user's real secret value in an unmasked plain-text input.
  it('matches a password format nested inside anyOf (Optional[SecretStr] shape), even with a non-secret-looking key', () => {
    expect(
      isLikelySecretField('credentials', {
        anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }],
      }),
    ).toBe(true);
  });

  it('matches secret: true nested inside oneOf, even with a non-secret-looking key', () => {
    expect(
      isLikelySecretField('credentials', {
        oneOf: [{ type: 'string', secret: true }, { type: 'null' }],
      }),
    ).toBe(true);
  });

  it('does not false-positive on an anyOf/oneOf branch with no secret marker', () => {
    expect(
      isLikelySecretField('base_url', {
        anyOf: [{ type: 'string' }, { type: 'null' }],
      }),
    ).toBe(false);
  });

  it('ignores non-array/non-object anyOf/oneOf values instead of throwing', () => {
    expect(isLikelySecretField('base_url', { anyOf: 'not-an-array' })).toBe(false);
  });
});

describe('classifySchemaField', () => {
  it('classifies secret before type-based checks', () => {
    expect(classifySchemaField('password', { type: 'string' })).toBe('secret');
  });

  it('classifies a nested anyOf secret (Optional[SecretStr]) as secret even though its own type looks like a plain string', () => {
    expect(
      classifySchemaField('credentials', {
        type: 'string',
        anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }],
      }),
    ).toBe('secret');
  });

  it('classifies boolean', () => {
    expect(classifySchemaField('enabled', { type: 'boolean' })).toBe('boolean');
  });

  it('classifies number/integer', () => {
    expect(classifySchemaField('port', { type: 'number' })).toBe('number');
    expect(classifySchemaField('port', { type: 'integer' })).toBe('number');
  });

  it('classifies a non-empty enum', () => {
    expect(classifySchemaField('region', { type: 'string', enum: ['us', 'eu'] })).toBe('enum');
  });

  it('classifies nullable primitive and enum branches', () => {
    expect(classifySchemaField('enabled', { anyOf: [{ type: 'boolean' }, { type: 'null' }] })).toBe('boolean');
    expect(classifySchemaField('auth_type', { anyOf: [{ type: 'string', enum: ['Bearer'] }, { type: 'null' }] })).toBe('enum');
  });

  it('falls back to string', () => {
    expect(classifySchemaField('base_url', { type: 'string' })).toBe('string');
    expect(classifySchemaField('base_url', undefined)).toBe('string');
  });

  /**
   * BLOCKER regression (D1). `SECRET_NAME_RE` matched the SUBSTRING `token`,
   * so `max_output_tokens` — a declared `type: 'integer'` — classified as a
   * secret. It rendered as `<input type="password">` and serialised
   * `"16000"`, a string. `mapCurrentModelCandidate`
   * (services/elitea-main/internal/infra/db/repos/models.go) then skipped the
   * whole row with "skipping a malformed model configuration", so a model
   * saved through the AI-Configuration form never reached any model picker.
   *
   * Both halves are asserted: the declared type wins, AND the name heuristic
   * on its own no longer reads a token BUDGET as a token CREDENTIAL.
   */
  it('never masks a declared integer field, whatever it is called (max_output_tokens blocker)', () => {
    expect(classifySchemaField('max_output_tokens', { type: 'integer', default: 16000 })).toBe('number');
    expect(classifySchemaField('max_tokens', { type: 'integer' })).toBe('number');
    expect(classifySchemaField('token_limit', { type: 'integer' })).toBe('number');
    expect(classifySchemaField('context_window', { type: 'integer' })).toBe('number');
    expect(classifySchemaField('max_output_tokens', { anyOf: [{ type: 'integer' }, { type: 'null' }] })).toBe('number');
  });

  it('does not read a token budget as a credential even when the schema declares no type', () => {
    expect(classifySchemaField('max_output_tokens', undefined)).toBe('string');
    expect(classifySchemaField('max_tokens', undefined)).toBe('string');
    expect(classifySchemaField('token_limit', undefined)).toBe('string');
    expect(isLikelySecretField('max_output_tokens', undefined)).toBe(false);
    expect(isLikelySecretField('max_tokens', undefined)).toBe(false);
    expect(isLikelySecretField('token_limit', undefined)).toBe(false);
  });

  it('still classifies real credential names as secrets', () => {
    expect(classifySchemaField('api_token', { type: 'string' })).toBe('secret');
    expect(classifySchemaField('access_token', { type: 'string' })).toBe('secret');
    expect(classifySchemaField('api_key', { type: 'string' })).toBe('secret');
    expect(classifySchemaField('token', { type: 'string' })).toBe('secret');
    expect(classifySchemaField('private_key', undefined)).toBe('secret');
    expect(classifySchemaField('client_secret', undefined)).toBe('secret');
    expect(classifySchemaField('password', undefined)).toBe('secret');
    expect(classifySchemaField('apiKey', undefined)).toBe('secret');
  });

  it('does not mistake an ordinary trailing "key" for a credential', () => {
    expect(classifySchemaField('sort_key', { type: 'string' })).toBe('string');
    expect(classifySchemaField('partition_key', undefined)).toBe('string');
  });

  /** An explicit schema marker still outranks everything, including a declared type. */
  it('keeps an explicitly marked field masked even when it declares a numeric type', () => {
    expect(classifySchemaField('rotation', { type: 'integer', format: 'password' })).toBe('secret');
  });

  it("classifies a configuration_sections reference as its own kind, not free text", () => {
    expect(classifySchemaField('ai_credentials', LLM_MODEL_DATA_SCHEMA.properties.ai_credentials)).toBe('configuration');
  });
});

describe('configurationSectionsOf', () => {
  it('reads the sections a reference field draws from', () => {
    expect(configurationSectionsOf(LLM_MODEL_DATA_SCHEMA.properties.ai_credentials)).toEqual(['ai_credentials']);
  });

  it('is undefined for an ordinary property', () => {
    expect(configurationSectionsOf({ type: 'string' })).toBeUndefined();
    expect(configurationSectionsOf(undefined)).toBeUndefined();
    expect(configurationSectionsOf({ configuration_sections: [] })).toBeUndefined();
    expect(configurationSectionsOf({ configuration_sections: 'ai_credentials' })).toBeUndefined();
  });
});

describe('initialValueForSchemaField', () => {
  it('prefers the schema default for a non-secret field', () => {
    expect(initialValueForSchemaField('label', { default: 'x' })).toBe('x');
    expect(initialValueForSchemaField('enabled', { type: 'boolean', default: true })).toBe(true);
  });

  it('defaults boolean to false', () => {
    expect(initialValueForSchemaField('enabled', { type: 'boolean' })).toBe(false);
  });

  it('defaults number/string/undefined to empty string', () => {
    expect(initialValueForSchemaField('port', { type: 'number' })).toBe('');
    expect(initialValueForSchemaField('base_url', { type: 'string' })).toBe('');
    expect(initialValueForSchemaField('base_url', undefined)).toBe('');
  });

  // Regression test for the confirmed [warning] finding: a secret-typed
  // field must never be pre-filled from the schema's own `default` — it
  // must always start empty so the masked SecretManagementInput never
  // silently shows a schema-authored value as if it were the user's own.
  it('forces an empty initial value for a format: password field, ignoring a non-null default', () => {
    expect(initialValueForSchemaField('api_key', { format: 'password', default: 'sk-leaked-default' })).toBe('');
  });

  it('forces an empty initial value for a secret: true field, ignoring a non-null default', () => {
    expect(initialValueForSchemaField('token', { secret: true, default: 'leaked-default' })).toBe('');
  });

  it('forces an empty initial value for a name-heuristic secret field, ignoring a non-null default', () => {
    expect(initialValueForSchemaField('api_key', { type: 'string', default: 'leaked-default' })).toBe('');
  });

  it('forces an empty initial value for a nested anyOf secret field, ignoring a non-null default', () => {
    expect(
      initialValueForSchemaField('credentials', {
        anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }],
        default: 'leaked-default',
      }),
    ).toBe('');
  });
});

describe('initialDataForSchema', () => {
  it('builds one entry per property', () => {
    const result = initialDataForSchema({
      properties: { api_key: { type: 'string' }, enabled: { type: 'boolean' } },
    });
    expect(result).toEqual({ api_key: '', enabled: false });
  });

  it('returns an empty object for a schema with no properties', () => {
    expect(initialDataForSchema(undefined)).toEqual({});
    expect(initialDataForSchema({})).toEqual({});
  });

  // Regression test tying both findings together: a newly-selected
  // credential type whose schema declares a secret field only via a nested
  // `anyOf` marker, with a non-null schema `default`, must seed that
  // field's initial `data` entry as empty — never the classifier-missed
  // plain value, and never the leaked default.
  it('never seeds a secret-shaped (anyOf-nested) field from its schema default', () => {
    const result = initialDataForSchema({
      properties: {
        credentials: {
          anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }],
          default: 'sk-leaked-default',
        },
        base_url: { type: 'string', default: 'https://api.example.com' },
      },
    });
    expect(result).toEqual({ credentials: '', base_url: 'https://api.example.com' });
  });

  /**
   * The whole `llm_model` form, seeded. `max_output_tokens` must arrive as
   * the NUMBER 16000 (it used to arrive as `''`, because the field was
   * classified secret and secrets are forced empty), and `ai_credentials` as
   * `null` — never `''`, which the server stores as a bare string.
   */
  it('seeds the real llm_model schema with typed values, not masked blanks', () => {
    expect(initialDataForSchema(LLM_MODEL_DATA_SCHEMA)).toEqual({
      ai_credentials: null,
      context_window: 128000,
      max_output_tokens: 16000,
      name: '',
      supports_vision: true,
    });
  });
});
