//! The `data_analysis` internal tool (ADR-0033): deterministic tools over CSV,
//! TSV, JSON and Excel files in the conversation's attachment bucket, plus one
//! door to the code sandbox for what they cannot express.
//!
//! It replaces the SDK's `pandas_analyze_data(query, filename)`, which asked the
//! agent's model for pandas code and ran it in the worker. Nothing here
//! evaluates code, and no tool calls a model: the agent's model drives the
//! tools. The engine is a small typed table in this module (no polars, no
//! arrow); files are read as hostile input, with row, column, cell and time
//! bounds checked while a table is built (decision 4).
//!
//! * `table`: columns, limits, deadline, type inference.
//! * `formats`: CSV/TSV/JSON/xlsx readers and writers.
//! * `ops`: filter, sort, aggregate, pivot, join, describe, correlate.
//! * `chart`: SVG charts (rasterised by `elitea-office-core`).
//! * `tools`: the ADK tools and the host I/O around them.

// Row counts and cell values move between usize, i64 and f64 throughout a
// statistics engine; every cast is a count or an index bounded by `Limits`
// (at most five million cells), far inside the exact range of an f64.
#![allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    clippy::cast_possible_wrap,
    clippy::too_many_lines,
    clippy::too_many_arguments,
    clippy::similar_names,
    clippy::needless_pass_by_value,
    // Exact float comparison is the point where it appears: integrality
    // (`v == v.trunc()`), tie detection in ranks, zero variance.
    clippy::float_cmp,
    clippy::manual_midpoint,
    clippy::match_same_arms,
    clippy::needless_range_loop,
    clippy::struct_excessive_bools,
    clippy::unnecessary_wraps,
    clippy::if_not_else,
    clippy::single_match_else
)]

pub(crate) mod chart;
pub mod code;
pub(crate) mod formats;
pub(crate) mod ops;
pub(crate) mod schema;
pub(crate) mod table;
pub mod tools;
