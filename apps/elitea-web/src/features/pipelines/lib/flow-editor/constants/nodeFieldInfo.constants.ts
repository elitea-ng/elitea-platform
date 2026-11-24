/**
 * Ported from `EliteaUI` (`origin/main` `46d85cda7`):
 * - `src/[fsd]/features/pipelines/flow-editor/lib/constants/nodeFieldInfo.constants.js`
 * - `src/[fsd]/features/pipelines/flow-editor/lib/helpers/nodeFieldInfo.helpers.js`
 *
 * Extended with replatform-admitted node types (`Aggregate`, `Map`, `Parallel`,
 * `SplitOut`) and per-node resilience `settings` fields (`on_error`,
 * `always_output_data`, `execute_once`, `retry_on_fail`, `max_tries`,
 * `wait_between_tries_ms`, `timeout_ms`) for Wave U2's Node Panel.
 */
import { PipelineNodeTypes, type PipelineNodeType } from './flowEditor.constants';

// Keys of the node panel fields; input-mapping fields use their YAML mapping key
export const NODE_FIELD_KEYS = {
  name: 'name',
  type: 'type',
  trigger: 'trigger',
  agent: 'agent',
  version: 'version',
  task: 'task',
  system: 'system',
  input: 'input',
  output: 'output',
  chatHistory: 'chat_history',
  structuredOutput: 'structured_output',
  code: 'code',
  decisionCriteria: 'decision_criteria',
  decisionOutputs: 'decision_outputs',
  defaultOutput: 'default_output',
  userMessage: 'user_message',
  approve: 'approve',
  edit: 'edit',
  editStateKey: 'edit_state_key',
  reject: 'reject',
  toolkits: 'toolkits',
  tools: 'tools',
  toolkit: 'toolkit',
  mcp: 'mcp',
  tool: 'tool',
  addOptionalParameter: 'add_optional_parameter',
  printer: 'printer',
  finalMessage: 'final_message',
  condition: 'condition',
  routes: 'routes',
  jinjaTemplate: 'jinja_template',
  variablesToClean: 'variables_to_clean',
  inputMapping: 'input_mapping',
  loopTool: 'loop_tool',
  variablesMapping: 'variables_mapping',
  conditionInput: 'condition_input',
  conditionalOutputs: 'conditional_outputs',
  pipeline: 'pipeline',
  configuration: 'configuration',
  // Replatform graph extensions (Aggregate, Map, Parallel, SplitOut)
  source: 'source',
  destination: 'destination',
  worker: 'worker',
  item: 'item',
  index: 'index',
  outputs: 'outputs',
  maxItems: 'max_items',
  maxConcurrency: 'max_concurrency',
  reduction: 'reduction',
  broadcast: 'broadcast',
  split: 'split',
  retain: 'retain',
  missingList: 'missing_list',
  nullList: 'null_list',
  removeSource: 'remove_source',
  layout: 'layout',
  groupBy: 'group_by',
  operations: 'operations',
  branches: 'branches',
  wait: 'wait',
  join: 'join',
  // HTTP Request node (5d-2)
  operation: 'operation',
  request: 'request',
  response: 'response',
  responseMode: 'response_mode',
  acceptedStatuses: 'accepted_statuses',
  maxBytes: 'max_bytes',
  idempotencyKey: 'idempotency_key',
  // Per-node resilience settings (5b-1)
  onError: 'on_error',
  alwaysOutputData: 'always_output_data',
  executeOnce: 'execute_once',
  retryOnFail: 'retry_on_fail',
  maxTries: 'max_tries',
  waitBetweenTriesMs: 'wait_between_tries_ms',
  timeoutMs: 'timeout_ms',
} as const;

const K = NODE_FIELD_KEYS;

export const COMMON_NODE_FIELD_INFO: Readonly<Record<string, string>> = {
  [K.name]:
    "Unique name of this node. It's used to connect nodes and in run logs. Renaming updates all references.",
  [K.type]:
    'How the value is provided. Fixed: used exactly as typed. Variable: taken from a state variable. F-String: text with {variable} placeholders filled from state.',
  [K.trigger]:
    'What starts the pipeline. Chat Message: a user message. Schedule: runs on a timer. Webhook: runs when an external system calls a URL. Schedule and Webhook are unavailable if the pipeline needs user interaction.',
  // Per-node resilience settings (5b-1)
  [K.onError]:
    'What happens when this node fails: stop the pipeline, continue on the normal route, or route to an error output branch.',
  [K.alwaysOutputData]:
    'When on, the node emits an output entry even if execution produced no data.',
  [K.executeOnce]:
    'When on, this node runs at most once per pipeline execution and skips subsequent visits.',
  [K.retryOnFail]:
    'When on, failed executions are retried automatically up to Max tries.',
  [K.maxTries]:
    'Maximum number of attempts (including the first run) when Retry on fail is enabled.',
  [K.waitBetweenTriesMs]:
    'Delay in milliseconds before each retry attempt.',
  [K.timeoutMs]:
    'Maximum execution time in milliseconds before this node times out.',
};

const TOOL_NODE_FIELD_INFO: Readonly<Record<string, string>> = {
  [K.tool]: '',
  [K.addOptionalParameter]:
    "Add an optional argument of the selected tool. Unset optional arguments use the tool's defaults.",
  [K.input]: 'State variables available for tool parameters.',
  [K.output]: "State variables where the tool's result is saved.",
  [K.structuredOutput]:
    "When on, the tool's result is parsed as JSON and its fields are saved to the matching Output variables.",
};

const TOOLKIT_NODE_FIELD_INFO: Readonly<Record<string, string>> = {
  ...TOOL_NODE_FIELD_INFO,
  [K.toolkit]: 'The toolkit from this project to use, e.g. Jira, GitHub or Confluence.',
  [K.tool]: 'The toolkit action this node runs.',
};

const DEPRECATED_COMMON_FIELD_INFO: Readonly<Record<string, string>> = {
  [K.input]: TOOL_NODE_FIELD_INFO[K.input] ?? '',
  [K.output]: TOOL_NODE_FIELD_INFO[K.output] ?? '',
  [K.structuredOutput]: TOOL_NODE_FIELD_INFO[K.structuredOutput] ?? '',
};

const FUNCTION_INFO =
  "Deprecated: use a Toolkit or MCP node instead. Same meaning as the Toolkit node's Toolkit, Tool and Tool parameters.";
const TOOL_INFO =
  'Deprecated: use a Toolkit, MCP or Agent node. The tool to run and the instruction sent to it.';
const LOOP_INFO = 'Deprecated: runs the selected tool repeatedly for each item described in Task.';
const LOOP_FROM_TOOL_INFO =
  "Deprecated: runs Loop tool for every item returned by Tool. Variables mapping maps each item to the loop tool's inputs.";
const CUSTOM_INFO = 'Deprecated: select a specific node type instead. Raw node configuration.';
const CUSTOM_FIELD_INFO: Readonly<Record<string, string>> = {
  ...DEPRECATED_COMMON_FIELD_INFO,
  [K.configuration]: CUSTOM_INFO,
};
const CONDITION_INFO =
  'Deprecated: use Router. Variables to evaluate, the condition template, possible next nodes, and the fallback node.';

export const NODE_FIELD_INFO: Readonly<Partial<Record<PipelineNodeType, Readonly<Record<string, string>>>>> = {
  [PipelineNodeTypes.Agent]: {
    [K.agent]: 'The agent or pipeline from this project that this node runs.',
    [K.version]: 'Which version of the selected agent or pipeline to run.',
    [K.task]:
      'The instruction or question sent to the agent. Use F-String to include state values, e.g. Summarize {issue_text}.',
    [K.input]: 'State variables this node can read. Only these can be used in Task and other fields.',
    [K.output]: "State variables where the agent's answer is saved for the next nodes.",
    [K.chatHistory]:
      'Previous messages passed to the agent as context. Set to an empty list to start the agent without history.',
  },
  [PipelineNodeTypes.Aggregate]: {
    [K.source]: 'State variable containing the list of items to aggregate.',
    [K.layout]: 'Shape of the aggregated output (plain values or structured records).',
    [K.groupBy]: 'Fields used to partition items into separate groups before aggregating.',
    [K.operations]: 'Aggregation functions (such as count, sum, or collect) applied to each group.',
    [K.output]: 'State variable(s) where the aggregated result is saved.',
  },
  [PipelineNodeTypes.Code]: {
    [K.code]:
      "Python code to run. Read state values from the node's Input. The returned value is saved to Output.",
    [K.input]: 'State variables available to the code.',
    [K.output]: "State variables where the code's result is saved.",
    [K.structuredOutput]:
      'When on, the result must be a dictionary. Its keys are saved to the matching Output variables.',
  },
  [PipelineNodeTypes.Decision]: {
    [K.decisionCriteria]:
      'Describe in plain language how to choose the next step. An LLM reads this and the Input values and picks one of the Decision outputs.',
    [K.decisionOutputs]: 'Nodes the decision can route to.',
    [K.defaultOutput]: 'Node to go to if none of the Decision outputs fits.',
    [K.input]: 'State variables the decision can consider.',
  },
  [PipelineNodeTypes.Hitl]: {
    [K.userMessage]:
      'Message shown to the user when the pipeline pauses, e.g. what to review. F-String can include state values.',
    [K.approve]: 'Node to continue with when the user approves.',
    [K.edit]: 'Node to continue with after the user edits a value. Requires Edit state key.',
    [K.editStateKey]: 'State variable the user can edit before the pipeline continues on the Edit route.',
    [K.reject]: 'Node to go to when the user rejects. Defaults to End.',
    [K.input]: 'State variables available to the User message.',
  },
  [PipelineNodeTypes.LLM]: {
    [K.toolkits]: 'Toolkits and MCPs from this project that the LLM may call while answering.',
    [K.tools]:
      'Tools of the selected toolkit or MCP the LLM is allowed to use. Fewer tools means faster and more predictable answers.',
    [K.system]: "Instructions that define the LLM's role, rules and response format.",
    [K.task]: 'The request the LLM should complete. Use F-String to include state values.',
    [K.input]: 'State variables this node can read in System and Task.',
    [K.output]: "State variables where the LLM's answer is saved.",
    [K.chatHistory]:
      'Previous messages sent to the LLM as context. Use an empty list for an independent request.',
    [K.structuredOutput]:
      'When on, the LLM returns JSON matching the Output variables, and each value is saved to its variable.',
  },
  [PipelineNodeTypes.Map]: {
    [K.worker]: 'Node executed once for each item in the source collection.',
    [K.source]: 'State variable holding the list of items to iterate over.',
    [K.item]: 'State key that receives the current item inside each worker run.',
    [K.index]: 'State key that receives the zero-based item index inside each worker run.',
    [K.outputs]: 'Worker state keys collected from each item run.',
    [K.destination]: 'State variable where the collected worker outputs are written.',
    [K.maxItems]: 'Maximum number of items this Map node will process.',
    [K.maxConcurrency]: 'Maximum number of worker runs executed in parallel.',
    [K.reduction]: 'How worker outputs are combined into the destination state variable.',
    [K.broadcast]: 'Read-only state variables shared with every worker run.',
  },
  [PipelineNodeTypes.Mcp]: {
    ...TOOL_NODE_FIELD_INFO,
    [K.mcp]: 'The MCP server from this project to call. You may need to log in first.',
    [K.tool]: 'The MCP tool this node runs.',
  },
  [PipelineNodeTypes.Parallel]: {
    [K.branches]: 'Independent branch nodes executed concurrently.',
    [K.maxConcurrency]: 'Maximum number of branches allowed to run at the same time.',
    [K.wait]: 'Waits for all declared branches to finish before continuing.',
    [K.onError]: 'How branch failures affect sibling branches and the Parallel node outcome.',
    [K.join]: 'How branch results are merged into the output state variable(s).',
    [K.output]: 'State variable(s) where merged branch results are saved.',
  },
  [PipelineNodeTypes.Printer]: {
    [K.printer]: 'Content shown to the user in chat at this step. Use F-String to include state values.',
    [K.finalMessage]:
      'Note shown after the content. The pipeline pauses until the user replies. Default: "How to proceed? To resume the pipeline - type anything…"',
  },
  [PipelineNodeTypes.Router]: {
    [K.condition]: 'Template that returns the name of the route to take, based on Input values.',
    [K.routes]: 'Nodes the router can send the flow to.',
    [K.defaultOutput]: 'Node to go to if the condition matches no route.',
    [K.input]: 'State variables available to the Condition.',
  },
  [PipelineNodeTypes.SplitOut]: {
    [K.source]: 'State variable containing the list or field to split into items.',
    [K.split]: 'How each source entry is extracted into an output item.',
    [K.destination]: 'Key name assigned to each split item.',
    [K.retain]: 'Which sibling fields from the source record are kept alongside each split item.',
    [K.missingList]: 'Behavior when the source list field is missing.',
    [K.nullList]: 'Behavior when the source list value is null.',
    [K.removeSource]: 'When on, removes the original list field from each emitted record.',
    [K.output]: 'State variable(s) where the split items are saved.',
  },
  [PipelineNodeTypes.StateModifier]: {
    [K.jinjaTemplate]:
      'Jinja2 template that builds a new value from Input variables, e.g. {{ title }} - {{ status }}. The result is saved to Output.',
    [K.variablesToClean]: 'State variables reset to empty at this step, e.g. to clear temporary data.',
    [K.input]: 'State variables available in the template.',
    [K.output]: 'State variable(s) where the template result is saved.',
  },
  [PipelineNodeTypes.Toolkit]: TOOLKIT_NODE_FIELD_INFO,
  [PipelineNodeTypes.Function]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.toolkit]: FUNCTION_INFO,
    [K.tool]: FUNCTION_INFO,
    [K.inputMapping]: FUNCTION_INFO,
  },
  [PipelineNodeTypes.Tool]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.tool]: TOOL_INFO,
    [K.task]: TOOL_INFO,
  },
  [PipelineNodeTypes.Loop]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.tool]: LOOP_INFO,
    [K.task]: LOOP_INFO,
  },
  [PipelineNodeTypes.LoopFromTool]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.tool]: LOOP_FROM_TOOL_INFO,
    [K.loopTool]: LOOP_FROM_TOOL_INFO,
    [K.variablesMapping]: LOOP_FROM_TOOL_INFO,
  },
  [PipelineNodeTypes.Condition]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.conditionInput]: CONDITION_INFO,
    [K.condition]: CONDITION_INFO,
    [K.conditionalOutputs]: CONDITION_INFO,
    [K.defaultOutput]: CONDITION_INFO,
  },
  [PipelineNodeTypes.Pipeline]: {
    ...DEPRECATED_COMMON_FIELD_INFO,
    [K.pipeline]: 'Deprecated: use the Agent node. The pipeline to run as a step.',
  },
  [PipelineNodeTypes.Http]: {
    [K.operation]:
      'Name of the project HTTP action rule configured under Settings → HTTP actions. Origin, method, and credentials are managed by the rule.',
    [K.request]:
      'Structured request bindings (path, query, headers, body, and optional idempotency key) populated from pipeline state or literal values.',
    [K.response]:
      'Response decoding contract: mode (json, text, or empty), accepted HTTP status ranges, and maximum response size in bytes.',
    [K.responseMode]: 'How the response body is decoded into the output state variable (json, text, or empty).',
    [K.acceptedStatuses]: 'HTTP status code ranges treated as successful responses (default 200-299).',
    [K.maxBytes]: 'Maximum allowed response body size in bytes (1 to 2097152).',
    [K.idempotencyKey]: 'Optional state variable or literal value sent as the rule-configured idempotency key.',
    [K.output]: 'Single state variable (dict or str) where {status, content_type, byte_length, data} is saved.',
    [K.timeoutMs]: 'Per-attempt HTTP request timeout in milliseconds (1000 to 30000 ms).',
  },
  [PipelineNodeTypes.Custom]: CUSTOM_FIELD_INFO,
  // Custom nodes are rendered as the Default node type
  [PipelineNodeTypes.Default]: CUSTOM_FIELD_INFO,
};

export const getToolParamFallbackInfo = (param: string): string =>
  `Value passed to the tool's ${param} argument.`;

export const getAgentInputFallbackInfo = (variable: string): string =>
  `Value passed to the agent's ${variable} input.`;

export const getValueDataTypeInfo = (dataType = 'string'): string =>
  `Value of ${dataType} type is expected`;

export const getNodeFieldInfo = (nodeType: string | undefined, fieldKey: string): string => {
  const nodeInfoMap = NODE_FIELD_INFO as Readonly<Record<string, Readonly<Record<string, string>> | undefined>>;
  const nodeEntry = nodeType && Object.hasOwn(nodeInfoMap, nodeType) ? nodeInfoMap[nodeType] : undefined;
  const byNode = nodeEntry && Object.hasOwn(nodeEntry, fieldKey) ? nodeEntry[fieldKey] : undefined;
  const common = Object.hasOwn(COMMON_NODE_FIELD_INFO, fieldKey) ? COMMON_NODE_FIELD_INFO[fieldKey] : undefined;
  return byNode || common || '';
};

// Field texts of one node type: `getNodeFieldInfoOf(type)(fieldKey)`
export const getNodeFieldInfoOf =
  (nodeType: string | undefined) =>
  (fieldKey: string): string =>
    getNodeFieldInfo(nodeType, fieldKey);

// Agent Task and Chat history have fixed texts; other agent inputs and tool parameters show their
// own description, or a fallback. Tool parameters never use the fixed texts: a parameter may be named `input`
export const getInputMappingFieldInfo = (
  nodeType: string | undefined,
  key: string,
  description?: string,
): string => {
  const isAgent = nodeType === PipelineNodeTypes.Agent;
  const agentEntry = NODE_FIELD_INFO[PipelineNodeTypes.Agent];
  const fixedInfo = isAgent && agentEntry && Object.hasOwn(agentEntry, key) ? agentEntry[key] : undefined;
  if (fixedInfo) return fixedInfo;
  if (description) return description;
  return isAgent ? getAgentInputFallbackInfo(key) : getToolParamFallbackInfo(key);
};
