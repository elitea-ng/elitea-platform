import type { ChangeEvent, FocusEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { applicationCreationSchema } from '@/entities/application-form';
import { MAX_DESCRIPTION_LENGTH, MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { exceedsLimit } from '@/shared/lib/textLimits';
import { CharacterCounter } from '@/shared/ui/CharacterCounter';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { definitionLock } from '../lib/definitionLock';
import { useCreateAgentFormState } from '../model/useCreateAgentFormState';
import { advanceMetaProps, type CreateAgentMetaControls } from './CreateAgentForm.meta';
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

/** Ported from `EliteaUI` `features/agent/ui/agent-details/configurations/form/CreateAgentForm.jsx`. */
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
  readonly slots: CreateAgentFormProps['generalSlots'];
}

/**
 * Split out of `CreateAgentForm` purely to keep its own cyclomatic
 * complexity under the oxlint budget (12) — same reason
 * `StyledShowContextModal.tsx` splits `ModalHeader`/`ModalBody`. The `name`/
 * `description` props are grouped option objects, not 12 flat props — same
 * "group into one object" answer to the §3.5 12-prop budget `BasicAccordion`
 * `slotSx` and `InputBase` `actions`/`expand` already establish.
 */
function GeneralFields({ name, description, disabled, slots }: GeneralFieldsProps): ReactNode {
  const iconSlot = slots?.icon;
  const tagsSlot = slots?.tags;
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
            slotProps={{ htmlInput: { 'data-testid': 'agent-name-input' } }}
          />
          {name.focused && name.atMax && (
            exceedsLimit(name.value, MAX_NAME_LENGTH) ? (
              <CharacterCounter value={name.value} maxLength={MAX_NAME_LENGTH} textVariant="labelSmall" sx={nameCharactersLabelSx} data-testid="agent-name-counter" />
            ) : (
              <Typography
                variant="labelSmall"
                sx={nameCharactersLabelSx}
              >
                {t('features.agents.createAgentForm.nameCharactersLeft', ' 0 is left from {{max}} characters', { max: MAX_NAME_LENGTH })}
              </Typography>
            )
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
          slotProps={{ htmlInput: { 'data-testid': 'agent-description-input' } }}
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
          textVariant="labelSmall"
          visible={(description.focused && description.value.length > 0) || exceedsLimit(description.value, MAX_DESCRIPTION_LENGTH)}
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
  /** Locks instructions, welcome message and starters only (an installed agent). Defaults to `disabled`. */
  readonly definitionDisabled?: boolean | undefined;
  readonly showInstructions?: boolean | undefined;
  readonly entityType?: 'application' | 'pipeline' | undefined;
  /**
   * Rendered as `BasicAccordion`'s `summaryAction` — that summary row is ITSELF a
   * native `<button>` (`StyledAccordionSummary` wraps MUI's `ButtonBase`). This slot
   * must not be (or contain) a `<button>`: nested buttons are invalid HTML and React
   * warns (this file's own test suite confirms it). `GenerateAgentButton` (A1d) renders
   * as a non-button interactive element (an MUI `Chip`, a `role="button"` `Box`).
   */
  readonly generateAgentButtonSlot?: ReactNode | undefined;
  /** The general block's editable icon/tag controls plus optional section slots for pipeline accordion ordering (`General -> Tools -> Welcome -> Starters -> Triggers & schedules -> Advanced`). */
  readonly generalSlots?:
    | {
        readonly icon?: ReactNode | undefined;
        readonly tags?: ReactNode | undefined;
        readonly afterGeneral?: ReactNode | undefined;
        readonly beforeAdvanced?: ReactNode | undefined;
      }
    | undefined;
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
  /** Shows the version meta controls (output contract, citations, conversation history) when given, with the server's refusals of them. */
  readonly metaControls?: CreateAgentMetaControls | undefined;
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
  if (!slot) return null;
  return <Box data-testid="agent-instructions-ai-edit-slot" sx={instructionsAiEditSlotSx}>{slot}</Box>;
}

const sectionSlotSx: SxProps<Theme> = { marginTop: '1rem' };

function SectionSlot({ slot }: { readonly slot: ReactNode }): ReactNode {
  if (!slot) return null;
  return <Box sx={sectionSlotSx}>{slot}</Box>;
}

function readSectionSlots(generalSlots: CreateAgentFormProps['generalSlots']): {
  readonly afterGeneral: ReactNode;
  readonly beforeAdvanced: ReactNode;
} {
  return {
    afterGeneral: generalSlots?.afterGeneral ?? null,
    beforeAdvanced: generalSlots?.beforeAdvanced ?? null,
  };
}

export function CreateAgentForm({
  values,
  onFieldChange,
  disabled = false,
  definitionDisabled,
  showInstructions = true,
  entityType = 'application',
  generateAgentButtonSlot,
  generalSlots,
  modelSettingsSlot,
  instructionsAiEditSlot,
  metaControls,
  sx,
}: CreateAgentFormProps): ReactNode {
  const versionDetails = values.version_details;
  const lockDefinition = definitionLock(definitionDisabled, disabled);
  const state = useCreateAgentFormState(values, onFieldChange);
  const sectionSlots = readSectionSlots(generalSlots);

  // Client-side "required" validation-error state, restored to parity with
  // the baseline's `formik.touched?.name && Boolean(formik.errors.name)` —
  // see `requiredFieldError`'s own doc comment. `*Touched` stand in for
  // Formik's own per-field `touched` map (no ambient form context here).
  // Validated off `state.name` (the optimistic local mirror), not `values.name`, so a caller that
  // doesn't round-trip `onFieldChange` synchronously cannot leave the error one keystroke behind.
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
                slots={generalSlots}
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
            disabled={lockDefinition}
            lineNumbers={entityType === 'pipeline'}
          />
        </Box>
      )}
      <ApplicationVariables
        variables={state.variables}
        onChangeVariable={state.onChangeVariable}
      />
      <SectionSlot slot={sectionSlots.afterGeneral} />
      <Box sx={welcomeMessageInputSx}>
        <WelcomeMessageInput
          welcomeMessage={versionDetails?.welcome_message}
          onWelcomeMessageChange={state.onWelcomeMessageChange}
          versionId={versionDetails?.id}
          disabled={lockDefinition}
        />
      </Box>
      <ConversationStartersEditor
        starters={versionDetails?.conversation_starters}
        onStartersChange={state.onConversationStartersChange}
        disabled={lockDefinition}
        sx={conversationStartersSx}
      />
      <SectionSlot slot={sectionSlots.beforeAdvanced} />
      <Box sx={advanceSettingsSx}>
        <ApplicationAdvanceSettings
          stepLimit={versionDetails?.meta?.step_limit}
          onStepLimitChange={state.onStepLimitChange}
          modelSettingsSlot={modelSettingsSlot}
          outputContract={state.outputContract} onOutputContractChange={state.onOutputContractChange}
          {...advanceMetaProps(state, metaControls)}
        />
      </Box>
    </Box>
  );
}
