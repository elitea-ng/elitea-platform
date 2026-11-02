/**
 * Port of `apps/elitea-ui/src/[fsd]/features/interactive-tours/lib/constants/sidebarTourTargets.constants.js`
 */

import { buildTourSelector } from '../helpers/tourSelector.helpers';

export const SIDEBAR_TOUR_TARGET_IDS = {
  logo: 'sidebar-logo',
  themeToggle: 'sidebar-switch-dark-light-mode',
  projectSwitcher: 'sidebar-project-switcher',
  newChat: 'sidebar-new-chat',
  navChat: 'sidebar-nav-chat',
  navAgents: 'sidebar-nav-agents',
  navSkills: 'sidebar-nav-skills',
  navPipelines: 'sidebar-nav-pipelines',
  navCredentials: 'sidebar-nav-credentials',
  navToolkits: 'sidebar-nav-toolkits',
  navApplications: 'sidebar-nav-applications',
  navMcps: 'sidebar-nav-mcps',
  navArtifacts: 'sidebar-nav-artifacts',
  agentHub: 'sidebar-agent-hub',
  settings: 'sidebar-settings',
  resources: 'sidebar-resources',
  notifications: 'sidebar-notifications',
  user: 'sidebar-user',
  supportAssistant: 'sidebar-support-assistant',
};

export const SIDEBAR_TOUR_TARGETS = {
  logo: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.logo),
  themeToggle: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.themeToggle),
  projectSwitcher: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.projectSwitcher),
  newChat: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.newChat),
  navChat: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navChat),
  navAgents: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navAgents),
  navSkills: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navSkills),
  navPipelines: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navPipelines),
  navCredentials: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navCredentials),
  navToolkits: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navToolkits),
  navApplications: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navApplications),
  navMcps: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navMcps),
  navArtifacts: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.navArtifacts),
  agentHub: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.agentHub),
  settings: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.settings),
  resources: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.resources),
  notifications: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.notifications),
  user: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.user),
  supportAssistant: buildTourSelector(SIDEBAR_TOUR_TARGET_IDS.supportAssistant),
};
