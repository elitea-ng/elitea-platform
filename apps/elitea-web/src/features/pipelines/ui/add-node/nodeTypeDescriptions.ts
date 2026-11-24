import { PipelineNodeTypes, type PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';

/**
 * Node-type descriptions rendered in the Add-node palette (`NodeTypeCard`).
 *
 * The 10 core `EliteaUI` node types use verbatim copy from `EliteaUI`
 * `origin/main` (`46d85cda7`, `flowEditor.constants.js:149-160`).
 * The 4 replatform-admitted node types (`Aggregate`, `Map`, `Parallel`,
 * `SplitOut`) use concise descriptions in the same style (Q9 ruling).
 */
export const PipelineNodeDescriptions: Readonly<Partial<Record<PipelineNodeType, string>>> = {
  [PipelineNodeTypes.Agent]: 'Executes tasks using predefined goals, tools, and logic.',
  [PipelineNodeTypes.Aggregate]: 'Groups and summarizes items into aggregated outputs.',
  [PipelineNodeTypes.Code]: 'Runs custom code to process or transform data.',
  [PipelineNodeTypes.Decision]: 'Evaluates conditions and selects the next step.',
  [PipelineNodeTypes.Hitl]: 'Pauses flow for human input or approval.',
  [PipelineNodeTypes.Http]: 'Calls a project-governed HTTP action rule with typed bindings.',
  [PipelineNodeTypes.LLM]: 'Generates or processes text using a language model.',
  [PipelineNodeTypes.Map]: 'Runs a worker step across each item in a collection.',
  [PipelineNodeTypes.Mcp]: 'Connects to external data or services via MCP protocol.',
  [PipelineNodeTypes.Parallel]: 'Executes multiple branches concurrently and joins results.',
  [PipelineNodeTypes.Printer]: 'Outputs results for display, logging, or debugging.',
  [PipelineNodeTypes.Router]: 'Directs flow to different paths based on conditions.',
  [PipelineNodeTypes.SplitOut]: 'Splits a collection into individual items for processing.',
  [PipelineNodeTypes.StateModifier]: 'Updates or enriches the pipeline state.',
  [PipelineNodeTypes.Toolkit]: 'Provides access to a set of tools or actions.',
};
