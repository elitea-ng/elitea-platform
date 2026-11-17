import type { DelegationGrant, DelegationItem, VersionToolRef } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * Pure helpers of the attach review (ADR-0044 amendment 1, A1.2 rule 2; epic
 * 2156 story S3): what a callee brings, grouped for a person to read, why an
 * item is withheld, and what state an edge's grant is in.
 */

/** A stable key of one item: the API identifies an item by its three fields. */
export function itemKey(item: DelegationItem): string {
  return `${item.kind}|${item.toolkit ?? ''}|${item.name}`;
}

/** One toolkit of the callee with the tools selected on it and the credential reference it names. */
export interface ToolkitGroup {
  readonly key: string;
  readonly label: string;
  /** The toolkit item itself; approving the toolkit is one switch, its tools follow it. */
  readonly toolkit: DelegationItem;
  readonly tools: readonly DelegationItem[];
  readonly credentials: readonly DelegationItem[];
}

export interface GroupedItems {
  readonly toolkits: readonly ToolkitGroup[];
  readonly dataClasses: readonly DelegationItem[];
  readonly egressHosts: readonly DelegationItem[];
}

/** `toolkit:<id>` to the name the callee's version carries for it; a built-in toolkit is named by its type. */
export function toolkitNames(tools: readonly VersionToolRef[] | undefined): ReadonlyMap<string, string> {
  const names = new Map<string, string>();
  for (const tool of tools ?? []) {
    if (tool.type === 'application') continue;
    const name = tool.name?.trim();
    if (tool.tool_id !== undefined && name !== undefined && name !== '') names.set(`toolkit:${tool.tool_id}`, name);
  }
  return names;
}

function toolkitLabel(key: string, names: ReadonlyMap<string, string>): string {
  const known = names.get(key);
  if (known !== undefined) return known;
  if (key.startsWith('builtin:')) return key.slice('builtin:'.length);
  return t('agents.delegation.toolkitFallback', 'Toolkit {{id}}', { id: key.replace(/^toolkit:/, '') });
}

/** Groups the flat item list of a grant by toolkit, so a person reads "Git: push, pull; credential" and not 14 lines. */
export function groupItems(items: readonly DelegationItem[], names: ReadonlyMap<string, string> = new Map()): GroupedItems {
  const toolkits = new Map<string, { toolkit: DelegationItem; tools: DelegationItem[]; credentials: DelegationItem[] }>();
  for (const item of items) {
    if (item.kind === 'toolkit') toolkits.set(item.name, { toolkit: item, tools: [], credentials: [] });
  }
  for (const item of items) {
    const group = item.toolkit === undefined ? undefined : toolkits.get(item.toolkit);
    if (item.kind === 'tool') group?.tools.push(item);
    if (item.kind === 'credential') group?.credentials.push(item);
  }
  return {
    toolkits: [...toolkits.entries()].map(([key, group]) => ({ key, label: toolkitLabel(key, names), ...group })),
    dataClasses: items.filter((item) => item.kind === 'data_class'),
    egressHosts: items.filter((item) => item.kind === 'egress_host'),
  };
}

/** Why an item the approver cannot hold is disabled. Egress hosts and data classes are never withheld. */
export function withheldReason(item: DelegationItem): string {
  if (item.kind === 'credential') {
    return t('agents.delegation.reason.credential', 'You cannot use this credential, so it stays out of the approval.');
  }
  return t('agents.delegation.reason.toolkit', 'You do not have access to this toolkit, so it stays out of the approval.');
}

/** The tool name shown for an item: `*` is "every tool of the toolkit". */
export function toolLabel(item: DelegationItem): string {
  return item.name === '*' ? t('agents.delegation.allTools', 'All tools') : item.name;
}

export type GrantView = 'approved' | 'pending' | 'awaiting' | 'stale';

/**
 * What a row says about its edge's grant: approved, `awaiting` (a governed
 * request is open, or a pending grant a different approver must decide),
 * `pending` (an editor can approve it now) or `stale` (the callee changed
 * after approval, re-approval needed). Superseded and revoked grants are not
 * the edge's grant; the caller passes only active ones.
 */
export function grantView(grant: DelegationGrant): GrantView {
  if (grant.state === 'approved') return 'approved';
  if (grant.state === 'stale') return 'stale';
  return grant.governed ? 'awaiting' : 'pending';
}

export function grantViewLabel(view: GrantView): string {
  switch (view) {
    case 'approved':
      return t('agents.delegation.state.approved', 'Delegation approved');
    case 'awaiting':
      return t('agents.delegation.state.awaiting', 'Awaiting approval');
    case 'stale':
      return t('agents.delegation.state.stale', 'Needs re-approval');
    default:
      return t('agents.delegation.state.pending', 'Delegation not approved');
  }
}

/** The edge kind of an attach: a pipeline's child, or an agent used as a tool. */
export function edgeKindFor(parentIsPipeline: boolean): 'pipeline_child' | 'agent_tool' {
  return parentIsPipeline ? 'pipeline_child' : 'agent_tool';
}

/**
 * The items to send to approve, given the toolkits the person unticked. A
 * toolkit takes its tools and credential reference with it; data classes and
 * egress hosts are never left out (dropping a class would widen a grant, and a
 * host is a project bound, not something an approver holds). `undefined`
 * (approve all the approver holds) when nothing is unticked, so the server
 * decides with its own access reading.
 */
export function itemsToApprove(approvable: readonly DelegationItem[], unticked: ReadonlySet<string>): readonly DelegationItem[] | undefined {
  if (unticked.size === 0) return undefined;
  return approvable.filter((item) => {
    const owner = item.kind === 'toolkit' ? item.name : item.toolkit;
    return owner === undefined || !unticked.has(owner);
  });
}

/** Whether the toolkit group is out of the approval: its toolkit is among the withheld items. */
export function isWithheld(group: ToolkitGroup, withheld: ReadonlySet<string>): boolean {
  return withheld.has(itemKey(group.toolkit));
}

/** The reason a withheld toolkit stays out: its credential when that is what the approver lacks, else the toolkit. */
export function groupWithheldReason(group: ToolkitGroup, withheld: ReadonlySet<string>): string {
  const credential = group.credentials.find((item) => withheld.has(itemKey(item)));
  return withheldReason(credential ?? group.toolkit);
}
