//! Files the model is not asked about (`_process_file_with_chunks`): the
//! parser still reads them and they still get a file node.

/// Why a file's model extraction is skipped, or `None`.
#[must_use]
pub fn reason(text: &str, min_lines: usize, min_chars: usize) -> Option<String> {
    let line_count = text.matches('\n').count() + 1;
    let char_count = text.chars().count();
    if line_count < min_lines || char_count < min_chars {
        return Some(format!("small ({line_count} lines, {char_count} chars)"));
    }
    let lower = text.to_lowercase();
    let license_matches = [
        "apache license",
        "mit license",
        "bsd license",
        "gpl license",
        "licensed under the",
        "permission is hereby granted",
        "copyright (c)",
        "copyright 20",
        "all rights reserved",
        "without warranties or conditions",
        "provided \"as is\"",
    ]
    .iter()
    .filter(|indicator| lower.contains(*indicator))
    .count();
    if license_matches >= 3 {
        let code_lines = text
            .split('\n')
            .map(elitea_engine_core::pystr::strip)
            .filter(|line| {
                !line.is_empty()
                    && !["#", "//", "/*", "*", "<!--", "\"\"\"", "'''"]
                        .iter()
                        .any(|prefix| line.starts_with(prefix))
            })
            .count();
        #[allow(clippy::cast_precision_loss)]
        if (code_lines as f64) < line_count as f64 * 0.2 {
            return Some(format!(
                "license/boilerplate ({code_lines} code lines of {line_count})"
            ));
        }
    }
    let lines: Vec<&str> = elitea_engine_core::pystr::strip(text)
        .split('\n')
        .map(elitea_engine_core::pystr::strip)
        .filter(|line| !line.is_empty())
        .collect();
    let export_lines = lines
        .iter()
        .filter(|line| {
            ["export ", "import ", "from ", "module.exports", "exports."]
                .iter()
                .any(|prefix| line.starts_with(prefix))
                || line.contains("require(")
        })
        .count();
    #[allow(clippy::cast_precision_loss)]
    if !lines.is_empty() && export_lines as f64 / lines.len() as f64 > 0.8 {
        return Some(format!(
            "barrel/re-export file ({export_lines}/{} export lines)",
            lines.len()
        ));
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn small_license_and_barrel_files_are_skipped() {
        assert!(reason("x = 1\n", 20, 300).is_some_and(|r| r.starts_with("small")));
        let license = format!(
            "# Licensed under the Apache License\n# Copyright (c) 2024\n# All rights reserved\n{}x = 1\n",
            "# Permission is hereby granted...\n".repeat(30)
        );
        assert!(reason(&license, 20, 300).is_some_and(|r| r.starts_with("license")));
        let barrel = format!("{}const x = 1;\n", "export * from './a';\n".repeat(30));
        assert!(reason(&barrel, 20, 300).is_some_and(|r| r.starts_with("barrel")));
        let code = "def f(x):\n    return x * 2\n".repeat(20);
        assert_eq!(reason(&code, 20, 300), None);
    }
}
