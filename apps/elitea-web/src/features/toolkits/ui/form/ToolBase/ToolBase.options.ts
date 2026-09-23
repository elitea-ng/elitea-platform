import type { ToolBasePropertyCredentialContext } from './ToolBaseProperty';
import type { ToolBaseFieldOrder, ToolBaseFieldVisibility, ToolBaseProps, ToolBaseSections } from './ToolBase.types';
import type { ToolSectionSubsection } from './ToolSection';
import type { EditToolDetail, EditToolField, SetEditToolDetail, SetToolErrors, ToolErrors, ToolSchema } from './types';

/**
 * `ToolBase.tsx`'s destructuring-default resolution, split into its own
 * file/functions: the baseline's 19 flat destructured defaults
 * (`ToolBase.jsx:26-57`) all live directly in the render function's own
 * body, and each default value is its own `eslint(complexity)` branch —
 * concentrating all 19 (plus every other conditional in a 612-line
 * component) in one function is exactly what scored the baseline-shaped
 * port a complexity of 69 (§3.5 budget: 12) before this split. Two small
 * groups here, each safely under budget; no behaviour change.
 */
export interface ResolvedFieldPresentation {
  readonly showOnlyRequiredFields: boolean;
  readonly showOnlyConfigurationFields: boolean;
  readonly showNameFieldForcedly: boolean;
  readonly showToolkitIcon: boolean;
  readonly hideNameDescriptionInput: boolean;
  readonly hideNameInput: boolean;
}

export function resolveFieldPresentation(v: ToolBaseFieldVisibility | undefined): ResolvedFieldPresentation {
  const {
    showOnlyRequiredFields = false,
    showOnlyConfigurationFields = false,
    showNameFieldForcedly = false,
    showToolkitIcon = false,
    hideNameDescriptionInput = false,
    hideNameInput = false,
  } = v ?? {};
  return { showOnlyRequiredFields, showOnlyConfigurationFields, showNameFieldForcedly, showToolkitIcon, hideNameDescriptionInput, hideNameInput };
}

export interface ResolvedFieldBehavior {
  readonly disabledConfigFieldsForOldToolkits: boolean;
  readonly checkboxAsteriskRequired: boolean;
  readonly shouldInitRequiredFields: boolean;
  readonly showSections: boolean;
  readonly showTools: boolean;
  readonly isMCP: boolean;
}

export function resolveFieldBehavior(v: ToolBaseFieldVisibility | undefined): ResolvedFieldBehavior {
  const {
    disabledConfigFieldsForOldToolkits = false,
    checkboxAsteriskRequired = true,
    shouldInitRequiredFields = true,
    showSections = false,
    showTools = true,
    isMCP = false,
  } = v ?? {};
  return { disabledConfigFieldsForOldToolkits, checkboxAsteriskRequired, shouldInitRequiredFields, showSections, showTools, isMCP };
}

export interface ResolvedFieldOrder {
  readonly editFieldRootPath: string;
  readonly priorityFieldsOrder: readonly string[];
  readonly fieldNeedToRenderAtBottom: readonly string[];
  readonly excludedFields: readonly string[];
  readonly advancedFields: readonly string[];
}

export function resolveFieldOrder(v: ToolBaseFieldOrder | undefined): ResolvedFieldOrder {
  const {
    editFieldRootPath = 'settings',
    priorityFieldsOrder = [],
    fieldNeedToRenderAtBottom = [],
    excludedFields = [],
    advancedFields = [],
  } = v ?? {};
  return { editFieldRootPath, priorityFieldsOrder, fieldNeedToRenderAtBottom, excludedFields, advancedFields };
}

export interface ResolvedSections {
  readonly sections: Readonly<Record<string, { required: boolean; subsections: readonly ToolSectionSubsection[] }>>;
  readonly sectionProps: readonly string[];
}

export function resolveSections(v: ToolBaseSections | undefined): ResolvedSections {
  return { sections: v?.sections ?? {}, sectionProps: v?.sectionProps ?? [] };
}

const EMPTY_EDIT_TOOL_DETAIL: EditToolDetail = { settings: {} };
const EMPTY_SCHEMA: ToolSchema = {};
const EMPTY_TOOL_ERRORS: ToolErrors = {};
const NOOP_EDIT_FIELD: EditToolField = () => undefined;
const NOOP_SET_EDIT_TOOL_DETAIL: SetEditToolDetail = () => undefined;
const NOOP_SET_TOOL_ERRORS: SetToolErrors = () => undefined;

/**
 * R1 fix support: `ToolBaseProps` is flat now (see `ToolBase.types.ts`'s own
 * module doc comment), so every field the baseline defaulted via a plain
 * destructuring default (`ToolBase.jsx:27-32`: `editToolDetail = {}`,
 * `setEditToolDetail = () => {}`, `editField = () => {}`, `toolErrors = {}`,
 * `setToolErrors = () => {}`, `showValidation = false`) needs the same
 * resolution here — split into its own function for the same
 * complexity-budget reason as `resolveFieldPresentation`/
 * `resolveFieldBehavior`/`resolveFieldOrder` above. `schema` has no baseline
 * default (`ToolBase.jsx:30` destructures it bare) but every internal
 * `ToolBase.render.tsx`/`ToolBase.fields.ts` helper types its own `schema`
 * parameter as a required `ToolSchema` (not `| undefined`), so an empty
 * schema is resolved here rather than threading `| undefined` through every
 * one of those signatures — `schema?.properties`-style guards throughout
 * this file family already tolerate an empty schema identically to a
 * missing one.
 */
export interface ResolvedCoreProps {
  readonly editToolDetail: EditToolDetail;
  readonly setEditToolDetail: SetEditToolDetail;
  readonly editField: EditToolField;
  readonly toolErrors: ToolErrors;
  readonly setToolErrors: SetToolErrors;
  readonly showValidation: boolean;
  readonly schema: ToolSchema;
  readonly disabled: boolean;
  readonly shouldUseAccordionView: boolean;
}

export function resolveCoreProps(props: ToolBaseProps): ResolvedCoreProps {
  return {
    editToolDetail: props.editToolDetail ?? EMPTY_EDIT_TOOL_DETAIL,
    setEditToolDetail: props.setEditToolDetail ?? NOOP_SET_EDIT_TOOL_DETAIL,
    editField: props.editField ?? NOOP_EDIT_FIELD,
    toolErrors: props.toolErrors ?? EMPTY_TOOL_ERRORS,
    setToolErrors: props.setToolErrors ?? NOOP_SET_TOOL_ERRORS,
    showValidation: props.showValidation ?? false,
    schema: props.schema ?? EMPTY_SCHEMA,
    disabled: props.disabled ?? false,
    shouldUseAccordionView: props.shouldUseAccordionView ?? true,
  };
}

/** `credentialContext` (an advanced escape hatch) wins when supplied; otherwise the plain `onCredentialReload` prop (the baseline's own `ToolBase.jsx:56` shape) is wrapped into the same struct `ToolBaseProperty`'s `credentialContext` prop expects. */
export function resolveCredentialContext(props: ToolBaseProps): ToolBasePropertyCredentialContext | undefined {
  const base = props.credentialContext ?? (props.onCredentialReload ? { onCredentialReload: props.onCredentialReload } : undefined);
  // #902: merged ON TOP of either branch, so the escape hatch above does not
  // swallow the project scope the composition root knows.
  if (props.isTeamProject === undefined) return base;
  return { ...base, isTeamProject: props.isTeamProject };
}
