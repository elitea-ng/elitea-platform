//! Budgeted YAML parsing for stored and fetched documents.
//!
//! `serde_yaml_ng` bounds alias *jumps* and nesting, but not how many nodes or
//! scalar bytes the aliases expand into. [`from_str`] first replays the
//! document through a visitor that allocates nothing and stops at the first
//! node, scalar-byte or depth bound. Only then is the target type built. Both
//! passes replay the same expansion, so the second pass stays within the
//! budget the first one proved. The same input always fails the same way.

use std::cell::Cell;
use std::fmt;

use serde::de::{
    self, DeserializeOwned, DeserializeSeed, Deserializer, EnumAccess, MapAccess, SeqAccess,
    VariantAccess, Visitor,
};

/// Bounds on a YAML document after anchor and alias expansion.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct YamlBudget {
    /// Scalars, sequences, mappings and tags, counting mapping keys.
    pub nodes: usize,
    /// UTF-8 bytes of all scalars, including mapping keys and tags.
    pub scalar_bytes: usize,
    /// Nested sequences, mappings and tags.
    pub depth: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum YamlBudgetLimit {
    Nodes,
    ScalarBytes,
    Depth,
}

impl YamlBudgetLimit {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Nodes => "nodes",
            Self::ScalarBytes => "scalar_bytes",
            Self::Depth => "depth",
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum BoundedYamlError {
    #[error("the YAML document exceeds its expansion budget")]
    BudgetExceeded(YamlBudgetLimit),
    #[error("the YAML document is malformed")]
    Malformed(#[source] serde_yaml_ng::Error),
}

/// Deserialize one YAML document after proving its expansion fits `budget`.
///
/// # Errors
///
/// [`BoundedYamlError::BudgetExceeded`] names the first bound the expansion crossed;
/// [`BoundedYamlError::Malformed`] carries the parser error.
#[expect(
    clippy::disallowed_methods,
    reason = "the only parser entry point; both passes run under the budget"
)]
pub fn from_str<T: DeserializeOwned>(
    yaml: &str,
    budget: YamlBudget,
) -> Result<T, BoundedYamlError> {
    let meter = Meter {
        budget,
        nodes: Cell::new(0),
        scalar_bytes: Cell::new(0),
        exceeded: Cell::new(None),
    };
    let seed = CountingSeed {
        meter: &meter,
        depth: 0,
    };
    if let Err(source) = seed.deserialize(serde_yaml_ng::Deserializer::from_str(yaml)) {
        let limit = meter
            .exceeded
            .get()
            .or_else(|| parser_expansion_guard(&source));
        return Err(limit.map_or(
            BoundedYamlError::Malformed(source),
            BoundedYamlError::BudgetExceeded,
        ));
    }
    T::deserialize(serde_yaml_ng::Deserializer::from_str(yaml)).map_err(BoundedYamlError::Malformed)
}

/// The text `serde_yaml_ng` (0.10) gives its own alias-repetition guard.
pub(crate) const PARSER_REPETITION_GUARD: &str = "repetition limit exceeded";

/// `serde_yaml_ng` refuses alias replay past its own repetition limit, which can
/// fire before the meter crosses a large node budget. It is the same refusal
/// (too much expansion), so it is reported as the node budget rather than as a
/// malformed document. The crate exposes no error kind, so its fixed text is
/// matched; `bounded_yaml_tests` pins it. Its recursion guard (128) cannot fire
/// first: every budget's depth is lower.
fn parser_expansion_guard(source: &serde_yaml_ng::Error) -> Option<YamlBudgetLimit> {
    (source.to_string() == PARSER_REPETITION_GUARD).then_some(YamlBudgetLimit::Nodes)
}

/// Like [`from_str`], for callers whose error type carries `serde_yaml_ng::Error`.
/// A budget violation is reported as a malformed document.
///
/// # Errors
///
/// The parser error, or a custom error when the budget is exceeded.
pub fn from_str_as_yaml_error<T: DeserializeOwned>(
    yaml: &str,
    budget: YamlBudget,
) -> Result<T, serde_yaml_ng::Error> {
    from_str(yaml, budget).map_err(|error| match error {
        BoundedYamlError::Malformed(source) => source,
        BoundedYamlError::BudgetExceeded(_) => {
            de::Error::custom("the YAML document exceeds its expansion budget")
        }
    })
}

struct Meter {
    budget: YamlBudget,
    nodes: Cell<usize>,
    scalar_bytes: Cell<usize>,
    exceeded: Cell<Option<YamlBudgetLimit>>,
}

impl Meter {
    fn exceed<E: de::Error>(&self, limit: YamlBudgetLimit) -> E {
        self.exceeded.set(Some(limit));
        E::custom("the YAML document exceeds its expansion budget")
    }

    fn node<E: de::Error>(&self) -> Result<(), E> {
        let nodes = self.nodes.get().saturating_add(1);
        if nodes > self.budget.nodes {
            return Err(self.exceed(YamlBudgetLimit::Nodes));
        }
        self.nodes.set(nodes);
        Ok(())
    }

    fn scalar<E: de::Error>(&self, bytes: usize) -> Result<(), E> {
        self.node()?;
        let total = self.scalar_bytes.get().saturating_add(bytes);
        if total > self.budget.scalar_bytes {
            return Err(self.exceed(YamlBudgetLimit::ScalarBytes));
        }
        self.scalar_bytes.set(total);
        Ok(())
    }
}

#[derive(Clone, Copy)]
struct CountingSeed<'meter> {
    meter: &'meter Meter,
    depth: usize,
}

impl CountingSeed<'_> {
    /// Count one container node and return the seed for its children.
    fn enter<E: de::Error>(self) -> Result<Self, E> {
        self.meter.node()?;
        let depth = self.depth + 1;
        if depth > self.meter.budget.depth {
            return Err(self.meter.exceed(YamlBudgetLimit::Depth));
        }
        Ok(Self { depth, ..self })
    }
}

impl<'de> DeserializeSeed<'de> for CountingSeed<'_> {
    type Value = ();

    fn deserialize<D: Deserializer<'de>>(self, deserializer: D) -> Result<(), D::Error> {
        deserializer.deserialize_any(self)
    }
}

impl<'de> Visitor<'de> for CountingSeed<'_> {
    type Value = ();

    fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("any YAML value")
    }

    fn visit_bool<E: de::Error>(self, _: bool) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_i64<E: de::Error>(self, _: i64) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_i128<E: de::Error>(self, _: i128) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_u64<E: de::Error>(self, _: u64) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_u128<E: de::Error>(self, _: u128) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_f64<E: de::Error>(self, _: f64) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_str<E: de::Error>(self, value: &str) -> Result<(), E> {
        self.meter.scalar(value.len())
    }

    fn visit_bytes<E: de::Error>(self, value: &[u8]) -> Result<(), E> {
        self.meter.scalar(value.len())
    }

    fn visit_unit<E: de::Error>(self) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_none<E: de::Error>(self) -> Result<(), E> {
        self.meter.node()
    }

    fn visit_some<D: Deserializer<'de>>(self, deserializer: D) -> Result<(), D::Error> {
        self.deserialize(deserializer)
    }

    fn visit_newtype_struct<D: Deserializer<'de>>(self, deserializer: D) -> Result<(), D::Error> {
        self.deserialize(deserializer)
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut sequence: A) -> Result<(), A::Error> {
        let child = self.enter()?;
        while sequence.next_element_seed(child)?.is_some() {}
        Ok(())
    }

    fn visit_map<A: MapAccess<'de>>(self, mut mapping: A) -> Result<(), A::Error> {
        let child = self.enter()?;
        while mapping.next_key_seed(child)?.is_some() {
            mapping.next_value_seed(child)?;
        }
        Ok(())
    }

    fn visit_enum<A: EnumAccess<'de>>(self, tagged: A) -> Result<(), A::Error> {
        let child = self.enter()?;
        let ((), content) = tagged.variant_seed(child)?;
        content.newtype_variant_seed(child)
    }
}

#[cfg(test)]
#[path = "bounded_yaml_tests.rs"]
mod tests;
