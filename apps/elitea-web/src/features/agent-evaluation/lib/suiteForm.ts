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

export type RunLimitAction = 'stop' | 'report';

export interface SuiteForm {
  name: string;
  tokenLimitPerCase: string;
  costLimitPerCase: string;
  tokenLimitPerRun: string;
  costLimitPerRun: string;
  onRunLimit: RunLimitAction;
}

export const EMPTY_SUITE_FORM: SuiteForm = {
  name: '',
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
  return Number.isFinite(value) && value > 0 && value <= MAX_COST_LIMIT ? value : 'invalid';
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
      'features.agentEvaluation.suites.badCost',
      'A cost limit is an amount in USD above 0, or empty for no limit.',
    );
  }
  return undefined;
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
  return {
    name: form.name.trim(),
    token_limit_per_case: tokens(form.tokenLimitPerCase),
    cost_limit_per_case: cost(form.costLimitPerCase),
    token_limit_per_run: tokens(form.tokenLimitPerRun),
    cost_limit_per_run: cost(form.costLimitPerRun),
    on_run_limit: form.onRunLimit,
  };
}
