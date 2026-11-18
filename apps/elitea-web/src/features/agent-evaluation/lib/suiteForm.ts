/**
 * The suite editor's form: its working state, its validation and the body it
 * sends.
 *
 * The numeric fields are STRINGS in the form for the reason the dimension form
 * gives (model/types.ts): a number input cannot hold `""`, `"1."` or a lone
 * `"-"` while it is typed. They are converted once, at save. An empty field is
 * "no limit".
 */
import type { EvalSuite, EvalSuiteWriteRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/** The server's own bounds (evaluation.MaxTokenLimit and MaxCostLimit). */
const MAX_TOKEN_LIMIT = 2_000_000_000;
const MAX_COST_LIMIT = 1_000_000;
/** The smallest cost the server's column holds. */
const MIN_COST_LIMIT = 0.0001;

/** The server's own bounds (evaluation.MaxStepLimit and MaxTimeLimitSeconds). */
const MAX_STEP_LIMIT = 1000;
const MAX_TIME_LIMIT_SECONDS = 3600;

export type RunLimitAction = 'stop' | 'report';

/**
 * How a run executes a case. `predict_blocking` is one model call and the
 * default; `runtime_plane` is a real agent run on the worker, and only then do
 * a steps limit, a time limit and the tool outputs setting mean anything.
 */
export type ExecutionMode = 'predict_blocking' | 'runtime_plane';

export interface SuiteForm {
  name: string;
  executionMode: ExecutionMode;
  stepLimitPerCase: string;
  timeLimitPerCaseSeconds: string;
  storeToolOutputs: boolean;
  tokenLimitPerCase: string;
  costLimitPerCase: string;
  tokenLimitPerRun: string;
  costLimitPerRun: string;
  onRunLimit: RunLimitAction;
}

export const EMPTY_SUITE_FORM: SuiteForm = {
  name: '',
  executionMode: 'predict_blocking',
  stepLimitPerCase: '',
  timeLimitPerCaseSeconds: '',
  storeToolOutputs: true,
  tokenLimitPerCase: '',
  costLimitPerCase: '',
  tokenLimitPerRun: '',
  costLimitPerRun: '',
  onRunLimit: 'stop',
};

function shown(value: number | null | undefined): string {
  return value === null || value === undefined ? '' : String(value);
}

export function suiteToForm(suite: EvalSuite): SuiteForm {
  return {
    name: suite.name,
    executionMode: suite.execution_mode === 'runtime_plane' ? 'runtime_plane' : 'predict_blocking',
    stepLimitPerCase: shown(suite.step_limit_per_case),
    timeLimitPerCaseSeconds: shown(suite.time_limit_per_case_seconds),
    storeToolOutputs: suite.store_tool_outputs !== false,
    tokenLimitPerCase: shown(suite.token_limit_per_case),
    costLimitPerCase: shown(suite.cost_limit_per_case),
    tokenLimitPerRun: shown(suite.token_limit_per_run),
    costLimitPerRun: shown(suite.cost_limit_per_run),
    onRunLimit: suite.on_run_limit === 'report' ? 'report' : 'stop',
  };
}

function parseTokens(raw: string): number | null | 'invalid' {
  const text = raw.trim();
  if (text === '') return null;
  const value = Number(text);
  return Number.isInteger(value) && value >= 1 && value <= MAX_TOKEN_LIMIT ? value : 'invalid';
}

function parseCost(raw: string): number | null | 'invalid' {
  const text = raw.trim();
  if (text === '') return null;
  const value = Number(text);
  return Number.isFinite(value) && value >= MIN_COST_LIMIT && value <= MAX_COST_LIMIT ? value : 'invalid';
}

function parseWhole(raw: string, max: number): number | null | 'invalid' {
  const text = raw.trim();
  if (text === '') return null;
  const value = Number(text);
  return Number.isInteger(value) && value >= 1 && value <= max ? value : 'invalid';
}

/** The runtime_plane controls: checked only when the suite runs agent runs. */
function runtimeFormError(form: SuiteForm): string | undefined {
  if (form.executionMode !== 'runtime_plane') return undefined;
  if (parseWhole(form.stepLimitPerCase, MAX_STEP_LIMIT) === 'invalid') {
    return t(
      'features.agentEvaluation.suites.badSteps',
      'A steps limit is a whole number from 1 to {{max}}, or empty for no limit.',
      { max: MAX_STEP_LIMIT },
    );
  }
  if (parseWhole(form.timeLimitPerCaseSeconds, MAX_TIME_LIMIT_SECONDS) === 'invalid') {
    return t(
      'features.agentEvaluation.suites.badTime',
      'A time limit is a whole number of seconds from 1 to {{max}}, or empty for the default.',
      { max: MAX_TIME_LIMIT_SECONDS },
    );
  }
  return undefined;
}

/** The first thing wrong with the form, as a sentence, or `undefined`. */
export function suiteFormError(form: SuiteForm): string | undefined {
  if (form.name.trim() === '') {
    return t('features.agentEvaluation.suites.needName', 'Give the limits a name.');
  }
  if (parseTokens(form.tokenLimitPerCase) === 'invalid' || parseTokens(form.tokenLimitPerRun) === 'invalid') {
    return t(
      'features.agentEvaluation.suites.badTokens',
      'A token limit is a whole number of at least 1, or empty for no limit.',
    );
  }
  if (parseCost(form.costLimitPerCase) === 'invalid' || parseCost(form.costLimitPerRun) === 'invalid') {
    return t(
      'features.agentEvaluation.suites.badCostMin',
      'A cost limit is an amount in USD of at least 0.0001, or empty for no limit.',
    );
  }
  return runtimeFormError(form);
}

/** The request body. Only call it when `suiteFormError` is `undefined`. */
export function suiteFormToInput(form: SuiteForm): EvalSuiteWriteRequest {
  const tokens = (raw: string): number | null => {
    const value = parseTokens(raw);
    return value === 'invalid' ? null : value;
  };
  const cost = (raw: string): number | null => {
    const value = parseCost(raw);
    return value === 'invalid' ? null : value;
  };
  const runtime = form.executionMode === 'runtime_plane';
  const whole = (raw: string, max: number): number | null => {
    const value = parseWhole(raw, max);
    return value === 'invalid' ? null : value;
  };
  return {
    name: form.name.trim(),
    // The three controls are sent only for runtime_plane: the server refuses a
    // step or time limit on a suite that runs one model call per case.
    execution_mode: form.executionMode,
    step_limit_per_case: runtime ? whole(form.stepLimitPerCase, MAX_STEP_LIMIT) : null,
    time_limit_per_case_seconds: runtime ? whole(form.timeLimitPerCaseSeconds, MAX_TIME_LIMIT_SECONDS) : null,
    store_tool_outputs: runtime ? form.storeToolOutputs : true,
    token_limit_per_case: tokens(form.tokenLimitPerCase),
    cost_limit_per_case: cost(form.costLimitPerCase),
    token_limit_per_run: tokens(form.tokenLimitPerRun),
    cost_limit_per_run: cost(form.costLimitPerRun),
    on_run_limit: form.onRunLimit,
  };
}
