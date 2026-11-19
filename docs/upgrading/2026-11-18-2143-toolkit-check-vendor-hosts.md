# Connection checks for nine more credential types (issue 2143)

**What changed.** `Test connection` now works for `ado`, `qtest`, `testrail`, `xray`,
`zephyr_enterprise`, `zephyr_essential`, `report_portal`, `testio` and client-secret `sharepoint`
credentials. Before they answered `unsupported_type`. `openapi` still does. The toolkit edit page also
no longer disables **Save** when a credential's check answers `auth_failed`: it shows a warning beside
Save instead. No migration, no contract change.

**Who is affected.** Operators who set `ELITEA_TOOLKIT_CHECK_ALLOWLIST`. The variable REPLACES the
built-in list, so a deployment that set it does not get the new vendor hosts and the new checks answer
`unreachable` ("refused by the egress policy of the toolkit connection check") for them.

**What to do.** Add the hosts your credentials use: `dev.azure.com`, `*.visualstudio.com`,
`*.qtestnet.com`, `*.testrail.io`, `xray.cloud.getxray.app` (and its `us.`, `eu.` and `au.` regional
hosts), `api.zephyrscale.smartbear.com`, `login.microsoftonline.com` and `graph.microsoft.com`
(SharePoint), `api.test.io`. A self-hosted TestRail, qTest, Zephyr Enterprise, Report Portal or Azure
DevOps Server host has to be named too, with its private range if it has one. Deployments that leave the
variable empty need nothing.
