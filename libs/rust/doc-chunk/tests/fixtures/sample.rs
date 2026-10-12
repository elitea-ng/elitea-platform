use std::collections::HashMap;

const LIMIT: usize = 3;

/// Counts words.
///
/// Splits on whitespace.
pub fn count_words(text: &str) -> HashMap<String, usize> {
    let mut counts = HashMap::new();
    for word in text.split_whitespace() {
        *counts.entry(word.to_lowercase()).or_insert(0) += 1;
    }
    counts
}

pub struct Cache {
    items: Vec<String>,
}

impl Cache {
    pub fn new() -> Self {
        Self { items: Vec::new() }
    }

    /// Adds an item, evicting the oldest past the limit.
    pub fn add(&mut self, item: String) {
        self.items.push(item);
        if self.items.len() > LIMIT {
            self.items.remove(0);
        }
    }
}
