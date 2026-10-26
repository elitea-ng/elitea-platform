//! `elitea-doc-extract-sidecar`: one request on stdin, one JSON answer on
//! stdout. See the library for the protocol.

use std::io::Write;

fn main() {
    elitea_doc_extract_sidecar::limit_memory();
    let answer = elitea_doc_extract_sidecar::handle(std::io::stdin().lock());
    let mut out = std::io::stdout().lock();
    let _ = out.write_all(answer.to_json().as_bytes());
    let _ = out.flush();
}
