# Delegation: a nested agent in the desktop app follows its edge's grant (issue 2214)

**What changed.** The desktop app runs an agent's loop on the person's computer and runs a saved agent
that agent calls as its own cloud turn (`startLocalChildTurn`). That turn has the called agent as its
root, so the worker never saw the edge from the desktop's agent to it. The main service now applies the
edge's delegation grant when it starts that turn: the nested agent runs with `own ∩ (caller ∪ grant)`
(what its caller and the approved grant cover; what it holds beyond is narrowed out), or is refused when
the edge has no grant and it holds a toolkit, credential or tool its caller does not (`enforce` mode;
`report` mode only records). A refusal answers 403 `delegation_refused` and a narrowing adds
`delegation_notice` to the 202; both carry the notice the web app already shows (client contract 1.61,
additive). The desktop app ends the sub-agent lane with the notice and draws the same card.

**Who is affected.** Only deployments that turn on `ELITEA_DELEGATION_GRANTS_ENFORCED`
(`main.env.ELITEA_DELEGATION_GRANTS_ENFORCED`, **off by default**) and use the desktop app's local agent
loop with agents that call saved agents. With the switch off nothing changes: the child start is what it
was. The run principal's bound is unchanged: the worker applies it to the nested agent's turn on its
own `principal_bound` switch.

**What to do.** Nothing, unless you turn the switch on. The same advice applies as for the worker side:
turn it on only after the migration of existing compositions (story S6) has created their pending
grants and the owners have reviewed them, because until a grant is approved a nested agent that holds
more than the desktop's agent is refused. Upgrade the main service with the desktop app: an older
desktop app ignores the new field and the lane shows no notice, but the refusal still reaches the model
as the typed error `delegation_refused`.
