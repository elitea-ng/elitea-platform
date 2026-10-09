//! `generate_labels` and `generate_summaries`: a model names and describes
//! each community, from its centroids, their relationships and the other
//! architectural members.

use super::is_architectural;
use crate::extract::{Answer, Tuning};
use crate::graph::Graph;
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pystr;
use elitea_engine_core::pyvalue::{py_str, py_truthy};
use elitea_engine_core::stream::StopSignal;
use serde_json::{Map, Value};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;

/// `max_tokens` of `generate_summaries`, as ingestion called it.
const SUMMARY_MAX_TOKENS: usize = 200;
/// A label longer than this (characters) is cut.
const LABEL_MAX_CHARS: usize = 80;
/// Trailing characters stripped from a label.
const LABEL_TRAILING: &str = ".,;:!?'\"\u{2026}\u{bb}\u{ab})]}";

/// The label prompt's fixed tail (`_build_label_prompt`).
const LABEL_INSTRUCTIONS: &str = "Reply with ONLY the label — no quotes, no explanation, no punctuation. \
Examples of good labels:\n\
- Authentication & Session Management\n\
- REST API Request Handling\n\
- Database Connection Pooling\n\
- UI Component Rendering Pipeline\n\
- Test Infrastructure & Fixtures\n";

/// Each node's outgoing and incoming edges `(other end, relation type)`, in
/// edge export order.
#[derive(Default)]
struct Incidence<'g> {
    outgoing: HashMap<&'g str, Vec<(&'g str, String)>>,
    incoming: HashMap<&'g str, Vec<(&'g str, String)>>,
}

impl<'g> Incidence<'g> {
    fn new(graph: &'g Graph) -> Self {
        let mut incidence = Self::default();
        for (source, target, edge) in graph.edges() {
            // `data.get("relation_type", "related_to")`
            let relation = edge
                .get("relation_type")
                .map_or_else(|| "related_to".to_owned(), py_str);
            incidence
                .outgoing
                .entry(source)
                .or_default()
                .push((target, relation.clone()));
            incidence
                .incoming
                .entry(target)
                .or_default()
                .push((source, relation));
        }
        incidence
    }
}

/// `nx_graph.nodes.get(id, {}).get("name", id)`.
fn name_of(graph: &Graph, id: &str) -> String {
    graph
        .node(id)
        .and_then(|node| node.get("name"))
        .map_or_else(|| id.to_owned(), py_str)
}

/// One community as the prompts read it.
struct View<'c> {
    /// `(id, name, type)` per centroid, in centroid order.
    centroids: Vec<(String, String, String)>,
    members: Vec<&'c str>,
    label: String,
}

impl<'c> View<'c> {
    fn new(community: &'c Value) -> Self {
        let field = |value: &Value, key: &str| value.get(key).map(py_str).unwrap_or_default();
        let centroids = community
            .get("centroids")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .map(|centroid| {
                (
                    field(centroid, "id"),
                    field(centroid, "name"),
                    field(centroid, "type"),
                )
            })
            .collect();
        let members = community
            .get("members")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
            .collect();
        let label = community
            .get("label")
            .map_or_else(|| "Unknown".to_owned(), py_str);
        Self {
            centroids,
            members,
            label,
        }
    }

    /// `- name (type)[: signature][ — docstring cut at doc_limit chars...]`.
    fn centroid_lines(&self, graph: &Graph, doc_limit: usize) -> Vec<String> {
        let empty = Map::new();
        self.centroids
            .iter()
            .map(|(id, name, entity_type)| {
                let node = graph.node(id).unwrap_or(&empty);
                let mut line = format!("- {name} ({entity_type})");
                if let Some(signature) = node.get("signature").filter(|value| py_truthy(value)) {
                    line.push_str(": ");
                    line.push_str(&py_str(signature));
                }
                // `node.get("docstring", "") or node.get("description", "")`
                let doc = node
                    .get("docstring")
                    .filter(|value| py_truthy(value))
                    .or_else(|| node.get("description"))
                    .filter(|value| py_truthy(value))
                    .map(py_str);
                if let Some(doc) = doc {
                    let mut short = pystr::strip(pystr::prefix_chars(&doc, doc_limit)).to_owned();
                    if doc.chars().count() > doc_limit {
                        short.push_str("...");
                    }
                    line.push_str(" — ");
                    line.push_str(&short);
                }
                line
            })
            .collect()
    }

    /// The centroids' outgoing then incoming relationships, deduplicated,
    /// at most `limit`.
    fn relation_lines(
        &self,
        graph: &Graph,
        incidence: &Incidence<'_>,
        limit: usize,
    ) -> Vec<String> {
        let mut seen = HashSet::new();
        let mut lines = Vec::new();
        let mut walked = HashSet::new();
        for (id, _, _) in &self.centroids {
            if !walked.insert(id.as_str()) {
                continue;
            }
            let own = name_of(graph, id);
            let outgoing = incidence.outgoing.get(id.as_str()).into_iter().flatten();
            let incoming = incidence.incoming.get(id.as_str()).into_iter().flatten();
            let candidates = outgoing
                .map(|(target, relation)| {
                    format!("- {own} --[{relation}]--> {}", name_of(graph, target))
                })
                .chain(incoming.map(|(source, relation)| {
                    format!("- {} --[{relation}]--> {own}", name_of(graph, source))
                }));
            for line in candidates {
                if seen.insert(line.clone()) {
                    lines.push(line);
                }
            }
        }
        lines.truncate(limit);
        lines
    }

    /// The members that are not centroids, and of those the architectural.
    fn others(&self, graph: &Graph) -> (usize, Vec<&'c str>) {
        let centroid_ids: HashSet<&str> = self.centroids.iter().map(|c| c.0.as_str()).collect();
        let others: Vec<&str> = self
            .members
            .iter()
            .copied()
            .filter(|member| !centroid_ids.contains(member))
            .collect();
        let architectural = others
            .iter()
            .copied()
            .filter(|member| is_architectural(graph.node(member)))
            .collect();
        (others.len(), architectural)
    }
}

/// `- name (type)` of a member.
fn roster_line(graph: &Graph, id: &str) -> String {
    let entity_type = super::type_of(graph.node(id));
    format!("- {} ({entity_type})", name_of(graph, id))
}

/// Lines joined, or `fallback` when there are none.
fn block(lines: &[String], fallback: &str) -> String {
    if lines.is_empty() {
        fallback.to_owned()
    } else {
        lines.join("\n")
    }
}

/// `_build_label_prompt`.
fn label_prompt(graph: &Graph, incidence: &Incidence<'_>, community: &Value) -> String {
    let view = View::new(community);
    let centroid_lines = view.centroid_lines(graph, 120);
    let relation_lines = view.relation_lines(graph, incidence, 10);
    let (_, architectural) = view.others(graph);
    let mut roster: Vec<String> = architectural
        .iter()
        .take(15)
        .map(|id| roster_line(graph, id))
        .collect();
    if architectural.len() > 15 {
        roster.push(format!("- ... and {} more", architectural.len() - 15));
    }
    format!(
        "Given this code community, generate a short, descriptive label \
         (3-7 words) that captures its primary capability or responsibility.\n\n\
         ## Key Entities (Centroids)\n{}\n\n## Key Relationships\n{}\n\n\
         ## Other Architectural Members\n{}\n\nCurrent heuristic label: \"{}\"\n\n{LABEL_INSTRUCTIONS}",
        block(&centroid_lines, "- None"),
        block(&relation_lines, "- None"),
        block(&roster, "- None"),
        view.label,
    )
}

/// `_build_summary_prompt` (`max_tokens=200`).
fn summary_prompt(graph: &Graph, incidence: &Incidence<'_>, community: &Value) -> String {
    let view = View::new(community);
    let centroid_lines = view.centroid_lines(graph, 150);
    let relation_lines = view.relation_lines(graph, incidence, 15);
    let (other_count, architectural) = view.others(graph);
    let noise = other_count - architectural.len();
    let mut roster: Vec<String> = architectural
        .iter()
        .take(20)
        .map(|id| roster_line(graph, id))
        .collect();
    if architectural.len() > 20 {
        roster.push(format!(
            "- ... and {} more architectural members",
            architectural.len() - 20
        ));
    }
    if noise > 0 {
        roster.push(format!("- ({noise} implementation-level members omitted)"));
    }
    format!(
        "Summarize this code community labeled '{}' in 5-8 sentences \
         (~{SUMMARY_MAX_TOKENS} tokens max).\n\n## Key Entities (Centroids)\n{}\n\n\
         ## Key Relationships\n{}\n\n## Other Members\n{}\n\n\
         Describe the community's purpose, main responsibilities, \
         and how the centroids relate to each other.",
        view.label,
        centroid_lines.join("\n"),
        block(&relation_lines, "- None captured"),
        block(&roster, "- None"),
    )
}

/// `generate_labels`' clean-up of an answer: trimmed, unquoted, whitespace
/// collapsed, cut to 80 characters, trailing punctuation dropped.
fn clean_label(raw: &str) -> String {
    let label = pystr::strip(pystr::strip_chars(pystr::strip(raw), "\"'"));
    let label = pystr::split_whitespace(label).collect::<Vec<_>>().join(" ");
    let label = pystr::prefix_chars(&label, LABEL_MAX_CHARS);
    label
        .trim_end_matches(|c| LABEL_TRAILING.contains(c))
        .to_owned()
}

/// Ask `ask` every prompt, at most `workers` at once; per prompt its
/// answer, or `None` for a failed call. A stop (or a cancelled call) is
/// [`EngineError::cancelled`].
async fn ask_all<F>(
    ask: &F,
    prompts: Vec<String>,
    workers: usize,
    stop: &StopSignal,
    stage: &str,
) -> Result<Vec<Option<String>>, EngineError>
where
    F: Fn(String) -> Answer,
{
    if stop.is_requested() {
        return Err(EngineError::cancelled());
    }
    let semaphore = Arc::new(tokio::sync::Semaphore::new(workers.max(1)));
    let mut calls = tokio::task::JoinSet::new();
    for (position, prompt) in prompts.iter().enumerate() {
        let semaphore = Arc::clone(&semaphore);
        let answer = ask(prompt.clone());
        calls.spawn(async move {
            let _permit = semaphore.acquire_owned().await;
            (position, answer.await)
        });
    }
    let mut answers = vec![None; prompts.len()];
    loop {
        let joined = tokio::select! {
            joined = calls.join_next() => joined,
            () = stop.stopped() => return Err(EngineError::cancelled()),
        };
        let Some(joined) = joined else {
            break;
        };
        match joined {
            Ok((position, Ok(text))) => answers[position] = Some(text),
            Ok((_, Err(error))) if error == EngineError::cancelled() => return Err(error),
            Ok((position, Err(error))) => {
                tracing::warn!(stage, position, error = %error.message, "community model call failed");
            }
            Err(error) => {
                tracing::warn!(stage, %error, "community model call panicked");
            }
        }
    }
    Ok(answers)
}

/// [`label_and_summarize_with`] over a [`crate::extract::Model`].
///
/// # Errors
///
/// See [`label_and_summarize_with`].
pub async fn label_and_summarize(
    model: &crate::extract::Model,
    community_data: &mut Value,
    graph: &Graph,
    tuning: Tuning,
    stop: &StopSignal,
) -> Result<(usize, usize), EngineError> {
    label_and_summarize_with(
        |prompt| {
            let model = model.clone();
            Box::pin(async move { model.ask(prompt).await })
        },
        community_data,
        graph,
        tuning,
        stop,
    )
    .await
}

/// `generate_labels` then `generate_summaries` over `community_data` (as
/// [`detect`](super::detect) produced it), asking `ask` — a
/// [`Model`](crate::extract::Model)'s call — one prompt per community,
/// `min(tuning.parallel_files, communities)` at once (ingestion's
/// `max_parallel_extractions`). Each call is made once; `tuning`'s
/// attempts are not used, as Python's `llm_callable` was not retried here.
///
/// A label answer replaces the heuristic label when it is not empty after
/// clean-up; a failed call keeps it. Summaries are asked after every label
/// is in, so their prompts carry the new labels; a summary is the trimmed
/// answer, `null` for a failed call. Returns `(labels set, summaries set)`.
///
/// # Errors
///
/// [`EngineError::cancelled`] once `stop` is requested (or a call reports
/// it); `community_data` then holds what was set before.
pub async fn label_and_summarize_with<F>(
    ask: F,
    community_data: &mut Value,
    graph: &Graph,
    tuning: Tuning,
    stop: &StopSignal,
) -> Result<(usize, usize), EngineError>
where
    F: Fn(String) -> Answer,
{
    let Some(communities) = community_data
        .get_mut("communities")
        .and_then(Value::as_object_mut)
        .filter(|communities| !communities.is_empty())
    else {
        return Ok((0, 0));
    };
    let incidence = Incidence::new(graph);
    let workers = tuning.parallel_files.min(communities.len());

    let prompts = communities
        .values()
        .map(|community| label_prompt(graph, &incidence, community))
        .collect();
    let answers = ask_all(&ask, prompts, workers, stop, "label").await?;
    let mut labels = 0;
    for (community, answer) in communities.values_mut().zip(answers) {
        let Some(label) = answer.map(|raw| clean_label(&raw)) else {
            continue;
        };
        if label.is_empty() {
            continue;
        }
        if let Some(fields) = community.as_object_mut() {
            fields.insert("label".to_owned(), Value::String(label));
            labels += 1;
        }
    }

    let prompts = communities
        .values()
        .map(|community| summary_prompt(graph, &incidence, community))
        .collect();
    let answers = ask_all(&ask, prompts, workers, stop, "summary").await?;
    let mut summaries = 0;
    for (community, answer) in communities.values_mut().zip(answers) {
        let summary = answer.map(|text| pystr::strip(&text).to_owned());
        if summary.is_some() {
            summaries += 1;
        }
        if let Some(fields) = community.as_object_mut() {
            fields.insert(
                "summary".to_owned(),
                summary.map_or(Value::Null, Value::String),
            );
        }
    }
    Ok((labels, summaries))
}

#[cfg(test)]
pub(super) fn clean_label_for_tests(raw: &str) -> String {
    clean_label(raw)
}
