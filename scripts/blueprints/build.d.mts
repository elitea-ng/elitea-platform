/** Types of the blueprint builder for its TypeScript callers (the docs site's integration). */
export interface BuildOptions {
  root: string;
  parseYaml: (text: string) => unknown;
  dumpYaml: (value: unknown) => string;
}

export interface BuiltBlueprints {
  /** Built path (index.json, agents/<slug>.agent.md, teams/<slug>.team.zip, …) → bytes. */
  files: Map<string, Uint8Array>;
  index: unknown;
}

export function buildBlueprints(options: BuildOptions): Promise<BuiltBlueprints>;
