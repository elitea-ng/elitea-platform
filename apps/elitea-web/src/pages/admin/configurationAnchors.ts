/**
 * The old Configuration anchors of the LLM gateway editor (ADR-0059 R3.5a,
 * issue 2535). A bookmark of `/configuration#llm_proxy`, or of one tab of it
 * (`#llm_proxy/usage`), opens the page that tab became under the six-area
 * navigation; `./AdminConsoleOutlet` redirects. The tab keys are the ones the
 * editor's tabs use (`./AdminLlmProxyEditor.tsx`).
 */
const GATEWAY_TAB_ROUTES: Readonly<Record<string, string>> = {
  status: '/models/status',
  providers: '/models/providers',
  models: '/models/pricing',
  tools: '/models/tool-prices',
  usage: '/models/usage',
  logs: '/models/logs',
  alerts: '/budgets/alerts',
};

/** The section id of the gateway editor (`config_schemas.go`, `llmProxySection`). */
const LLM_PROXY_SECTION = 'llm_proxy';

/** The page an LLM Proxy anchor now opens; `undefined` for any other anchor. */
export function gatewayAnchorRoute(hash: string): string | undefined {
  const anchor = hash.startsWith('#') ? hash.slice(1) : hash;
  const [section, tab] = anchor.split(/[/:.]/, 2);
  if (section !== LLM_PROXY_SECTION) return undefined;
  if (tab === undefined || tab === '') return GATEWAY_TAB_ROUTES['providers'];
  return GATEWAY_TAB_ROUTES[tab] ?? GATEWAY_TAB_ROUTES['providers'];
}
