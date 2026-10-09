/**
 * Admission judged on the document that will actually be STORED — the live
 * `yamlCode` — rather than on `yamlJsonObject`, the canvas's last good parse.
 *
 * ## Why the two can disagree, and why it mattered
 *
 * `usePipelineGraphDraft` reads `usePipelineYamlStore.getState().yamlCode` and
 * PUTs it verbatim as `instructions`. The save veto
 * (`ui/settings/GraphAdmissionGate.tsx` via `ui/settings/useGraphAdmission.ts`)
 * used to judge `yamlJsonObject` instead. In the Flow tab the two are kept in
 * step — `EditorPanel`'s `setYamlJsonObject` re-dumps the document into
 * `yamlCode` on every canvas edit — but in the **Yaml tab they diverge by
 * design**: `onChangeCode` writes only `yamlCode`, and `onParseCodeToJson`
 * runs only when the user clicks back to Flow (`onSelectChatMode`). So a user
 * could type an inadmissible document (a node id with a space, a
 * non-empty `interrupt_before`, or text that does not parse at all), watch the
 * banner stay clear and the Save button stay enabled, and store it — the
 * runtime then refuses it at first run with `graph.pipeline.invalid_configuration`.
 *
 * The pipelines e2e that covers this surface (`authorYaml`,
 * `pipelines.validation.spec.ts`) always clicks Flow before asserting, which
 * is exactly the step that closes the window, so it could not see it.
 *
 * ## The three states, and why `parseFailed` is not an issue list
 *
 * `GraphAdmissionIssue.rule` is a closed union of rules transcribed from the
 * Rust compiler, each citing a `file:line`. "Your YAML is not YAML" is not one
 * of those rules — `PipelineDefinition::from_yaml` never reaches a rule,
 * `serde_yaml` refuses first — so it is reported as its own flag rather than
 * by inventing a catalogue entry that mirrors no compiler line.
 *
 * - **unseeded** (`yamlCode` blank): `hasGraph: false`. Nothing is gated. Same
 *   reasoning `useGraphAdmission`'s own doc comment gives — an editor that has
 *   not loaded yet is not an invalid pipeline, and `usePipelineGraphDraft`
 *   returns `undefined` for exactly this state so no save writes it either.
 * - **unparseable**: `hasGraph: true`, `parseFailed: true`, no issues.
 * - **parsed**: the ordinary `collectGraphAdmissionIssues` pass.
 */
import { useMemo } from 'react';

import { load } from 'js-yaml';

import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import type { GraphAdmissionIssue } from './graphAdmission.types';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';

const NO_ISSUES: readonly GraphAdmissionIssue[] = [];

export interface LivePipelineGraphAdmission {
  /** The parsed live document, or `undefined` when the editor is unseeded or the text does not parse. */
  readonly document: YamlPipelineDocument | undefined;
  /** `true` when the live `yamlCode` is not parseable YAML at all. */
  readonly parseFailed: boolean;
  readonly issues: readonly GraphAdmissionIssue[];
  /** `false` only while the editor holds no document at all — nothing should be gated then. */
  readonly hasGraph: boolean;
  /** The single question every save gate asks: may this document be stored? */
  readonly isAdmissible: boolean;
}

const UNSEEDED: LivePipelineGraphAdmission = {
  document: undefined,
  parseFailed: false,
  issues: NO_ISSUES,
  hasGraph: false,
  isAdmissible: true,
};

const UNPARSEABLE: LivePipelineGraphAdmission = {
  document: undefined,
  parseFailed: true,
  issues: NO_ISSUES,
  hasGraph: true,
  isAdmissible: false,
};

/**
 * Judge one YAML string.
 *
 * The imperative caller is `model/usePipelineGraphDraft.ts`, which already
 * holds the exact string it is about to store and hands the verdict out on
 * `PipelineGraphDraft.admission` — so there is no second store-reading reader
 * here, and the page's save path gets its judgement at click time without
 * subscribing to every keystroke.
 */
export function judgeLivePipelineGraph(yamlCode: string): LivePipelineGraphAdmission {
  // `usePipelineGraphDraft`'s own bail-out, verbatim: a blank document is an
  // editor that has not been seeded, not a pipeline with no nodes.
  if (yamlCode.trim() === '') return UNSEEDED;

  let document: YamlPipelineDocument | undefined;
  try {
    document = load(yamlCode) as YamlPipelineDocument | undefined;
  } catch {
    return UNPARSEABLE;
  }
  // `load('null')`/`load('# comment')` answer `undefined`, and a scalar
  // document is not a mapping the compiler could read either.
  if (document === null || typeof document !== 'object' || Array.isArray(document)) return UNPARSEABLE;
  if (Object.keys(document).length === 0) return UNSEEDED;
  // The runtime reads an unquoted integer id (`id: 1`) as the string "1"; judge the document as it will run.
  document = normalizePipelineNodeIdentifiers(document);

  const issues = collectGraphAdmissionIssues(document);
  return { document, parseFailed: false, issues, hasGraph: true, isAdmissible: issues.length === 0 };
}

/**
 * @public
 * The reactive read, for anything that DISABLES a control: a button whose
 * enabled-ness is judged once at mount would be wrong for the rest of the
 * session.
 */
export function useLivePipelineGraphAdmission(): LivePipelineGraphAdmission {
  const yamlCode = usePipelineYamlStore((state) => state.yamlCode);
  return useMemo(() => judgeLivePipelineGraph(yamlCode), [yamlCode]);
}
