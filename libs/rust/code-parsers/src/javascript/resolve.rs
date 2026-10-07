//! The multi-file pass of `parse_multiple_files`: the global export index,
//! the method index, the symbol registry, and `_enhance_file_relationships`
//! with its `_resolve_import_path`.
//!
//! The enhancement of one file reads only what every file's first pass
//! produced (exports, symbols), never another file's enhanced
//! relationships, so files are enhanced in parallel; every registry is
//! filled and searched in the caller's order, as Python's dicts are.

use super::visitor::{ExportEntry, FileOutput, ImportBinding, file_stem};
use crate::model::{ParseResult, Relationship, RelationshipType, SymbolType};
use indexmap::IndexMap;
use rayon::prelude::*;
use serde_json::Value;
use std::collections::{HashMap, HashSet};

/// The read-only cross-file state every file's enhancement uses.
struct Index<'a> {
    files: &'a [String],
    /// The deepest directory holding every file of the pass: no import is
    /// looked up outside it.
    root: Option<String>,
    /// File path → index (`target_file in per_file_exports`).
    by_path: HashMap<&'a str, usize>,
    /// `per_file_exports`.
    exports: &'a [Vec<ExportEntry>],
    /// `global_exports`: export name → `(file, entry)`, file order.
    global_exports: HashMap<&'a str, Vec<(usize, usize)>>,
    /// `per_file_method_index`: per file, class name → method names.
    methods: Vec<HashMap<String, HashSet<String>>>,
    /// The files with a non-empty method index, in order (the dict's keys).
    method_files: Vec<usize>,
    /// `global_symbol_registry`: top-level name → first file.
    registry: HashMap<String, usize>,
}

impl Index<'_> {
    fn has_method(&self, file: usize, class: &str, member: &str) -> bool {
        self.methods[file]
            .get(class)
            .is_some_and(|m| m.contains(member))
    }
}

/// Run the multi-file pass; results come back in `files`' order.
pub(super) fn resolve_cross_file(files: &[String], outputs: Vec<FileOutput>) -> Vec<ParseResult> {
    let mut results = Vec::with_capacity(outputs.len());
    let mut imports = Vec::with_capacity(outputs.len());
    let mut exports = Vec::with_capacity(outputs.len());
    for output in outputs {
        results.push(output.result);
        imports.push(output.imports);
        exports.push(output.exports);
    }

    let mut global_exports: HashMap<&str, Vec<(usize, usize)>> = HashMap::new();
    for (file, entries) in exports.iter().enumerate() {
        for (entry, export) in entries.iter().enumerate() {
            global_exports
                .entry(export.name.as_str())
                .or_default()
                .push((file, entry));
        }
    }

    let mut methods = Vec::with_capacity(results.len());
    let mut method_files = Vec::new();
    let mut registry: HashMap<String, usize> = HashMap::new();
    for (file, result) in results.iter().enumerate() {
        let mut classes: HashMap<String, HashSet<String>> = HashMap::new();
        for symbol in &result.symbols {
            if symbol.symbol_type == SymbolType::Method
                && let Some(parent) = symbol.parent_symbol.as_deref().filter(|p| !p.is_empty())
            {
                let class = parent.rsplit('.').next().unwrap_or_default();
                classes
                    .entry(class.to_owned())
                    .or_default()
                    .insert(symbol.name.clone());
            }
        }
        if !classes.is_empty() {
            method_files.push(file);
        }
        methods.push(classes);

        let prefix = format!("{}.", file_stem(&result.file_path));
        for symbol in &result.symbols {
            if matches!(
                symbol.symbol_type,
                SymbolType::Class | SymbolType::Function | SymbolType::Constant
            ) && symbol
                .full_name
                .as_deref()
                .and_then(|f| f.strip_prefix(&prefix))
                == Some(symbol.name.as_str())
            {
                registry.entry(symbol.name.clone()).or_insert(file);
            }
        }
    }

    let index = Index {
        files,
        root: common_directory(files),
        by_path: files
            .iter()
            .enumerate()
            .map(|(i, f)| (f.as_str(), i))
            .collect(),
        exports: &exports,
        global_exports,
        methods,
        method_files,
        registry,
    };
    results
        .par_iter_mut()
        .zip(imports.par_iter())
        .enumerate()
        .for_each(|(file, (result, bindings))| enhance(file, result, bindings, &index));
    results
}

/// One resolved export candidate: its file and entry.
type Candidate<'a> = (usize, &'a ExportEntry);

/// `resolved_import_targets`: import local name → its export candidates.
fn resolve_bindings<'a>(
    file: usize,
    bindings: &'a IndexMap<String, ImportBinding>,
    index: &'a Index<'a>,
) -> HashMap<&'a str, Vec<Candidate<'a>>> {
    let mut resolved = HashMap::new();
    for (local, binding) in bindings {
        let target = resolve_import_path(
            &index.files[file],
            &binding.source_path,
            index.root.as_deref(),
        )
        .and_then(|t| index.by_path.get(t.as_str()).copied());
        let candidates: Vec<Candidate<'a>> = if let Some(target) = target {
            let entries = index.exports[target].iter().map(|e| (target, e));
            if binding.is_namespace {
                entries.collect()
            } else if binding.is_default {
                entries.filter(|(_, e)| e.is_default).collect()
            } else {
                let wanted = binding
                    .imported
                    .as_deref()
                    .filter(|i| !i.is_empty())
                    .unwrap_or(local);
                entries.filter(|(_, e)| e.name == wanted).collect()
            }
        } else {
            // Not a file of this pass (a package, an unresolvable path):
            // a name exported by exactly one file anywhere.
            let key = binding
                .imported
                .as_deref()
                .filter(|i| !i.is_empty())
                .unwrap_or(local);
            match index.global_exports.get(key) {
                Some(entries) if entries.len() == 1 => entries
                    .iter()
                    .map(|&(f, e)| (f, &index.exports[f][e]))
                    .collect(),
                _ => Vec::new(),
            }
        };
        if !candidates.is_empty() {
            resolved.insert(local.as_str(), candidates);
        }
    }
    resolved
}

fn annotate(rel: &mut Relationship, key: &str, value: impl Into<Value>) {
    rel.annotations.insert(key.to_owned(), value.into());
}

/// `annotations.setdefault(key, value)`.
fn annotate_default(rel: &mut Relationship, key: &str, value: &str) {
    rel.annotations
        .entry(key.to_owned())
        .or_insert_with(|| Value::from(value));
}

/// `_enhance_file_relationships` for one file.
fn enhance(
    file: usize,
    result: &mut ParseResult,
    bindings: &IndexMap<String, ImportBinding>,
    index: &Index<'_>,
) {
    let path = index.files[file].as_str();
    let resolved = resolve_bindings(file, bindings, index);
    let file_of = |c: &Candidate<'_>| index.files[c.0].clone();

    for rel in &mut result.relationships {
        match rel.relationship_type {
            RelationshipType::Imports => {
                if let Some(candidates) = resolved.get(rel.target_symbol.as_str()) {
                    rel.target_file = Some(file_of(&candidates[0]));
                }
            }
            RelationshipType::References
            | RelationshipType::Calls
            | RelationshipType::Inheritance => {
                let root = rel.target_symbol.split('.').next().unwrap_or_default();
                let Some(candidates) = resolved.get(root) else {
                    continue;
                };
                rel.target_file = Some(file_of(&candidates[0]));
                let is_call = rel.relationship_type == RelationshipType::Calls;
                if rel.target_symbol.contains('.') {
                    let parts: Vec<&str> = rel.target_symbol.split('.').collect();
                    if let [ns, member] = parts[..] {
                        let is_namespace =
                            bindings.values().any(|b| b.local == ns && b.is_namespace);
                        let exported = candidates.iter().any(|(_, e)| e.name == member);
                        if is_namespace || exported {
                            let (ns, member) = (ns.to_owned(), member.to_owned());
                            annotate(rel, "resolved_member", member);
                            annotate(rel, "namespace", ns);
                            annotate(rel, "resolution", "namespace_member");
                            if is_call {
                                annotate(rel, "call_kind", "namespace_function");
                            }
                        }
                    }
                } else {
                    let first = candidates[0].1;
                    annotate(rel, "resolution", "import_binding");
                    let symbol = if first.internal_symbol.is_empty() {
                        first.name.clone()
                    } else {
                        first.internal_symbol.clone()
                    };
                    annotate(rel, "resolved_symbol", symbol);
                    if first.is_default {
                        annotate(rel, "default_export", true);
                    }
                    if is_call {
                        annotate(rel, "call_kind", "imported_function");
                    }
                }
            }
            _ => {}
        }
    }

    // The global registry catches a bare reference no binding resolved.
    if !index.registry.is_empty() {
        for rel in &mut result.relationships {
            if rel.relationship_type == RelationshipType::References
                && rel.target_file.is_none()
                && !rel.target_symbol.contains('.')
                && let Some(&found) = index.registry.get(&rel.target_symbol)
                && index.files[found] != path
            {
                rel.target_file = Some(index.files[found].clone());
                annotate(rel, "resolution", "global_registry");
            }
        }
    }

    // Deduplicate on (type, source, target, target file); first wins.
    let mut seen = HashSet::new();
    result.relationships.retain(|r| {
        seen.insert((
            r.relationship_type,
            r.source_symbol.clone(),
            r.target_symbol.clone(),
            r.target_file.clone(),
        ))
    });

    resolve_method_calls(file, result, index);
}

/// The second pass of `_enhance_file_relationships`: dotted calls against
/// the class → method index.
///
/// Python quirk: for `this.m()` and `super.m()` the class is the FIRST
/// segment of the call's source — the module name, not the class — so they
/// resolve only in a file named after its class (`Dog.js` with
/// `class Dog`).
fn resolve_method_calls(file: usize, result: &mut ParseResult, index: &Index<'_>) {
    let path = index.files[file].as_str();
    let mut inheritance: HashMap<String, String> = HashMap::new();
    for rel in &result.relationships {
        if rel.relationship_type == RelationshipType::Inheritance && rel.source_file == path {
            inheritance.insert(rel.source_symbol.clone(), rel.target_symbol.clone());
        }
    }
    let all_files = 0..index.files.len();
    for rel in &mut result.relationships {
        if rel.relationship_type != RelationshipType::Calls {
            continue;
        }
        let Some((root, member)) = rel.target_symbol.split_once('.') else {
            continue;
        };
        let (root, member) = (root.to_owned(), member.to_owned());
        let class = rel
            .source_symbol
            .split('.')
            .next()
            .unwrap_or_default()
            .to_owned();
        let mark = |rel: &mut Relationship, kind: &str, class: &str| {
            annotate_default(rel, "resolution", kind);
            annotate(rel, "call_kind", kind);
            annotate(rel, "resolved_member", member.clone());
            annotate(rel, "resolved_class", class.to_owned());
        };
        match root.as_str() {
            "this" => {
                if index.has_method(file, &class, &member) {
                    mark(rel, "instance_method", &class);
                } else if let Some(base) = inheritance.get(&class)
                    && let Some(found) = all_files
                        .clone()
                        .find(|&f| f != file && index.has_method(f, base, &member))
                {
                    mark(rel, "inherited_method", base);
                    rel.target_file = Some(index.files[found].clone());
                }
            }
            "super" => {
                if let Some(base) = inheritance.get(&class)
                    && let Some(found) = all_files
                        .clone()
                        .find(|&f| index.has_method(f, base, &member))
                {
                    mark(rel, "super_method", base);
                    rel.target_file = Some(index.files[found].clone());
                }
            }
            _ => {
                if index.has_method(file, &root, &member) {
                    mark(rel, "class_method", &root);
                } else if let Some(&found) = index
                    .method_files
                    .iter()
                    .find(|&&f| index.has_method(f, &root, &member))
                {
                    mark(rel, "class_method", &root);
                    if rel.target_file.is_none() {
                        rel.target_file = Some(index.files[found].clone());
                    }
                }
            }
        }
    }
}

/// A path as `pathlib.PurePosixPath` holds it: no empty or `.` segments,
/// `..` kept.
#[derive(Debug, Clone, PartialEq, Eq)]
struct PyPath {
    absolute: bool,
    parts: Vec<String>,
}

impl PyPath {
    fn parse(text: &str) -> Self {
        Self {
            absolute: text.starts_with('/'),
            parts: text
                .split('/')
                .filter(|p| !p.is_empty() && *p != ".")
                .map(str::to_owned)
                .collect(),
        }
    }

    /// `str(path)`.
    fn render(&self) -> String {
        let joined = self.parts.join("/");
        if self.absolute {
            format!("/{joined}")
        } else if joined.is_empty() {
            ".".to_owned()
        } else {
            joined
        }
    }

    /// `path.parent`.
    fn parent(&self) -> Self {
        let mut parent = self.clone();
        parent.parts.pop();
        parent
    }

    /// `path / other`.
    fn join(&self, other: &str) -> Self {
        let other = Self::parse(other);
        if other.absolute {
            return other;
        }
        let mut joined = self.clone();
        joined.parts.extend(other.parts);
        joined
    }

    /// Python 3.12's `with_suffix(ext)`: the suffix is the name from its
    /// last dot, unless that dot leads or ends the name. `None` where Python
    /// raises (an empty name).
    fn with_suffix(&self, ext: &str) -> Option<String> {
        let name = self.parts.last()?;
        let stem = match name.rfind('.') {
            Some(i) if i > 0 && i < name.len() - 1 => &name[..i],
            _ => name.as_str(),
        };
        let mut changed = self.clone();
        *changed.parts.last_mut()? = format!("{stem}{ext}");
        Some(changed.render())
    }
}

/// `_resolve_import_path`: a relative specifier (`./`, `../`) as a file
/// that exists, trying the path itself, then `.js`, `.jsx`, `.mjs`, `.cjs`
/// in place of its suffix, then `index.js` / `index.jsx` in it as a
/// directory — first on the joined path, then on its resolved form.
///
/// Python quirk: the joined path keeps `..` (pathlib does not collapse it),
/// so `../x` is found as `dir/../x.js`, a string no file of the pass has;
/// such an import resolves only through the global export index.
///
/// Deliberate difference: Python asks the file system about any path,
/// following symlinks (`is_file`, `realpath`). Here a path is looked up
/// only inside `root` (the directory of the pass's files; `None` finds
/// nothing), one component at a time without following a symlink, and the
/// resolved form collapses `..` by name. Where no symlink is involved and
/// the path stays inside `root`, the answer is Python's; any other path is
/// not a file of the pass, so its import resolves through the global
/// export index as before.
pub(super) fn resolve_import_path(
    from_file: &str,
    source_path: &str,
    root: Option<&str>,
) -> Option<String> {
    if !(source_path.starts_with("./") || source_path.starts_with("../")) {
        return None;
    }
    let root = root?;
    let candidate = PyPath::parse(from_file).parent().join(source_path);
    let raw = candidate.render();
    let mut variants = vec![raw.clone()];
    if let Some(resolved) = collapse_parents(&raw)
        && resolved != raw
    {
        variants.push(resolved);
    }
    let is_file = |p: &str| lstat_within(root, p).is_some_and(|m| m.is_file());
    for variant in variants {
        if is_file(&variant) {
            return Some(variant);
        }
        let path = PyPath::parse(&variant);
        for ext in [".js", ".jsx", ".mjs", ".cjs"] {
            if let Some(p) = path.with_suffix(ext)
                && is_file(&p)
            {
                return Some(p);
            }
        }
        if lstat_within(root, &variant).is_some_and(|m| m.is_dir()) {
            for ext in [".js", ".jsx"] {
                let p = path.join(&format!("index{ext}")).render();
                if is_file(&p) {
                    return Some(p);
                }
            }
        }
    }
    None
}

/// The deepest directory that holds every one of `files` (absolute paths),
/// or `None` for no files or relative ones.
fn common_directory(files: &[String]) -> Option<String> {
    let mut common: Option<Vec<&str>> = None;
    for file in files {
        let directory = file
            .strip_prefix('/')?
            .rsplit_once('/')
            .map_or("", |(d, _)| d);
        let parts: Vec<&str> = directory.split('/').filter(|p| !p.is_empty()).collect();
        common = Some(match common {
            None => parts,
            Some(previous) => previous
                .iter()
                .zip(&parts)
                .take_while(|(a, b)| a == b)
                .map(|(a, _)| *a)
                .collect(),
        });
    }
    common.map(|parts| format!("/{}", parts.join("/")))
}

/// What the file system says about `path`, walked from `root` one name at a
/// time as the kernel would (`..` leaves a directory, a missing or
/// non-directory step fails), but never through a symlink and never above
/// `root`. `None` when the path is outside `root`, missing, or reached
/// through a symlink.
fn lstat_within(root: &str, path: &str) -> Option<std::fs::Metadata> {
    let rest = if root == "/" {
        path.strip_prefix('/')?
    } else {
        path.strip_prefix(root)?.strip_prefix('/')?
    };
    let mut current = std::path::PathBuf::from(root);
    let mut depth = 0usize;
    let mut meta = std::fs::symlink_metadata(&current).ok()?;
    if meta.file_type().is_symlink() {
        return None;
    }
    for name in rest.split('/') {
        match name {
            "" | "." => {}
            ".." => {
                // Leaving a directory: the step must be one, and inside root.
                if !meta.is_dir() || depth == 0 {
                    return None;
                }
                current.pop();
                depth -= 1;
                meta = std::fs::symlink_metadata(&current).ok()?;
            }
            _ => {
                if !meta.is_dir() {
                    return None;
                }
                current.push(name);
                depth += 1;
                meta = std::fs::symlink_metadata(&current).ok()?;
                if meta.file_type().is_symlink() {
                    return None;
                }
            }
        }
    }
    Some(meta)
}

/// `os.path.realpath` of an absolute path with no symlink on it: `.` and
/// `..` collapsed by name (`/..` is `/`). `None` for a relative path (its
/// resolved form is never a file of the pass).
fn collapse_parents(path: &str) -> Option<String> {
    let rest = path.strip_prefix('/')?;
    let mut parts: Vec<&str> = Vec::new();
    for name in rest.split('/') {
        match name {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            _ => parts.push(name),
        }
    }
    Some(format!("/{}", parts.join("/")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pathlib_joins_and_suffixes() {
        let base = PyPath::parse("/a//b/./c.js").parent();
        assert_eq!(base.render(), "/a/b");
        assert_eq!(base.join("./x").render(), "/a/b/x");
        assert_eq!(base.join("../x/").render(), "/a/b/../x");
        assert_eq!(
            base.join("./x").with_suffix(".js").as_deref(),
            Some("/a/b/x.js")
        );
        assert_eq!(
            base.join("./x.min").with_suffix(".js").as_deref(),
            Some("/a/b/x.js")
        );
        assert_eq!(
            base.join("./.env").with_suffix(".js").as_deref(),
            Some("/a/b/.env.js")
        );
        assert_eq!(
            base.join("./x.").with_suffix(".js").as_deref(),
            Some("/a/b/x..js")
        );
        assert_eq!(
            base.join("..").with_suffix(".js").as_deref(),
            Some("/a/b/...js")
        );
        assert_eq!(PyPath::parse("/").with_suffix(".js"), None);
    }

    #[test]
    fn parent_segments_collapse_by_name() {
        assert_eq!(
            collapse_parents("/nonexistent-dwjs/a/../b").as_deref(),
            Some("/nonexistent-dwjs/b")
        );
        assert_eq!(collapse_parents("/..").as_deref(), Some("/"));
        assert_eq!(collapse_parents("a/../b"), None);
    }

    #[test]
    fn the_common_directory_of_the_files() {
        let files = |list: &[&str]| list.iter().map(|f| (*f).to_owned()).collect::<Vec<_>>();
        assert_eq!(
            common_directory(&files(&["/r/a/x.js", "/r/a/b/y.js", "/r/a/c/z.js"])).as_deref(),
            Some("/r/a")
        );
        assert_eq!(common_directory(&files(&["/x.js"])).as_deref(), Some("/"));
        assert_eq!(
            common_directory(&files(&["/r/a/x.js", "/s/y.js"])).as_deref(),
            Some("/")
        );
        assert_eq!(common_directory(&files(&["rel/x.js"])), None);
        assert_eq!(common_directory(&[]), None);
    }
}
