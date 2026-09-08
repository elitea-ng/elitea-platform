import type { ChangeEvent, FocusEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { applicationCreationSchema } from '@/entities/application-form';
import { MAX_DESCRIPTION_LENGTH, MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { CharacterCounter } from '@/shared/ui/CharacterCounter';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { useCreateAgentFormState } from '../model/useCreateAgentFormState';
import type { AgentDraftValues, AgentFieldChange } from '../model/types';

import {
  accordionContentSx,
  accordionSx,
  advanceSettingsSx,
  conversationStartersSx,
  descriptionCharactersLabelSx,
  descriptionWrapperSx,
  instructionsAiEditSlotSx,
  instructionsContainerSx,
  nameCharactersLabelSx,
  nameContainerSx,
  nameWrapperInputSx,
  rootContainerSx,
  welcomeMessageInputSx,
} from './CreateAgentForm.styles';
import { ApplicationAdvanceSettings } from './ApplicationAdvanceSettings';
import { ApplicationVariables } from './ApplicationVariables';
import { ConversationStartersEditor } from './ConversationStartersEditor';
import { InstructionsInput } from './InstructionsInput';
import { WelcomeMessageInput } from './WelcomeMessageInput';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/form/CreateAgentForm.jsx`.
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): `values`/`onFieldChange` replace every
 * `useFormikContext()` read/`setFieldValue` call. `onFieldChange` mirrors
 * Formik's own `setFieldValue(path, value)` signature exactly — see that
 * file's doc comment for why this is the ONE generic escape hatch instead
 * of one typed callback per field.
 *
 * Cross-sub-unit slots, all disclosed, all EXPECTED not to have landed yet
 * per this batch's own brief:
 *  - `generateAgentButtonSlot` — the baseline's `GenerateAgentButton`
 *    (`ui/generate-agent-modal/`, this batch's own A1d designation). Only
 *    `GenerateAgentReviewForm`/`SuggestionItem`/`ResourceSuggestions` of
 *    that cluster had landed in this worktree at the time this file was
 *    written — no `GenerateAgentButton` itself. Rendered only outside
 *    pipeline mode (`entityType !== 'pipeline'`), matching the baseline's
 *    own conditional.
 *  - `iconSlot` — the baseline's editable `EntityIcon`
 *    (`components/EntityIcon.jsx`, `editable={true}`, `onChangeIcon`,
 *    upload flow). Sibling A1h's own scoped `EntityIcon.tsx`
 *    (`../ui/EntityIcon.tsx`) explicitly does NOT cover this: its own doc
 *    comment states it is "for `ToolCard.jsx`'s ONE call site... which
 *    always passes `editable={false}`" and that the editable/upload mode
 *    "is a separate, large feature... no owner in this sub-unit's file
 *    list." A real gap, not a naming mismatch.
 *  - `tagsSlot` — the baseline's `TagEditor`
 *    (`pages/Common/Components/TagEditor.jsx`), consumed by three
 *    different domains (`CreateSkillForm`, this file, `ApplicationEditForm`)
 *    with no `shared/ui` port anywhere in this worktree
 *    (`find shared/ui -iname '*Tag*'` — only `HeadingChip`, a read-only
 *    display chip, no editable tag-input equivalent).
 *  - `conversationStartersSlot` — CLOSED, and the prop is GONE (#307). Every
 *    caller left that slot empty, so the one field the agent edit page did
 *    save had no input anywhere in the app. `./ConversationStartersEditor
 *    .tsx` is the port, rendered DIRECTLY here — which is what the baseline
 *    does too (`CreateAgentForm.jsx:206`, reading the same
 *    `version_details.conversation_starters` off its form context).
 *
 * Each slot keeps this file's LAYOUT position faithful to the baseline
 * (same accordion, same ordering) even where its CONTENT cannot be filled
 * yet — matching `entities/application-form/ui/ApplicationConfigurationLayout
 * .tsx`'s own established "take the panel as an injected slot" precedent for
 * exactly this kind of not-yet-buildable dependency.
 */
interface GeneralFieldsNameProps {
  readonly value: string;
  readonly atMax: boolean;
  readonly focused: boolean;
  readonly onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onFocus: () => void;
  readonly onBlur: () => void;
  /** Set only once the field has been touched (blurred) — matches the baseline's `formik.touched?.name && formik.errors.name`. */
  readonly error?: string | undefined;
}

interface GeneralFieldsDescriptionProps {
  readonly value: string;
  readonly focused: boolean;
  readonly onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onFocus: () => void;
  readonly onBlur: (event: FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  /** Set only once the field has been touched (blurred) — matches the baseline's `formik.touched?.description && formik.errors.description`. */
  readonly error?: string | undefined;
}

/**
 * `message` (a real `t()` string, chosen by the caller) once `field` has
 * been `touched` AND `applicationCreationSchema`'s `safeParse` has rejected
 * it — the zod re-expression of the baseline's `formik.touched?.name &&
 * formik.errors.name` (this app's own `AgentEditor.tsx` already runs the
 * identical `safeParse` for the Save button's disabled state). Renders
 * `message`, not the schema's own raw (non-`t()`, out of this file's scope)
 * issue text — "name"/"description" only ever have the one "required" rule,
 * so which exact issue fired doesn't matter, only whether one did. Takes
 * `touched`/`message` themselves purely to keep `CreateAgentForm` under the
 * oxlint complexity budget.
 */
function requiredFieldError(
  touched: boolean,
  validation: ReturnType<typeof applicationCreationSchema.safeParse>,
  field: 'name' | 'description',
  message: string,
): string | undefined {
  const hasError = touched && !validation.success && validation.error.issues.some((issue) => issue.path[0] === field);
  return hasError ? message : undefined;
}

interface GeneralFieldsProps {
  readonly name: GeneralFieldsNameProps;
  readonly description: GeneralFieldsDescriptionProps;
  readonly disabled: boolean;
  readonly iconSlot: ReactNode;
  readonly tagsSlot: ReactNode;
}

/**
 * Split out of `CreateAgentForm` purely to keep its own cyclomatic
 * complexity under the oxlint budget (12) — same reason
 * `StyledShowContextModal.tsx` splits `ModalHeader`/`ModalBody`. The `name`/
 * `description` props are grouped option objects, not 12 flat props — same
 * "group into one object" answer to the §3.5 12-prop budget `BasicAccordion`
 * `slotSx` and `InputBase` `actions`/`expand` already establish.
 */
function GeneralFields({ name, description, disabled, iconSlot, tagsSlot }: GeneralFieldsProps): ReactNode {
  return (
    <Box sx={accordionContentSx}>
      <Box sx={nameContainerSx}>
        {iconSlot}
        <Box sx={nameWrapperInputSx}>
          <StyledInputEnhancer
            autoComplete="off"
            id="name"
            name="name"
            label={t('features.agents.createAgentForm.nameLabel', 'Name')}
            disabled={disabled}
            onChange={name.onChange}
            onFocus={name.onFocus}
            onBlur={name.onBlur}
            value={name.value}
            required
            error={Boolean(name.error)}
            helperText={name.error}
            slotProps={{ htmlInput: { maxLength: MAX_NAME_LENGTH, 'data-testid': 'agent-name-input' } }}
          />
          {name.focused && name.atMax && (
            <Typography
              variant="labelTiny"
              sx={nameCharactersLabelSx}
            >
              {t('features.agents.createAgentForm.nameCharactersLeft', ' 0 is left from {{max}} characters', { max: MAX_NAME_LENGTH })}
            </Typography>
          )}
        </Box>
      </Box>

      <Box sx={descriptionWrapperSx}>
        <StyledInputEnhancer
          autoComplete="off"
          id="description"
          label={t('features.agents.createAgentForm.descriptionLabel', 'Description')}
          required
          expand={{ minRows: 1, maxRows: 15 }}
          onChange={description.onChange}
          onFocus={description.onFocus}
          onBlur={description.onBlur}
          value={description.value}
          disabled={disabled}
          error={Boolean(description.error)}
          helperText={description.error}
          slotProps={{ htmlInput: { maxLength: MAX_DESCRIPTION_LENGTH, 'data-testid': 'agent-description-input' } }}
        />
        {/* #848 — never unmount: unlike `nameCharactersLabelSx` above
          * (absolutely positioned, so it overlays rather than pushes), this
          * counter sits in normal flow directly above `tagsSlot` below, and
          * unmounting on blur shifts that control up, swallowing a click
          * already headed for it. `visibility: hidden` keeps the line's
          * height reserved. */}
        <CharacterCounter
          value={description.value}
          maxLength={MAX_DESCRIPTION_LENGTH}
          textVariant="labelTiny"
          visible={description.focused && description.value.length > 0}
          sx={descriptionCharactersLabelSx}
          data-testid="agent-description-counter"
        />
      </Box>

      {tagsSlot}
    </Box>
  );
}

export interface CreateAgentFormProps {
  readonly values: AgentDraftValues;
  readonly onFieldChange: AgentFieldChange;
  readonly disabled?: boolean | undefined;
  readonly showInstructions?: boolean | undefined;
  readonly entityType?: 'application' | 'pipeline' | undefined;
  /**
   * Rendered as `BasicAccordion`'s `summaryAction` — that summary row is
   * ITSELF a native `<button>` (`StyledAccordionSummary` wraps MUI's
   * `ButtonBase`; see that component's own doc comment). This slot's
   * content must therefore not be (or contain) a literal `<button>` —
   * nested `<button>`s are invalid HTML and React warns on them at
   * runtime (confirmed by this file's own test suite). The real
   * `GenerateAgentButton` (A1d) must render as a non-button interactive
   * element (an MUI `Chip`, a `role="button"` `Box`, etc) when used here.
   */
  readonly generateAgentButtonSlot?: ReactNode | undefined;
  readonly iconSlot?: ReactNode | undefined;
  readonly tagsSlot?: ReactNode | undefined;
  /**
   * The model picker (`widgets/agent-model-settings`), rendered inside the
   * "Advanced" panel where the baseline puts it. Injected rather than
   * imported because it needs the project's model catalogue and
   * `widgets/llm-model-selector`, and `.dependency-cruiser.cjs` forbids
   * `features/` importing `widgets/` — the same reason
   * `AgentEditor.tsx`'s `renderLlmModelSelector` is a slot.
   */
  readonly modelSettingsSlot?: ReactNode | undefined;
  /**
   * The "Edit with AI" trigger for the Instructions field
   * (`features/agents`' `EditInstructionsWithAiButton`). Rendered beside the
   * Instructions block, and omitted entirely when the caller has nothing to
   * mount — the button gates itself on top of that (see its own doc
   * comment), so a caller passing it never has to know whether the backend
   * serves the feature.
   */
  readonly instructionsAiEditSlot?: ReactNode | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

/**
 * The Instructions block's action row. The wrapper carries its own bottom margin, so
 * it must not render at all when the slot is empty -- an empty row would still push
 * the editor down. That is the common case rather than the exception: the "Edit with
 * AI" trigger is backed by an endpoint no deployment routes today, so the caller
 * frequently has nothing to mount here.
 */
function InstructionsAiEditRow({ slot }: { readonly slot?: ReactNode | undefined }) {
  if (!slot) {
    return null;
  }
  return (
    <Box
      data-testid="agent-instructions-ai-edit-slot"
      sx={instructionsAiEditSlotSx}
    >
      {slot}
    </Box>
  );
}

export function CreateAgentForm({
  values,
  onFieldChange,
  disabled = false,
  showInstructions = true,
  entityType = 'application',
  generateAgentButtonSlot,
  iconSlot,
  tagsSlot,
  modelSettingsSlot,
  instructionsAiEditSlot,
  sx,
}: CreateAgentFormProps): ReactNode {
  const versionDetails = values.version_details;
  const state = useCreateAgentFormState(values, onFieldChange);

  // Client-side "required" validation-error state, restored to parity with
  // the baseline's `formik.touched?.name && Boolean(formik.errors.name)` —
  // see `requiredFieldError`'s own doc comment. `*Touched` stand in for
  // Formik's own per-field `touched` map (no ambient form context here).
  // Validated off `state.name` (this component's own optimistic local
  // mirror of what the input shows), not `values.name`: a caller that
  // doesn't round-trip `onFieldChange` synchronously would otherwise leave
  // the error stale one keystroke behind. `state.description` has no such
  // local mirror (reads straight off `values.description`), so equivalent either way.
  const [nameTouched, setNameTouched] = useState(false);
  const [descriptionTouched, setDescriptionTouched] = useState(false);
  const validation = useMemo(
    () => applicationCreationSchema.safeParse({ name: state.name, description: state.description, version_details: values.version_details }),
    [state.name, state.description, values.version_details],
  );
  const nameRequiredMessage = t('features.agents.createAgentForm.nameRequired', 'Name is required');
  const descriptionRequiredMessage = t('features.agents.createAgentForm.descriptionRequired', 'Description is required');
  const nameError = requiredFieldError(nameTouched, validation, 'name', nameRequiredMessage);
  const descriptionError = requiredFieldError(descriptionTouched, validation, 'description', descriptionRequiredMessage);

  const handleNameBlur = useCallback(() => {
    state.onNameBlur();
    setNameTouched(true);
  }, [state]);

  const handleDescriptionBlur = useCallback(
    (event: FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      state.onDescriptionBlur(event);
      setDescriptionTouched(true);
    },
    [state],
  );

  return (
    <Box sx={combineSx(rootContainerSx, sx)}>
      <BasicAccordion
        showMode="left"
        slotSx={{ accordion: accordionSx }}
        items={[
          {
            title: t('features.agents.createAgentForm.generalTitle', 'General'),
            summaryAction: entityType !== 'pipeline' ? generateAgentButtonSlot : undefined,
            content: (
              <GeneralFields
                name={{
                  value: state.name,
                  atMax: state.nameAtMax,
                  focused: state.nameFocused,
                  onChange: state.onChangeName,
                  onFocus: state.onNameFocus,
                  onBlur: handleNameBlur,
                  error: nameError,
                }}
                description={{
                  value: state.description,
                  focused: state.descriptionFocused,
                  onChange: state.onDescriptionChange,
                  onFocus: state.onDescriptionFocus,
                  onBlur: handleDescriptionBlur,
                  error: descriptionError,
                }}
                disabled={disabled}
                iconSlot={iconSlot}
                tagsSlot={tagsSlot}
              />
            ),
          },
        ]}
      />
      {showInstructions && (
        <Box sx={instructionsContainerSx}>
          <InstructionsAiEditRow slot={instructionsAiEditSlot} />
          <InstructionsInput
            instructions={versionDetails?.instructions}
            onInstructionsChange={state.onInstructionsChange}
            disabled={disabled}
          />
        </Box>
      )}
      <ApplicationVariables
        variables={state.variables}
        onChangeVariable={state.onChangeVariable}
      />
      <Box sx={welcomeMessageInputSx}>
        <WelcomeMessageInput
          welcomeMessage={versionDetails?.welcome_message}
          onWelcomeMessageChange={state.onWelcomeMessageChange}
          versionId={versionDetails?.id}
          disabled={disabled}
        />
      </Box>
      <ConversationStartersEditor
        starters={versionDetails?.conversation_starters}
        onStartersChange={state.onConversationStartersChange}
        disabled={disabled}
        sx={conversationStartersSx}
      />
      <Box sx={advanceSettingsSx}>
        <ApplicationAdvanceSettings
          stepLimit={versionDetails?.meta?.step_limit}
          onStepLimitChange={state.onStepLimitChange}
          modelSettingsSlot={modelSettingsSlot}
        />
      </Box>
    </Box>
  );
}
