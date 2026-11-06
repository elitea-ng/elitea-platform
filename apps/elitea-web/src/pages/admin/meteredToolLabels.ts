/**
 * Labels for metered tool operations (#1238): web search, web fetch,
 * internet host lookups and cloud browser time (ADR-0049) that the gateway
 * bills per unit. In the usage ledger
 * a tool row reuses the model columns, with the vendor as the provider and
 * the operation as the model, so the tables would otherwise show
 * "web_search" beside "gpt-5" and a token count of zero.
 */
import { t } from "@/shared/i18n";

export type MeteredUnit = "request" | "credit" | "page" | "second";

const PROVIDER_LABELS: Readonly<Record<string, string>> = {
  tavily: "Tavily",
  firecrawl: "Firecrawl",
  brave: "Brave Search",
  ddg_instant: "DuckDuckGo Instant Answers",
  direct: "Direct fetch",
  shodan: "Shodan",
  browser: "Cloud browser",
};

export function meteredProviderLabel(provider: string): string {
  return PROVIDER_LABELS[provider] ?? provider;
}

export function meteredOperationLabel(operation: string): string {
  switch (operation) {
    case "web_search":
      return t("pages.admin.llmProxy.tools.operation.webSearch", "Web search");
    case "web_search_advanced":
      return t(
        "pages.admin.llmProxy.tools.operation.webSearchAdvanced",
        "Web search (advanced)",
      );
    case "web_fetch":
      return t("pages.admin.llmProxy.tools.operation.webFetch", "Web fetch");
    case "host_search":
      return t(
        "pages.admin.llmProxy.tools.operation.hostSearch",
        "Host search",
      );
    case "host_lookup":
      return t(
        "pages.admin.llmProxy.tools.operation.hostLookup",
        "Host lookup",
      );
    case "engine_seconds":
      return t(
        "pages.admin.llmProxy.tools.operation.engineSeconds",
        "Browser time",
      );
    default:
      return operation;
  }
}

/** "Web search · Tavily" for a tool row of the usage ledger. */
export function meteredRowLabel(operation: string, provider: string): string {
  return `${meteredOperationLabel(operation)} · ${meteredProviderLabel(provider)}`;
}

/** "3 credits": what a tool row consumed, shown where a model row shows tokens. */
export function meteredUnitsLabel(units: number, unit?: string): string {
  const amount = units.toLocaleString();
  const kind: MeteredUnit =
    unit === "credit" || unit === "page" || unit === "second"
      ? unit
      : "request";
  switch (kind) {
    case "credit":
      return units === 1
        ? t("pages.admin.llmProxy.tools.unit.credit", "{{amount}} credit", {
            amount,
          })
        : t("pages.admin.llmProxy.tools.unit.credits", "{{amount}} credits", {
            amount,
          });
    case "page":
      return units === 1
        ? t("pages.admin.llmProxy.tools.unit.page", "{{amount}} page", {
            amount,
          })
        : t("pages.admin.llmProxy.tools.unit.pages", "{{amount}} pages", {
            amount,
          });
    case "second":
      return units === 1
        ? t("pages.admin.llmProxy.tools.unit.second", "{{amount}} second", {
            amount,
          })
        : t("pages.admin.llmProxy.tools.unit.seconds", "{{amount}} seconds", {
            amount,
          });
    case "request":
      return units === 1
        ? t("pages.admin.llmProxy.tools.unit.request", "{{amount}} request", {
            amount,
          })
        : t("pages.admin.llmProxy.tools.unit.requests", "{{amount}} requests", {
            amount,
          });
  }
}
