---
date: 2026-11-20
---

## Jira analysis toolkit: bulk extraction and flow metrics

There is a new **Jira analysis** toolkit. It pulls issues out of Jira in bulk with the history of every status change, writes them as CSV files in an artifact bucket, and calculates lead time, cycle time, life time, throughput, flow efficiency and flow load over those files. It uses your existing Jira credential. The files are plain CSV, so **Data Analysis** and the Code sandbox read them as they are. See [Jira analysis](/integrations/toolkits/analyse_jira_toolkit) for the tools, the dataset columns, the limits and what differs from the upstream toolkit.
