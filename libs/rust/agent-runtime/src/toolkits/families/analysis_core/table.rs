//! The dataset the analysis toolkits write and read: a header and rows of
//! text cells, an empty cell meaning "no value". It is the CSV file in the
//! artifact bucket, so `data_analysis` reads what these toolkits write.

/// A number as a dataset cell: at most six decimals, no trailing zeros
/// (`6`, `3.5`, `0.333333`).
pub(crate) fn format_number(value: f64) -> String {
    let rounded = (value * 1_000_000.0).round() / 1_000_000.0;
    rounded.to_string()
}

/// A table of text cells.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(crate) struct Table {
    pub(crate) columns: Vec<String>,
    pub(crate) rows: Vec<Vec<String>>,
}

impl Table {
    /// An empty table with these columns.
    pub(crate) fn new<S: AsRef<str>>(columns: &[S]) -> Self {
        Self {
            columns: columns
                .iter()
                .map(|name| name.as_ref().to_owned())
                .collect(),
            rows: Vec::new(),
        }
    }

    /// Add a row. A short row is padded with empty cells and a long one cut,
    /// so every row has exactly one cell per column.
    pub(crate) fn push(&mut self, mut row: Vec<String>) {
        row.resize(self.columns.len(), String::new());
        self.rows.push(row);
    }

    /// The position of a column.
    pub(crate) fn column(&self, name: &str) -> Option<usize> {
        self.columns.iter().position(|column| column == name)
    }

    /// Whether the table has no rows.
    pub(crate) fn is_empty(&self) -> bool {
        self.rows.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::Table;

    #[test]
    fn a_pushed_row_always_has_one_cell_per_column() {
        let mut table = Table::new(&["a", "b"]);
        table.push(vec!["1".to_owned()]);
        table.push(vec!["1".to_owned(), "2".to_owned(), "3".to_owned()]);
        assert_eq!(table.rows[0], ["1", ""]);
        assert_eq!(table.rows[1], ["1", "2"]);
        assert_eq!(table.column("b"), Some(1));
        assert_eq!(table.column("c"), None);
        assert!(!table.is_empty());
    }
}
