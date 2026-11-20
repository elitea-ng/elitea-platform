---
date: 2026-11-19
order: 3
---
## Tavily toolkit, and crawl and map in Web Search

There is a new **Tavily** toolkit. It gives an agent `web_search`, `web_extract`, `web_crawl` and `web_map` with the project's own Tavily credential, and its own settings for allowed and blocked domains, page size, search and extract depth, and a cap on Tavily calls per run. The **Web Search** toolkit gains `web_crawl` (Tavily) and `web_map` (Tavily and Firecrawl) for the providers an administrator enabled. Crawls and maps are billed in the provider's units under "Web fetch" in **Settings › Usage**. Upgrade the worker before Main. Deployments that restrict worker egress already allow `api.tavily.com` for the Web Search toolkit. See [The Tavily toolkit](/integrations/toolkits/web_search_toolkit#the-tavily-toolkit).
