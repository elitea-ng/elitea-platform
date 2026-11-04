"""Evidence redaction (DESIGN §3.8). Collectors store ids, counts, digests and states only; this module is
the last line: it refuses a run directory that contains credential-shaped text."""
import pathlib
import re

# PEM blocks, JWT-shaped bearer tokens (base64url JSON header "eyJ"), mock credentials, and cookie values.
PATTERNS = {
    'pem': re.compile(rb'-----BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE)-----'),
    'jwt': re.compile(rb'eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.'),
    'mock_key': re.compile(rb'mock-key-[A-Za-z0-9]'),
    'session_cookie': re.compile(rb'elitea_session=[A-Za-z0-9%_.-]{8,}'),
    'bearer': re.compile(rb'(?i)authorization:\s*bearer\s+\S{8,}'),
}
SCANNED_SUFFIXES = {'.json', '.log', '.txt', '.md', '.jsonl', '.tsv', '.sql'}
MAX_SCAN_BYTES = 64 * 1024 * 1024


def scan_bytes(data):
    return sorted(name for name, pattern in PATTERNS.items() if pattern.search(data))


def scan_tree(root):
    """Return [(relative path, [pattern names])] for every offending file under root."""
    root = pathlib.Path(root)
    hits = []
    for path in sorted(root.rglob('*')):
        if not path.is_file() or path.suffix not in SCANNED_SUFFIXES:
            continue
        data = path.read_bytes()[:MAX_SCAN_BYTES]
        names = scan_bytes(data)
        if names:
            hits.append((str(path.relative_to(root)), names))
    return hits


def scrub_text(text):
    """Replace credential-shaped substrings in free text (bounded log excerpts)."""
    out = text.encode('utf-8', 'replace')
    for name, pattern in PATTERNS.items():
        out = pattern.sub(b'<redacted:' + name.encode() + b'>', out)
    return out.decode('utf-8', 'replace')
