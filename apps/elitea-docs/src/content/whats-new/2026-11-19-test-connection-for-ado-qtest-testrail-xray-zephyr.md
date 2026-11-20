---
date: 2026-11-19
order: 0
---
## Test connection for ADO, qTest, TestRail, Xray, Zephyr, SharePoint, Report Portal and TestIO; Save is never blocked by a check

**Test Connection** now works for Azure DevOps, qTest, TestRail, Xray Cloud, Zephyr Enterprise, Zephyr Essential, Report Portal, TestIO and client-secret SharePoint credentials. Before, they answered "Checking connection is not supported yet". OpenAPI still has no check, because a custom API has no endpoint that says who you are. On a toolkit's edit page, a credential the provider rejected now shows a warning beside **Save** and no longer disables it. A check that cannot run, is unsupported, cannot reach the provider or errors never warns. Operators who set `ELITEA_TOOLKIT_CHECK_ALLOWLIST` must add the new vendor hosts. See [Test connection](/menus/credentials#test-connection).
