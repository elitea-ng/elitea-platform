//! One exact-integer parser: Track A shaping and typed `sum_int` must agree.
//!
//! The Worker builds `serde_json` with `arbitrary_precision`. Data shaping reads
//! `Number::as_str()`; the shared runtime reducer reads `Number`'s `Display`
//! text (`elitea_agent_runtime::exact_number`). Any divergence between the two
//! paths fails here, in the Worker's CI run.

use serde_json::{Number, Value, json};

use super::data_shaping::{ShapingCode, exact_i64};
use super::state_reducers::{ReducerFailure, StateReducer};

const EDGE_CASES: &[&str] = &[
    "0",
    "-0",
    "0.0",
    "-0.0",
    "1",
    "-1",
    "2.0",
    "1e2",
    "1E2",
    "1.50e1",
    "100e-2",
    "1e+2",
    "9223372036854775807",
    "-9223372036854775808",
    "9223372036854775807.0",
    "92233720368547758.07e2",
    "9223372036854775808",
    "-9223372036854775809",
    "18446744073709551615",
    "1e19",
    "1e20",
    "123456789012345678901",
    "2.5",
    "1e-1",
    "0.5",
    "-2.5",
    "1e99999999999999999999",
];

/// The verdict a `sum_int` channel at 0 reaches for `number`, in shaping terms.
fn reducer_verdict(number: &Number) -> Result<i64, &'static str> {
    match StateReducer::SumInt.reduce_checked(&json!(0), &Value::Number(number.clone())) {
        Ok(value) => value.as_i64().ok_or("non-i64 result"),
        Err(ReducerFailure::Overflow) => Err("overflow"),
        Err(ReducerFailure::TypeMismatch) => Err("not_integer"),
        Err(ReducerFailure::Limit) => Err("limit"),
    }
}

fn shaping_verdict(number: &Number) -> Result<i64, &'static str> {
    exact_i64(number).map_err(|code| match code {
        ShapingCode::IntegerOverflow => "overflow",
        _ => "not_integer",
    })
}

#[test]
fn shaping_and_typed_sum_int_read_every_edge_case_identically() {
    for text in EDGE_CASES {
        let number: Number = serde_json::from_str(text).expect("arbitrary-precision number");
        assert_eq!(number.as_str(), number.to_string(), "{text}");
        assert_eq!(reducer_verdict(&number), shaping_verdict(&number), "{text}");
    }
}

#[test]
fn the_edge_cases_cover_both_bounds_and_both_refusals() {
    let verdicts = EDGE_CASES
        .iter()
        .map(|text| shaping_verdict(&serde_json::from_str(text).expect("number")))
        .collect::<Vec<_>>();
    for expected in [
        Ok(i64::MAX),
        Ok(i64::MIN),
        Ok(0),
        Ok(100),
        Err("overflow"),
        Err("not_integer"),
    ] {
        assert!(verdicts.contains(&expected), "{expected:?}");
    }
}
