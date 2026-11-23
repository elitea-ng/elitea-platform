// The internal-listener surface of the Rust worker (ADR-0059 decision 7,
// "Internal paths"; story R1.9). The worker is outside the Go workspace, so
// the listener tests of R1.8 cannot see it; the worker job of ci-rust.yml
// runs this one with every feature set it tests.
//
// The default worker serves nothing: it is a client of elitea-main, NATS and
// the sidecars. The `elitea-sandbox-supervisor` binary (feature
// `sandbox-supervisor`) serves three tonic services on mTLS listeners, one per
// runtime profile. None of them writes provider, policy, identity or role
// data. This test compares, against the reviewed allowlist
// (internal_listener_allowlist.txt beside this file), both ways:
//
// - the served methods: every tonic server module the crate's non-test
//   source names (`<service>_server::`), expanded to the routes that
//   server answers, read from the server code tonic generated into OUT_DIR
//   (the `"/<service>/<method>" =>` arms of its router);
// - every listener and registration site of the non-test source, with how
//   often it occurs in each file.
//
// The allowlist is reviewed text, one entry a line, fields separated by "|":
//
//   grpc | <listener>  | /<service>/<method> | <writes> | <decision> | <reason>
//   site | <file>      | <construct>         | <count>
//
// writes and decision have the meanings of R1.8's Go checks
// (services/elitea-main/internal/transport/internal_listener_checks_test.go):
// writes is read, write (run, content, project or metering data only) or
// admin (can change provider, policy, identity or role data); decision is
// main-route, claim (a grant Main signed for this supervisor, bound to the
// run), workload (the caller's mTLS workload identity) or none. A method
// declared admin must be decided by Main's own route (main-route) and listed
// in docs/ARCHITECTURE.md. The listener and the reason are declared by the
// reviewer; the test cannot prove what a handler writes. It proves that every
// served method and every site was reviewed.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::path::{Path, PathBuf};

const ALLOWLIST: &str = include_str!("internal_listener_allowlist.txt");
const ARCHITECTURE: &str = include_str!("../../../../docs/ARCHITECTURE.md");
const SERVICE: &str = "elitea-worker-rust";
const WRITES: [&str; 3] = ["read", "write", "admin"];
const DECISIONS: [&str; 4] = ["main-route", "claim", "workload", "none"];

// The constructs that build a listener or register a service, as they read
// in source with comments and string contents removed.
const SITE_CONSTRUCTS: [&str; 16] = [
    "Server::builder",
    ".add_service(",
    ".add_optional_service(",
    ".add_routes(",
    "TcpListener::bind",
    "UnixListener::bind",
    "UdpSocket::bind",
    "TcpListenerStream",
    "UnixListenerStream",
    "serve_with_incoming",
    "serve_with_shutdown",
    "serve_connection",
    "axum::serve",
    "hyper::server",
    "hyper_util::server",
    "NamedService for",
];

#[derive(Debug, Clone)]
struct Entry {
    kind: String,
    listener: String,
    name: String,
    writes: String,
    decision: String,
    reason: String,
    count: usize,
    line: usize,
}

fn parse_allowlist(text: &str) -> Result<Vec<Entry>, String> {
    let mut entries = Vec::new();
    let mut seen = BTreeMap::new();
    for (index, raw) in text.lines().enumerate() {
        let line = index + 1;
        let trimmed = raw.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            continue;
        }
        let fields: Vec<&str> = trimmed.split('|').map(str::trim).collect();
        let entry = match fields.as_slice() {
            ["site", file, construct, count] => Entry {
                kind: "site".into(),
                listener: (*file).into(),
                name: (*construct).into(),
                writes: String::new(),
                decision: String::new(),
                reason: String::new(),
                count: count
                    .parse()
                    .ok()
                    .filter(|count| *count > 0)
                    .ok_or_else(|| format!("line {line}: a site's count is a positive number"))?,
                line,
            },
            ["grpc", listener, name, writes, decision, reason] => {
                if !WRITES.contains(writes) || !DECISIONS.contains(decision) || reason.is_empty() {
                    return Err(format!(
                        "line {line}: writes must be one of {WRITES:?}, decision one of {DECISIONS:?}, and the reason given: {trimmed:?}"
                    ));
                }
                Entry {
                    kind: "grpc".into(),
                    listener: (*listener).into(),
                    name: (*name).into(),
                    writes: (*writes).into(),
                    decision: (*decision).into(),
                    reason: (*reason).into(),
                    count: 0,
                    line,
                }
            }
            _ => return Err(format!("line {line}: not an allowlist entry: {trimmed:?}")),
        };
        let key = format!("{} {} {}", entry.kind, entry.listener, entry.name);
        if let Some(previous) = seen.insert(key, line) {
            return Err(format!("line {line}: repeats line {previous}"));
        }
        entries.push(entry);
    }
    Ok(entries)
}

// What a lexer pass keeps of a Rust source text.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Keep {
    // Comments become spaces; string literals stay.
    Strings,
    // Comments and the contents of string literals become spaces.
    Code,
}

// blank replaces comments (and, for Keep::Code, string contents) with spaces,
// so a construct named in a comment or a message is not a site. Lifetimes and
// char literals are told apart by the closing quote.
fn blank(source: &str, keep: Keep) -> String {
    let chars: Vec<char> = source.chars().collect();
    let mut out = String::with_capacity(source.len());
    let mut index = 0;
    let space = |c: char| if c == '\n' { '\n' } else { ' ' };
    while index < chars.len() {
        let c = chars[index];
        let next = chars.get(index + 1).copied();
        if c == '/' && next == Some('/') {
            while index < chars.len() && chars[index] != '\n' {
                out.push(' ');
                index += 1;
            }
        } else if c == '/' && next == Some('*') {
            let mut depth = 0usize;
            loop {
                if index >= chars.len() {
                    break;
                }
                if chars[index] == '/' && chars.get(index + 1) == Some(&'*') {
                    depth += 1;
                    out.push_str("  ");
                    index += 2;
                } else if chars[index] == '*' && chars.get(index + 1) == Some(&'/') {
                    depth -= 1;
                    out.push_str("  ");
                    index += 2;
                    if depth == 0 {
                        break;
                    }
                } else {
                    out.push(space(chars[index]));
                    index += 1;
                }
            }
        } else if c == 'r'
            && (next == Some('#') || next == Some('"'))
            && !ident_before(&chars, index)
        {
            // A raw string: r"..." or r#"..."#.
            let mut hashes = 0;
            let mut cursor = index + 1;
            while chars.get(cursor) == Some(&'#') {
                hashes += 1;
                cursor += 1;
            }
            if chars.get(cursor) != Some(&'"') {
                out.push(c);
                index += 1;
                continue;
            }
            out.extend(&chars[index..=cursor]);
            index = cursor + 1;
            while index < chars.len() {
                if chars[index] == '"' && (1..=hashes).all(|n| chars.get(index + n) == Some(&'#')) {
                    out.extend(&chars[index..=index + hashes]);
                    index += hashes + 1;
                    break;
                }
                out.push(if keep == Keep::Code {
                    space(chars[index])
                } else {
                    chars[index]
                });
                index += 1;
            }
        } else if c == '"' {
            out.push('"');
            index += 1;
            while index < chars.len() && chars[index] != '"' {
                let width = if chars[index] == '\\' { 2 } else { 1 };
                for offset in 0..width {
                    if let Some(&inner) = chars.get(index + offset) {
                        out.push(if keep == Keep::Code {
                            space(inner)
                        } else {
                            inner
                        });
                    }
                }
                index += width;
            }
            out.push('"');
            index += 1;
        } else if c == '\'' {
            // A char literal is 'x' or '\..'; anything else is a lifetime.
            let end = if next == Some('\\') {
                (index + 3..chars.len().min(index + 12)).find(|&n| chars[n] == '\'')
            } else if chars.get(index + 2) == Some(&'\'') {
                Some(index + 2)
            } else {
                None
            };
            let end = end.unwrap_or(index);
            out.extend(&chars[index..=end]);
            index = end + 1;
        } else {
            out.push(c);
            index += 1;
        }
    }
    out
}

fn ident_before(chars: &[char], index: usize) -> bool {
    index > 0 && (chars[index - 1].is_alphanumeric() || chars[index - 1] == '_')
}

// test_only_attribute is an outer attribute that compiles its item only in a
// test build: #[cfg(test)] or #[cfg(all(test, ...))]. #[cfg(any(test, ...))]
// and #[cfg(not(test))] are not.
fn test_only_attribute(attribute: &str) -> bool {
    let compact: String = attribute.chars().filter(|c| !c.is_whitespace()).collect();
    compact == "#[cfg(test)]" || compact.starts_with("#[cfg(all(test,")
}

// attribute_end is the index after the attribute that starts at `start`
// (a '#' followed by '['), by bracket depth.
fn attribute_end(chars: &[char], start: usize) -> usize {
    let mut depth = 0usize;
    for (offset, &c) in chars[start..].iter().enumerate() {
        match c {
            '[' => depth += 1,
            ']' => {
                depth -= 1;
                if depth == 0 {
                    return start + offset + 1;
                }
            }
            _ => {}
        }
    }
    chars.len()
}

fn item_end(chars: &[char], mut index: usize) -> usize {
    let mut depth = 0usize;
    while index < chars.len() {
        match chars[index] {
            '#' if depth == 0 && chars.get(index + 1) == Some(&'[') => {
                index = attribute_end(chars, index);
                continue;
            }
            ';' if depth == 0 => return index + 1,
            '{' | '(' | '[' => depth += 1,
            '}' | ')' | ']' => {
                depth = depth.saturating_sub(1);
                if depth == 0 && chars[index] == '}' {
                    let rest = chars[index + 1..].iter().position(|c| !c.is_whitespace());
                    return match rest {
                        Some(offset) if chars[index + 1 + offset] == ';' => index + offset + 2,
                        _ => index + 1,
                    };
                }
            }
            _ => {}
        }
        index += 1;
    }
    chars.len()
}

// A source file's compiled text: comments and test-only items removed, with
// (Keep::Code) or without string contents.
struct Compiled {
    code: String,
    strings: String,
}

fn compiled(source: &str) -> Compiled {
    // The items are found on the code view, where every brace is real, and
    // removed from both views.
    let code = blank(source, Keep::Code);
    let strings = blank(source, Keep::Strings);
    Compiled {
        code: strip_test_items_like(&code, &code),
        strings: strip_test_items_like(&code, &strings),
    }
}

// strip_test_items_like removes from `other` every item a test-only
// attribute compiles out of `code`: the attribute, the attributes after it
// and the item, up to its `;` or its closing brace. `code` is blanked
// (Keep::Code), so braces in strings and comments are gone; the two texts are
// the same length, char for char.
fn strip_test_items_like(code: &str, other: &str) -> String {
    let code: Vec<char> = code.chars().collect();
    let other: Vec<char> = other.chars().collect();
    assert_eq!(
        code.len(),
        other.len(),
        "the blanked views differ in length"
    );
    let mut out = String::with_capacity(other.len());
    let mut index = 0;
    while index < code.len() {
        if code[index] == '#' && code.get(index + 1) == Some(&'[') {
            let end = attribute_end(&code, index);
            let attribute: String = code[index..end].iter().collect();
            if test_only_attribute(&attribute) {
                index = item_end(&code, end);
                out.push('\n');
                continue;
            }
            out.extend(&other[index..end]);
            index = end;
            continue;
        }
        out.push(other[index]);
        index += 1;
    }
    out
}

// module_declarations is every out-of-line `mod name;` of a compiled text,
// with its `#[path = "..."]` when it has one.
fn module_declarations(strings: &str) -> Vec<(String, Option<String>)> {
    let mut found = Vec::new();
    let mut search = 0;
    while let Some(offset) = strings[search..].find("mod ") {
        let start = search + offset;
        search = start + 4;
        let before = strings[..start].chars().next_back();
        if before.is_some_and(|c| c.is_alphanumeric() || c == '_') {
            continue;
        }
        let rest = &strings[start + 4..];
        let name: String = rest
            .trim_start()
            .chars()
            .take_while(|c| c.is_alphanumeric() || *c == '_')
            .collect();
        let after = rest.trim_start()[name.len()..].trim_start();
        if name.is_empty() || !after.starts_with(';') {
            continue;
        }
        // The attributes of this item: the text since the previous item end.
        let head_start = strings[..start].rfind([';', '}', '{']).map_or(0, |n| n + 1);
        let head = &strings[head_start..start];
        let path = head.find("#[path").and_then(|at| {
            let quoted = &head[at..];
            let open = quoted.find('"')?;
            let close = quoted[open + 1..].find('"')?;
            Some(quoted[open + 1..open + 1 + close].to_string())
        });
        found.push((name, path));
    }
    found
}

// crate_files is every file of the crate a non-test build compiles, by
// following the module declarations from the crate roots.
fn crate_files(root: &Path) -> BTreeMap<PathBuf, Compiled> {
    let mut files = BTreeMap::new();
    let mut queue: Vec<PathBuf> = ["src/lib.rs", "src/main.rs", "src/sandbox_main.rs"]
        .iter()
        .map(|file| root.join(file))
        .collect();
    while let Some(file) = queue.pop() {
        if files.contains_key(&file) {
            continue;
        }
        let source = std::fs::read_to_string(&file)
            .unwrap_or_else(|error| panic!("read {}: {error}", file.display()));
        let compiled = compiled(&source);
        let directory = file
            .parent()
            .expect("a source file has a directory")
            .to_path_buf();
        let is_module_root = matches!(
            file.file_name().and_then(|name| name.to_str()),
            Some("lib.rs" | "main.rs" | "mod.rs" | "sandbox_main.rs")
        );
        let children = if is_module_root {
            directory.clone()
        } else {
            directory.join(file.file_stem().expect("a source file has a stem"))
        };
        for (name, path) in module_declarations(&compiled.strings) {
            let child = if let Some(path) = path {
                directory.join(path)
            } else {
                let flat = children.join(format!("{name}.rs"));
                if flat.exists() {
                    flat
                } else {
                    children.join(name).join("mod.rs")
                }
            };
            assert!(
                child.exists(),
                "{} declares a module whose file {} does not exist: teach the scan this layout",
                file.display(),
                child.display()
            );
            queue.push(child);
        }
        files.insert(file, compiled);
    }
    files
}

// generated_servers maps each tonic server module tonic generated into
// OUT_DIR (`<service>_server`) to the routes its router answers.
fn generated_servers() -> BTreeMap<String, BTreeSet<String>> {
    let mut servers = BTreeMap::new();
    let out_dir = Path::new(env!("OUT_DIR"));
    let mut files: Vec<PathBuf> = std::fs::read_dir(out_dir)
        .expect("read OUT_DIR")
        .map(|entry| entry.expect("an OUT_DIR entry").path())
        .filter(|path| path.extension().is_some_and(|extension| extension == "rs"))
        .collect();
    files.sort();
    for file in files {
        let text = std::fs::read_to_string(&file).expect("read a generated file");
        for (module, body) in server_modules(&text) {
            let name_at = body
                .find("pub const SERVICE_NAME: &str = \"")
                .unwrap_or_else(|| panic!("{module} in {} has no SERVICE_NAME", file.display()));
            let rest = &body[name_at + "pub const SERVICE_NAME: &str = \"".len()..];
            let service = &rest[..rest.find('"').expect("SERVICE_NAME is a string")];
            let prefix = format!("\"/{service}/");
            let mut routes = BTreeSet::new();
            let mut search = 0;
            while let Some(offset) = body[search..].find(&prefix) {
                let start = search + offset + 1;
                let end = start + body[start..].find('"').expect("a route is a string");
                search = end;
                if body[end + 1..].trim_start().starts_with("=>") {
                    routes.insert(body[start..end].to_string());
                }
            }
            assert!(
                !routes.is_empty(),
                "{module} answers no route: the generated layout changed"
            );
            servers.insert(module, routes);
        }
    }
    assert!(!servers.is_empty(), "OUT_DIR holds no tonic server module");
    servers
}

// server_modules is each `pub mod <name>_server {` of a generated file with
// the text up to the next generated module.
fn server_modules(text: &str) -> Vec<(String, &str)> {
    let mut starts = Vec::new();
    let mut search = 0;
    while let Some(offset) = text[search..].find("pub mod ") {
        let start = search + offset;
        search = start + 8;
        let name: String = text[search..]
            .chars()
            .take_while(|c| c.is_alphanumeric() || *c == '_')
            .collect();
        starts.push((start, name));
    }
    let mut modules = Vec::new();
    for (index, (start, name)) in starts.iter().enumerate() {
        if !name.ends_with("_server") {
            continue;
        }
        let end = starts.get(index + 1).map_or(text.len(), |(next, _)| *next);
        modules.push((name.clone(), &text[*start..end]));
    }
    modules
}

// Scan is what the crate's non-test source builds, registers and serves.
struct Scan {
    // "file construct" -> how often.
    sites: BTreeMap<String, usize>,
    // The routes of every server module the source names.
    served: BTreeSet<String>,
}

fn count(code: &str, needle: &str) -> usize {
    code.match_indices(needle)
        .filter(|(at, _)| {
            // A needle that starts with a word char must start a word.
            !needle.starts_with(|c: char| c.is_alphanumeric())
                || !code[..*at]
                    .chars()
                    .next_back()
                    .is_some_and(|c| c.is_alphanumeric() || c == '_')
        })
        .count()
}

fn scan_files(
    files: &BTreeMap<String, String>,
    servers: &BTreeMap<String, BTreeSet<String>>,
) -> Scan {
    let mut scan = Scan {
        sites: BTreeMap::new(),
        served: BTreeSet::new(),
    };
    for (file, code) in files {
        for construct in SITE_CONSTRUCTS {
            let occurrences = count(code, construct);
            if occurrences > 0 {
                scan.sites
                    .insert(format!("{file} {construct}"), occurrences);
            }
        }
        for (module, routes) in servers {
            let occurrences = count(code, &format!("{module}::"));
            if occurrences > 0 {
                scan.sites.insert(format!("{file} {module}"), occurrences);
                scan.served.extend(routes.iter().cloned());
            }
        }
    }
    scan
}

fn check(scan: &Scan, entries: &[Entry]) -> Vec<String> {
    let mut problems = Vec::new();
    let mut listed = BTreeSet::new();
    let mut sites = BTreeMap::new();
    for entry in entries {
        if entry.kind == "grpc" {
            listed.insert(entry.name.clone());
            if entry.writes == "admin" && entry.decision != "main-route" {
                problems.push(format!(
                        "allowlist line {}: {} can change administration data without an authorization decision in elitea-main (decision {}; it must be main-route)",
                        entry.line, entry.name, entry.decision
                    ));
            }
            if entry.writes != "read" && entry.decision == "none" {
                problems.push(format!(
                    "allowlist line {}: {} writes with no decision",
                    entry.line, entry.name
                ));
            }
            if !scan.served.contains(&entry.name) {
                problems.push(format!(
                    "allowlist line {}: the worker does not serve {} (remove the line)",
                    entry.line, entry.name
                ));
            }
        } else {
            sites.insert(format!("{} {}", entry.listener, entry.name), entry);
        }
    }
    for route in &scan.served {
        if !listed.contains(route) {
            problems.push(format!(
                "the worker serves {route}, which is not on the allowlist: review what it writes, what decides it and who calls it, and list it"
            ));
        }
    }
    for (key, occurrences) in &scan.sites {
        match sites.get(key) {
            None => problems.push(format!(
                "{key} builds a listener or names a served tonic service and is not on the allowlist: review it and list it"
            )),
            Some(entry) if entry.count != *occurrences => problems.push(format!(
                "allowlist line {}: {key} occurs {occurrences} times, not {}: review the new one",
                entry.line, entry.count
            )),
            Some(_) => {}
        }
    }
    for (key, entry) in &sites {
        if !scan.sites.contains_key(key) {
            problems.push(format!(
                "allowlist line {}: no {key} (remove the line)",
                entry.line
            ));
        }
    }
    problems.sort();
    problems
}

// architecture_problems compares docs/ARCHITECTURE.md's table of internal
// methods that change administration data with the allowlist's admin lines,
// for this service.
fn architecture_problems(document: &str, entries: &[Entry]) -> Vec<String> {
    let begin = "<!-- internal-admin-writes:begin -->";
    let end = "<!-- internal-admin-writes:end -->";
    let heading = "\n## Internal listeners and administration data\n";
    let Some((_, section)) = document.split_once(heading) else {
        return vec![format!("docs/ARCHITECTURE.md has no {heading:?} section")];
    };
    let section = section.split("\n## ").next().unwrap_or_default();
    let Some(table) = section
        .split_once(begin)
        .and_then(|(_, rest)| rest.split_once(end))
    else {
        return vec![format!("the {begin} ... {end} table is not in the section")];
    };
    let mut documented = BTreeSet::new();
    for row in table
        .0
        .lines()
        .map(str::trim)
        .filter(|row| row.starts_with('|'))
    {
        let cells: Vec<&str> = row
            .trim_matches('|')
            .split('|')
            .map(|cell| cell.trim().trim_matches('`'))
            .collect();
        if cells.len() == 4 && cells[0] == SERVICE {
            documented.insert(format!("{} {} {}", cells[1], cells[2], cells[3]));
        }
    }
    let expected: BTreeSet<String> = entries
        .iter()
        .filter(|entry| entry.kind == "grpc" && entry.writes == "admin")
        .map(|entry| format!("{} {} {}", entry.listener, entry.name, entry.reason))
        .collect();
    let mut problems = Vec::new();
    for missing in expected.difference(&documented) {
        problems.push(format!("docs/ARCHITECTURE.md does not list {missing}"));
    }
    for extra in documented.difference(&expected) {
        problems.push(format!(
            "docs/ARCHITECTURE.md lists {extra}, which the allowlist does not mark admin"
        ));
    }
    problems
}

fn crate_scan() -> Scan {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let files: BTreeMap<String, String> = crate_files(root)
        .into_iter()
        .map(|(file, compiled)| {
            let relative = file
                .strip_prefix(root)
                .expect("a crate file")
                .to_string_lossy()
                .replace('\\', "/");
            (relative, compiled.code)
        })
        .collect();
    scan_files(&files, &generated_servers())
}

fn report(problems: &[String]) -> String {
    let mut out = String::new();
    for problem in problems {
        let _ = writeln!(out, "  {problem}");
    }
    out
}

#[test]
fn the_worker_serves_only_reviewed_methods_and_builds_only_reviewed_listeners() {
    let entries = parse_allowlist(ALLOWLIST).expect("the allowlist parses");
    let problems = check(&crate_scan(), &entries);
    assert!(
        problems.is_empty(),
        "the worker's internal listeners differ from the allowlist:\n{}",
        report(&problems)
    );
}

#[test]
fn every_allowlist_entry_gives_a_reason() {
    let entries = parse_allowlist(ALLOWLIST).expect("the allowlist parses");
    assert!(
        entries.iter().any(|entry| entry.kind == "grpc"),
        "the allowlist lists no method"
    );
    assert!(
        entries.iter().any(|entry| entry.kind == "site"),
        "the allowlist lists no site"
    );
    for entry in entries.iter().filter(|entry| entry.kind == "grpc") {
        assert!(
            entry.reason.split_whitespace().count() >= 2,
            "allowlist line {}: {} needs a reason (who calls it and what it writes)",
            entry.line,
            entry.name
        );
    }
    assert!(
        parse_allowlist("grpc | l | /s/M | write | claim | ").is_err(),
        "an empty reason parsed"
    );
}

#[test]
fn the_architecture_page_lists_the_worker_administration_writes() {
    let entries = parse_allowlist(ALLOWLIST).expect("the allowlist parses");
    let problems = architecture_problems(ARCHITECTURE, &entries);
    assert!(problems.is_empty(), "{}", report(&problems));
}

// The fixtures: each check must fail on what it exists to catch.

#[test]
fn the_check_fails_on_an_unlisted_fixture_method() {
    let entries = parse_allowlist(ALLOWLIST).expect("the allowlist parses");
    let mut scan = crate_scan();
    scan.served
        .insert("/fixture.admin.v1.AdminService/SetProviderPolicy".into());
    let problems = check(&scan, &entries);
    assert!(
        problems
            .iter()
            .any(|problem| problem.contains("/fixture.admin.v1.AdminService/SetProviderPolicy")),
        "an unlisted fixture method passed: {problems:?}"
    );
}

#[test]
fn the_check_fails_on_an_administration_write_main_does_not_decide() {
    let entries = parse_allowlist(
        "grpc | supervisor | /fixture.admin.v1.AdminService/SetProviderPolicy | admin | claim | the fixture",
    )
    .expect("the fixture parses");
    let scan = Scan {
        sites: BTreeMap::new(),
        served: BTreeSet::from(["/fixture.admin.v1.AdminService/SetProviderPolicy".to_string()]),
    };
    let problems = check(&scan, &entries);
    assert!(
        problems
            .iter()
            .any(|problem| problem.contains("without an authorization decision in elitea-main")),
        "an administration write decided by a grant passed: {problems:?}"
    );
}

const FIXTURE_SOURCE: &str = r#"
// Server::builder() in a comment is no site.
use crate::protocol::fixture_admin_service_server::FixtureAdminServiceServer;

pub async fn serve_admin() {
    let message = "TcpListener::bind in a string is no site";
    let listener = tokio::net::TcpListener::bind("0.0.0.0:0").await;
    let _ = tonic::transport::Server::builder()
        .add_service(FixtureAdminServiceServer::new(Admin))
        .serve_with_incoming(TcpListenerStream::new(listener));
    let _ = tonic::transport::Server::builder();
    let lifetime: &'static str = message;
}

#[cfg(test)]
mod tests {
    fn fake() { let _ = tonic::transport::Server::builder().add_service(x); }
}

#[cfg(all(test, feature = "sandbox-supervisor"))]
fn fake_listener() { let _ = std::os::unix::net::UnixListener::bind("/tmp/x"); }

#[cfg(not(test))]
fn real_listener() { let _ = std::os::unix::net::UnixListener::bind("/run/x"); }
"#;

#[test]
fn the_source_check_fails_on_unreviewed_fixture_listeners() {
    let files = BTreeMap::from([("src/fixture.rs".to_string(), compiled(FIXTURE_SOURCE).code)]);
    let servers = BTreeMap::from([(
        "fixture_admin_service_server".to_string(),
        BTreeSet::from(["/fixture.admin.v1.AdminService/SetProviderPolicy".to_string()]),
    )]);
    let scan = scan_files(&files, &servers);
    let expected = BTreeMap::from([
        ("src/fixture.rs Server::builder".to_string(), 2),
        ("src/fixture.rs .add_service(".to_string(), 1),
        ("src/fixture.rs TcpListener::bind".to_string(), 1),
        ("src/fixture.rs TcpListenerStream".to_string(), 1),
        ("src/fixture.rs serve_with_incoming".to_string(), 1),
        ("src/fixture.rs UnixListener::bind".to_string(), 1),
        ("src/fixture.rs fixture_admin_service_server".to_string(), 1),
    ]);
    assert_eq!(
        scan.sites, expected,
        "the scan read comments, strings or test-only items"
    );
    let entries = parse_allowlist(
        "site | src/fixture.rs | Server::builder | 1\nsite | src/gone.rs | TcpListener::bind | 1",
    )
    .expect("the fixture parses");
    let problems = check(&scan, &entries);
    for want in [
        "/fixture.admin.v1.AdminService/SetProviderPolicy, which is not on the allowlist",
        "src/fixture.rs Server::builder occurs 2 times, not 1",
        "src/fixture.rs .add_service( builds a listener",
        "src/fixture.rs fixture_admin_service_server builds a listener or names a served tonic service",
        "no src/gone.rs TcpListener::bind",
    ] {
        assert!(
            problems.iter().any(|problem| problem.contains(want)),
            "the source check missed {want}: {problems:?}"
        );
    }
}

#[test]
fn the_module_walk_skips_test_only_declarations_and_follows_paths() {
    let source = "pub mod kept;\n#[cfg(test)]\nmod skipped;\n#[cfg(feature = \"x\")]\n#[path = \"other.rs\"]\npub(crate) mod renamed;\n";
    let declared = module_declarations(&compiled(source).strings);
    assert_eq!(
        declared,
        vec![
            ("kept".to_string(), None),
            ("renamed".to_string(), Some("other.rs".to_string()))
        ]
    );
}
