import type { ToolFormComponent } from '../../../lib/helpers/toolComponent.helpers';
import type { RawToolkitTypeSchema } from '../../../lib/helpers/toolkitSchema.helpers';

import { resolveExcludedFields } from './ToolkitForm.helpers';
import { useToolkitFormConfiguration } from './ToolkitForm.configuration.hooks';
import { useToolkitFormCore } from './ToolkitForm.core.hooks';
import { useCredentialLikeFieldSlot } from './useCredentialLikeFieldSlot';
import type { ResolvedToolkitFormProps, ToolkitConfigurationState } from './ToolkitForm.types';

/**
 * `useToolkitFormState` — every `useState`/`useMemo`/`useCallback`/
 * `useEffect` `ToolkitForm.tsx` needs, composed from `useToolkitFormCore`
 * (`ToolkitForm.core.hooks.ts`) and `useToolkitFormConfiguration`
 * (`ToolkitForm.configuration.hooks.ts`) — split across those two files
 * purely to stay under the §3.5 400-line-per-file / complexity-12 budgets
 * (the un-split component measured 660 lines / complexity 42).
 * `ToolkitForm.tsx`'s own function body is now just this hook call plus a
 * JSX render — see that file's own doc comment for the full DISCLOSED
 * REDESIGNS list (Formik/Redux, backend gaps, etc.), which applies here
 * unchanged since this is the same logic, just relocated.
 */
export interface ToolkitFormState {
  readonly isLoading: boolean;
  readonly view: string;
  readonly setView: (view: string) => void;
  readonly onManualViewChange: (view: string) => void;
  readonly isValidSchema: boolean;
  readonly effectiveToolSchema: RawToolkitTypeSchema | undefined;
  /** A read that feeds the "Tools" section failed (#440). Show it; an empty section must not stand for it. */
  readonly toolListReadFailed: boolean;
  /** Runs both reads again. */
  readonly retryToolListRead: () => void;
  readonly hasErrors: boolean;
  readonly configuration: ToolkitConfigurationState;
  readonly isCreatingConfiguration: boolean;
  readonly isTestingConnection: boolean;
  readonly onCreateConfiguration: () => Promise<boolean>;
  readonly onTestConnection: () => Promise<boolean>;
  readonly onRevertCredentials: () => void;
  readonly setShowValidation: (show: boolean) => void;
  readonly editField: (field: string, value: unknown, replace?: boolean, options?: { readonly isAutoSelect?: boolean }) => Promise<void>;
  readonly setToolErrors: (updater: (prev: Readonly<Record<string, boolean>>) => Record<string, boolean>) => void;
  readonly ToolComponent: ToolFormComponent | undefined;
  readonly toolComponentProps: Record<string, unknown>;
}

export function useToolkitFormState(props: ResolvedToolkitFormProps): ToolkitFormState {
  const core = useToolkitFormCore(props);
  const config = useToolkitFormConfiguration(props, core);

  const {
    editToolDetail,
    onChangeToolDetail,
    validationTrigger,
    configurationViewOptions,
    hideConfigurationNameInput,
    showOnlyRequiredFields,
    showOnlyConfigurationFields,
    showNameFieldForcedly,
    showToolkitIcon,
    hideNameDescriptionInput,
    hideNameInput,
    isMCP,
    disabled,
    onSyntaxError,
    projectId,
    slots,
  } = props;
  const { view, setView, onManualViewChange, isValidSchema, effectiveToolSchema, toolListReadFailed, retryToolListRead, hasErrors, mergedToolErrors, editField, setToolErrors, showValidation, configurationErrors, setConfigurationErrors, configurationName, setConfigurationName, configuration, setConfiguration, toolType, ToolComponent } = core;
  const { isCreatingConfiguration, isTestingConnection, onCreateConfiguration, onTestConnection, onRevertCredentials, shouldShowDisabledConfigFields, onCredentialReload, isLoading } = config;
  const renderCredentialLikeField = useCredentialLikeFieldSlot(projectId, slots?.renderCredentialPicker);

  // A PLAIN object literal, not `useMemo`'d: the baseline itself never
  // memoized this bag either (`ToolkitForm.jsx:502-535` builds it fresh
  // every render), and the dynamically-resolved `ToolComponent` (`ToolBase`/
  // `ToolJira`/`ToolConfluence`/`ToolCustom` — sibling A4 components with
  // their own, independently-designed FLAT prop contracts this file cannot
  // change) genuinely needs every one of these ~29 keys individually, so
  // there is no smaller "grouped" shape to memoize against without breaking
  // those real consumers. `useMemo` here would need a dependency array this
  // wide regardless — recomputing a plain object on every render is exactly
  // as cheap as the baseline's own always-fresh object literal, and avoids
  // the §3.5 `hook-deps` budget (8) a same-sized `useMemo` dependency array
  // would blow past for no behavioural gain.
  const toolComponentProps: Record<string, unknown> = {
    editToolDetail,
    setEditToolDetail: onChangeToolDetail,
    editField,
    toolErrors: mergedToolErrors,
    setToolErrors,
    showValidation: showValidation || Boolean(validationTrigger),
    configurationErrors,
    setConfigurationErrors,
    configurationName,
    setConfigurationName,
    configuration,
    setConfiguration,
    schema: effectiveToolSchema,
    configurationSchema: undefined,
    configurationViewOptions,
    hideConfigurationNameInput,
    showOnlyRequiredFields,
    showOnlyConfigurationFields,
    showNameFieldForcedly,
    showToolkitIcon,
    hideNameDescriptionInput,
    hideNameInput,
    disabledConfigFieldsForOldToolkits: shouldShowDisabledConfigFields,
    shouldInitRequiredFields: false,
    isMCP,
    needToCheckSection: false,
    disabled,
    onSyntaxError,
    excludedFields: resolveExcludedFields(toolType),
    onCredentialReload,
    onCreateConfiguration,
    onTestConnection,
    // `projectId`/`slots` are what let `ToolBase` render a REAL
    // `SharepointOAuthStatus` (with a real, caller-supplied `McpAuthModal`)
    // instead of the nothing it rendered while this bag had neither.
    projectId,
    /*
     * #308 — `slots.renderCredentialLikeField` had NO supplier anywhere, and
     * `ToolBaseProperty.dispatch.tsx`'s `renderCredentialLike` returns `null`
     * without one, so every model/credential field rendered as blank space.
     * The default is merged UNDER the caller's `slots` so a page that supplies
     * its own renderer still wins.
     *
     * A page that only needs the credential picker must supply
     * `slots.renderCredentialPicker`, NOT this slot: the default above already
     * reads that narrower slot and keeps the three model pickers. Replacing
     * this slot wholesale drops them.
     */
    slots: { renderCredentialLikeField, ...slots },
  };

  return {
    isLoading,
    view,
    setView,
    onManualViewChange,
    isValidSchema,
    effectiveToolSchema,
    toolListReadFailed,
    retryToolListRead,
    hasErrors,
    configuration,
    isCreatingConfiguration,
    isTestingConnection,
    onCreateConfiguration,
    onTestConnection,
    onRevertCredentials,
    setShowValidation: core.setShowValidation,
    editField,
    setToolErrors,
    ToolComponent,
    toolComponentProps,
  };
}
