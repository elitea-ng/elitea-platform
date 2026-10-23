/**
 * The configuration sections the credential form's type catalogue is read
 * for. Split out of `useCredentialFormController.ts` to keep that file under
 * the §3.5 400-line budget.
 */
import { useMemo } from 'react';

/**
 * The part of `CredentialFormMode` this module reads.
 *
 * Declared here, NOT imported from `useCredentialFormController.ts`. That
 * module imports this one, so a type import back would close a cycle, and
 * `.dependency-cruiser.cjs`'s `no-circular` rule counts a type-only edge.
 * `CredentialFormMode` is structurally assignable to this shape.
 */
interface TypeSectionMode {
  readonly kind: 'create' | 'edit';
  readonly configurationMode?: boolean;
}

/**
 * `/credentials/create-credential` shows credential types only. The same
 * component also serves `/settings/create-configuration`
 * (`mode.configurationMode`), whose picker needs the model and provider
 * sections instead. Baseline: `pages/Credentials/CreateCredential.jsx:41-51`.
 */
const CREDENTIAL_SECTIONS: readonly string[] = ['credentials'];
const MODEL_CONFIGURATION_SECTIONS: readonly string[] = [
  'llm',
  'embedding',
  'ai_credentials',
  'image_generation',
  'asr',
  'tts',
];

/**
 * The catalogue query the credential form runs.
 *
 * `sections` is the `section` query parameter. An empty `sections` with
 * `enabled: true` asks for the unfiltered catalogue. `enabled: false` holds
 * the request back until the mode knows which sections it wants.
 */
export interface TypeSectionQuery {
  readonly sections: readonly string[];
  readonly enabled: boolean;
}

const UNFILTERED: readonly string[] = [];

/**
 * The sections the type catalogue is read for.
 *
 * DEFECT this repairs. The call site passed `{}` whenever `prefill.section`
 * was absent, which is every normal entry point.
 * `buildAvailableConfigurationsTypeUrl` then emitted
 * `/configurations/available/?` with an empty query, and the server answers
 * an empty section list with EVERY section: 49 descriptors, 136,007 bytes,
 * uncompressed and with no `Cache-Control` or `ETag`. The size is the smaller
 * half of the damage. `CredentialForm` hands `availableTypes.data` straight to
 * `<CredentialTypeSelector>`, so the Create Credential picker also listed the
 * 17 non-credential types — `llm_model`, `embedding_model`, `s3`, `pgvector`,
 * `service_prompt`, `environment_settings`, `project_icon`, `project_context`
 * and the six `ai_credentials` providers (`pgvector` has since been removed
 * from the catalogue). None of them carries a `hidden` flag, so nothing
 * downstream filtered them out.
 *
 * On edit the descriptor is found by `detail.data.type`, whose section can be
 * any of them. That path waits for the detail, then asks for that one
 * section.
 *
 * SECOND DEFECT this repairs. The first repair returned an empty section
 * list for an edit whose detail names no section. An empty list disabled the
 * query for ever, so the descriptor never resolved and the form showed no
 * schema field. Save stayed enabled, so the person could save an empty body.
 * `services/elitea-main/internal/api/v2/configurations/dto.go:20` always
 * emits the key, but the value can be empty. This path now reads the
 * unfiltered catalogue instead. It costs one large response on a rare row,
 * and it keeps the form usable.
 */
export function useTypeSections(
  mode: TypeSectionMode,
  prefillSection: string | undefined,
  detailSection: string | undefined,
  detailLoaded: boolean,
): TypeSectionQuery {
  return useMemo(() => {
    if (prefillSection !== undefined && prefillSection !== '') {
      return { sections: [prefillSection], enabled: true };
    }
    if (mode.kind === 'edit') {
      if (detailSection !== undefined && detailSection !== '') {
        return { sections: [detailSection], enabled: true };
      }
      // Hold the request until the detail arrives. Then read everything.
      return { sections: UNFILTERED, enabled: detailLoaded };
    }
    const sections = mode.configurationMode === true ? MODEL_CONFIGURATION_SECTIONS : CREDENTIAL_SECTIONS;
    return { sections, enabled: true };
  }, [prefillSection, mode.kind, mode.configurationMode, detailSection, detailLoaded]);
}
