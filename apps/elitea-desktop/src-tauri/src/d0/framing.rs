//! Framing text the system instructions carry but do not author (a
//! workspace's AGENTS.md, a skill the person picked): one delimited block
//! per source, which its own text cannot close, reopen or step out of.

/// `text` as an attribute value inside `"…"`: the markup characters and
/// every control character (a file name may hold a newline) escaped.
pub fn attribute_value(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '"' => out.push_str("&quot;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            c if c.is_control() => out.push_str(&format!("&#x{:x};", u32::from(c))),
            c => out.push(c),
        }
    }
    out
}

/// `text`, made unable to break its frame: an opening or closing tag named
/// in `tags` (any case) gets a backslash after its `<`, and a line that
/// would read as one of `headings` (matched case-insensitively at its
/// start, leading blanks ignored) is escaped with one in front. Everything
/// else is unchanged.
pub fn neutralised(text: &str, tags: &[&str], headings: &[&str]) -> String {
    let starts_tag = |rest: &[u8]| {
        let rest = rest.strip_prefix(b"/").unwrap_or(rest);
        tags.iter().any(|tag| {
            let tag = tag.as_bytes();
            rest.len() >= tag.len() && rest[..tag.len()].eq_ignore_ascii_case(tag)
        })
    };
    let mut out = String::with_capacity(text.len());
    for (index, line) in text.split('\n').enumerate() {
        if index > 0 {
            out.push('\n');
        }
        let heading = line.trim_start().to_ascii_lowercase();
        if headings
            .iter()
            .any(|h| heading.starts_with(&h.to_ascii_lowercase()))
        {
            out.push('\\');
        }
        let bytes = line.as_bytes();
        let mut from = 0;
        for (at, byte) in bytes.iter().enumerate() {
            if *byte == b'<' && starts_tag(&bytes[at + 1..]) {
                out.push_str(&line[from..=at]);
                out.push('\\');
                from = at + 1;
            }
        }
        out.push_str(&line[from..]);
    }
    out
}
