/**
 * Saved teams (ADR-0050 decision 8, issue 1620): a project's saved rosters
 * of agents with their lead — the Teams tab of Agents, and "Save as team" in
 * a conversation's row menu in the Chats list.
 */
export { TeamsPanel } from './ui/TeamsPanel';
export type { TeamsPanelProps } from './ui/TeamsPanel';
export { SaveAsTeamDialog } from './ui/SaveAsTeamDialog';
export { TEAM_PERMISSIONS, useTeamAbilities } from './model/useAgentTeams';
