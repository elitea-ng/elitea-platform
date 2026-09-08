/**
 * The schema-driven form for one Configuration section — unit A14, issue #200.
 *
 * The server sends field specs; this renders a control per spec. It renders
 * exactly the widget kinds the AVAILABLE sections need — boolean, string,
 * enum-as-select, multiline string, number, the `*_links` editor and the
 * toolkit-to-tools map behind Guardrails — and, for a spec it has no honest
 * widget for, a disabled row saying so.
 *
 * That last part is the whole design. The reference's `SchemaField.jsx` falls
 * back to a raw JSON CodeMirror for `object` fields and to a chips input for
 * arrays it does not recognise, so a field whose editor was never written still
 * LOOKS editable. Here it says it is not. No available section declares such a
 * field today, so the branch is unreached in production and covered by a unit
 * test — but it is the branch that keeps the next section honest when one does.
 *
 * The `toolMap` widget exists because Guardrails made that branch reachable.
 * Guardrails is `order: 1`, so it is the section this page LANDS on, and two of
 * its five fields are `{toolkit: [tools]}` maps — `blocked_tools` and
 * `sensitive_tools`, which are the substance of the feature. Shipping them as
 * unsupported rows would have made the page's first screen a form whose two
 * most important controls do nothing.
 */
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ConfigurationHtmlField } from './ConfigurationHtmlField';
import { ConfigurationLinksEditor, toConfigLinks } from './ConfigurationLinksEditor';
import {
  ConfigurationListEditor,
  fromConfigListRows,
  toConfigListRows,
  type ConfigListItemType,
} from './ConfigurationListEditor';
import {
  ConfigurationToolMapEditor,
  fromConfigToolMapRows,
  toConfigToolMapRows,
} from './ConfigurationToolMapEditor';
import { useConfigSuggestions, type AdminConfigField } from './api/adminConfigurationApi';
import { isFieldVisible, listItemTypeFor, widgetFor } from './configurationFields';

// Re-exported so the spec-reading helpers keep ONE import path for their
// existing consumers (`useAdminConfigurationPage`, and the two page test
// suites). Moving the file should not have moved the module's public surface.
export {
  isFieldVisible,
  isToolMapField,
  listItemTypeFor,
  widgetFor,
  type ConfigWidget,
} from './configurationFields';

interface FieldProps {
  readonly field: AdminConfigField;
  readonly value: unknown;
  readonly disabled: boolean;
  readonly onChange: (key: string, next: unknown) => void;
}

function fieldLabel(field: AdminConfigField): string {
  return field.title === '' ? field.key : field.title;
}

/** Title + description above a control that cannot carry its own label. */
function FieldHeading({ field }: { readonly field: AdminConfigField }) {
  return (
    <>
      <Typography variant="bodyMedium">{fieldLabel(field)}</Typography>
      {field.description !== undefined && field.description !== '' ? (
        <Typography variant="bodySmall" color="text.secondary" component="div">
          {field.description}
        </Typography>
      ) : null}
    </>
  );
}

function BooleanField({ field, value, disabled, onChange }: FieldProps) {
  return (
    <FormControlLabel
      control={
        <Switch
          checked={value === true}
          disabled={disabled}
          slotProps={{ input: { 'aria-label': fieldLabel(field) } }}
          onChange={(event) => {
            onChange(field.key, event.target.checked);
          }}
        />
      }
      label={
        <Box>
          <FieldHeading field={field} />
        </Box>
      }
      sx={{ alignItems: 'flex-start', margin: 0, gap: '0.5rem' }}
    />
  );
}

function LinksField({ field, value, disabled, onChange }: FieldProps) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.375rem' }}>
      <FieldHeading field={field} />
      <ConfigurationLinksEditor
        fieldKey={field.key}
        label={fieldLabel(field)}
        links={toConfigLinks(value)}
        disabled={disabled}
        onChange={(next) => {
          onChange(field.key, next);
        }}
      />
    </Box>
  );
}

/**
 * The toolkit-to-tools map behind `blocked_tools` and `sensitive_tools`.
 *
 * The toolkit-name suggestions are fetched ONCE per field rather than per row —
 * they are the same list for every row, and the query is shared by key anyway,
 * but hoisting it keeps the row component free of a second network concern.
 */
function ToolMapField({ field, value, disabled, onChange }: FieldProps) {
  const toolkitOptions = useConfigSuggestions(field.enum_source_keys);
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.375rem' }}>
      <FieldHeading field={field} />
      <ConfigurationToolMapEditor
        label={fieldLabel(field)}
        rows={toConfigToolMapRows(value)}
        disabled={disabled}
        toolkitOptions={toolkitOptions}
        toolSource={field.enum_source_values}
        onChange={(next) => {
          onChange(field.key, fromConfigToolMapRows(next));
        }}
      />
    </Box>
  );
}

/**
 * A spec this platform has no honest editor for.
 *
 * The reference falls back to a raw JSON CodeMirror, so a field whose editor was
 * never written still looks editable. This says it is not, and says why.
 */
function UnsupportedField({ field }: { readonly field: AdminConfigField }) {
  return (
    <TextField
      size="small"
      label={fieldLabel(field)}
      value=""
      disabled
      helperText={t(
        'pages.admin.configuration.field.unsupported',
        'This platform has no editor for this field yet, so it is shown read-only rather than as a control that discards what you type.',
      )}
    />
  );
}

/**
 * A field the SERVER says cannot be set here, inside a section that can.
 *
 * The current value is shown — read-only, so the operator can see what the
 * platform holds — and the server's own sentence is the helper text. Hiding the
 * field instead would make the control simply vanish relative to the reference,
 * which reads as a page that lost a feature rather than a platform that does not
 * have one; and rendering it as a live control would let a save be attempted
 * that the server refuses with a 400, teaching the operator nothing.
 */
function UnavailableField({ field, value }: { readonly field: AdminConfigField; readonly value: unknown }) {
  return (
    <TextField
      size="small"
      label={fieldLabel(field)}
      data-testid={`admin-config-field-unavailable-${field.key}`}
      value={typeof value === 'string' ? value : ''}
      disabled
      multiline={field.format === 'textarea'}
      minRows={field.format === 'textarea' ? 3 : undefined}
      helperText={field.unavailable_reason}
    />
  );
}

function ListField({ field, value, disabled, onChange }: FieldProps) {
  const itemType: ConfigListItemType = listItemTypeFor(field) ?? 'string';
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.375rem' }}>
      <FieldHeading field={field} />
      <ConfigurationListEditor
        fieldKey={field.key}
        label={fieldLabel(field)}
        itemType={itemType}
        rows={toConfigListRows(value)}
        disabled={disabled}
        onChange={(next) => {
          // The editor works in TEXT and the field stores typed elements, so the
          // conversion happens on every keystroke rather than at save time. That
          // keeps `values[key]` the single source the form reads back — the
          // alternative, holding text in a second piece of state, is how a
          // discard ends up restoring one half of a field.
          onChange(field.key, fromConfigListRows(next, itemType));
        }}
      />
    </Box>
  );
}

function SelectField({ field, value, disabled, onChange }: FieldProps) {
  return (
    <TextField
      select
      size="small"
      label={fieldLabel(field)}
      helperText={field.description}
      value={typeof value === 'string' ? value : ''}
      disabled={disabled}
      onChange={(event) => {
        onChange(field.key, event.target.value);
      }}
    >
      {(field.enum ?? []).map((option) => (
        <MenuItem key={option} value={option}>
          {option}
        </MenuItem>
      ))}
    </TextField>
  );
}

function NumberField({ field, value, disabled, onChange }: FieldProps) {
  return (
    <TextField
      size="small"
      type="number"
      label={fieldLabel(field)}
      helperText={field.description}
      value={typeof value === 'number' ? String(value) : ''}
      disabled={disabled}
      onChange={(event) => {
        const text = event.target.value;
        // An empty box is `null`, never `0`. `Number('')` is 0, and a budget or
        // a project id silently becoming zero is a value a form must not invent.
        onChange(field.key, text === '' ? null : Number(text));
      }}
    />
  );
}

function TextFieldRow({ field, value, disabled, onChange, multiline }: FieldProps & { readonly multiline: boolean }) {
  return (
    <TextField
      size="small"
      label={fieldLabel(field)}
      helperText={field.description}
      multiline={multiline}
      minRows={multiline ? 4 : undefined}
      value={typeof value === 'string' ? value : ''}
      disabled={disabled}
      onChange={(event) => {
        onChange(field.key, event.target.value);
      }}
    />
  );
}

function FieldRow(props: FieldProps) {
  switch (widgetFor(props.field)) {
    case 'boolean':
      return <BooleanField {...props} />;
    case 'links':
      return <LinksField {...props} />;
    case 'none':
      return <UnsupportedField field={props.field} />;
    case 'unavailable':
      return <UnavailableField field={props.field} value={props.value} />;
    case 'list':
      return <ListField {...props} />;
    case 'toolMap':
      return <ToolMapField {...props} />;
    case 'select':
      return <SelectField {...props} />;
    case 'number':
      return <NumberField {...props} />;
    case 'multiline':
      return <TextFieldRow {...props} multiline />;
    case 'html':
      return <ConfigurationHtmlField {...props} />;
    case 'text':
      return <TextFieldRow {...props} multiline={false} />;
  }
}

export function ConfigurationSectionForm({
  fields,
  values,
  disabled,
  onChange,
}: {
  readonly fields: readonly AdminConfigField[];
  readonly values: Readonly<Record<string, unknown>>;
  readonly disabled: boolean;
  readonly onChange: (key: string, next: unknown) => void;
}) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
      {fields
        .filter((field) => isFieldVisible(field, values))
        .map((field) => (
          <FieldRow
            key={field.key}
            field={field}
            value={values[field.key]}
            disabled={disabled}
            onChange={onChange}
          />
        ))}
    </Box>
  );
}
