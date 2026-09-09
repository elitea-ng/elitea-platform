/**
 * The two render-prop slots `./credentialPicker.tsx`'s `ToolkitCredentialPicker`
 * fills (#308):
 *
 *  - `ToolBaseSlots.renderCredentialPicker` — the toolkit form's
 *    `configuration`-kind field (`ToolBaseProperty.dispatch.tsx`).
 *  - `IndexScheduleModalProps.renderCredentialsSelect` — the index schedule
 *    modal's credential select (`IndexScheduleModal.tsx`).
 *
 * Both context shapes are declared STRUCTURALLY here rather than imported.
 * `no-deep-slice-import` lets `pages/` enter a slice only through its
 * `index.ts`, and `features/toolkits`' barrel exports neither type (its §3.5
 * 20-symbol budget is full). TypeScript checks the two ends structurally at
 * each supplier's call site, which is the same technique
 * `./sharepointAuthModals.tsx` already uses for `features/mcps`' prop shapes.
 */
import { useCallback, type ReactNode } from 'react';

import { ToolkitCredentialPicker } from './credentialPicker';
import type { SelectedCredentialRefusal } from './credentialPicker';

/** The subset of `features/toolkits`' `CredentialLikeFieldContext` a credential picker reads. A SUPERTYPE of the real context, so the supplier stays assignable to the slot under `strictFunctionTypes`. */
interface CredentialFieldContextLike {
  readonly schema: {
    readonly section?: string | undefined;
    readonly configuration_types?: readonly string[] | undefined;
  };
  readonly label: string;
  readonly required: boolean;
  readonly disabled: boolean;
  readonly error: boolean;
  readonly helperText: string | undefined;
  readonly value: unknown;
  readonly onChange: (value: unknown, options?: unknown) => void;
  readonly specifiedProjectId: string | number | undefined;
}

/** The subset of `features/toolkits`' `CredentialsSelectSlotProps` this supplier reads. */
interface ScheduleCredentialsSlotPropsLike {
  readonly value: unknown;
  readonly onChange: (value: unknown) => void;
  readonly label: string;
  readonly configurationTypes: readonly string[];
  readonly error: boolean;
  readonly helperText: string;
  readonly disabled: boolean;
  readonly onlyPublic: boolean;
}

const EMPTY_CONFIGURATION_TYPES: readonly string[] = [];

/**
 * The toolkit form's credential picker.
 *
 * `specifiedProjectId` wins over the selected project, matching every model
 * picker in `useCredentialLikeFieldSlot`: a toolkit shown in another project's
 * context lists that project's credentials, not the viewer's.
 */
export function useToolkitCredentialPickerSlot(
  projectId: string | undefined,
  /**
   * Optional refusal sink. One toolkit form can render SEVERAL
   * credential-kind fields, so each report is keyed by the field it came from
   * (`section:label`, the pair that identifies a field inside one form) —
   * without a key the second field's `null` would erase the first field's
   * refusal. See `./useCredentialSaveGate.ts`.
   */
  onRefusalChange?: (fieldKey: string, refusal: SelectedCredentialRefusal | null) => void,
): (context: CredentialFieldContextLike) => ReactNode {
  return useCallback(
    (context: CredentialFieldContextLike) => {
      const section = context.schema.section ?? 'credentials';
      const fieldKey = `${section}:${context.label}`;
      return (
        <ToolkitCredentialPicker
          projectId={context.specifiedProjectId === undefined ? projectId : String(context.specifiedProjectId)}
          section={section}
          configurationTypes={context.schema.configuration_types ?? EMPTY_CONFIGURATION_TYPES}
          value={context.value}
          onChange={context.onChange}
          field={{
            label: context.label,
            required: context.required,
            error: context.error,
            helperText: context.helperText,
            disabled: context.disabled,
          }}
          {...(onRefusalChange === undefined ? {} : { onRefusalChange: (refusal: SelectedCredentialRefusal | null) => onRefusalChange(fieldKey, refusal) })}
        />
      );
    },
    [projectId, onRefusalChange],
  );
}

/**
 * The index schedule modal's credential select.
 *
 * The modal owns the value and the error state and passes both in; this
 * supplier only turns them into the real picker. The section is always
 * `credentials`: `IndexActions`' `resolveCredentialsData` finds the modal's
 * property by scanning for a section that contains exactly that.
 */
export function useScheduleCredentialsSelectSlot(projectId: string | undefined): (props: ScheduleCredentialsSlotPropsLike) => ReactNode {
  return useCallback(
    (props: ScheduleCredentialsSlotPropsLike) => (
      <ToolkitCredentialPicker
        projectId={projectId}
        section="credentials"
        configurationTypes={props.configurationTypes}
        value={props.value}
        onChange={(value) => props.onChange(value)}
        field={{ label: props.label, error: props.error, helperText: props.helperText, disabled: props.disabled }}
        onlyPublic={props.onlyPublic}
      />
    ),
    [projectId],
  );
}
