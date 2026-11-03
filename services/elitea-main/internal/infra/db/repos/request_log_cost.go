package repos

// requestLogCostSQL is the cost in USD of one gateway request log row. It
// reads the row as `l` (gateway.llm_request_logs) and its catalogue price as
// `m` (LEFT JOIN gateway.gateway_models AS m ON m.provider = l.provider AND
// m.model_name = l.model). A metered tool call costs what the gateway
// billed. A model call costs its tokens at the catalogue rate; a model with
// no price costs 0.
//
// The per-run spend cap (enforceSpendCapsSQL), the agent breaker's hourly
// spend (spendOverLimitSQL) and the unattended chain's spend
// (continueRunChainSQL) all use this one expression, so the three agree on
// what a run spent.
const requestLogCostSQL = `CASE WHEN l.operation = 'tool' THEN coalesce(l.cost_usd, 0)
                ELSE coalesce(l.prompt_tokens::numeric * m.input_cost_per_1m_tokens / 1000000, 0)
                   + coalesce(l.completion_tokens::numeric * m.output_cost_per_1m_tokens / 1000000, 0)
           END`
