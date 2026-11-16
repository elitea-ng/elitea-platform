/**
 * The hire wizard's rules, with no React in them (ADR-0051 decision 3): which
 * versions can be hired, which requirements still need a binding, what a hire
 * will do, and what a refusal says.
 */
import type {
  MarketplaceBinding,
  MarketplaceRequirement,
  MarketplaceRequirements,
  MarketplaceVersion,
} from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

export type HireMode = 'install' | 'fork';

/** The binding select's value for "Disable for now". */
export const DISABLED_VALUE = '__disabled__';

/** A yanked or withdrawn version cannot be hired; a deprecated one can, with a warning. */
export function isHireable(version: MarketplaceVersion): boolean {
  return version.state !== 'yanked' && version.withdrawn !== true;
}

/** The version the wizard starts on: the current one, else the newest that can be hired. */
export function defaultVersionId(versions: readonly MarketplaceVersion[]): string {
  const current = versions.find((version) => version.state === 'current' && isHireable(version));
  return (current ?? versions.find(isHireable))?.id ?? '';
}

/** Toolkit and MCP requirements are the ones a hire cannot finish without. */
export function mustBind(requirement: MarketplaceRequirement): boolean {
  return requirement.kind === 'toolkit' || requirement.kind === 'mcp';
}

/** What a person chose for one requirement: a project entity, or "disable for now". */
type BindingChoice = { readonly kind: 'entity'; readonly id: string } | { readonly kind: 'disabled' };

export type BindingChoices = Readonly<Record<string, BindingChoice | undefined>>;

/** The required toolkit and MCP types with no choice yet: hiring cannot finish while any is left. */
export function unboundRequirements(
  requirements: MarketplaceRequirements,
  choices: BindingChoices,
): MarketplaceRequirement[] {
  return requirements.items.filter((requirement) => mustBind(requirement) && choices[requirement.key] === undefined);
}

/** One choice changed: a blank value clears it, the disabled value turns the requirement off, anything else names a project entity. */
export function applyChoice(choices: BindingChoices, key: string, value: string): BindingChoices {
  const next = { ...choices };
  if (value === '') delete next[key];
  else next[key] = value === DISABLED_VALUE ? { kind: 'disabled' } : { kind: 'entity', id: value };
  return next;
}

/** The choices the server holds: a binding to an entity, or "disabled". */
export function choicesOf(bindings: readonly MarketplaceBinding[]): BindingChoices {
  const out: Record<string, BindingChoice> = {};
  for (const binding of bindings) {
    if (binding.disabled === true) out[binding.requirement_key] = { kind: 'disabled' };
    else if (binding.toolkit_id !== undefined)
      out[binding.requirement_key] = { kind: 'entity', id: binding.toolkit_id };
  }
  return out;
}

/** The request's `bindings`: one per choice made. */
export function bindingsOf(requirements: MarketplaceRequirements, choices: BindingChoices): MarketplaceBinding[] {
  const out: MarketplaceBinding[] = [];
  for (const requirement of requirements.items) {
    const choice = choices[requirement.key];
    if (choice === undefined) continue;
    out.push(
      choice.kind === 'disabled'
        ? { requirement_key: requirement.key, disabled: true }
        : { requirement_key: requirement.key, toolkit_id: choice.id },
    );
  }
  return out;
}

/** Above low tier, or with write tools, the agent starts with its tools off and a change request enables them. */
export function needsApproval(requirements: MarketplaceRequirements): boolean {
  return requirements.tier !== 'low' || requirements.has_write_tools;
}

const REFUSALS: Readonly<Record<string, () => string>> = {
  hire_needs_shared_project: () =>
    t(
      'marketplace.hire.error.sharedProject',
      'This hire needs a second person to approve it. A personal project has none. Hire it in a shared project.',
    ),
  binding_missing: () => t('marketplace.hire.error.binding', 'Bind every required toolkit, or choose Disable for now.'),
  version_not_hireable: () =>
    t('marketplace.hire.error.version', 'This version cannot be hired now. Choose another version.'),
};

/** The server's refusal code (`error`) of a failed hire, when it sent one. */
export function hireErrorCode(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const body = error.failure.body as { readonly error?: unknown } | null;
  return typeof body?.error === 'string' ? body.error : undefined;
}

/** One sentence for a failed hire. A code we know gets its own words; the rest get a plain failure line. */
export function hireErrorMessage(error: unknown): string {
  const known = REFUSALS[hireErrorCode(error) ?? ''];
  return known === undefined ? t('marketplace.hire.error.generic', 'The hire failed. Try again.') : known();
}
