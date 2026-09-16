import type { ReactNode } from 'react';

import type { ToolBasePropertyCredentialContext, ToolBasePropertySlots } from './ToolBaseProperty';
import type { ToolSectionSubsection } from './ToolSection';
import type { EditToolDetail, EditToolField, OnCredentialReload, SetEditToolDetail, SetToolErrors, ToolErrors, ToolSchema, ToolSlotRenderer, ValidationErrorMessages } from './types';
import type { NameDescriptionInputSlotProps } from './ToolBase.slots';
import type { SharepointAuthModalRenderers } from '../../../sharepoint/ui/SharepointOAuthStatus';

/**
 * `ToolBase.tsx`'s prop-group interfaces, split out to stay under the §3.5
 * 400-line file budget.
 *
 * **R1 FIX (was: `ToolBaseProps` used a grouped `{toolDetail: {value,
 * onChange}, formState, fieldVisibility, fieldOrder, ...}` shape).** The
 * live composition root (`ToolkitForm/ToolkitForm.hooks.ts`'s
 * `toolComponentProps`, not owned by this cluster) spreads a FLAT prop bag
 * — `editToolDetail`/`setEditToolDetail`/`editField`/`toolErrors`/
 * `setToolErrors`/`showValidation`/`schema`/... — onto whichever of
 * `ToolJira`/`ToolConfluence`/`ToolBase`/`ToolCustom` it resolves
 * (`lib/helpers/toolComponent.helpers.ts`'s `getToolComponent`), the exact
 * same shape `ToolCustomProps` (`../ToolCustom.tsx`) already accepts. The
 * grouped shape made `toolDetail` (and friends) `undefined` at runtime
 * whenever `ToolBase`/`ToolJira`/`ToolConfluence` were reached that way —
 * `ToolBase`'s very first line (`toolDetail.value`) threw. `ToolBaseProps`
 * is now flat, byte-for-byte the baseline `ToolBase.jsx`'s own 28
 * destructured props (`ToolBase.jsx:26-57`) plus the still-legitimate
 * extension points (`sections`/`credentialContext`/`slots`/
 * `shouldUseAccordionView`) a caller may still supply — see `ToolBase.tsx`'s
 * own module doc comment for how these are resolved back into the
 * (unchanged) internal grouped shapes `ToolBase.options.ts`/
 * `ToolBase.render.tsx` consume.
 */
export interface ToolBaseFieldVisibility {
  readonly showOnlyRequiredFields?: boolean | undefined;
  readonly showOnlyConfigurationFields?: boolean | undefined;
  readonly showNameFieldForcedly?: boolean | undefined;
  readonly showToolkitIcon?: boolean | undefined;
  readonly hideNameDescriptionInput?: boolean | undefined;
  readonly hideNameInput?: boolean | undefined;
  readonly disabledConfigFieldsForOldToolkits?: boolean | undefined;
  readonly checkboxAsteriskRequired?: boolean | undefined;
  readonly shouldInitRequiredFields?: boolean | undefined;
  readonly showSections?: boolean | undefined;
  readonly showTools?: boolean | undefined;
  readonly isMCP?: boolean | undefined;
}

export interface ToolBaseFieldOrder {
  readonly editFieldRootPath?: string | undefined;
  readonly priorityFieldsOrder?: readonly string[] | undefined;
  readonly fieldNeedToRenderAtBottom?: readonly string[] | undefined;
  readonly excludedFields?: readonly string[] | undefined;
  readonly advancedFields?: readonly string[] | undefined;
}

export interface ToolBaseSections {
  readonly sections: Readonly<Record<string, { required: boolean; subsections: readonly ToolSectionSubsection[] }>>;
  readonly sectionProps: readonly string[];
}

export interface ToolBaseSlots extends ToolBasePropertySlots {
  readonly renderNameDescriptionInput?: ToolSlotRenderer<NameDescriptionInputSlotProps> | undefined;
  readonly mcpAuthStatus?: ReactNode;
  /**
   * OVERRIDE, not the only source (R2's `renderNameDescriptionInput`
   * precedent): omitted, `ToolBaseStatusSlots` now renders the real,
   * intra-slice `<SharepointOAuthStatus>` (`../../../sharepoint/ui`) for a
   * sharepoint-titled schema instead of nothing. Supply this only to replace
   * that widget wholesale.
   */
  readonly sharepointOAuthStatus?: ReactNode;
  /**
   * The `features/mcps`-owned login/logout modals `SharepointOAuthStatus`
   * cannot import itself (`no-sideways-features`). Supplied by
   * `pages/toolkits` through `ConfigurationTab`/`ToolkitForm`; omitted, the
   * status pill still renders and still runs its connection check, but the
   * "open the OAuth modal on 401" step is a no-op — which is what the whole
   * SharePoint delegated path silently was before this was wired.
   */
  readonly sharepointAuthModals?: SharepointAuthModalRenderers | undefined;
  readonly openApiOAuthStatus?: ReactNode;
  readonly toolActionsExtra?:
    | {
        /** Discovery preview only; saved selections remain tool names. */
        readonly availableTools?: readonly string[] | undefined;
        readonly onLoadTools?: (() => void) | undefined;
        readonly isLoadingTools?: boolean | undefined;
        readonly canLoadTools?: boolean | undefined;
        readonly mcpAuthModal?: ReactNode;
      }
    | undefined;
}

/**
 * The flat "core" fields — everything `ToolBaseFieldVisibility`/
 * `ToolBaseFieldOrder` don't already cover. Intersected into `ToolBaseProps`
 * below rather than repeated inline, so `ToolBase.tsx` can hand the whole
 * `props` object straight to `resolveFieldPresentation`/`resolveFieldOrder`
 * (`ToolBase.options.ts`) — a value of an intersection type is structurally
 * assignable to each of its constituent types, so no adapter object needs
 * building at the call site.
 */
interface ToolBaseCoreProps {
  readonly editToolDetail?: EditToolDetail | undefined;
  readonly setEditToolDetail?: SetEditToolDetail | undefined;
  /** The project whose credentials the form is editing — forwarded by `ToolkitForm.hooks.ts`'s `toolComponentProps`; `SharepointOAuthStatus` needs it to resolve the referenced SharePoint credential. */
  readonly projectId?: string | undefined;
  readonly editField?: EditToolField | undefined;
  readonly toolErrors?: ToolErrors | undefined;
  readonly setToolErrors?: SetToolErrors | undefined;
  readonly showValidation?: boolean | undefined;
  readonly schema?: ToolSchema | undefined;
  readonly validationErrorMessages?: ValidationErrorMessages | undefined;
  /** `setConfigurationName` in the baseline (`ToolBase.jsx:36`) — fires on an `elitea_title`/`title` edit. */
  readonly setConfigurationName?: ((value: string) => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly onCredentialReload?: OnCredentialReload | undefined;
  /** Advanced/rarely-used escape hatch: a caller that already has a full `ToolBasePropertyCredentialContext` (e.g. `specifiedProjectId`/`presetOptions`) may supply it directly instead of the plain `onCredentialReload` above. */
  readonly credentialContext?: ToolBasePropertyCredentialContext | undefined;
  /** Caller-supplied metadata sections (the baseline's own `useToolkitConfigurationProperties({toolType})` result) — a real, disclosed, pre-existing gap (no port of that legacy top-level hook in scope), unrelated to R1/R2/R3. */
  readonly sections?: ToolBaseSections | undefined;
  readonly slots?: ToolBaseSlots | undefined;
  /** The baseline's `useToolkitView().shouldUseAccordionView` (route-derived) — a real, disclosed, pre-existing gap, unrelated to R1/R2/R3. Defaults to `true`, matching the baseline's own default render path. */
  readonly shouldUseAccordionView?: boolean | undefined;
}

/**
 * `ToolBase`/`ToolJira`/`ToolConfluence`'s public prop contract — flat, and
 * (by construction, via the intersection) structurally identical to
 * `ToolCustomProps` (`../ToolCustom.tsx`) wherever the two overlap. See this
 * file's own module doc comment for the R1 fix this replaces.
 */
export type ToolBaseProps = ToolBaseCoreProps & ToolBaseFieldVisibility & ToolBaseFieldOrder;
