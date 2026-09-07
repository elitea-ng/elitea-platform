import userEvent from '@testing-library/user-event';
import type { ReactElement, ReactNode } from 'react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { renderWithTheme as renderWithThemeOnly } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { ToolBaseProperty } from './ToolBaseProperty';
import type { ToolBasePropertyProps } from './ToolBaseProperty';
import type { ToolPropertySchema } from './types';

// jsdom has neither `ResizeObserver` nor CodeMirror's layout-measurement
// APIs — `ObjectField`/`CodeLanguageField`'s `ResizableCodeMirrorEditor`
// needs both to mount AND to accept the typing/blur interactions the tests
// below drive. Same polyfill `ResizableCodeMirrorEditor.test.tsx` itself
// installs (see that file's own doc comment for why).
installCodeMirrorTestPolyfills();

/** The box editor is always the first `.cm-content` in the tree — mirrors `ResizableCodeMirrorEditor.test.tsx`'s own helper. */
function getCodeMirrorContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

/**
 * #441 wired the secret field to the real secret list and permission list,
 * so a secret-kind field now runs two TanStack queries. `renderWithTheme`
 * supplies a theme only, so every render below adds a throwaway
 * `QueryClient`. No handler is registered on purpose: these tests assert the
 * field's own markup, not its data, and an unhandled request simply leaves
 * both queries empty. `SecretFieldInput.permission.test.tsx` owns the
 * data-dependent assertions.
 */
function renderWithTheme(ui: ReactElement): ReturnType<typeof renderWithThemeOnly> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithThemeOnly(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

function baseProps(overrides: Partial<ToolBasePropertyProps> = {}): ToolBasePropertyProps {
  return {
    field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
    formState: { toolErrors: {}, showValidation: false, setToolErrors: vi.fn() },
    settings: {},
    editField: vi.fn(),
    handleInputChange: () => vi.fn(),
    ...overrides,
  };
}

describe('ToolBaseProperty', () => {
  it('renders nothing for a hidden field', () => {
    const { container } = renderWithTheme(
      <ToolBaseProperty {...baseProps({ field: { key: 'secretish', schema: { type: 'string', hidden: true }, required: false } })} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing when visible_when does not match', () => {
    const schema: ToolPropertySchema = { type: 'string', visible_when: { field: 'auth_type', value: 'custom' } };
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'custom_header_name', schema, required: false },
          settings: { auth_type: 'basic' },
        })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders when visible_when matches (case-insensitively for strings)', () => {
    const schema: ToolPropertySchema = { title: 'Header', type: 'string', visible_when: { field: 'auth_type', value: 'Custom' } };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'custom_header_name', schema, required: false },
          settings: { auth_type: 'custom' },
        })}
      />,
    );
    expect(getByText('Header')).toBeInTheDocument();
  });

  it('renders a boolean field as a checkbox', () => {
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'cloud', schema: { title: 'Cloud', type: 'boolean' }, required: true } })}
      />,
    );
    expect(getByRole('checkbox', { name: /Cloud/ })).toBeInTheDocument();
  });

  it('renders a masked, disabled field for a secret field when the toolkit is disabled', () => {
    const { getByDisplayValue } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'api_key', schema: { title: 'API Key', type: 'string', secret: true }, required: true },
          disabled: true,
        })}
      />,
    );
    expect(getByDisplayValue('********')).toBeDisabled();
  });

  it('renders an editable secret input for a secret field when enabled', () => {
    const { getByLabelText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'api_key', schema: { title: 'API Key', type: 'string', secret: true }, required: true },
        })}
      />,
    );
    expect(getByLabelText(/API Key/)).not.toBeDisabled();
  });

  it('renders an enum field as a select with the current value', () => {
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'] };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'slow' } })}
      />,
    );
    expect(getByText('slow')).toBeInTheDocument();
  });

  // R3 regression guard: `EnumSelectField` (`ToolBaseProperty.renderers.tsx`)
  // used to pass `description` as `SingleSelect`'s `placeholder`, which only
  // ever shows in place of an unset/unmatched value — the description
  // disappeared the instant a value was selected. On the pre-fix code
  // `getByRole('button', {name: ...})` below finds nothing (no persistent
  // tooltip concept existed); after the fix, a persistent `InfoTooltip`
  // renders alongside the select regardless of the selected value.
  it('renders a persistent info-icon tooltip for an enum field with a description, even after a value is selected', () => {
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'], description: 'Pick a processing mode' };
    const { getByRole, getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'slow' } })}
      />,
    );
    // The selected value itself still renders as the select's own value —
    // proves `description` is no longer squatting in the `placeholder` slot
    // (which only ever shows for an empty/unmatched value).
    expect(getByText('slow')).toBeInTheDocument();
    expect(getByRole('button', { name: 'Pick a processing mode' })).toBeInTheDocument();
  });

  it('does not render an info-icon tooltip for an enum field with no description', () => {
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'] };
    const { queryByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'slow' } })}
      />,
    );
    expect(queryByRole('button')).not.toBeInTheDocument();
  });

  it('renders an array field via ArrayFieldInput', () => {
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'scopes', schema: { title: 'Scopes', type: 'array' }, required: false },
          settings: { scopes: ['a', 'b'] },
        })}
      />,
    );
    expect(getByRole('textbox')).toHaveValue('a, b');
  });

  it('renders the selected_tools field via ToolActionsSelector', () => {
    const schema: ToolPropertySchema = { items: { enum: ['google', 'wiki'] } };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty {...baseProps({ field: { key: 'selected_tools', schema, required: false } })} />,
    );
    expect(getByText('Google')).toBeInTheDocument();
    expect(getByText('Wiki')).toBeInTheDocument();
  });

  it('renders an object field as a JSON code editor inside an accordion', () => {
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'headers', schema: { title: 'Headers', type: 'object' }, required: false },
          settings: { headers: { a: 1 } },
        })}
      />,
    );
    expect(getByText('Headers')).toBeInTheDocument();
  });

  /*
   * The MCP detail screen's own axe failure (`nested-interactive`, serious).
   * `headers` is served with a description, and the hint that description
   * renders is a real `<button>`; while it lived in the accordion TITLE it
   * was a focusable descendant of `StyledAccordionSummary`'s own button.
   * Asserted structurally — the summary button must contain no focusable
   * control — because that is exactly what axe measures.
   */
  it('keeps an object field description out of the accordion summary button', () => {
    const schema: ToolPropertySchema = {
      title: 'Headers',
      type: 'object',
      description: 'HTTP headers for authentication and configuration',
    };
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'headers', schema, required: false }, settings: { headers: {} } })}
      />,
    );

    const hint = getByRole('button', { name: 'HTTP headers for authentication and configuration' });
    const summary = getByRole('button', { name: 'Headers' });
    expect(summary).not.toContainElement(hint);
    expect(summary.querySelector('button, [tabindex], a[href]')).toBeNull();
  });

  it('renders an object field without the accordion wrapper when noAccordionWrapper is set (the Advanced Settings group)', () => {
    const { getByText, queryByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'headers', schema: { title: 'Headers', type: 'object' }, required: false },
          settings: { headers: { a: 1 } },
          visibility: { noAccordionWrapper: true },
        })}
      />,
    );
    expect(getByText('Headers')).toBeInTheDocument();
    expect(queryByRole('button', { expanded: false })).not.toBeInTheDocument();
  });

  // R2 regression guard: `renderOpenapiSpec` (`ToolBaseProperty.dispatch.tsx`)
  // used to unconditionally return `null` unless a caller supplied a
  // `slots.renderOpenApiSpecField` render-prop — no real caller ever did
  // (the live composition root, `ToolkitForm.hooks.ts`, has no `slots`
  // concept at all), so this was silently blank for every OpenAPI toolkit
  // in production. On the pre-fix code `getByText('Schema')` below throws
  // (no such element, `container` was `{container).toBeEmptyDOMElement()`);
  // after the fix, the real `../ToolOpenAPI/OpenAPISchemaInput.tsx` renders
  // by default.
  it('renders the real OpenAPI schema editor by default when no slot is supplied', () => {
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'schema_text', schema: { ui_component: 'openapi_spec' }, required: false } })}
      />,
    );
    expect(getByText('Schema')).toBeInTheDocument();
  });

  it('renders the real OpenAPIActions table for an openapi_spec field whose current value already has parsed paths', () => {
    const settings = { schema_text: JSON.stringify({ paths: { '/pets': { get: { operationId: 'listPets' } } } }) };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'schema_text', schema: { ui_component: 'openapi_spec' }, required: false },
          settings,
        })}
      />,
    );
    expect(getByText('listPets')).toBeInTheDocument();
  });

  it('invokes the renderOpenApiSpecField slot with the resolved context', () => {
    const renderOpenApiSpecField = vi.fn(() => <div>openapi widget</div>);
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'schema_text', schema: { ui_component: 'openapi_spec' }, required: false },
          settings: { schema_text: '{"paths":{}}' },
          slots: { renderOpenApiSpecField },
        })}
      />,
    );
    expect(getByText('openapi widget')).toBeInTheDocument();
    expect(renderOpenApiSpecField).toHaveBeenCalledWith(
      expect.objectContaining({ value: '{"paths":{}}', disabled: false }),
    );
  });

  it('returns null for a configuration field when no credential slot is supplied (disclosed gap)', () => {
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'jira_configuration', schema: { type: 'configuration' }, required: false } })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('invokes the renderCredentialLikeField slot for a configuration-typed field', () => {
    const renderCredentialLikeField = vi.fn(() => <div>credential widget</div>);
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'jira_configuration', schema: { type: 'configuration' }, required: true },
          slots: { renderCredentialLikeField },
        })}
      />,
    );
    expect(getByText('credential widget')).toBeInTheDocument();
    expect(renderCredentialLikeField).toHaveBeenCalledWith(
      expect.objectContaining({ kind: 'configuration', propertyKey: 'jira_configuration', required: true }),
    );
  });

  it('invokes the renderCredentialLikeField slot for an llm_model-typed field', () => {
    const renderCredentialLikeField = vi.fn(() => <div>llm widget</div>);
    renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'model', schema: { type: 'llm_model' }, required: false },
          slots: { renderCredentialLikeField },
        })}
      />,
    );
    expect(renderCredentialLikeField).toHaveBeenCalledWith(expect.objectContaining({ kind: 'llm_model' }));
  });

  it('renders a default text field for a plain string, and commits edits via editField', () => {
    const editField = vi.fn();
    const { getByLabelText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
          editField,
        })}
      />,
    );
    expect(getByLabelText('Label')).toBeInTheDocument();
  });

  it('hides a configuration field with no value when disableConfigFields is set', () => {
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'url', schema: { title: 'URL', type: 'string', configuration: true }, required: false },
          settings: {},
          visibility: { disableConfigFields: true },
        })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('shows a configuration field with a value when disableConfigFields is set', () => {
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'url', schema: { title: 'URL', type: 'string', configuration: true }, required: false },
          settings: { url: 'https://example.com' },
          visibility: { disableConfigFields: true },
        })}
      />,
    );
    expect(getByText('URL')).toBeInTheDocument();
  });

  it('hides a non-configuration field when showOnlyConfigurationFields is set', () => {
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
          visibility: { showOnlyConfigurationFields: true },
        })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders a multiline field for a string with lines > 1', () => {
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'notes', schema: { title: 'Notes', type: 'string', lines: 4 }, required: false },
          settings: { notes: 'line one' },
        })}
      />,
    );
    const textbox = getByRole('textbox') as HTMLTextAreaElement;
    expect(textbox.tagName).toBe('TEXTAREA');
    expect(textbox).toHaveValue('line one');
  });

  it('renders a plain single-line field for a string with lines = 1 (not multiline)', () => {
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'notes', schema: { title: 'Notes', type: 'string', lines: 1 }, required: false },
        })}
      />,
    );
    expect((getByRole('textbox') as HTMLInputElement).tagName).toBe('INPUT');
  });

  it('renders a code editor for a string field tagged with code_language', () => {
    const { getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'payload', schema: { title: 'Payload', type: 'string', code_language: 'json' }, required: false },
          settings: { payload: '{}' },
        })}
      />,
    );
    expect(getByText('Payload')).toBeInTheDocument();
  });

  it('hides a non-required field when showOnlyRequiredFields is set', () => {
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
          visibility: { showOnlyRequiredFields: true },
        })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('commits a boolean toggle via editField (ToolBaseProperty.jsx:408-411)', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'available_by_mcp', schema: { title: 'Available by MCP', type: 'boolean' }, required: false },
          editField,
        })}
      />,
    );
    await user.click(getByRole('checkbox'));
    expect(editField).toHaveBeenCalledWith('settings.available_by_mcp', true);
  });

  it('commits an enum selection via editField (ToolBaseProperty.jsx:437-449)', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'] };
    const { getByRole } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'fast' }, editField })}
      />,
    );
    await user.click(getByRole('combobox'));
    await user.click(getByRole('option', { name: 'slow' }));
    expect(editField).toHaveBeenCalledWith('settings.mode', 'slow');
  });

  it('falls back to the schema default when the current enum value matches no option (ToolBaseProperty.jsx:429-435)', () => {
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'], default: 'slow' };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'stale-value' } })} />,
    );
    expect(getByText('slow')).toBeInTheDocument();
  });

  it('falls back to empty when neither the current value nor the schema default match any option', () => {
    const schema: ToolPropertySchema = { title: 'Mode', type: 'string', enum: ['fast', 'slow'] };
    const { queryByText } = renderWithTheme(
      <ToolBaseProperty {...baseProps({ field: { key: 'mode', schema, required: false }, settings: { mode: 'stale-value' } })} />,
    );
    expect(queryByText('stale-value')).not.toBeInTheDocument();
  });

  it('commits a secret-field edit via editField (ToolBaseProperty.jsx:324-339)', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const { getByLabelText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'api_key', schema: { title: 'API Key', type: 'string', secret: true }, required: false },
          editField,
        })}
      />,
    );
    await user.type(getByLabelText(/API Key/), 'x');
    expect(editField).toHaveBeenCalledWith('settings.api_key', 'x');
  });

  it('commits a code-language field edit on blur via editField (ToolBaseProperty.jsx:455-471)', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const { container } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'payload', schema: { title: 'Payload', type: 'string', code_language: 'json' }, required: false },
          settings: { payload: '{}' },
          editField,
        })}
      />,
    );
    const content = getCodeMirrorContent(container);
    await user.click(content);
    await user.keyboard('X');
    content.blur();
    expect(editField).toHaveBeenCalledWith('settings.payload', 'X{}');
  });

  describe('object field (ToolBaseProperty.jsx:340-391)', () => {
    it('commits valid JSON, parsed, via editField with skipValidation=true', async () => {
      const user = userEvent.setup();
      const editField = vi.fn();
      const { container } = renderWithTheme(
        <ToolBaseProperty
          {...baseProps({
            field: { key: 'headers', schema: { title: 'Headers', type: 'object' }, required: false },
            settings: {},
            editField,
          })}
        />,
      );
      const content = getCodeMirrorContent(container);
      await user.click(content);
      await user.keyboard('{Control>}a{/Control}');
      // `{` is userEvent.keyboard's own key-description delimiter — `{{` types
      // the literal character (https://testing-library.com/docs/user-event/keyboard).
      // This editor auto-closes it with a matching `}` (confirmed empirically:
      // typing `{{"a":1}}` here doubles the closing brace to `{"a":1}}`), so the
      // closing brace is NOT typed — the auto-inserted one already completes it.
      await user.keyboard('{{"a":1');
      content.blur();
      expect(editField).toHaveBeenCalledWith('settings.headers', { a: 1 }, true);
    });

    it('commits {} via editField when the typed text is not valid JSON', async () => {
      const user = userEvent.setup();
      const editField = vi.fn();
      const { container } = renderWithTheme(
        <ToolBaseProperty
          {...baseProps({
            field: { key: 'headers', schema: { title: 'Headers', type: 'object' }, required: false },
            settings: {},
            editField,
          })}
        />,
      );
      const content = getCodeMirrorContent(container);
      await user.click(content);
      await user.keyboard('{Control>}a{/Control}');
      await user.keyboard('not json');
      content.blur();
      expect(editField).toHaveBeenCalledWith('settings.headers', {}, true);
    });

    it('commits {} via editField, without skipValidation, when the field is cleared to empty', async () => {
      const user = userEvent.setup();
      const editField = vi.fn();
      const { container } = renderWithTheme(
        <ToolBaseProperty
          {...baseProps({
            field: { key: 'headers', schema: { title: 'Headers', type: 'object' }, required: false },
            settings: { headers: { a: 1 } },
            editField,
          })}
        />,
      );
      const content = getCodeMirrorContent(container);
      await user.click(content);
      await user.keyboard('{Control>}a{/Control}{Backspace}');
      content.blur();
      expect(editField).toHaveBeenCalledWith('settings.headers', {});
    });
  });

  it('commits a selected-tool toggle via editField (ToolBaseProperty.jsx:277-289)', async () => {
    const user = userEvent.setup();
    const editField = vi.fn();
    const schema: ToolPropertySchema = { items: { enum: ['google', 'wiki'] } };
    const { getByText } = renderWithTheme(
      <ToolBaseProperty {...baseProps({ field: { key: 'selected_tools', schema, required: false }, editField })} />,
    );
    await user.click(getByText('Google'));
    expect(editField).toHaveBeenCalledWith('settings.selected_tools', ['google']);
  });

  it('forwards the resolved OpenApiSpecFieldContext callbacks to editField (ToolBaseProperty.jsx:236-259)', () => {
    const editField = vi.fn();
    let capturedContext: { onSchemaChange: (text: string) => void; onSelectionChange: (name: string, enabled: boolean) => void } | undefined;
    renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'schema_text', schema: { ui_component: 'openapi_spec' }, required: false },
          settings: { schema_text: '{}', selected_tools: ['existingTool'] },
          editField,
          slots: {
            renderOpenApiSpecField: (context) => {
              capturedContext = context;
              return <div>openapi widget</div>;
            },
          },
        })}
      />,
    );
    capturedContext?.onSchemaChange('paths: {}');
    expect(editField).toHaveBeenCalledWith('settings.schema_text', 'paths: {}');

    capturedContext?.onSelectionChange('newTool', true);
    expect(editField).toHaveBeenCalledWith('settings.selected_tools', ['existingTool', 'newTool']);

    capturedContext?.onSelectionChange('existingTool', false);
    expect(editField).toHaveBeenCalledWith('settings.selected_tools', []);
  });

  it('forwards the resolved CredentialLikeFieldContext.onChange to editField, including options (ToolBaseProperty.jsx:500-514)', () => {
    const editField = vi.fn();
    let capturedOnChange: ((value: unknown, options?: unknown) => void) | undefined;
    renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'jira_configuration', schema: { type: 'configuration' }, required: false },
          editField,
          slots: {
            renderCredentialLikeField: (context) => {
              capturedOnChange = context.onChange;
              return <div>credential widget</div>;
            },
          },
        })}
      />,
    );
    capturedOnChange?.('config-id-1', { label: 'My Config' });
    expect(editField).toHaveBeenCalledWith('settings.jira_configuration', 'config-id-1', undefined, { label: 'My Config' });
  });

  it('shows the characters-remaining hint when a focused `label` field is exactly at MAX_NAME_LENGTH (ToolBaseProperty.jsx:630-637)', async () => {
    const user = userEvent.setup();
    const { getByLabelText, getByText } = renderWithTheme(
      <ToolBaseProperty
        {...baseProps({
          field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
          settings: { label: 'x'.repeat(32) },
        })}
      />,
    );
    await user.click(getByLabelText('Label'));
    expect(getByText('0 is left from 32 characters left')).toBeInTheDocument();
  });

  it('caps typed input in the `label` field at MAX_NAME_LENGTH (32) characters, mirroring ToolBaseProperty.jsx:603', async () => {
    const user = userEvent.setup();

    function ControlledLabelField(): ReactNode {
      const [label, setLabel] = useState('');
      return (
        <ToolBaseProperty
          {...baseProps({
            field: { key: 'label', schema: { title: 'Label', type: 'string' }, required: false },
            settings: { label },
            handleInputChange: () => (event) => setLabel(event.target.value),
          })}
        />
      );
    }

    const { getByLabelText } = renderWithTheme(<ControlledLabelField />);
    const input = getByLabelText('Label') as HTMLInputElement;
    await user.type(input, 'x'.repeat(40));
    expect(input.value).toHaveLength(32);
  });
});
