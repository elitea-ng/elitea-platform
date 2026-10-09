//! Bounded, deterministic selection for large `OpenAPI` responses.

use std::collections::{HashMap, HashSet};

use serde_json::{Map, Value, json};

const DEFAULT_RESPONSE_LIMIT: usize = 50;
const MAX_RESPONSE_LIMIT: usize = 200;
const MAX_RESPONSE_SEARCH_BYTES: usize = 4 * 1024;
const MAX_SERIALIZED_RESPONSE_CHARS: usize = 50_000;
const MAX_COLLECTION_DEPTH: usize = 4;
const MAX_DISCOVERED_COLLECTIONS: usize = 100;
const MAX_REPORTED_CANDIDATES: usize = 20;
const COMMON_COLLECTION_KEYS: [&str; 5] = ["items", "results", "value", "data", "records"];
const BM25_K1: f64 = 1.5;
const BM25_B: f64 = 0.75;
const PHRASE_SCORE_MULTIPLIER: f64 = 1.5;

#[derive(Clone, Debug, Default)]
struct SearchQuery {
    terms: Vec<String>,
    phrases: Vec<Vec<String>>,
    negative_terms: Vec<String>,
    negative_phrases: Vec<Vec<String>>,
}

impl SearchQuery {
    fn enabled(&self) -> bool {
        !self.terms.is_empty()
            || !self.phrases.is_empty()
            || !self.negative_terms.is_empty()
            || !self.negative_phrases.is_empty()
    }

    fn scoring_terms(&self) -> Vec<String> {
        let mut terms = self.terms.clone();
        for phrase in &self.phrases {
            for term in phrase {
                push_unique(&mut terms, term.clone());
            }
        }
        terms
    }
}

pub(super) struct ResponseSelection {
    query: SearchQuery,
    limit: usize,
}

impl ResponseSelection {
    pub(super) fn parse(arguments: &Map<String, Value>) -> Result<Option<Self>, ()> {
        let search = match arguments.get("response_search") {
            None | Some(Value::Null) => None,
            Some(Value::String(value))
                if value.len() <= MAX_RESPONSE_SEARCH_BYTES && !value.contains('\0') =>
            {
                let value = value.trim();
                (!value.is_empty()).then_some(value)
            }
            Some(_) => return Err(()),
        };
        let limit = match arguments.get("response_limit") {
            None | Some(Value::Null) => None,
            Some(Value::Number(value)) => value
                .as_u64()
                .and_then(|value| usize::try_from(value).ok())
                .filter(|value| (1..=MAX_RESPONSE_LIMIT).contains(value)),
            Some(_) => return Err(()),
        };
        if arguments.contains_key("response_limit")
            && !matches!(arguments.get("response_limit"), None | Some(Value::Null))
            && limit.is_none()
        {
            return Err(());
        }
        if search.is_none() && limit.is_none() {
            return Ok(None);
        }
        let query = search.map_or_else(|| Ok(SearchQuery::default()), parse_search)?;
        Ok(Some(Self {
            query,
            limit: limit.unwrap_or(DEFAULT_RESPONSE_LIMIT),
        }))
    }

    pub(super) fn apply(&self, content: &str, preferred_paths: &[Vec<String>]) -> String {
        serde_json::from_str(content).map_or_else(
            |_| self.select_text(content),
            |parsed| self.select_json(&parsed, preferred_paths),
        )
    }

    fn select_json(&self, parsed: &Value, preferred_paths: &[Vec<String>]) -> String {
        let candidates = discover_candidates(parsed, preferred_paths);
        let Some(ranked) = choose_candidate(parsed, &candidates, preferred_paths, &self.query)
        else {
            let mut candidate_paths = candidates
                .iter()
                .map(|candidate| format_path(&candidate.path))
                .collect::<Vec<_>>();
            candidate_paths.sort();
            let candidate_count = candidate_paths.len();
            candidate_paths.truncate(MAX_REPORTED_CANDIDATES);
            let has_candidates = candidate_count > 0;
            return serialize(&json!({
                "_elitea_response_selection": {
                    "format":"json",
                    "collection_path":Value::Null,
                    "collection_kind":Value::Null,
                    "total_items":Value::Null,
                    "matched_items":Value::Null,
                    "returned_items":0,
                    "truncated":false,
                    "ranking":if self.query.enabled() { "bm25" } else { "none" },
                    "result_order":Value::Null,
                    "status":if has_candidates { "ambiguous_collection" } else { "collection_not_found" },
                    "candidate_paths":candidate_paths,
                    "candidate_count":candidate_count,
                    "message":if has_candidates { "Multiple response collections are equally plausible." } else { "No response collection was found." }
                },
                "data":Value::Null
            }));
        };
        bounded_json_result(
            parsed,
            &ranked.candidate,
            &ranked.indices,
            self.limit,
            self.query.enabled(),
        )
    }

    fn select_text(&self, content: &str) -> String {
        let segments = text_segments(content);
        let values = segments
            .iter()
            .map(|value| Value::String(value.clone()))
            .collect::<Vec<_>>();
        let (indices, _) = rank_values(&values, &self.query);
        let matches = indices
            .iter()
            .map(|index| segments[*index].clone())
            .collect::<Vec<_>>();
        bounded_text_result(&segments, &matches, self.limit, self.query.enabled())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum CollectionKind {
    Array,
    ObjectMap,
}

impl CollectionKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Array => "array",
            Self::ObjectMap => "object_map",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Candidate {
    path: Vec<String>,
    kind: CollectionKind,
    size: usize,
}

impl Candidate {
    fn collection<'a>(&self, value: &'a Value) -> Option<&'a Value> {
        value_at_path(value, &self.path)
    }

    fn search_documents(&self, value: &Value) -> Vec<Value> {
        match (self.kind, self.collection(value)) {
            (CollectionKind::Array, Some(Value::Array(items))) => items.clone(),
            (CollectionKind::ObjectMap, Some(Value::Object(items))) => items
                .iter()
                .map(|(key, value)| json!([key, value]))
                .collect(),
            _ => Vec::new(),
        }
    }

    fn select(&self, value: &Value, indices: &[usize]) -> Value {
        match (self.kind, self.collection(value)) {
            (CollectionKind::Array, Some(Value::Array(items))) => Value::Array(
                indices
                    .iter()
                    .filter_map(|index| items.get(*index).cloned())
                    .collect(),
            ),
            (CollectionKind::ObjectMap, Some(Value::Object(items))) => {
                let entries = items.iter().collect::<Vec<_>>();
                Value::Object(
                    indices
                        .iter()
                        .filter_map(|index| entries.get(*index))
                        .map(|(key, value)| ((*key).clone(), (*value).clone()))
                        .collect(),
                )
            }
            _ => Value::Null,
        }
    }

    fn empty(&self) -> Value {
        match self.kind {
            CollectionKind::Array => Value::Array(Vec::new()),
            CollectionKind::ObjectMap => Value::Object(Map::new()),
        }
    }
}

struct RankedCandidate {
    candidate: Candidate,
    indices: Vec<usize>,
    top_score: f64,
}

fn parse_search(input: &str) -> Result<SearchQuery, ()> {
    let characters = input.char_indices().collect::<Vec<_>>();
    let mut cursor = 0usize;
    let mut query = SearchQuery::default();
    while cursor < characters.len() {
        while cursor < characters.len() && characters[cursor].1.is_whitespace() {
            cursor += 1;
        }
        if cursor == characters.len() {
            break;
        }
        let negative = characters[cursor].1 == '-';
        if negative {
            cursor += 1;
            if cursor == characters.len() || characters[cursor].1.is_whitespace() {
                return Err(());
            }
        }
        let phrase = characters[cursor].1 == '"';
        let mut raw = String::new();
        if phrase {
            cursor += 1;
            let mut closed = false;
            while cursor < characters.len() {
                let character = characters[cursor].1;
                cursor += 1;
                if character == '"' {
                    closed = true;
                    break;
                }
                if character == '\\' {
                    if cursor == characters.len() {
                        return Err(());
                    }
                    let escaped = characters[cursor].1;
                    if !matches!(escaped, '"' | '\\') {
                        raw.push('\\');
                    }
                    raw.push(escaped);
                    cursor += 1;
                } else {
                    raw.push(character);
                }
            }
            if !closed || (cursor < characters.len() && !characters[cursor].1.is_whitespace()) {
                return Err(());
            }
        } else {
            while cursor < characters.len() && !characters[cursor].1.is_whitespace() {
                if characters[cursor].1 == '"' {
                    return Err(());
                }
                raw.push(characters[cursor].1);
                cursor += 1;
            }
        }
        let tokens = tokenize(&raw);
        if tokens.is_empty() {
            return Err(());
        }
        if phrase {
            push_unique(
                if negative {
                    &mut query.negative_phrases
                } else {
                    &mut query.phrases
                },
                tokens,
            );
        } else {
            for token in tokens {
                push_unique(
                    if negative {
                        &mut query.negative_terms
                    } else {
                        &mut query.terms
                    },
                    token,
                );
            }
        }
    }
    query.enabled().then_some(query).ok_or(())
}

fn tokenize(value: &str) -> Vec<String> {
    let mut tokens = Vec::new();
    let mut current = String::new();
    for character in value.chars() {
        if character.is_alphanumeric()
            || ((character == '\'' || character == '’') && !current.is_empty())
        {
            current.extend(character.to_lowercase());
        } else if !current.is_empty() {
            while current.ends_with(['\'', '’']) {
                current.pop();
            }
            if !current.is_empty() {
                tokens.push(std::mem::take(&mut current));
            }
        }
    }
    while current.ends_with(['\'', '’']) {
        current.pop();
    }
    if !current.is_empty() {
        tokens.push(current);
    }
    tokens
}

fn value_token_data(value: &Value) -> (Vec<String>, Vec<Vec<String>>) {
    fn visit(value: &Value, tokens: &mut Vec<String>, sequences: &mut Vec<Vec<String>>) {
        match value {
            Value::Object(values) => {
                for (key, value) in values {
                    let key_tokens = tokenize(key);
                    tokens.extend(key_tokens.clone());
                    if !key_tokens.is_empty() {
                        sequences.push(key_tokens);
                    }
                    visit(value, tokens, sequences);
                }
            }
            Value::Array(values) => {
                for value in values {
                    visit(value, tokens, sequences);
                }
            }
            Value::String(value) => push_scalar_tokens(value, tokens, sequences),
            Value::Bool(value) => push_scalar_tokens(&value.to_string(), tokens, sequences),
            Value::Number(value) => push_scalar_tokens(&value.to_string(), tokens, sequences),
            Value::Null => {}
        }
    }
    let mut tokens = Vec::new();
    let mut sequences = Vec::new();
    visit(value, &mut tokens, &mut sequences);
    (tokens, sequences)
}

fn push_scalar_tokens(value: &str, tokens: &mut Vec<String>, sequences: &mut Vec<Vec<String>>) {
    let value = tokenize(value);
    tokens.extend(value.clone());
    if !value.is_empty() {
        sequences.push(value);
    }
}

fn contains_phrase(sequence: &[String], phrase: &[String]) -> bool {
    !phrase.is_empty()
        && phrase.len() <= sequence.len()
        && sequence
            .windows(phrase.len())
            .any(|window| window == phrase)
}

fn sequences_contain_phrase(sequences: &[Vec<String>], phrase: &[String]) -> bool {
    sequences
        .iter()
        .any(|sequence| contains_phrase(sequence, phrase))
}

fn rank_values(values: &[Value], query: &SearchQuery) -> (Vec<usize>, f64) {
    if !query.enabled() {
        return ((0..values.len()).collect(), 0.0);
    }
    let scoring_terms = query.scoring_terms();
    let scoring_term_set = scoring_terms.iter().cloned().collect::<HashSet<_>>();
    let mut document_frequencies = HashMap::<String, usize>::new();
    let mut documents = Vec::new();
    for (source_index, value) in values.iter().enumerate() {
        let (tokens, sequences) = value_token_data(value);
        let mut frequencies = HashMap::<String, usize>::new();
        for token in &tokens {
            if scoring_term_set.contains(token) {
                *frequencies.entry(token.clone()).or_default() += 1;
            }
        }
        for term in frequencies.keys() {
            *document_frequencies.entry(term.clone()).or_default() += 1;
        }
        let token_set = tokens.iter().cloned().collect::<HashSet<_>>();
        let excluded = query
            .negative_terms
            .iter()
            .any(|term| token_set.contains(term))
            || query
                .negative_phrases
                .iter()
                .any(|phrase| sequences_contain_phrase(&sequences, phrase));
        let required_phrases_match = query
            .phrases
            .iter()
            .all(|phrase| sequences_contain_phrase(&sequences, phrase));
        let has_scoring_match =
            scoring_terms.is_empty() || frequencies.values().any(|count| *count > 0);
        documents.push((
            source_index,
            tokens.len().max(1),
            frequencies,
            !excluded && required_phrases_match && has_scoring_match,
        ));
    }
    if documents.is_empty() {
        return (Vec::new(), 0.0);
    }
    let document_count = bounded_f64(documents.len());
    let total_length = documents
        .iter()
        .map(|(_, length, _, _)| *length)
        .sum::<usize>();
    let average_length = bounded_f64(total_length) / document_count;
    let inverse_document_frequency = scoring_terms
        .iter()
        .map(|term| {
            let frequency = bounded_f64(*document_frequencies.get(term).unwrap_or(&0));
            (
                term.clone(),
                (1.0 + (document_count - frequency + 0.5) / (frequency + 0.5)).ln(),
            )
        })
        .collect::<HashMap<_, _>>();
    let phrase_boost = query
        .phrases
        .iter()
        .map(|phrase| {
            PHRASE_SCORE_MULTIPLIER
                * phrase
                    .iter()
                    .map(|term| *inverse_document_frequency.get(term).unwrap_or(&0.0))
                    .sum::<f64>()
        })
        .sum::<f64>();
    let mut ranked = Vec::new();
    for (source_index, length, frequencies, eligible) in documents {
        if !eligible {
            continue;
        }
        let normalization =
            BM25_K1 * (1.0 - BM25_B + BM25_B * bounded_f64(length) / average_length);
        let mut score = 0.0;
        for term in &scoring_terms {
            let frequency = bounded_f64(*frequencies.get(term).unwrap_or(&0));
            if frequency > 0.0 {
                score += inverse_document_frequency[term]
                    * (frequency * (BM25_K1 + 1.0) / (frequency + normalization));
            }
        }
        score += phrase_boost;
        ranked.push((score, source_index));
    }
    ranked.sort_by(|(left_score, left_index), (right_score, right_index)| {
        right_score
            .total_cmp(left_score)
            .then_with(|| left_index.cmp(right_index))
    });
    let top_score = ranked.first().map_or(0.0, |(score, _)| *score);
    (
        ranked.into_iter().map(|(_, index)| index).collect(),
        top_score,
    )
}

fn discover_candidates(value: &Value, preferred_paths: &[Vec<String>]) -> Vec<Candidate> {
    if let Value::Array(values) = value {
        return vec![Candidate {
            path: Vec::new(),
            kind: CollectionKind::Array,
            size: values.len(),
        }];
    }
    let mut candidates = Vec::new();
    visit_candidates(value, &mut Vec::new(), 0, preferred_paths, &mut candidates);
    candidates
}

fn visit_candidates(
    node: &Value,
    path: &mut Vec<String>,
    depth: usize,
    preferred_paths: &[Vec<String>],
    candidates: &mut Vec<Candidate>,
) {
    if depth > MAX_COLLECTION_DEPTH || candidates.len() >= MAX_DISCOVERED_COLLECTIONS {
        return;
    }
    let Value::Object(values) = node else {
        return;
    };
    let has_schema_descendant = preferred_paths
        .iter()
        .any(|preferred| preferred.len() > path.len() && preferred.starts_with(path.as_slice()));
    if preferred_paths.iter().any(|preferred| preferred == path)
        || (!has_schema_descendant && is_runtime_object_map(values))
    {
        candidates.push(Candidate {
            path: path.clone(),
            kind: CollectionKind::ObjectMap,
            size: values.len(),
        });
        return;
    }
    for (key, child) in values {
        path.push(key.clone());
        match child {
            Value::Array(items) => candidates.push(Candidate {
                path: path.clone(),
                kind: CollectionKind::Array,
                size: items.len(),
            }),
            Value::Object(_) => {
                visit_candidates(child, path, depth + 1, preferred_paths, candidates);
            }
            _ => {}
        }
        path.pop();
        if candidates.len() >= MAX_DISCOVERED_COLLECTIONS {
            break;
        }
    }
}

fn is_runtime_object_map(values: &Map<String, Value>) -> bool {
    if values.len() < 3
        || values
            .values()
            .any(|value| value.as_object().is_none_or(Map::is_empty))
    {
        return false;
    }
    let mut field_counts = HashMap::<&str, usize>::new();
    for value in values.values().filter_map(Value::as_object) {
        for field in value.keys() {
            *field_counts.entry(field.as_str()).or_default() += 1;
        }
    }
    field_counts
        .values()
        .max()
        .is_some_and(|count| count.saturating_mul(10) >= values.len().saturating_mul(7))
}

fn choose_candidate(
    root: &Value,
    candidates: &[Candidate],
    preferred_paths: &[Vec<String>],
    query: &SearchQuery,
) -> Option<RankedCandidate> {
    if candidates.is_empty() {
        return None;
    }
    let preferred = preferred_paths
        .iter()
        .filter_map(|path| candidates.iter().find(|candidate| candidate.path == *path))
        .cloned()
        .collect::<Vec<_>>();
    let eligible = if preferred.is_empty() {
        candidates.to_vec()
    } else {
        preferred
    };
    let mut ranked = eligible
        .iter()
        .map(|candidate| {
            let (indices, top_score) = rank_values(&candidate.search_documents(root), query);
            RankedCandidate {
                candidate: candidate.clone(),
                indices,
                top_score,
            }
        })
        .collect::<Vec<_>>();
    if eligible.len() == 1 {
        return ranked.pop();
    }
    if !query.scoring_terms().is_empty() {
        let mut matching = ranked
            .iter()
            .filter(|candidate| !candidate.indices.is_empty())
            .collect::<Vec<_>>();
        if matching.len() == 1 {
            return matching.pop().map(clone_ranked);
        }
        matching.sort_by(|left, right| right.top_score.total_cmp(&left.top_score));
        if matching.len() > 1 && !approximately_equal(matching[0].top_score, matching[1].top_score)
        {
            return Some(clone_ranked(matching[0]));
        }
    }
    let mut common = eligible
        .iter()
        .filter_map(|candidate| {
            candidate.path.last().and_then(|key| {
                COMMON_COLLECTION_KEYS
                    .iter()
                    .position(|common| key.eq_ignore_ascii_case(common))
                    .map(|priority| (priority, candidate))
            })
        })
        .collect::<Vec<_>>();
    common.sort_by(|(left_priority, left), (right_priority, right)| {
        left_priority
            .cmp(right_priority)
            .then_with(|| left.path.len().cmp(&right.path.len()))
            .then_with(|| left.path.cmp(&right.path))
    });
    if let Some((priority, best)) = common.first() {
        let ties = common
            .iter()
            .filter(|(other_priority, other)| {
                other_priority == priority && other.path.len() == best.path.len()
            })
            .count();
        if ties == 1 {
            return ranked
                .into_iter()
                .find(|item| item.candidate.path == best.path);
        }
    }
    let mut by_size = eligible.iter().collect::<Vec<_>>();
    by_size.sort_by_key(|candidate| std::cmp::Reverse(candidate.size));
    if by_size.len() == 1 || by_size[0].size > by_size[1].size.saturating_mul(2) {
        return ranked
            .into_iter()
            .find(|item| item.candidate.path == by_size[0].path);
    }
    None
}

fn clone_ranked(value: &RankedCandidate) -> RankedCandidate {
    RankedCandidate {
        candidate: value.candidate.clone(),
        indices: value.indices.clone(),
        top_score: value.top_score,
    }
}

fn approximately_equal(left: f64, right: f64) -> bool {
    (left - right).abs() <= 1e-12_f64.max(1e-9 * left.abs().max(right.abs()))
}

fn bounded_json_result(
    original: &Value,
    candidate: &Candidate,
    matching_indices: &[usize],
    limit: usize,
    query_enabled: bool,
) -> String {
    for returned in (1..=matching_indices.len().min(limit)).rev() {
        let selected = candidate.select(original, &matching_indices[..returned]);
        let data = replace_collection(original, &candidate.path, selected);
        let output = selection_output(
            "json",
            &format_path(&candidate.path),
            candidate.kind.as_str(),
            candidate.size,
            matching_indices.len(),
            returned,
            returned < matching_indices.len(),
            query_enabled,
            &data,
        );
        if serialized_chars(&output) <= MAX_SERIALIZED_RESPONSE_CHARS {
            return output;
        }
    }
    let empty = selection_output(
        "json",
        &format_path(&candidate.path),
        candidate.kind.as_str(),
        candidate.size,
        matching_indices.len(),
        0,
        false,
        query_enabled,
        &replace_collection(original, &candidate.path, candidate.empty()),
    );
    if matching_indices.is_empty() && serialized_chars(&empty) <= MAX_SERIALIZED_RESPONSE_CHARS {
        return empty;
    }
    content_too_large(
        "json",
        &format_path(&candidate.path),
        candidate.kind.as_str(),
        candidate.size,
        matching_indices.len(),
        query_enabled,
    )
}

fn bounded_text_result(
    segments: &[String],
    matches: &[String],
    limit: usize,
    query_enabled: bool,
) -> String {
    for returned in (1..=matches.len().min(limit)).rev() {
        let output = selection_output(
            "text",
            "$segments",
            "segments",
            segments.len(),
            matches.len(),
            returned,
            returned < matches.len(),
            query_enabled,
            &Value::String(matches[..returned].join("\n")),
        );
        if serialized_chars(&output) <= MAX_SERIALIZED_RESPONSE_CHARS {
            return output;
        }
    }
    let empty = selection_output(
        "text",
        "$segments",
        "segments",
        segments.len(),
        matches.len(),
        0,
        false,
        query_enabled,
        &Value::String(String::new()),
    );
    if matches.is_empty() && serialized_chars(&empty) <= MAX_SERIALIZED_RESPONSE_CHARS {
        return empty;
    }
    content_too_large(
        "text",
        "$segments",
        "segments",
        segments.len(),
        matches.len(),
        query_enabled,
    )
}

#[allow(clippy::too_many_arguments)]
fn selection_output(
    format: &str,
    path: &str,
    kind: &str,
    total: usize,
    matched: usize,
    returned: usize,
    truncated: bool,
    query_enabled: bool,
    data: &Value,
) -> String {
    serialize(&json!({
        "_elitea_response_selection": {
            "format":format,
            "collection_path":path,
            "collection_kind":kind,
            "total_items":total,
            "matched_items":matched,
            "returned_items":returned,
            "truncated":truncated,
            "ranking":if query_enabled { "bm25" } else { "none" },
            "result_order":if query_enabled { "relevance" } else { "source" }
        },
        "data":data
    }))
}

fn content_too_large(
    format: &str,
    path: &str,
    kind: &str,
    total: usize,
    matched: usize,
    query_enabled: bool,
) -> String {
    serialize(&json!({
        "_elitea_response_selection": {
            "format":format,
            "collection_path":path,
            "collection_kind":kind,
            "total_items":total,
            "matched_items":matched,
            "returned_items":0,
            "truncated":matched > 0,
            "ranking":if query_enabled { "bm25" } else { "none" },
            "result_order":if query_enabled { "relevance" } else { "source" },
            "status":"content_too_large",
            "message":"A selected item or response metadata exceeds the safe serialized response size.",
            "max_serialized_chars":MAX_SERIALIZED_RESPONSE_CHARS
        },
        "data":Value::Null
    }))
}

fn value_at_path<'a>(mut value: &'a Value, path: &[String]) -> Option<&'a Value> {
    for part in path {
        value = value.as_object()?.get(part)?;
    }
    Some(value)
}

fn replace_collection(original: &Value, path: &[String], replacement: Value) -> Value {
    if path.is_empty() {
        return replacement;
    }
    let mut result = original.clone();
    let mut current = &mut result;
    for part in &path[..path.len() - 1] {
        let Some(next) = current
            .as_object_mut()
            .and_then(|value| value.get_mut(part))
        else {
            return Value::Null;
        };
        current = next;
    }
    let Some(object) = current.as_object_mut() else {
        return Value::Null;
    };
    object.insert(path[path.len() - 1].clone(), replacement);
    result
}

fn format_path(path: &[String]) -> String {
    let mut result = "$".to_owned();
    for part in path {
        if valid_identifier(part) {
            result.push('.');
            result.push_str(part);
        } else {
            result.push('[');
            result.push_str(&serialize(&Value::String(part.clone())));
            result.push(']');
        }
    }
    result
}

fn valid_identifier(value: &str) -> bool {
    value
        .as_bytes()
        .first()
        .is_some_and(|byte| byte.is_ascii_alphabetic() || *byte == b'_')
        && value
            .as_bytes()
            .iter()
            .all(|byte| byte.is_ascii_alphanumeric() || *byte == b'_')
}

fn text_segments(content: &str) -> Vec<String> {
    let mut paragraphs = Vec::new();
    let mut current = Vec::new();
    for line in content.lines() {
        if line.trim().is_empty() {
            if !current.is_empty() {
                paragraphs.push(current.join("\n").trim().to_owned());
                current.clear();
            }
        } else {
            current.push(line.trim());
        }
    }
    if !current.is_empty() {
        paragraphs.push(current.join("\n").trim().to_owned());
    }
    if paragraphs.len() > 1 {
        return paragraphs;
    }
    let lines = content
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(str::to_owned)
        .collect::<Vec<_>>();
    if lines.len() > 1 { lines } else { paragraphs }
}

fn serialize(value: &Value) -> String {
    serde_json::to_string(value).unwrap_or_else(|_| {
        "{\"_elitea_response_selection\":{\"status\":\"content_too_large\"},\"data\":null}"
            .to_owned()
    })
}

fn serialized_chars(value: &str) -> usize {
    value.chars().count()
}

fn bounded_f64(value: usize) -> f64 {
    f64::from(u32::try_from(value).unwrap_or(u32::MAX))
}

fn push_unique<T: PartialEq>(values: &mut Vec<T>, value: T) {
    if !values.contains(&value) {
        values.push(value);
    }
}
