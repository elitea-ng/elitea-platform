/**
 * Public API — spec §3.3: named exports only.
 *
 * Connector requests (ADR-0044 decision 1, #1501): the "Request connector"
 * offer shown where an unapproved toolkit or MCP server is refused under the
 * `approved_only` catalogue mode, and the refusal reader that finds one.
 */
export { ConnectorNotApprovedNotice } from './ui/ConnectorNotApprovedNotice';
export { ConnectorRequestDialog } from './ui/ConnectorRequestDialog';
export {
  TOOL_CATEGORIES,
  describeConnectorRequestError,
  readConnectorRefusal,
  splitList,
  useProjectConnectorRequests,
  type ConnectorTarget,
} from './api/connectorRequests';
