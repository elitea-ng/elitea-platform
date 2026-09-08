/**
 * pages/credentials/CredentialFormFields.tsx — one schema-driven form field
 * for `CredentialForm.tsx`'s per-type "data" section. Split into its own
 * file (out of `CredentialForm.tsx`) purely to keep each function under the
 * §3.5 cyclomatic-complexity budget — see that file's own doc comment for
 * the field-kind design rationale (secret/boolean/number/string, chosen by
 * `SchemaField.classify`).
 *
 * The `'configuration'` kind is the newest one and is a REFERENCE field, not
 * a value field: it names another stored configuration row instead of
 * carrying data of its own. `CredentialConfigurationField` below renders the
 * picker and writes the object shape every reader expects.
 */
import type { ReactNode } from 'react';
import { useId, useMemo } from 'react';

import FormControl from '@mui/material/FormControl';
import FormHelperText from '@mui/material/FormHelperText';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';

import { t } from '@/shared/i18n';
import { CommonBooleanField } from '@/shared/ui/CommonBooleanField';
import { CommonNumberField } from '@/shared/ui/CommonNumberField';
import { CommonStringField } from '@/shared/ui/CommonStringField';
import { SecretManagementInput } from '@/shared/ui/SecretManagementInput';

import { useSecretFieldOptions } from '@/entities/secret';
import { SchemaField, useConfigurationsList } from '@/features/credentials';
import type { ConfigSchemaNode } from '@/features/credentials';

import { buildConfigurationReference, configurationReferenceTitle, toConfigurationReference } from './configurationReference';

export interface CredentialSchemaFieldProps {
  readonly fieldKey: string;
  readonly property: ConfigSchemaNode | undefined;
  readonly value: unknown;
  readonly error: string | undefined;
  readonly required: boolean;
  /** The project whose stored rows a `'configuration'` reference field picks from. */
  readonly projectId: string;
  readonly onChange: (fieldKey: string, value: unknown) => void;
}

interface CommonMeta {
  readonly label: string;
  readonly description?: string;
  readonly isRequired: boolean;
}

function metaFor(fieldKey: string, property: ConfigSchemaNode | undefined, required: boolean): CommonMeta {
  const label = property?.title ?? fieldKey;
  return {
    label,
    isRequired: required,
    ...(property?.description !== undefined ? { description: property.description } : {}),
  };
}

/**
 * #441: `secrets` was never supplied here, so a credential's secret field
 * rendered as a plain masked text box — no mode toggle, no saved-secret
 * picker, and no "Create new secret" entry for any user, an administrator
 * included. `useSecretFieldOptions()` supplies the option list, the refresh
 * action and the create grant `SecretField` expects from its caller.
 *
 * A component, not a helper called from `CredentialSchemaField`: the hook
 * queries, and this mounts on the secret branch alone.
 */
function CredentialSecretField({ field, label }: { readonly field: CredentialSchemaFieldProps; readonly label: string }): ReactNode {
  const { fieldKey, value, error, required, onChange } = field;
  const secrets = useSecretFieldOptions();
  return (
    <SecretManagementInput
      name={fieldKey}
      label={label}
      required={required}
      value={typeof value === 'string' ? value : ''}
      onChange={(next) => {
        onChange(fieldKey, next);
      }}
      secrets={secrets}
      {...(error !== undefined ? { error: true, helperText: error } : {})}
    />
  );
}

/**
 * The pool a linked-configuration picker reads. Same page size the
 * AI-Configuration screen uses for its own section lists: enough to cover
 * every credential a project realistically holds, without paging a control
 * that has to show them all at once.
 */
const LINKED_CONFIGURATION_PAGE_SIZE = 200;

/**
 * The `'configuration'` kind: a picker over the rows already stored in the
 * sections the schema names, writing a REFERENCE object.
 *
 * DEFECT this replaces: the property fell through to the plain string
 * widget, so `ai_credentials` on the LLM/embedding/image/ASR/TTS model
 * schemas rendered as a free-text box and the form stored
 * `"ai_credentials": "vllm_creds"` — a bare string. See
 * `./configurationReference.ts` for the three readers that want the object.
 *
 * `private` is written as `false`, which is what the options listed here
 * mean: the picker lists rows visible in THIS project (its own plus the
 * shared platform ones), and `private: true` tells the gateway to resolve
 * the title in the CALLER's personal project instead. A stored row that
 * already carries `private: true` keeps it.
 *
 * A component, not a helper called from `CredentialSchemaField`: the query
 * mounts only on this branch.
 */
function CredentialConfigurationField({
  field,
  label,
  sections,
}: {
  readonly field: CredentialSchemaFieldProps;
  readonly label: string;
  readonly sections: readonly string[];
}): ReactNode {
  const { fieldKey, value, error, required, projectId, onChange } = field;
  const labelId = useId();
  const listQuery = useConfigurationsList(
    { projectId, section: sections, includeShared: true, pageSize: LINKED_CONFIGURATION_PAGE_SIZE },
    { enabled: projectId !== '' && sections.length > 0 },
  );

  const options = useMemo(() => {
    const rows = [...(listQuery.data?.items ?? []), ...(listQuery.data?.shared?.items ?? [])];
    const titles: string[] = [];
    for (const row of rows) {
      const title = row.elitea_title;
      if (typeof title === 'string' && title !== '' && !titles.includes(title)) titles.push(title);
    }
    return titles;
  }, [listQuery.data]);

  // A saved row whose linked configuration is not in the list (deleted, or
  // owned by another project) must still be VISIBLE. Dropping it would make
  // the control read as "nothing linked", and the next save would then clear
  // a link the user never touched.
  const selected = toConfigurationReference(value);
  const selectedTitle = configurationReferenceTitle(value);
  const allOptions = selectedTitle !== '' && !options.includes(selectedTitle) ? [selectedTitle, ...options] : options;

  return (
    <FormControl
      variant="standard"
      fullWidth
      required={required}
      error={error !== undefined}
    >
      {/* `id`/`labelId` pair up so the visible label becomes the select's
          accessible name — the same reason `CredentialsSelect` does it. */}
      <InputLabel
        shrink
        id={labelId}
      >
        {label}
      </InputLabel>
      <Select<string>
        labelId={labelId}
        value={selectedTitle}
        displayEmpty
        onChange={(event) => {
          const next = event.target.value;
          onChange(fieldKey, next === '' ? null : buildConfigurationReference(next, selected?.private ?? false));
        }}
      >
        <MenuItem value="">{t('credentials.form.noLinkedConfiguration', 'None')}</MenuItem>
        {allOptions.map((title) => (
          <MenuItem
            key={title}
            value={title}
          >
            {title}
          </MenuItem>
        ))}
      </Select>
      {error !== undefined && <FormHelperText>{error}</FormHelperText>}
    </FormControl>
  );
}

function renderBooleanField({ fieldKey, value, onChange }: CredentialSchemaFieldProps, meta: CommonMeta): ReactNode {
  return (
    <CommonBooleanField
      key={fieldKey}
      fieldKey={fieldKey}
      value={typeof value === 'boolean' ? value : false}
      meta={meta}
      onChange={onChange}
    />
  );
}

function renderNumberField({ fieldKey, property, value, onChange }: CredentialSchemaFieldProps, meta: CommonMeta): ReactNode {
  return (
    <CommonNumberField
      key={fieldKey}
      fieldKey={fieldKey}
      value={typeof value === 'number' ? value : null}
      meta={meta}
      fieldType={property?.type === 'integer' ? 'integer' : 'number'}
      {...(property !== undefined ? { property } : {})}
      onChange={onChange}
    />
  );
}

function schemaNodesOf(value: unknown): readonly ConfigSchemaNode[] {
  if (!Array.isArray(value)) return [];
  return value.filter((entry): entry is ConfigSchemaNode => typeof entry === 'object' && entry !== null);
}

function enumOptionsOf(property: ConfigSchemaNode | undefined): readonly string[] | undefined {
  const candidates = [property, ...schemaNodesOf(property?.['anyOf']), ...schemaNodesOf(property?.['oneOf'])];
  for (const candidate of candidates) {
    if (typeof candidate !== 'object' || candidate === null) continue;
    const values = candidate.enum;
    if (Array.isArray(values)) return values.filter((entry): entry is string => typeof entry === 'string');
  }
  return undefined;
}

function renderStringField({ fieldKey, property, value, error, onChange }: CredentialSchemaFieldProps, meta: CommonMeta): ReactNode {
  const enumValues = enumOptionsOf(property);
  return (
    <CommonStringField
      key={fieldKey}
      fieldKey={fieldKey}
      value={typeof value === 'string' ? value : ''}
      meta={{ ...meta, ...(error !== undefined ? { error } : {}), ...(enumValues !== undefined ? { enumValues } : {}) }}
      onChange={onChange}
    />
  );
}

/** Dispatches to the right widget for one schema property, by `SchemaField.classify`'s kind. */
export function CredentialSchemaField(props: CredentialSchemaFieldProps): ReactNode {
  const kind = SchemaField.classify(props.fieldKey, props.property);
  const meta = metaFor(props.fieldKey, props.property, props.required);
  if (kind === 'secret')
    return (
      <CredentialSecretField
        key={props.fieldKey}
        field={props}
        label={meta.label}
      />
    );
  if (kind === 'configuration')
    return (
      <CredentialConfigurationField
        key={props.fieldKey}
        field={props}
        label={meta.label}
        sections={SchemaField.configurationSections(props.property) ?? []}
      />
    );
  if (kind === 'boolean') return renderBooleanField(props, meta);
  if (kind === 'number') return renderNumberField(props, meta);
  return renderStringField(props, meta);
}
