//! Seeded property tests for `split_out` and `aggregate`. A failing case
//! prints its seed, so `Rng::new(seed)` replays it exactly.

use serde_json::{Map, Value, json};

use super::aggregate::{AggregateNodeDefinition, run as aggregate_run};
use super::data_shaping::ShapingCode;
use super::split_out::{SplitOutNodeDefinition, run as split_run};

const CASES: u64 = 2_000;
const BASE_SEED: u64 = 0x5EED_0005_C0DE_A4A8;

/// `SplitMix64`.
struct Rng(u64);

impl Rng {
    fn new(seed: u64) -> Self {
        Self(seed)
    }

    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }

    /// Uniform in `0..n`; `n` must be positive.
    fn upto(&mut self, n: usize) -> usize {
        let bound = u64::try_from(n).unwrap_or(1).max(1);
        usize::try_from(self.next() % bound).unwrap_or(0)
    }

    fn pick<'a, T>(&mut self, items: &'a [T]) -> &'a T {
        &items[self.upto(items.len())]
    }
}

fn property(name: &str, check: impl Fn(&mut Rng) -> Result<(), String>) {
    for case in 0..CASES {
        let seed = BASE_SEED ^ case.wrapping_mul(0x1000_0000_01B3);
        if let Err(message) = check(&mut Rng::new(seed)) {
            panic!("{name} failed, seed {seed:#x} (case {case}): {message}");
        }
    }
}

fn parse(text: &str) -> Value {
    serde_json::from_str(text).expect("json literal")
}

const NUMBERS: [&str; 10] = [
    "1",
    "1.0",
    "-0",
    "0",
    "2.5",
    "1e2",
    "-17",
    "12345678901234567890",
    "0.1",
    "10e-1",
];
const WORDS: [&str; 5] = ["", "a", "SECRET", "k\u{e9}\u{1F600}", "line\nbreak"];
const KEYS: [&str; 5] = ["a", "b", "c", "", "k\u{e9}"];

/// Random JSON with container nesting at most `depth`.
fn json_value(rng: &mut Rng, depth: usize) -> Value {
    if depth > 0 && rng.upto(10) < 4 {
        if rng.upto(2) == 0 {
            return Value::Array(
                (0..rng.upto(4))
                    .map(|_| json_value(rng, depth - 1))
                    .collect(),
            );
        }
        let mut map = Map::new();
        for _ in 0..rng.upto(4) {
            map.insert((*rng.pick(&KEYS)).to_owned(), json_value(rng, depth - 1));
        }
        return Value::Object(map);
    }
    match rng.upto(5) {
        0 => Value::Null,
        1 => Value::Bool(rng.upto(2) == 0),
        2 | 3 => parse(rng.pick(&NUMBERS)),
        _ => Value::String((*rng.pick(&WORDS)).to_owned()),
    }
}

/// `count` rows `{<extra fields>, "f": [..]}`; lists have `min..=max` items.
fn parent_rows(rng: &mut Rng, count: usize, min: usize, max: usize) -> Vec<Value> {
    (0..count)
        .map(|_| {
            let mut row = Map::new();
            for key in ["id", "a", "b", "m"] {
                if key == "id" || rng.upto(2) == 0 {
                    row.insert(key.to_owned(), json_value(rng, 6));
                }
            }
            let items = min + rng.upto(max - min + 1);
            let list = (0..items).map(|_| json_value(rng, 6)).collect();
            row.insert("f".to_owned(), Value::Array(list));
            Value::Object(row)
        })
        .collect()
}

fn split_def(extra: &str) -> SplitOutNodeDefinition {
    SplitOutNodeDefinition::from_yaml(&format!(
        "id: sp\ntype: split_out\nsource: rows\noutput: [lines]\n{extra}\n"
    ))
    .expect("split_out definition")
}

fn agg_def(extra: &str) -> AggregateNodeDefinition {
    AggregateNodeDefinition::from_yaml(&format!(
        "id: ag\ntype: aggregate\nsource: lines\noutput: [out]\n{extra}\n"
    ))
    .expect("aggregate definition")
}

const ROWS_FIELD: &str =
    "split: {mode: rows_field, path: /f}\ndestination: f\nretain: {mode: all}\nmissing_list: empty";
const REGROUP: &str = "layout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /f}, output: f}]";

fn split(def: &SplitOutNodeDefinition, source: &Value) -> Result<Value, String> {
    split_run(def, Some(source)).map_err(|error| error.message())
}

fn aggregate(def: &AggregateNodeDefinition, source: &Value) -> Result<Value, String> {
    aggregate_run(def, Some(source)).map_err(|error| error.message())
}

fn same(left: &Value, right: &Value) -> Result<(), String> {
    if left == right {
        return Ok(());
    }
    Err(format!("left {left} != right {right}"))
}

// P1: Aggregate(regroup parent) after SplitOut restores the input.
#[test]
fn p1_regroup_inverts_split_out() {
    let (sp, ag) = (split_def(ROWS_FIELD), agg_def(REGROUP));
    property("P1", |rng| {
        let count = 1 + rng.upto(6);
        let input = Value::Array(parent_rows(rng, count, 1, 4));
        let lines = split(&sp, &input)?;
        same(&aggregate(&ag, &lines)?, &input)
    });
}

// P2: per parent positions strictly increase; parent_index never decreases.
#[test]
fn p2_envelope_order() {
    let sp = split_def(ROWS_FIELD);
    property("P2", |rng| {
        let count = 1 + rng.upto(6);
        let input = Value::Array(parent_rows(rng, count, 0, 4));
        let lines = split(&sp, &input)?;
        let mut last: Option<(u64, u64)> = None;
        for row in lines.as_array().ok_or("not a list")? {
            let parent = row["parent_index"].as_u64().ok_or("parent_index")?;
            let position = row["position"].as_u64().ok_or("position")?;
            match last {
                Some((previous, _)) if parent < previous => return Err("parent decreased".into()),
                Some((previous, at)) if parent == previous && position != at + 1 => {
                    return Err("position did not advance by one".into());
                }
                Some((previous, _)) if parent != previous && position != 0 => {
                    return Err("new parent must start at 0".into());
                }
                None if position != 0 => return Err("first position must be 0".into()),
                _ => {}
            }
            last = Some((parent, position));
        }
        let expected: usize = input
            .as_array()
            .ok_or("input")?
            .iter()
            .map(|row| row["f"].as_array().map_or(0, Vec::len))
            .sum();
        if lines.as_array().map(Vec::len) != Some(expected) {
            return Err("row count differs from the element count".into());
        }
        Ok(())
    });
}

// P3: counts add up to the input length; groups keep first-appearance order.
#[test]
fn p3_count_rows_and_group_order() {
    let ag = agg_def(
        "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}]",
    );
    let keys = [
        json!("x"),
        json!("y"),
        json!(7),
        json!([1, "z"]),
        json!({"p": true}),
        json!(false),
    ];
    property("P3", |rng| {
        let len = rng.upto(60);
        let picks: Vec<usize> = (0..len).map(|_| rng.upto(keys.len())).collect();
        let input = Value::Array(picks.iter().map(|&i| json!({"k": keys[i]})).collect());
        let mut expected: Vec<(usize, u64)> = Vec::new();
        for &pick in &picks {
            match expected.iter_mut().find(|(key, _)| *key == pick) {
                Some((_, n)) => *n += 1,
                None => expected.push((pick, 1)),
            }
        }
        let want = Value::Array(
            expected
                .iter()
                .map(|&(key, n)| json!({"k": keys[key], "n": n}))
                .collect(),
        );
        let got = aggregate(&ag, &input)?;
        let total: u64 = got
            .as_array()
            .ok_or("not a list")?
            .iter()
            .filter_map(|row| row["n"].as_u64())
            .sum();
        if usize::try_from(total) != Ok(len) {
            return Err(format!("counts sum to {total}, input has {len}"));
        }
        same(&got, &want)
    });
}

// P4: sum_int against an i128 reference, including values around i64 bounds.
#[test]
fn p4_sum_int_matches_i128_reference() {
    let ag = agg_def("operations: [{operation: sum_int, field: {path: /v}, output: s}]");
    let edges: [i128; 8] = [
        i128::from(i64::MAX),
        i128::from(i64::MIN),
        i128::from(i64::MAX) + 1,
        i128::from(i64::MIN) - 1,
        i128::from(i64::MAX) - 1,
        1,
        -1,
        0,
    ];
    property("P4", |rng| {
        let values: Vec<i128> = (0..rng.upto(6))
            .map(|_| match rng.upto(3) {
                0 => *rng.pick(&edges),
                1 => i128::from(rng.next().cast_signed()),
                _ => i128::from(rng.next() % 1_000) - 500,
            })
            .collect();
        let input = Value::Array(
            values
                .iter()
                .map(|value| json!({"v": parse(&value.to_string())}))
                .collect(),
        );
        let in_range = |value: i128| i64::try_from(value).is_ok();
        // The running sum is checked after every addition.
        let mut sum: i128 = 0;
        let mut running_ok = true;
        for &value in &values {
            sum += value;
            running_ok &= in_range(sum);
        }
        let result = aggregate_run(&ag, Some(&input));
        if values.iter().all(|&v| in_range(v)) && running_ok {
            let row = result.map_err(|error| error.message())?;
            let want = i64::try_from(sum).map_err(|_| "reference")?;
            return same(&row[0]["s"], &json!(want));
        }
        match result {
            Err(error) if error.code() == ShapingCode::IntegerOverflow => Ok(()),
            other => Err(format!(
                "expected integer_overflow, got {:?}",
                other.map(|_| ())
            )),
        }
    });
}

fn with_limit(rng: &mut Rng) -> (String, usize, usize) {
    let (name, low, spread) = match rng.upto(3) {
        0 => ("bytes", 2, 1_500),
        1 => ("values", 1, 150),
        _ => ("output_items", 1, 25),
    };
    let first = low + rng.upto(spread);
    (name.to_owned(), first, first + rng.upto(spread))
}

// P5: success under a limit implies success (same output) under a larger one.
#[test]
fn p5_limits_are_monotonic() {
    property("P5", |rng| {
        let count = 1 + rng.upto(5);
        let input = Value::Array(parent_rows(rng, count, 0, 4));
        let (name, small, large) = with_limit(rng);
        let sp = |limit: usize| split_def(&format!("{ROWS_FIELD}\nlimits: {{{name}: {limit}}}"));
        let (low, high) = (split(&sp(small), &input), split(&sp(large), &input));
        match (&low, &high) {
            (Ok(a), Ok(b)) => same(a, b)?,
            (Ok(_), Err(message)) => return Err(format!("{name} {small}->{large}: {message}")),
            _ => {}
        }
        let lines = split(&split_def(ROWS_FIELD), &input)?;
        let group = "operations: [{operation: collect_rows, output: r, retain: {mode: all}}]";
        let ag = |limit: usize| agg_def(&format!("{group}\nlimits: {{{name}: {limit}}}"));
        match (aggregate(&ag(small), &lines), aggregate(&ag(large), &lines)) {
            (Ok(a), Ok(b)) => same(&a, &b),
            (Ok(_), Err(message)) => Err(format!("aggregate {name} {small}->{large}: {message}")),
            _ => Ok(()),
        }
    });
}

// P6: deterministic, also after a checkpoint (to_vec/from_slice) round trip.
#[test]
fn p6_deterministic_and_checkpoint_stable() {
    let (sp, ag) = (split_def(ROWS_FIELD), agg_def(REGROUP));
    let summary = agg_def(
        "layout: split_out\nregroup: none\ngroup_by: [{path: /id, output: p}]\noperations: [{operation: count_rows, output: n}, {operation: collect_rows, output: r, retain: {mode: all}}]",
    );
    property("P6", |rng| {
        let count = 1 + rng.upto(5);
        let input = Value::Array(parent_rows(rng, count, 0, 4));
        let restored: Value =
            serde_json::from_slice(&serde_json::to_vec(&input).map_err(|e| e.to_string())?)
                .map_err(|e| e.to_string())?;
        let bytes = |value: &Value| serde_json::to_vec(value).map_err(|e| e.to_string());
        let first = split(&sp, &input)?;
        for other in [split(&sp, &input)?, split(&sp, &restored)?] {
            if bytes(&first)? != bytes(&other)? {
                return Err("split_out output differs".into());
            }
        }
        for node in [&ag, &summary] {
            let one = aggregate(node, &first)?;
            let again = aggregate(
                node,
                &serde_json::from_slice(&bytes(&first)?).map_err(|e| e.to_string())?,
            )?;
            if bytes(&one)? != bytes(&again)? {
                return Err("aggregate output differs".into());
            }
        }
        Ok(())
    });
}

// P7: equal-value number spellings group together; first spelling is emitted.
#[test]
fn p7_equal_spellings_group() {
    let ag = agg_def(
        "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}]",
    );
    let classes: [&[&str]; 3] = [
        &["1", "1.0", "1e0", "10e-1", "0.1e1", "100e-2"],
        &["0", "-0", "0.0", "-0.0", "0e5", "0e-3"],
        &["2", "2.0", "20e-1", "0.2e1"],
    ];
    property("P7", |rng| {
        let spellings: Vec<(usize, &str)> = (0..=rng.upto(30))
            .map(|_| {
                let class = rng.upto(classes.len());
                (class, *rng.pick(classes[class]))
            })
            .collect();
        let input = Value::Array(
            spellings
                .iter()
                .map(|(_, text)| json!({"k": parse(text)}))
                .collect(),
        );
        let mut want: Vec<(usize, &str, u64)> = Vec::new();
        for &(class, text) in &spellings {
            match want.iter_mut().find(|entry| entry.0 == class) {
                Some(entry) => entry.2 += 1,
                None => want.push((class, text, 1)),
            }
        }
        let got = aggregate(&ag, &input)?;
        let rows = got.as_array().ok_or("not a list")?;
        if rows.len() != want.len() {
            return Err(format!("{} groups, expected {}", rows.len(), want.len()));
        }
        for (row, (_, text, n)) in rows.iter().zip(&want) {
            let key = serde_json::to_string(&row["k"]).map_err(|e| e.to_string())?;
            let text = serde_json::to_string(&parse(text)).map_err(|e| e.to_string())?;
            if key != text || row["n"] != json!(n) {
                return Err(format!("group {key} x{} expected {text} x{n}", row["n"]));
            }
        }
        Ok(())
    });
}

// ---------------------------------------------------------------- adversarial

fn nested(levels: usize) -> Value {
    let mut value = json!(1);
    for _ in 0..levels {
        value = Value::Array(vec![value]);
    }
    value
}

#[test]
fn deep_element_fails_with_depth_limit() {
    let sp = split_def("split: {mode: list}\ndestination: d");
    for levels in [33, 200] {
        let error = split_run(&sp, Some(&Value::Array(vec![nested(levels)]))).expect_err("depth");
        assert_eq!(
            error.message(),
            "graph.shaping.limit_exceeded: limits.depth at item 0 position 0"
        );
    }
    let rows = agg_def("operations: [{operation: collect_rows, output: r, retain: {mode: all}}]");
    let deep = Value::Array(vec![json!({"x": nested(40)})]);
    let message = aggregate_run(&rows, Some(&deep))
        .expect_err("depth")
        .message();
    assert!(message.contains("limits.depth"), "{message}");
}

#[test]
fn too_many_input_rows_fail_fast() {
    let rows = Value::Array(vec![json!({"f": [], "k": 1}); 10_001]);
    let sp = split_def("split: {mode: rows_field, path: /f}\ndestination: d");
    assert_eq!(
        split_run(&sp, Some(&rows)).expect_err("input").message(),
        "graph.shaping.limit_exceeded: limits.input_items"
    );
    let ag = agg_def("operations: [{operation: count_rows, output: n}]");
    assert_eq!(
        aggregate_run(&ag, Some(&rows))
            .expect_err("input")
            .message(),
        "graph.shaping.limit_exceeded: limits.input_items"
    );
    let exact = Value::Array(vec![json!({"k": 1}); 10_000]);
    assert!(aggregate_run(&ag, Some(&exact)).is_ok());
}

#[test]
fn huge_exponent_is_unsupported_number() {
    let ag = agg_def("operations: [{operation: sum_int, field: {path: /v}, output: s}]");
    let input = Value::Array(vec![json!({"v": parse("1e99999999999999999999")})]);
    let error = aggregate_run(&ag, Some(&input)).expect_err("exponent");
    assert_eq!(error.code(), ShapingCode::UnsupportedNumber);
    assert_eq!(
        error.message(),
        "graph.shaping.unsupported_number: operations[0].field at item 0"
    );
}
