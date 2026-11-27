/** One blueprint as the app shows it (issue 3137); see scripts/blueprints.mjs. */
export interface Blueprint {
  slug: string;
  name: string;
  description: string;
  /** The Build with AI brief that Craft from it puts in the agent builder. */
  brief: string;
  showcases: string[];
  /** What it needs, as sentences (for example a connection or the desktop app). */
  requirements: string[];
  surface: string;
  /** A team's member names, lead first. */
  members?: string[];
}

export interface BlueprintData {
  agents: Blueprint[];
  teams: Blueprint[];
}
