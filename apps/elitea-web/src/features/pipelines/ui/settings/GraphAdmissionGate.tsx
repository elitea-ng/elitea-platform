/**
 * The save gate: while the live graph is one the native pipeline runtime
 * would refuse, the editor's Save button is disabled and the PUT is never
 * issued.
 *
 * ## Why the veto is expressed as a form error
 *
 * The pipeline editor's Save button is `entities/application-form`'s
 * `CreateApplicationTabBar`, and the page computes `canSave={
 * form.formState.isValid && !isSaving}` from the react-hook-form instance it
 * owns. RHF's own mechanism for "something outside the resolver says this
 * form may not be submitted" is a `root.*` error: `setError` publishes
 * `isValid: false` to every subscriber (react-hook-form 7.83,
 * `dist/index.esm.mjs:2751-2755`), which is exactly the flag the Save button
 * is already wired to. Nothing is monkey-patched and no new prop has to be
 * threaded through a page this unit does not own.
 *
 * Two consequences the implementation below has to handle, both measured
 * against the pinned RHF build rather than assumed:
 *
 *  1. A later resolver pass REPLACES `_formState.errors` wholesale, dropping
 *     the root error and flipping `isValid` back to `true` — so the veto has
 *     to be re-asserted whenever `isValid` goes true while the graph is
 *     still inadmissible. It converges: the re-assert publishes `false`, and
 *     `isValid` then stops changing.
 *  2. `clearErrors` does NOT recompute `isValid` (`dist/index.esm.mjs:
 *     2724-2741` never calls `_setValid`). Lifting the veto therefore has to
 *     run `trigger()`, or a fixed graph would leave Save disabled forever.
 *
 * `useLayoutEffect`, not `useEffect`: the re-assert must land before paint,
 * or the button is briefly enabled in a frame an automated click could hit.
 *
 * ## What it deliberately does NOT gate on
 *
 * An editor holding no graph at all (not yet seeded, or a screen with no
 * canvas). See `./useGraphAdmission.ts` — blocking Save there would break
 * saves that have nothing to do with the graph.
 */
import type { ReactNode } from 'react';
import { useLayoutEffect, useRef } from 'react';
import type { FieldValues, UseFormReturn } from 'react-hook-form';
import { useFormContext } from 'react-hook-form';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { blockingIssues, documentLevelIssues } from '../../lib/graphAdmission.helpers';
import type { GraphAdmissionIssue } from '../../lib/graphAdmission.types';
import { useGraphAdmission } from './useGraphAdmission';

/** The key under `errors.root` the veto occupies. `root.*` errors are RHF's own channel for non-field, externally-sourced refusals. */
const GRAPH_ADMISSION_KEY = 'pipelineGraphAdmission';
/** The same path in the form `setError`/`clearErrors` take. */
const GRAPH_ADMISSION_FIELD = `root.${GRAPH_ADMISSION_KEY}`;

const alertSx: SxProps<Theme> = { width: '100%', boxSizing: 'border-box' };
const listSx: SxProps<Theme> = { margin: '0.25rem 0 0', paddingLeft: '1.25rem', display: 'flex', flexDirection: 'column', gap: '0.25rem' };

export interface GraphAdmissionGateProps {
  /** The configuration panel collapses to a 1.75rem rail; the summary is withheld there, but the veto still runs. */
  readonly summaryHidden?: boolean;
}

/**
 * Holds the form's `isValid` at `false` for as long as the graph is
 * inadmissible. Renders nothing.
 */
function useGraphAdmissionSaveVeto(blocked: boolean): void {
  /*
   * RHF types `useFormContext()` as non-nullable, but it is a bare
   * `useContext` whose default is `null` — and this panel really does render
   * with no `<FormProvider>` above it (the chat-side editor, and this
   * slice's own component tests). A cast plus a guard is the honest reading;
   * the alternative is a crash on every such mount.
   */
  const form = useFormContext() as UseFormReturn<FieldValues> | null;
  const isValid = form?.formState.isValid;
  /*
   * The veto's own record of itself, read off the FORM rather than off this
   * component. A component-local ref alone was a real stuck-Save bug: the
   * gate can unmount while vetoing — `ConfigurationTab` swaps
   * `GeneralFormPanel` (and therefore this gate) for an error box the moment
   * the detail refetch `useRefetchPipelineAfterSave` now fires after every
   * save fails, and `PipelineConfigurationTabBoundary` can catch it away too
   * — while `EditPipeline` keeps the Save bar mounted. On remount the ref is
   * `false` again, so the lift below returned early, `trigger()` never ran,
   * and `isValid` kept the `false` the vanished veto had published. Nothing
   * on that page recovers it: the configuration form is still a disclosed
   * gap, so there is no RHF-registered input whose edit would re-run the
   * resolver. Only a reload did.
   *
   * The error survives that unmount because it lives on the page's form, so
   * it is the durable half of the answer; the ref stays as the same-mount
   * half, for the window in which a later resolver pass has already dropped
   * the root error (this module's doc comment, point 1) and the re-assert
   * has not yet landed.
   */
  const hasPublishedVeto = form?.formState.errors.root?.[GRAPH_ADMISSION_KEY] !== undefined;
  const setError = form?.setError;
  const clearErrors = form?.clearErrors;
  const trigger = form?.trigger;
  const isVetoing = useRef(false);

  useLayoutEffect(() => {
    if (setError === undefined || clearErrors === undefined || trigger === undefined) return;
    if (blocked) {
      isVetoing.current = true;
      setError(GRAPH_ADMISSION_FIELD, { type: 'graph-admission', message: 'the pipeline graph is not admissible' });
      return;
    }
    if (!isVetoing.current && !hasPublishedVeto) return;
    isVetoing.current = false;
    clearErrors(GRAPH_ADMISSION_FIELD);
    // `clearErrors` leaves `isValid` where the veto put it; only a real
    // validation pass lifts it. See this module's doc comment, point 2.
    void trigger();
  }, [blocked, isValid, hasPublishedVeto, setError, clearErrors, trigger]);
}

/** Stable key for one issue — see `NodeAdmissionIssues.tsx`. */
function issueKey(issue: GraphAdmissionIssue): string {
  return `${issue.rule}|${issue.field}|${issue.subject}`;
}

/** The node ids carrying at least one issue, in canvas order, de-duplicated. */
function offendingNodeIds(issues: readonly GraphAdmissionIssue[]): readonly string[] {
  return [...new Set(issues.flatMap((issue) => (issue.nodeId === undefined ? [] : [issue.nodeId])))];
}

/**
 * Notices that never block a save (today: a declared state `reducer`, which
 * only YAML can edit). The same outlined alert and list as the refusals; each
 * message names its field, so the list needs no translated title of its own.
 */
function GraphAdmissionNotices({ notices }: { readonly notices: readonly GraphAdmissionIssue[] }): ReactNode {
  if (notices.length === 0) return null;
  return (
    <Alert
      severity="warning"
      variant="outlined"
      sx={alertSx}
      data-testid="graph-admission-notice"
    >
      <Box
        component="ul"
        sx={listSx}
      >
        {notices.map((issue) => (
          <Typography
            key={issueKey(issue)}
            component="li"
            variant="bodySmall"
            title={issue.citation}
          >
            {issue.message}
          </Typography>
        ))}
      </Box>
    </Alert>
  );
}

export function GraphAdmissionGate({ summaryHidden = false }: GraphAdmissionGateProps): ReactNode {
  const { issues: allIssues, hasGraph, parseFailed } = useGraphAdmission();
  const issues = blockingIssues(allIssues);
  const notices = allIssues.filter((issue) => issue.severity === 'warning');
  /*
   * `parseFailed` is a veto in its own right. It is reachable only from the
   * Yaml tab, and only because that tab is the one place the stored document
   * (`yamlCode`) can be edited into something `js-yaml` cannot read — see
   * `lib/livePipelineGraphAdmission.ts`. The runtime's refusal for it is
   * `serde_yaml`'s, before any transcribed rule runs, which is why it carries
   * no `GraphAdmissionIssue` and needs its own line below.
   */
  useGraphAdmissionSaveVeto(hasGraph && (parseFailed || issues.length > 0));

  if (summaryHidden) return null;
  if (!parseFailed && issues.length === 0) return <GraphAdmissionNotices notices={notices} />;

  const documentIssues = documentLevelIssues(issues);
  const nodeIds = offendingNodeIds(issues);

  return (
    <>
      <Alert
        severity="error"
        variant="outlined"
        sx={alertSx}
        data-testid="graph-admission-gate"
      >
        <AlertTitle>{t('features.pipelines.graphAdmissionGate.title', 'This pipeline cannot be saved')}</AlertTitle>
        <Typography variant="bodySmall">
          {t('features.pipelines.graphAdmissionGate.body', 'The runtime would refuse this graph, so saving is blocked until it is fixed.')}
        </Typography>
        <Box
          component="ul"
          sx={listSx}
        >
          {parseFailed && (
            <Typography
              component="li"
              variant="bodySmall"
            >
              {t('features.pipelines.graphAdmissionGate.unparseable', 'The YAML does not parse, so the runtime cannot read a graph out of it.')}
            </Typography>
          )}
          {documentIssues.map((issue) => (
            <Typography
              key={issueKey(issue)}
              component="li"
              variant="bodySmall"
              title={issue.citation}
            >
              {issue.message}
            </Typography>
          ))}
          {nodeIds.map((nodeId) => (
            <Typography
              key={nodeId}
              component="li"
              variant="bodySmall"
            >
              {t('features.pipelines.graphAdmissionGate.nodeHint', 'See the errors on node')} {nodeId}
            </Typography>
          ))}
        </Box>
      </Alert>
      <GraphAdmissionNotices notices={notices} />
    </>
  );
}
