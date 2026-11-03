/**
 * The unattended grant (ADR-0045 decision 1) as the person edits it, and its
 * wire shape. The form keeps the chain limits as typed text; `toWireGrant`
 * checks that they make sense and drops the empty ones (an absent limit
 * takes the platform default). The platform owns the maximums: Main refuses a
 * value over one (422 `unattended_grant_refused`, reason `over_maximum`), and
 * the client shows that refusal. The desktop wire never carries `continue_output_limit` (cloud
 * only) or `sensitive_tools` (named cloud tools).
 */
import type { UnattendedChain, UnattendedGrant } from '@/shared/desktop/unattendedIpc';

/** A sensitive cloud tool, as a `toolkit/tool` pair. */
export interface SensitiveToolRef {
  toolkit: string;
  tool: string;
}

/** The chain limits as typed: an empty field takes the platform default. */
export interface ChainDraft {
  max_continuations: string;
  /** US dollars. */
  max_spend: string;
  deadline_hours: string;
}

export type ChainField = keyof ChainDraft;

/** The grant as the form edits it. */
export interface UnattendedGrantValue {
  continue_step_limit: boolean;
  /** Desktop only: the local 512 tool-call cap. */
  continue_tool_call_cap: boolean;
  /** Cloud only: the output limit. */
  continue_output_limit: boolean;
  /** Desktop only. */
  hook_asks: boolean;
  /** Desktop only. */
  remote_confirmations: boolean;
  /** Cloud only: the sensitive tools the grant names. */
  sensitive_tools: SensitiveToolRef[];
  /** A run brief and a goal check before the chain ends (ADR-0045 decision 11); on by default. */
  goal_check: boolean;
  chain: ChainDraft;
}

const SECONDS_PER_HOUR = 3600;

/** A new grant: the run continues by itself; the project's own guards (hooks, remote confirmations) still ask. */
export function defaultGrant(): UnattendedGrantValue {
  return {
    continue_step_limit: true,
    continue_tool_call_cap: true,
    continue_output_limit: true,
    hook_asks: false,
    remote_confirmations: false,
    sensitive_tools: [],
    goal_check: true,
    chain: { max_continuations: '', max_spend: '', deadline_hours: '' },
  };
}

/** A new grant id for a new chain. */
export function newGrantId(): string {
  return crypto.randomUUID();
}

const INTEGER = /^\d+$/u;
const AMOUNT = /^(?:\d+(?:\.\d{1,4})?|\.\d{1,4})$/u;
const NUMBER = /^(?:\d+(?:\.\d+)?|\.\d+)$/u;

function continuations(text: string): number | null {
  const value = INTEGER.test(text) ? Number(text) : Number.NaN;
  return Number.isSafeInteger(value) ? value : null;
}

function spend(text: string): number | null {
  const value = AMOUNT.test(text) ? Number(text) : Number.NaN;
  return value > 0 ? value : null;
}

function deadlineSeconds(text: string): number | null {
  const seconds = NUMBER.test(text) ? Math.round(Number(text) * SECONDS_PER_HOUR) : Number.NaN;
  return Number.isSafeInteger(seconds) && seconds >= 1 ? seconds : null;
}

/** The chain fields typed but not valid. */
export function chainErrors(chain: ChainDraft): ChainField[] {
  const checks: [ChainField, (text: string) => number | null][] = [
    ['max_continuations', continuations],
    ['max_spend', spend],
    ['deadline_hours', deadlineSeconds],
  ];
  return checks.filter(([field, parse]) => chain[field].trim() !== '' && parse(chain[field].trim()) === null).map(([field]) => field);
}

/** The chain limits on the wire: only the typed ones. Call it on a chain with no errors. */
function wireChain(chain: ChainDraft): UnattendedChain {
  const out: UnattendedChain = {};
  const [count, amount, hours] = [chain.max_continuations.trim(), chain.max_spend.trim(), chain.deadline_hours.trim()];
  if (count !== '') out.max_continuations = Number(count);
  if (amount !== '') out.max_spend = Number(amount);
  if (hours !== '') out.deadline_seconds = Math.round(Number(hours) * SECONDS_PER_HOUR);
  return out;
}

export type WireGrantResult = { ok: true; grant: UnattendedGrant } | { ok: false; invalid: ChainField[] };

/** The desktop wire of `value` (with `grantId` when given), or the chain fields that are not valid. */
export function toWireGrant(value: UnattendedGrantValue, grantId?: string): WireGrantResult {
  const invalid = chainErrors(value.chain);
  if (invalid.length > 0) return { ok: false, invalid };
  const chain = wireChain(value.chain);
  const grant: UnattendedGrant = {
    ...(grantId === undefined ? {} : { grant_id: grantId }),
    continue_step_limit: value.continue_step_limit,
    continue_tool_call_cap: value.continue_tool_call_cap,
    hook_asks: value.hook_asks,
    remote_confirmations: value.remote_confirmations,
    goal_check: value.goal_check,
    ...(Object.keys(chain).length === 0 ? {} : { chain }),
  };
  return { ok: true, grant };
}

const SWITCHES = ['continue_step_limit', 'continue_tool_call_cap', 'continue_output_limit', 'hook_asks', 'remote_confirmations'] as const;
const CHAIN_FIELDS: readonly ChainField[] = ['max_continuations', 'max_spend', 'deadline_hours'];

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null;

function readChain(raw: unknown): ChainDraft | null {
  if (!isRecord(raw) || !CHAIN_FIELDS.every((field) => typeof raw[field] === 'string')) return null;
  return { max_continuations: String(raw.max_continuations), max_spend: String(raw.max_spend), deadline_hours: String(raw.deadline_hours) };
}

function readTools(raw: unknown): SensitiveToolRef[] {
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((item): item is SensitiveToolRef => isRecord(item) && typeof item.toolkit === 'string' && typeof item.tool === 'string')
    .map(({ toolkit, tool }) => ({ toolkit, tool }));
}

/** A kept grant read back (stored JSON), or null when it is not a valid grant. A grant kept before the goal check has it on. */
export function readGrant(raw: unknown): UnattendedGrantValue | null {
  if (!isRecord(raw) || !SWITCHES.every((key) => typeof raw[key] === 'boolean')) return null;
  const chain = readChain(raw.chain);
  if (chain === null || chainErrors(chain).length > 0) return null;
  const value = defaultGrant();
  for (const key of SWITCHES) value[key] = raw[key] === true;
  return { ...value, sensitive_tools: readTools(raw.sensitive_tools), goal_check: raw.goal_check !== false, chain };
}
