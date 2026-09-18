import type { ChangeEvent } from 'react';

import type {
  CredentialLikeFieldContext,
  EditToolField,
  OnCredentialReload,
  OpenApiSpecFieldContext,
  SetToolErrors,
  ToolErrors,
  ToolPropertySchema,
  ToolSlotRenderer,
  ValidationErrorMessages,
} from './types';

/**
 * `ToolBaseProperty.tsx`'s prop-group interfaces, split out to stay under
 * the §3.5 400-line file budget — a file-organization change only, no
 * baseline equivalent. See `ToolBaseProperty.tsx`'s own module doc comment
 * for the full disclosed-redesign rationale.
 */
interface ToolBasePropertyField {
  readonly key: string;
  readonly schema: ToolPropertySchema | undefined;
  readonly required: boolean;
  readonly editFieldRootPath?: string | undefined;
}

export interface ToolBasePropertyFormState {
  readonly toolErrors: ToolErrors;
  readonly setToolErrors?: SetToolErrors | undefined;
  readonly showValidation: boolean;
  readonly validationErrorMessages?: ValidationErrorMessages | undefined;
}

interface ToolBasePropertyVisibility {
  readonly showOnlyRequiredFields?: boolean | undefined;
  readonly showOnlyConfigurationFields?: boolean | undefined;
  readonly disableConfigFields?: boolean | undefined;
  readonly noAccordionWrapper?: boolean | undefined;
}

export interface ToolBasePropertyCredentialContext {
  readonly specifiedProjectId?: string | number | undefined;
  readonly presetOptions?: unknown;
  readonly onCredentialReload?: OnCredentialReload | undefined;
  /** #902/ELITEA-0726: the selected project's scope, so a secret field's "Create new secret" entry can name it ("New Project Secret" / "New Private Secret"). `undefined` means the composition root does not know the scope — the generic label then stands, never a guess. */
  readonly isTeamProject?: boolean | undefined;
}

export interface ToolBasePropertySlots {
  readonly renderOpenApiSpecField?: ToolSlotRenderer<OpenApiSpecFieldContext> | undefined;
  readonly renderCredentialLikeField?: ToolSlotRenderer<CredentialLikeFieldContext> | undefined;
  /**
   * #308 — the credential picker for a `configuration`-kind field only.
   *
   * This is a SECOND, narrower slot beside `renderCredentialLikeField` on
   * purpose. The three model kinds are intra-slice: `useCredentialLikeFieldSlot`
   * renders `ModelSelectField` for them and no caller must supply anything.
   * The `configuration` kind is not: its picker is
   * `features/credentials`' `CredentialsSelect`, and `no-sideways-features`
   * forbids this slice from importing it, so only a `pages/`/`widgets/` root
   * can build it.
   *
   * Splitting the two keeps the page's duty to ONE kind. Making the page
   * supply `renderCredentialLikeField` instead would make it responsible for
   * the model kinds too (that slot replaces the default wholesale — see the
   * merge in `ToolkitForm.hooks.ts`), so a page that wired only the credential
   * picker would silently blank the model pickers again. That is the exact
   * defect #308 records.
   */
  readonly renderCredentialPicker?: ToolSlotRenderer<CredentialLikeFieldContext> | undefined;
}

export interface ToolBasePropertyProps {
  readonly field: ToolBasePropertyField;
  readonly formState: ToolBasePropertyFormState;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly editField: EditToolField;
  readonly handleInputChange: (fieldPath: string) => (event: ChangeEvent<HTMLInputElement>) => void;
  readonly visibility?: ToolBasePropertyVisibility | undefined;
  readonly disabled?: boolean | undefined;
  readonly credentialContext?: ToolBasePropertyCredentialContext | undefined;
  readonly slots?: ToolBasePropertySlots | undefined;
}
