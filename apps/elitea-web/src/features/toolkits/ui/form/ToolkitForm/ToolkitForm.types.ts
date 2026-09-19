import type { SxProps, Theme } from '@mui/material/styles';

import type { ValidationErrorEntry } from '../../../lib/helpers/toolkitForm.helpers';
import type { RawToolkitTypeSchema } from '../../../lib/helpers/toolkitSchema.helpers';
import type { UseCreateConfigurationInput } from '../../../model/useCreateConfiguration';

import type { ToolBaseSlots } from '../ToolBase/ToolBase.types';

import type { SaveToolkitPayload } from './ToolkitsOperationButtons';

/**
 * `ToolkitForm.tsx`'s own prop/state types — split out of that file purely
 * to stay under the §3.5 400-line-per-file budget (the original single-file
 * port was 660 lines). Re-exported from `ToolkitForm.tsx` itself (this
 * slice's real, landed intra-slice consumers — `ToolkitEditorParts.tsx`/
 * `ConfigurationTab.tsx` — import `ToolkitForm`/`ToolkitFormEditDetail`
 * from that exact path, not this one), so no caller-visible change.
 */
export interface ToolkitFormEditDetail {
  readonly index?: number;
  readonly id?: string | number;
  readonly type?: string;
  readonly name?: string;
  readonly settings?: Record<string, unknown>;
  readonly schema?: RawToolkitTypeSchema;
  readonly meta?: { readonly mcp_options?: Readonly<Record<string, unknown>> };
  readonly isLoadingConfigurations?: boolean;
  readonly [key: string]: unknown;
}

export interface ToolkitValidationInjected {
  readonly isError: boolean;
  readonly error: { readonly data?: { readonly settings_errors?: readonly ValidationErrorEntry[] } } | undefined;
  readonly refetch: () => void;
}

/** `elitea_title`/`private` mirror a saved-configuration reference on the toolkit's own settings — explicit `| undefined` throughout (this app's `exactOptionalPropertyTypes`; see `ToolFormContainer.tsx`'s own doc comment for the same convention). */
export interface ToolkitConfigurationState {
  readonly elitea_title?: string | undefined;
  readonly private?: boolean | undefined;
}

export interface ToolkitFormProps {
  readonly editToolDetail: ToolkitFormEditDetail;
  /**
   * `prev`/return are `| null` (not just `ToolkitFormEditDetail`) to match
   * this prop's real callers' own state shape — both `ToolkitEditor.tsx`
   * and `ConfigurationTab.tsx` (sibling A4 sub-units' files, independently
   * arrived at the identical `useState<ToolkitFormEditDetail | null>(null)`
   * "no tool selected yet" shape) declare `onChangeToolDetail` this way.
   * `ToolkitForm.tsx` itself never actually returns `null` from its own
   * updater calls (see `editField`'s `prevState ?? {}` guard) — this is a
   * type-level accommodation for real callers' state shape, not a
   * behavioural change.
   */
  readonly onChangeToolDetail: (
    updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null,
    options?: { readonly isAutoSelect?: boolean },
  ) => void;
  readonly isEditing: boolean;
  readonly isToolDirty?: boolean | undefined;
  readonly hasNotSavedCredentials?: boolean | undefined;
  readonly isViewToggleVisible?: boolean | undefined;
  readonly configurationViewOptions?: string | undefined;
  readonly hideConfigurationNameInput?: boolean | undefined;
  readonly showOnlyRequiredFields?: boolean | undefined;
  readonly showOnlyConfigurationFields?: boolean | undefined;
  readonly showNameFieldForcedly?: boolean | undefined;
  readonly showToolkitIcon?: boolean | undefined;
  readonly hideNameDescriptionInput?: boolean | undefined;
  readonly hideNameInput?: boolean | undefined;
  readonly hideOperationButtons?: boolean | undefined;
  readonly updateKey?: string | number | undefined;
  readonly sx?: SxProps<Theme> | undefined;
  readonly isMCP?: boolean | undefined;
  readonly onValidationStateChange?: ((state: { readonly hasErrors: boolean; readonly triggerValidation: () => void }) => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly onSyntaxError?: ((errors: readonly unknown[]) => void) | undefined;
  readonly validationTrigger?: boolean | undefined;
  readonly revertCredentialsRef?: { current: (() => void) | undefined } | undefined;

  readonly projectId: string | undefined;
  readonly personalProjectId?: string | number | undefined;
  readonly routeToolkitType?: string | undefined;
  readonly forceCustomView?: boolean | undefined;
  readonly formValues: Readonly<Record<string, unknown>>;
  readonly formInitialValues: Readonly<Record<string, unknown>> | undefined;
  readonly onSetFormField?: ((field: string, value: unknown) => void | Promise<void>) | undefined;
  readonly onResetForm?: ((values: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly isTeamProject?: boolean | undefined;
  readonly onSave: (payload: SaveToolkitPayload) => Promise<Readonly<Record<string, unknown>>>;
  readonly onSaveSuccess?: ((savedValues: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly onSaveError?: ((message: string) => void) | undefined;
  readonly onConfigurationCreated?: (() => void) | undefined;
  readonly toolkitValidation?: ToolkitValidationInjected | undefined;
  readonly onMcpScopesChanged?: ((url: string | undefined) => void) | undefined;
  readonly getAccessToken?: UseCreateConfigurationInput['getAccessToken'] | undefined;
  readonly onConfigAuthRequired?: UseCreateConfigurationInput['onConfigAuthRequired'] | undefined;
  /**
   * Forwarded verbatim into the resolved tool component's prop bag
   * (`ToolkitForm.hooks.ts`'s `toolComponentProps`), i.e. `ToolBase`'s own
   * `slots`. This composition root previously had NO slots concept at all,
   * which is what left `ToolBase`'s cross-slice slots permanently unfilled —
   * see `ToolBase.tsx`'s own R2 note for the same failure mode on
   * `renderNameDescriptionInput`. Today's real supplier is
   * `ConfigurationTab`, forwarding `pages/toolkits`' `McpAuthModal`/
   * `McpLogoutModal` renderers as `slots.sharepointAuthModals`.
   */
  readonly slots?: ToolBaseSlots | undefined;
}

/**
 * Every field below is optional-with-a-default in `ToolkitFormProps` —
 * merging this constant under the raw props (`{...DEFAULT_TOOLKIT_FORM_PROPS,
 * ...rawProps}`) resolves every default via one object spread instead of
 * ~12 per-field `= value` destructuring defaults, which oxlint's
 * `complexity` rule counts as one branch each — same "spread-merge over
 * default-param branches" technique `NameDescriptionInput.tsx`'s own doc
 * comment already documents for an identical budget problem.
 */
const DEFAULT_TOOLKIT_FORM_PROPS = {
  hasNotSavedCredentials: false,
  isViewToggleVisible: true,
  hideConfigurationNameInput: false,
  showOnlyRequiredFields: false,
  showOnlyConfigurationFields: false,
  showNameFieldForcedly: false,
  showToolkitIcon: false,
  hideNameDescriptionInput: false,
  hideNameInput: false,
  hideOperationButtons: false,
  forceCustomView: false,
} as const satisfies Partial<ToolkitFormProps>;

export type ResolvedToolkitFormProps = Omit<ToolkitFormProps, keyof typeof DEFAULT_TOOLKIT_FORM_PROPS> & typeof DEFAULT_TOOLKIT_FORM_PROPS;

/**
 * The one merge site. TypeScript's own inferred type for `{...a, ...b}`
 * widens a defaulted field back to `b`'s (optional, possibly-`undefined`)
 * declared type even when `b`'s value is structurally absent at that key —
 * it cannot see that "key omitted" and "key present with value `undefined`"
 * collapse to the same runtime `DEFAULT_TOOLKIT_FORM_PROPS` fallback here.
 * One explicit, reviewed cast at this single call site (matching
 * `shared/ui/lib/combineSx.ts`'s own "one explicit, reviewed cast replaces
 * what would otherwise be an unsafe cast at every call site" precedent)
 * documents that gap instead of forcing every field of
 * `DEFAULT_TOOLKIT_FORM_PROPS` back into a widened, always-optional type.
 */
export function resolveToolkitFormProps(rawProps: ToolkitFormProps): ResolvedToolkitFormProps {
  return { ...DEFAULT_TOOLKIT_FORM_PROPS, ...rawProps } as ResolvedToolkitFormProps;
}
