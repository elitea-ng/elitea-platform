/**
 * The console item ids a page passes to the read-only wrapper (ADR-0059
 * decision 6; R3.4b, issue 2613): the server's ids
 * (`internal/adminaccess/console.go`), so the level a page reads is the one
 * the capability document gives that item. A Configuration tab that stands
 * for its own item (R3.5a, issue 2535; R3.6, issue 2537) reads that item.
 */
export const ADMIN_ITEM = {
  users: 'users',
  identityProviders: 'identity-providers',
  scim: 'scim',
  projects: 'projects',
  appRequests: 'app-requests',
  providersModels: 'providers-models',
  modelsPricing: 'models-pricing',
  toolPrices: 'tool-prices',
  llmGovernance: 'llm-governance',
  budgetsAlerts: 'budgets-alerts',
} as const;
