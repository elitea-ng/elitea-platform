//! Property-based tests for RunConfig builder round-trip and configuration validation.
//!
//! Implements Property 2 (Runner Configuration Builder Round-Trip) from the
//! one-point-zero-readiness design document.

// prop_run_config_default_setter_equivalence deliberately builds a config via
// Default + field setters to compare against the builder — the struct-literal
// form clippy suggests would defeat the comparison. (File-level because the
// allow can't attach inside the proptest! macro expansion.)
#![allow(clippy::field_reassign_with_default)]

use adk_core::{
    BackpressurePolicy, RunConfig, RunConfigBuilder, StreamingMode, ToolConcurrencyConfig,
    ToolConfirmationDecision,
};
use proptest::prelude::*;
use std::collections::HashMap;

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

fn arb_streaming_mode() -> impl Strategy<Value = StreamingMode> {
    prop_oneof![Just(StreamingMode::None), Just(StreamingMode::SSE), Just(StreamingMode::Bidi),]
}

fn arb_backpressure_policy() -> impl Strategy<Value = BackpressurePolicy> {
    prop_oneof![Just(BackpressurePolicy::Queue), Just(BackpressurePolicy::Fail),]
}

fn arb_tool_confirmation_decision() -> impl Strategy<Value = ToolConfirmationDecision> {
    prop_oneof![Just(ToolConfirmationDecision::Approve), Just(ToolConfirmationDecision::Deny),]
}

fn arb_tool_confirmation_decisions()
-> impl Strategy<Value = HashMap<String, ToolConfirmationDecision>> {
    prop::collection::hash_map("[a-z_]{1,12}", arb_tool_confirmation_decision(), 0..5)
}

fn arb_tool_concurrency_config() -> impl Strategy<Value = ToolConcurrencyConfig> {
    (
        prop::option::of(1usize..100),
        prop::collection::hash_map("[a-z_]{1,10}", 1usize..50, 0..4),
        arb_backpressure_policy(),
    )
        .prop_map(|(max_concurrency, per_tool, backpressure)| ToolConcurrencyConfig {
            max_concurrency,
            per_tool,
            backpressure,
        })
}

fn arb_optional_string() -> impl Strategy<Value = Option<String>> {
    prop::option::of("[a-z][a-z0-9_-]{0,15}")
}

fn arb_transfer_targets() -> impl Strategy<Value = Vec<String>> {
    prop::collection::vec("[a-z][a-z0-9_-]{1,12}", 0..5)
}

// ---------------------------------------------------------------------------
// Property Tests
// ---------------------------------------------------------------------------

proptest! {
    #![proptest_config(ProptestConfig::with_cases(100))]

    /// **Feature: one-point-zero-readiness, Property 2: Runner Configuration Builder Round-Trip**
    /// *For any* valid RunConfig field values, constructing via the builder SHALL
    /// preserve all field values when read back.
    /// **Validates: Requirements 9.2, 14.3**
    #[test]
    fn prop_run_config_builder_roundtrip(
        streaming_mode in arb_streaming_mode(),
        decisions in arb_tool_confirmation_decisions(),
        cached_content in arb_optional_string(),
        transfer_targets in arb_transfer_targets(),
        parent_agent in arb_optional_string(),
        auto_cache in proptest::bool::ANY,
        history_max_events in prop::option::of(0usize..10000),
        tool_concurrency in arb_tool_concurrency_config(),
        record_payloads in proptest::bool::ANY,
        trace_payload_max_bytes in 0usize..65536,
    ) {
        // Build the config using the builder
        let mut builder = RunConfig::builder()
            .streaming_mode(streaming_mode)
            .tool_confirmation_decisions(decisions.clone())
            .transfer_targets(transfer_targets.clone())
            .auto_cache(auto_cache)
            .history_max_events(history_max_events)
            .tool_concurrency(tool_concurrency.clone())
            .record_payloads(record_payloads)
            .trace_payload_max_bytes(trace_payload_max_bytes);

        if let Some(ref name) = cached_content {
            builder = builder.cached_content(name.clone());
        }
        if let Some(ref name) = parent_agent {
            builder = builder.parent_agent(name.clone());
        }

        let config = builder.build();

        // Verify all fields are preserved
        prop_assert_eq!(config.streaming_mode, streaming_mode);
        prop_assert_eq!(&config.tool_confirmation_decisions, &decisions);
        prop_assert_eq!(&config.cached_content, &cached_content);
        prop_assert_eq!(&config.transfer_targets, &transfer_targets);
        prop_assert_eq!(&config.parent_agent, &parent_agent);
        prop_assert_eq!(config.auto_cache, auto_cache);
        prop_assert_eq!(config.history_max_events, history_max_events);
        prop_assert_eq!(config.record_payloads, record_payloads);
        prop_assert_eq!(config.trace_payload_max_bytes, trace_payload_max_bytes);

        // ToolConcurrencyConfig doesn't derive PartialEq, compare fields
        prop_assert_eq!(
            config.tool_concurrency.max_concurrency,
            tool_concurrency.max_concurrency
        );
        prop_assert_eq!(&config.tool_concurrency.per_tool, &tool_concurrency.per_tool);
        prop_assert_eq!(
            config.tool_concurrency.backpressure,
            tool_concurrency.backpressure
        );
    }

    /// **Feature: one-point-zero-readiness, Property 2: Configuration Validation**
    /// *For any* valid configuration inputs, the builder SHALL accept them.
    /// *For any* configuration constructed via Default + field setters, the resulting
    /// config SHALL have the same values as one built via the builder.
    /// **Validates: Requirements 9.2, 14.3**
    #[test]
    fn prop_run_config_default_setter_equivalence(
        streaming_mode in arb_streaming_mode(),
        auto_cache in proptest::bool::ANY,
        history_max_events in prop::option::of(0usize..10000),
        record_payloads in proptest::bool::ANY,
        trace_payload_max_bytes in 0usize..65536,
    ) {
        // Build via builder
        let via_builder = RunConfig::builder()
            .streaming_mode(streaming_mode)
            .auto_cache(auto_cache)
            .history_max_events(history_max_events)
            .record_payloads(record_payloads)
            .trace_payload_max_bytes(trace_payload_max_bytes)
            .build();

        // Build via Default + field setters — the field-by-field path is the
        // behavior under test here, so the struct-literal form clippy suggests
        // would defeat the comparison.
        let mut via_default = RunConfig::default();
        via_default.streaming_mode = streaming_mode;
        via_default.auto_cache = auto_cache;
        via_default.history_max_events = history_max_events;
        via_default.record_payloads = record_payloads;
        via_default.trace_payload_max_bytes = trace_payload_max_bytes;

        // Both approaches should produce equivalent configs
        prop_assert_eq!(via_builder.streaming_mode, via_default.streaming_mode);
        prop_assert_eq!(via_builder.auto_cache, via_default.auto_cache);
        prop_assert_eq!(via_builder.history_max_events, via_default.history_max_events);
        prop_assert_eq!(via_builder.record_payloads, via_default.record_payloads);
        prop_assert_eq!(
            via_builder.trace_payload_max_bytes,
            via_default.trace_payload_max_bytes
        );
    }

    /// **Feature: one-point-zero-readiness, Property 2: Builder Default Values**
    /// The builder initialized with `RunConfigBuilder::default()` SHALL produce
    /// a config identical to `RunConfig::default()`.
    /// **Validates: Requirements 9.2**
    #[test]
    fn prop_builder_default_matches_struct_default(
        // Use a dummy input to make proptest run this 100 times
        // (verifying no non-determinism in defaults)
        _dummy in 0u8..1,
    ) {
        let from_builder = RunConfigBuilder::default().build();
        let from_default = RunConfig::default();

        prop_assert_eq!(from_builder.streaming_mode, from_default.streaming_mode);
        prop_assert_eq!(
            &from_builder.tool_confirmation_decisions,
            &from_default.tool_confirmation_decisions
        );
        prop_assert_eq!(&from_builder.cached_content, &from_default.cached_content);
        prop_assert_eq!(&from_builder.transfer_targets, &from_default.transfer_targets);
        prop_assert_eq!(&from_builder.parent_agent, &from_default.parent_agent);
        prop_assert_eq!(from_builder.auto_cache, from_default.auto_cache);
        prop_assert_eq!(from_builder.history_max_events, from_default.history_max_events);
        prop_assert_eq!(from_builder.record_payloads, from_default.record_payloads);
        prop_assert_eq!(
            from_builder.trace_payload_max_bytes,
            from_default.trace_payload_max_bytes
        );
        prop_assert_eq!(
            from_builder.tool_concurrency.max_concurrency,
            from_default.tool_concurrency.max_concurrency
        );
        prop_assert_eq!(
            &from_builder.tool_concurrency.per_tool,
            &from_default.tool_concurrency.per_tool
        );
        prop_assert_eq!(
            from_builder.tool_concurrency.backpressure,
            from_default.tool_concurrency.backpressure
        );
    }
}
