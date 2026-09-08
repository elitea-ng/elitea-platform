/**
 * The platform-model dialog's form model: what it opens on, when it may save,
 * and the body it sends.
 *
 * ## The body is a MERGE over the stored row, not a rebuild of it
 *
 * The update replaces the `data` column whole. This function used to build that
 * column out of the four fields the dialog shows, so every field it did not show
 * was erased by any save at all. An `llm_model` declares nine: renaming a model,
 * or ticking a tier, reset its `context_window`, its `max_output_tokens` and its
 * three capability flags to whatever the registry defaults to — with a 200, and
 * with nothing on the screen that read the loss back.
 *
 * Adding the missing fields to the form would fix the five that exist today and
 * lose the next one the registry adds. So the stored object is the BASE and the
 * edited fields are written over it: a field this dialog has never heard of
 * survives, and only what the operator actually changed changes.
 *
 * The listing carries that object (`PlatformModel.data`) for exactly this.
 *
 * ## The chat-only fields are sent for the chat type ALONE
 *
 * The other four model types decode their `data` with unknown fields REFUSED, so
 * sending a tier or a capability they do not declare would turn a valid model
 * into an invalid binding rather than a model with an ignored extra.
 */
import type { PlatformModel, PlatformModelData, PlatformModelDraft } from './api/adminLlmPlatformModelsApi';
import {
  DEFAULT_SHARE_SCOPE,
  shareScopeOf,
  sharedWithOf,
  type ShareScope,
} from './platformModelGrant';

/** The value of the provider select before one has been chosen. */
export const NO_CREDENTIAL_CHOSEN = '';

/** The one model type whose schema declares the tiers and the capabilities. */
export const TIERED_MODEL_TYPE = 'llm_model';

/**
 * The Kind a NEW platform model opens on.
 *
 * The dialog took `modelTypes[0]`, and `model_types` arrives in the server's own
 * order, not in an order this screen chose. On this deployment the first entry
 * is `asr_model`, so "Add a platform model" opened on "Speech to text" — an
 * operator adding a chat model had to notice the wrong Kind and change it, and
 * one who did not published a model the gateway dispatches to the ASR section.
 * Chat is what almost every platform model is.
 *
 * The server still decides what is OFFERED: this default is used only when the
 * deployment actually dispatches it.
 */
const DEFAULT_MODEL_TYPE = 'llm_model';

/**
 * What a NEW chat model starts from — the registry's own defaults for the two
 * fields whose default is not `false`. Sending something else on a create would
 * make this form, rather than the catalogue, decide what a model can do.
 */
const DEFAULT_CONTEXT_WINDOW = '128000';
const DEFAULT_SUPPORTS_VISION = true;

/** The Kind to open on: chat when this deployment dispatches it, else whatever it does. */
function defaultModelType(modelTypes: readonly string[]): string {
  if (modelTypes.includes(DEFAULT_MODEL_TYPE)) return DEFAULT_MODEL_TYPE;
  return modelTypes[0] ?? DEFAULT_MODEL_TYPE;
}

/** Everything the form holds, so one reset statement can restate all of it. */
export interface ModelForm {
  readonly name: string;
  readonly type: string;
  readonly modelName: string;
  readonly credential: string;
  readonly lowTier: boolean;
  readonly highTier: boolean;
  /** Held as typed, not as a number: an empty box is a state a number has no value for. */
  readonly contextWindow: string;
  readonly openaiCompatible: boolean;
  readonly supportsReasoning: boolean;
  readonly supportsVision: boolean;
  /**
   * Which projects the model is offered to. It is a field of every kind, not
   * of the chat kind alone: it says who the ROW is for, not what the model can
   * do, and all five model types carry it.
   */
  readonly shareScope: ShareScope;
  /** Read only for `projects`, and cleared by the submit for every other scope. */
  readonly sharedWith: readonly number[];
}

/** One stored boolean, or the registry default when the row carries none. */
function storedFlag(
  data: Readonly<Record<string, unknown>>, field: string, fallback: boolean,
): boolean {
  const value = data[field];
  return typeof value === 'boolean' ? value : fallback;
}

/** One stored number as the text the box shows. */
function storedNumber(
  data: Readonly<Record<string, unknown>>, field: string, fallback: string,
): string {
  const value = data[field];
  return typeof value === 'number' && Number.isFinite(value) ? String(value) : fallback;
}

/**
 * The form a dialog OPENS on: the stored row's own values, or the blanks a new
 * model starts from.
 *
 * Everything is read back from the row rather than defaulted, for the reason the
 * merge exists: a form that opened on a default would send that default, and the
 * save writes what the form holds.
 */
export function formOf(
  editing: PlatformModel | undefined, modelTypes: readonly string[],
): ModelForm {
  // Split rather than ten optional chains, so the create case reads as the
  // blanks it is — the same split `normalisePlatformModels` makes.
  if (editing === undefined) {
    return {
      name: '',
      type: defaultModelType(modelTypes),
      modelName: '',
      credential: NO_CREDENTIAL_CHOSEN,
      lowTier: false,
      highTier: false,
      contextWindow: DEFAULT_CONTEXT_WINDOW,
      openaiCompatible: false,
      supportsReasoning: false,
      supportsVision: DEFAULT_SUPPORTS_VISION,
      // A NEW model is offered to everyone, which is what publishing one meant
      // before the grant existed. Opening on "no project" would make the
      // ordinary case the one that needs a second decision.
      shareScope: DEFAULT_SHARE_SCOPE,
      sharedWith: [],
    };
  }
  // `credential_name` is empty on a row written before the link was required.
  // It opens the form with the provider unchosen and Save disabled, which is
  // the state that row is in.
  //
  // The tiers keep their own reported fields; the capabilities are read out of
  // the stored object, which is where they live and where the merge writes them.
  const stored = editing.data ?? {};
  return {
    name: editing.elitea_title,
    type: editing.type,
    modelName: editing.model_name,
    credential: editing.credential_name,
    lowTier: editing.low_tier ?? false,
    highTier: editing.high_tier ?? false,
    contextWindow: storedNumber(stored, 'context_window', DEFAULT_CONTEXT_WINDOW),
    openaiCompatible: storedFlag(stored, 'openai_compatible', false),
    supportsReasoning: storedFlag(stored, 'supports_reasoning', false),
    supportsVision: storedFlag(stored, 'supports_vision', DEFAULT_SUPPORTS_VISION),
    // Read from the row's own reported fields, like the tiers: the server
    // interprets an absent scope as `all`, and a form that opened on a blank
    // would save "granted to nobody" over a model nobody had restricted.
    shareScope: shareScopeOf(editing),
    sharedWith: sharedWithOf(editing),
  };
}

/**
 * Whether Save may act. The provider is one of the three, and it is the one that
 * used to have a value meaning "none".
 *
 * The context window is NOT among them. An empty box leaves the stored value
 * alone (see `chatFields`), so it is a field an operator can decline to answer.
 */
export function formIsComplete(form: ModelForm): boolean {
  return (
    form.name.trim() !== '' &&
    form.modelName.trim() !== '' &&
    form.credential !== NO_CREDENTIAL_CHOSEN &&
    // "Selected projects" with nothing selected grants the model to nobody,
    // which is the OTHER choice on the same control. The server refuses that
    // body; Save is disabled instead, so the operator is told while they can
    // still pick a project.
    (form.shareScope !== 'projects' || form.sharedWith.length > 0)
  );
}

/**
 * The `data` fields only a chat model declares.
 *
 * `context_window` is OMITTED when the box does not hold a positive whole
 * number. The merge base then keeps the stored value, so clearing the box
 * declines to change it rather than writing a zero the gateway would treat as a
 * model that can read nothing.
 */
function chatFields(form: ModelForm): Partial<PlatformModelData> {
  if (form.type !== TIERED_MODEL_TYPE) return {};
  const contextWindow = Number.parseInt(form.contextWindow, 10);
  return {
    low_tier: form.lowTier,
    high_tier: form.highTier,
    openai_compatible: form.openaiCompatible,
    supports_reasoning: form.supportsReasoning,
    supports_vision: form.supportsVision,
    ...(Number.isFinite(contextWindow) && contextWindow > 0
      ? { context_window: contextWindow }
      : {}),
  };
}

/**
 * The body the form sends: the STORED object, with the edited fields over it.
 *
 * `stored` is the row's own `data` as the listing reported it, and it is absent
 * on a create and on a listing from a server that predates the field. Both cases
 * merge over nothing, which is what this function used to do for every case.
 */
export function modelDraftOf(
  form: ModelForm, stored: Readonly<Record<string, unknown>> | undefined,
): PlatformModelDraft {
  return {
    elitea_title: form.name.trim(),
    type: form.type,
    data: {
      ...stored,
      name: form.modelName.trim(),
      ai_credentials: { elitea_title: form.credential },
      ...grantFields(form),
      ...chatFields(form),
    },
  };
}

/**
 * The grant, written over the merge base on every save.
 *
 * `shared_with` is sent EMPTY for the two scopes that do not read it, never
 * omitted. `data` is replaced whole, so an omitted field would be dropped —
 * but the base this merges over carries the stored list, so omitting it would
 * keep naming the projects a withdrawn model no longer serves, and the next
 * switch back to "selected projects" would restore a grant nobody re-chose.
 */
function grantFields(form: ModelForm): Partial<PlatformModelData> {
  return {
    share_scope: form.shareScope,
    shared_with: form.shareScope === 'projects' ? [...form.sharedWith] : [],
  };
}
