import { areAgentLlmSettingsEqual, type AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { OutputContract } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { areOutputContractsEqual } from '@/shared/ui/OutputContractField';

/**
 * The draft state `pages/agents/CreateApplication.tsx` holds outside its RHF
 * form, and the create endpoint's error mapping.
 *
 * Split out of that page for the reason `./editApplicationMappers.ts`'s own
 * header gives for its existence: the page sat at 398 of the §3.5 400-line
 * budget, and the model picker needed room. Nothing here reads React state or
 * props — they are the two pure functions and the one interface the page
 * would otherwise declare above its component.
 */

/**
 * The subset of `AgentDraftValues.version_details` (`features/agents`'
 * internal, unexported type) that `CreateAgentForm` reads and writes on a
 * brand-new draft but that `applicationCreationSchema` (the page's RHF
 * validation schema — name/description/`conversation_starters` only) does not
 * cover. Held as separate local state rather than widening the RHF form's
 * generic type: `zodResolver(applicationCreationSchema)` is typed as
 * `Resolver<ApplicationCreationInput>`, and stretching the form's type param
 * to a superset would need an unsound resolver cast for zero real benefit,
 * since none of these fields is ever validated anyway (same as the baseline:
 * its yup schema didn't touch them either).
 */
export interface CreateAgentFormExtraFields {
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly variables: { readonly name: string; readonly value: string }[];
  readonly stepLimit: number | undefined;
  /**
   * The model the new version will run on, or `undefined` when its author
   * never touched the picker. `undefined` is sent as an omitted
   * `llm_settings` key (`entities/application-form/model/mutations.ts`), which
   * is what leaves the platform's fallback to the project catalogue default
   * in charge — the fallback every agent created before this control existed
   * still runs on.
   */
  readonly llmSettings: AgentLlmSettings | undefined;
  /** `meta.output_contract` (#1500). `null` is no contract and is not sent. */
  readonly outputContract: OutputContract | null;
}

/**
 * The `extraFields` half of "is this draft dirty?" (#133).
 *
 * RHF's `formState.isDirty` only covers the three fields
 * `applicationCreationSchema` validates (`name`, `description`,
 * `version_details.conversation_starters`); everything in
 * `CreateAgentFormExtraFields` is deliberately held OUTSIDE the form (see
 * that interface's own doc comment), so a user who typed only instructions or
 * picked only a model would be invisible to `isDirty` and would lose that
 * work on nav-away. Compared field-by-field against the values the page
 * started from rather than tracked with a sticky "user touched something"
 * boolean, so undoing an edit correctly disarms the guard again.
 */
export function areExtraFieldsEqual(a: CreateAgentFormExtraFields, b: CreateAgentFormExtraFields): boolean {
  if (a.instructions !== b.instructions) return false;
  if (a.welcomeMessage !== b.welcomeMessage) return false;
  if (a.stepLimit !== b.stepLimit) return false;
  if (!areOutputContractsEqual(a.outputContract, b.outputContract)) return false;
  // Key by key, never by identity: the picker hands back a fresh object on
  // every change, so an identity check would report "dirty" forever.
  if (!areAgentLlmSettingsEqual(a.llmSettings, b.llmSettings)) return false;
  if (a.variables.length !== b.variables.length) return false;
  return a.variables.every((variable, index) => {
    const other = b.variables[index];
    return other !== undefined && variable.name === other.name && variable.value === other.value;
  });
}

export interface CreateApplicationFieldErrors {
  readonly name?: string;
  readonly description?: string;
}

function isFastApiErrorDetail(value: unknown): value is { readonly msg?: unknown; readonly loc?: readonly unknown[] } {
  return typeof value === 'object' && value !== null;
}

/**
 * Maps the create-application endpoint's failure onto per-field messages —
 * old app: `useCreateApplication.jsx:85-107` inspects `error.data` (a
 * FastAPI/Pydantic-style array of `{loc, msg}` entries — the same shape
 * `shared/lib/http-error.ts`'s `ErrorDetail`/`messageFromValidationArray`
 * already ports for this backend family) and calls
 * `formik.setFieldError('name'|'description', msg)` per entry, so e.g. a
 * duplicate-name rejection is shown attributed to the Name field rather than
 * as one generic message.
 *
 * **Disclosed gap:** `features/agents/ui/CreateAgentForm.tsx` takes no
 * `nameError`/`descriptionError`-shaped prop (verified: `CreateAgentFormProps`
 * has no such field, and `GeneralFields`' `StyledInputEnhancer` calls pass no
 * `error`/`helperText`), so a mapped field error cannot be rendered literally
 * under the Name/Description inputs from the page alone. This still closes the
 * finding's real complaint — field-attributed text instead of one generic,
 * un-attributed message — via the page's banner; true inline-under-field
 * parity needs a follow-up prop on `CreateAgentForm`.
 */
export function mapCreateErrorToFieldErrors(error: unknown): CreateApplicationFieldErrors {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return {};
  const body: unknown = error.failure.body;
  if (!Array.isArray(body)) return {};
  const fieldErrors: { name?: string; description?: string } = {};
  for (const entry of body) {
    if (!isFastApiErrorDetail(entry) || typeof entry.msg !== 'string') continue;
    const loc = entry.loc ?? [];
    if (loc.includes('name')) fieldErrors.name = entry.msg;
    else if (loc.includes('description')) fieldErrors.description = entry.msg;
  }
  return fieldErrors;
}
