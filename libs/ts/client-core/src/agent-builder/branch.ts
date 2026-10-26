/**
 * The one question of the builder (ADR-0037 decision 2): a local agent (an
 * `AGENTS.md` in this folder) or an Elitea agent (generated, saved to the
 * project, runs anywhere). Only a desktop folder session can offer the local
 * branch: the web, the mobile app and a desktop thread that runs in the
 * cloud have no folder to write to.
 */
export type AgentBranch = 'local' | 'elitea';

export interface BranchSurface {
  /** A desktop folder session whose turns run on this computer. */
  readonly localFolder: boolean;
}

/** The branches to offer, in the order to show them. */
export function agentBranches(surface: BranchSurface): readonly AgentBranch[] {
  return surface.localFolder ? ['local', 'elitea'] : ['elitea'];
}

/** The branch to start on: with only one to offer, the question is not asked. */
export function initialBranch(surface: BranchSurface): AgentBranch | undefined {
  const branches = agentBranches(surface);
  return branches.length === 1 ? branches[0] : undefined;
}
