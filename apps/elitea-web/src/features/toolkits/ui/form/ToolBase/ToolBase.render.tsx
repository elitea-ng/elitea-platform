import type { ChangeEvent, ReactNode } from 'react';

import Box from '@mui/material/Box';

import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { NameDescriptionInput } from '../NameDescriptionInput';
import type { SharepointConfigRef } from '../../../sharepoint/lib/hooks/useResolvedSharepointConfig.hooks';
import { SharepointOAuthStatus } from '../../../sharepoint/ui/SharepointOAuthStatus';

import { isMainPassField, isSectionOrToolsField, pickSchemaFields, resolveRequired, schemaEntries } from './ToolBase.fields';
import { ToolActionsSelector } from './ToolActionsSelector';
import { ToolBaseProperty } from './ToolBaseProperty';
import type { ToolBasePropertyCredentialContext, ToolBasePropertyFormState, ToolBasePropertySlots } from './ToolBaseProperty';
import { ToolSection } from './ToolSection';
import type { EditToolDetail, EditToolField, SetEditToolDetail, ToolPropertySchema, ToolSchema } from './types';
import type { ResolvedFieldBehavior, ResolvedFieldOrder, ResolvedFieldPresentation, ResolvedSections } from './ToolBase.options';
import type { ToolBaseSlots } from './ToolBase.types';

/**
 * `ToolBase.tsx`'s JSX-producing pieces, split into standalone functions —
 * same complexity-budget reason as `ToolBase.options.ts`/`ToolBase.handlers.ts`:
 * a called function's own conditional JSX counts toward ITS complexity, not
 * the caller's, so extracting the baseline's `toolBaseConfiguration`/`tools`
 * blocks (`ToolBase.jsx:220-333,524-564`) out of the render body is what
 * gets `ToolBase` itself under the §3.5 budget. No behaviour change.
 */
interface PropertyPassParams {
  readonly formState: ToolBasePropertyFormState;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly editField: EditToolField;
  readonly handleInputChange: (fieldPath: string) => (event: ChangeEvent<HTMLInputElement>) => void;
  readonly disabled: boolean | undefined;
  readonly credentialContext: ToolBasePropertyCredentialContext | undefined;
  readonly slots: ToolBasePropertySlots | undefined;
  readonly editFieldRootPath: string;
  readonly showOnlyRequiredFields: boolean;
  readonly showDisabledConfigFields: boolean;
  readonly enableEditEliteaTitle: boolean;
}

/** One `<ToolBaseProperty>`'s worth of grouped props — a plain function (not a closure captured in the render body) so it does not add to `ToolBaseConfigurationBody`'s own complexity beyond the one call site per field-pass. */
function propertyPassProps(params: PropertyPassParams, key: string, propertySchema: ToolPropertySchema, required: boolean, noAccordionWrapper = false) {
  const { formState, settings, editField, handleInputChange, disabled, credentialContext, slots, editFieldRootPath, showOnlyRequiredFields, showDisabledConfigFields, enableEditEliteaTitle } =
    params;
  return {
    field: { key, schema: propertySchema, required, editFieldRootPath },
    formState,
    settings,
    editField,
    handleInputChange,
    visibility: {
      showOnlyRequiredFields,
      showOnlyConfigurationFields: false,
      disableConfigFields: Boolean(showDisabledConfigFields && propertySchema.configuration) || (key === 'elitea_title' && !enableEditEliteaTitle),
      noAccordionWrapper,
    },
    disabled: disabled && propertySchema.type !== 'configuration',
    credentialContext,
    slots,
  };
}

export interface FieldEntryGroups {
  readonly priorityEntries: ReturnType<typeof schemaEntries>;
  readonly mainEntries: ReturnType<typeof schemaEntries>;
  readonly advancedEntries: ReturnType<typeof schemaEntries>;
  readonly bottomEntries: ReturnType<typeof schemaEntries>;
}

/** The 4 field-pass entry lists — ported from `ToolBase.jsx`'s 4 repeated `Object.entries(schema.properties).filter(...)` blocks. */
export function resolveFieldEntryGroups(schema: ToolSchema, sectionProps: readonly string[], fieldOrder: ResolvedFieldOrder): FieldEntryGroups {
  const { priorityFieldsOrder, fieldNeedToRenderAtBottom, excludedFields, advancedFields } = fieldOrder;
  const isSectionToolsOrAdvanced = (key: string): boolean => isSectionOrToolsField(key, sectionProps) || advancedFields.includes(key);
  const isSectionToolsOrExcluded = (key: string): boolean => isSectionOrToolsField(key, sectionProps) || excludedFields.includes(key);

  return {
    priorityEntries: pickSchemaFields(schema, priorityFieldsOrder).filter(({ key }) => !isSectionToolsOrAdvanced(key)),
    mainEntries: schemaEntries(schema).filter(({ key }) =>
      isMainPassField(key, sectionProps, priorityFieldsOrder, fieldNeedToRenderAtBottom, excludedFields, advancedFields),
    ),
    advancedEntries: pickSchemaFields(schema, advancedFields).filter(({ key }) => !isSectionToolsOrExcluded(key)),
    bottomEntries: pickSchemaFields(schema, fieldNeedToRenderAtBottom).filter(({ key }) => !isSectionToolsOrExcluded(key)),
  };
}

function renderPropertyList(entries: ReturnType<typeof schemaEntries>, passParams: PropertyPassParams, schema: ToolSchema, noAccordionWrapper = false): ReactNode {
  return entries.map(({ key, schema: propertySchema }) => (
    <ToolBaseProperty
      key={key}
      {...propertyPassProps(passParams, key, propertySchema, resolveRequired(key, schema.required, passParams.settings['selected_tools'] as never), noAccordionWrapper)}
    />
  ));
}

export interface ToolBaseConfigurationBodyProps {
  readonly editToolDetail: EditToolDetail;
  readonly schema: ToolSchema;
  readonly passParams: PropertyPassParams;
  readonly entries: FieldEntryGroups;
  readonly presentation: ResolvedFieldPresentation;
  readonly behavior: ResolvedFieldBehavior;
  readonly sectionsResolved: ResolvedSections;
  readonly setEditToolDetail: SetEditToolDetail;
  readonly slots: ToolBaseSlots | undefined;
  readonly checkboxAsteriskRequired: boolean;
  readonly showDisabledConfigFields: boolean;
  readonly disabled: boolean | undefined;
}

/**
 * The name/description slot's resolved argument bag + gating — split out of
 * `ToolBaseConfigurationBody` to keep it under the §3.5 complexity budget.
 *
 * R2 fix: `slots?.renderNameDescriptionInput` remains a caller OVERRIDE
 * (still exercised by `ToolBase.test.tsx`), but when no slot is supplied
 * this now renders the real, now-landed `../NameDescriptionInput.tsx`
 * directly — matching the baseline, which rendered
 * `ToolkitForm.NameDescriptionInput` inline, unconditionally
 * (`ToolBase.jsx:225-245`). Previously this fell straight through to `null`
 * whenever no slot was supplied, which is what the live composition root
 * (`ToolkitForm.hooks.ts`, no `slots` concept) always does — every typed
 * toolkit's config form was missing its Name/Description fields entirely.
 * Two small adapters bridge the real component's own, independently-typed
 * contract: `editField` there is `(field: 'name'|'description', value:
 * string) => void` (a `EditToolField` call with a plain, unprefixed field
 * name already satisfies this — `'name'`/`'description'` are themselves
 * valid top-level `editToolDetail` paths, not `settings.*` ones); `toolErrors`
 * there is `Record<string, boolean|undefined>`, narrower than this file's
 * own `boolean|string`-valued `ToolErrors`, so only the two keys it reads
 * are forwarded, coerced to `boolean`.
 */
function resolveNameDescriptionSlot(
  editToolDetail: EditToolDetail,
  passParams: PropertyPassParams,
  presentation: ResolvedFieldPresentation,
  behavior: ResolvedFieldBehavior,
  slots: ToolBaseSlots | undefined,
  disabled: boolean | undefined,
): ReactNode {
  if (presentation.hideNameDescriptionInput) return null;
  const configurationTitle = (passParams.settings['elitea_title'] as string | undefined) || (passParams.settings['configuration_title'] as string | undefined) || '';

  if (slots?.renderNameDescriptionInput) {
    return (
      slots.renderNameDescriptionInput({
        type: editToolDetail.type ?? '',
        name: editToolDetail.name ?? '',
        toolkitName: editToolDetail.toolkit_name ?? '',
        description: editToolDetail.description ?? '',
        editField: passParams.editField,
        showValidation: passParams.formState.showValidation,
        toolErrors: passParams.formState.toolErrors,
        showOnlyRequiredFields: presentation.showOnlyRequiredFields,
        showOnlyConfigurationFields: presentation.showOnlyConfigurationFields,
        showNameFieldForcedly: presentation.showNameFieldForcedly,
        showToolkitIcon: presentation.showToolkitIcon,
        hideNameInput: presentation.hideNameInput,
        configurationTitle,
        isMCP: behavior.isMCP,
        disabled: Boolean(disabled),
      }) ?? null
    );
  }

  return (
    <NameDescriptionInput
      type={editToolDetail.type}
      name={editToolDetail.name}
      toolkitName={editToolDetail.toolkit_name}
      description={editToolDetail.description}
      editField={(field, value) => passParams.editField(field, value)}
      showValidation={passParams.formState.showValidation}
      toolErrors={{ name: Boolean(passParams.formState.toolErrors['name']), description: Boolean(passParams.formState.toolErrors['description']) }}
      showOnlyRequiredFields={presentation.showOnlyRequiredFields}
      showOnlyConfigurationFields={presentation.showOnlyConfigurationFields}
      showNameFieldForcedly={presentation.showNameFieldForcedly}
      showToolkitIcon={presentation.showToolkitIcon}
      hideNameInput={presentation.hideNameInput}
      configuration_title={configurationTitle}
      isMCP={behavior.isMCP}
      disabled={Boolean(disabled)}
    />
  );
}

/** The name/description slot + priority/main/advanced/sections/bottom field passes (`ToolBase.jsx:220-333`, `toolBaseConfiguration`). */
export function ToolBaseConfigurationBody({
  editToolDetail,
  schema,
  passParams,
  entries,
  presentation,
  behavior,
  sectionsResolved,
  setEditToolDetail,
  slots,
  checkboxAsteriskRequired,
  showDisabledConfigFields,
  disabled,
}: ToolBaseConfigurationBodyProps): ReactNode {
  const nameDescriptionSlot = resolveNameDescriptionSlot(editToolDetail, passParams, presentation, behavior, slots, disabled);

  return (
    <Box sx={configurationContainerSx}>
      {nameDescriptionSlot}
      {renderPropertyList(entries.priorityEntries, passParams, schema)}
      {renderPropertyList(entries.mainEntries, passParams, schema)}
      {entries.advancedEntries.length > 0 && (
        <Box sx={advancedContainerSx}>
          <BasicAccordion
            defaultExpanded={false}
            items={[{ title: 'Advanced Settings', content: renderPropertyList(entries.advancedEntries, passParams, schema, true) }]}
          />
        </Box>
      )}
      {behavior.showSections && sectionsResolved.sectionProps.length > 0 && (
        <ToolBaseSections
          sections={sectionsResolved.sections}
          schema={schema}
          passParams={passParams}
          setEditToolDetail={setEditToolDetail}
          checkboxAsteriskRequired={checkboxAsteriskRequired}
          showDisabledConfigFields={showDisabledConfigFields}
          disabled={disabled}
        />
      )}
      {renderPropertyList(entries.bottomEntries, passParams, schema)}
    </Box>
  );
}

interface ToolBaseSectionsProps {
  readonly sections: ResolvedSections['sections'];
  readonly schema: ToolSchema;
  readonly passParams: PropertyPassParams;
  readonly setEditToolDetail: SetEditToolDetail;
  readonly checkboxAsteriskRequired: boolean;
  readonly showDisabledConfigFields: boolean;
  readonly disabled: boolean | undefined;
}

/** The metadata-section list (`showSections` branch), its own function so its `.map()` stays out of `ToolBaseConfigurationBody`'s complexity count. */
function ToolBaseSections({ sections, schema, passParams, setEditToolDetail, checkboxAsteriskRequired, showDisabledConfigFields, disabled }: ToolBaseSectionsProps): ReactNode {
  return Object.entries(sections).map(([sectionKey, section]) => (
    <ToolSection
      key={sectionKey}
      identity={{ sectionKey, subsections: section.subsections, required: section.required, schema }}
      formState={passParams.formState}
      settings={passParams.settings}
      editField={passParams.editField}
      handleInputChange={passParams.handleInputChange}
      setEditToolDetail={setEditToolDetail}
      visibility={{ showOnlyConfigurationFields: false, disableConfigFields: showDisabledConfigFields, checkboxAsteriskRequired }}
      disabled={disabled}
      credentialContext={passParams.credentialContext}
      slots={passParams.slots}
    />
  ));
}

export interface ToolBaseToolsSectionProps {
  readonly schema: ToolSchema;
  readonly editToolDetail: EditToolDetail;
  readonly passParams: PropertyPassParams;
  readonly isMcpExposureEnabled: boolean;
  readonly shouldUseAccordionView: boolean;
  readonly slots: ToolBaseSlots | undefined;
  readonly disabled: boolean | undefined;
}

function resolveAvailableTools(schema: ToolSchema, settings: Readonly<Record<string, unknown>>): readonly string[] {
  const selectedToolsSchema = schema.properties?.['selected_tools'];
  const argsSchemas = selectedToolsSchema?.args_schemas;
  const hasArgsSchemas = argsSchemas !== undefined && Object.keys(argsSchemas).length > 0;
  const availableMcpTools = (settings['available_mcp_tools'] as readonly string[] | undefined) ?? [];
  return (hasArgsSchemas ? Object.keys(argsSchemas) : selectedToolsSchema?.items?.enum) ?? availableMcpTools;
}

function isPreconfiguredMcpType(editToolDetail: EditToolDetail): boolean {
  return Boolean(editToolDetail.type?.startsWith('mcp_')) && editToolDetail.type !== 'mcp';
}

/**
 * The SharePoint connection-status widget. UNLIKE its `mcpAuthStatus`/
 * `openApiOAuthStatus` neighbours, this one is NOT slot-only: SharePoint
 * lives under `features/toolkits/sharepoint/**`, i.e. inside this very
 * slice, so importing it is an ordinary intra-slice import with no
 * `no-sideways-features` problem. It was slot-only anyway until this change,
 * and — exactly like `renderNameDescriptionInput`/`renderOpenApiSpecField`
 * before the R2 fix — no caller ever supplied the slot, so a SharePoint
 * toolkit's settings form rendered no login UI at all in production.
 *
 * Only the two MODALS (`features/mcps`' `McpAuthModal`/`McpLogoutModal`)
 * genuinely have to come from above; they arrive as
 * `slots.sharepointAuthModals`.
 */
function SharepointStatus({ editToolDetail, projectId, slots }: { editToolDetail: EditToolDetail; projectId: string | undefined; slots: ToolBaseSlots | undefined }): ReactNode {
  if (slots?.sharepointOAuthStatus !== undefined) return slots.sharepointOAuthStatus;
  const modals = slots?.sharepointAuthModals;
  // `exactOptionalPropertyTypes`: both keys are OMITTED when absent, never
  // set to an explicit `undefined` — this codebase's standing convention.
  const configRef = editToolDetail.settings['sharepoint_configuration'] as SharepointConfigRef | undefined;
  return (
    <SharepointOAuthStatus
      values={{ ...(editToolDetail.id !== undefined && { id: editToolDetail.id }), settings: { ...(configRef !== undefined && { sharepoint_configuration: configRef }) } }}
      projectId={projectId}
      {...(modals?.renderAuthModal !== undefined && { renderAuthModal: modals.renderAuthModal })}
      {...(modals?.renderLogoutModal !== undefined && { renderLogoutModal: modals.renderLogoutModal })}
    />
  );
}

/** The 3 caller-injected connection-status slots (`ToolBase.jsx:573-577`), gated on the schema/toolkit-type booleans — split into its own function so `ToolBase` itself stays under the §3.5 complexity budget. */
export function ToolBaseStatusSlots({ schema, editToolDetail, projectId, slots, showTools }: { schema: ToolSchema; editToolDetail: EditToolDetail; projectId: string | undefined; slots: ToolBaseSlots | undefined; showTools: boolean }): ReactNode {
  const isAnyMcpType = schema.title === 'mcp' || isPreconfiguredMcpType(editToolDetail);
  const isSharepointToolkit = schema.title === 'sharepoint';
  const isOpenApiToolkit = editToolDetail.type === 'openapi';
  return (
    <>
      {isAnyMcpType && slots?.mcpAuthStatus}
      {isSharepointToolkit && (
        <SharepointStatus
          editToolDetail={editToolDetail}
          projectId={projectId}
          slots={slots}
        />
      )}
      {isOpenApiToolkit && showTools && slots?.openApiOAuthStatus}
    </>
  );
}

/** The "Make tools available by MCP" extra field — split out of `ToolBaseToolsSection` to keep it under the §3.5 complexity budget. */
function resolveMcpExposureField(
  isMcpExposureEnabled: boolean,
  editToolDetail: EditToolDetail,
  passParams: PropertyPassParams,
  disabled: boolean | undefined,
): ReactNode {
  if (!isMcpExposureEnabled) return null;
  return (
    <ToolBaseProperty
      field={{ key: 'available_by_mcp', schema: { title: 'Make tools available by MCP', type: 'boolean' }, required: false, editFieldRootPath: 'meta.mcp_options' }}
      formState={passParams.formState}
      settings={editToolDetail.meta?.mcp_options ?? {}}
      editField={passParams.editField}
      handleInputChange={passParams.handleInputChange}
      disabled={disabled}
    />
  );
}

/** The `selected_tools` chip picker (`ToolBase.jsx:524-564`, `renderTools`). */
export function ToolBaseToolsSection({ schema, editToolDetail, passParams, isMcpExposureEnabled, shouldUseAccordionView, slots, disabled }: ToolBaseToolsSectionProps): ReactNode {
  const isPreconfiguredMcp = isPreconfiguredMcpType(editToolDetail);
  const extraProperties = resolveMcpExposureField(isMcpExposureEnabled, editToolDetail, passParams, disabled);

  return (
    <ToolActionsSelector
      availableTools={slots?.toolActionsExtra?.availableTools ?? resolveAvailableTools(schema, passParams.settings)}
      onChange={(value) => passParams.editField('settings.selected_tools', value)}
      selectedTools={passParams.settings['selected_tools'] as readonly string[] | undefined}
      isRemoteMcp={schema.title === 'mcp'}
      isPreconfiguredMcp={isPreconfiguredMcp}
      extraProperties={extraProperties}
      disabled={disabled}
      onLoadTools={slots?.toolActionsExtra?.onLoadTools}
      isLoadingTools={slots?.toolActionsExtra?.isLoadingTools}
      canLoadTools={slots?.toolActionsExtra?.canLoadTools}
      mcpAuthModal={slots?.toolActionsExtra?.mcpAuthModal}
      shouldUseAccordionView={shouldUseAccordionView}
    />
  );
}

const configurationContainerSx = { display: 'flex', flexDirection: 'column' as const, gap: '0.5rem' };
const advancedContainerSx = { display: 'flex', flexDirection: 'column' as const, marginTop: '0.5rem' };
