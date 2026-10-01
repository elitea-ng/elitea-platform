use base64::{Engine, engine::general_purpose::STANDARD};
use flate2::read::GzDecoder;
use futures::{executor::block_on, future::join_all};
use serde::Deserialize;
use std::{io::Read, time::Instant};

#[derive(Deserialize)]
struct Transaction(u64, usize, i64, i64);
#[derive(Default)]
struct Ledger { totals: [i64; 5], accepted: u64, rejected: u64, refunds: u64, weighted: i64 }
trait Reducer {
    fn append(&mut self, row: &Transaction) -> Result<(), Box<dyn std::error::Error>>;
    fn merge(&mut self, other: Self);
}
impl Reducer for Ledger {
    fn append(&mut self, row: &Transaction) -> Result<(), Box<dyn std::error::Error>> {
        if row.1 >= 5 || row.2 <= 0 || !(-1..=1).contains(&row.3) { return Err("Invalid transaction".into()); }
        self.totals[row.1] += row.2 * row.3;
        self.weighted += (row.0 as i64 + 1) * row.2 * row.3;
        if row.3 == 0 { self.rejected += 1; }
        else { self.accepted += 1; if row.3 < 0 { self.refunds += 1; } }
        Ok(())
    }
    fn merge(&mut self, other: Self) {
        for (index, value) in other.totals.into_iter().enumerate() { self.totals[index] += value; }
        self.accepted += other.accepted; self.rejected += other.rejected;
        self.refunds += other.refunds; self.weighted += other.weighted;
    }
}
async fn reduce(rows: &[Transaction]) -> Result<Ledger, Box<dyn std::error::Error>> {
    let mut ledger = Ledger::default();
    for row in rows { ledger.append(row)?; }
    Ok(ledger)
}
async fn reconcile(rows: &[Transaction]) -> Result<Ledger, Box<dyn std::error::Error>> {
    let mut ledger = Ledger::default();
    for partial in join_all(rows.chunks(500).map(reduce)).await { ledger.merge(partial?); }
    Ok(ledger)
}
pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let started = Instant::now();
    let data = &state["typescript_data"];
    let compressed = STANDARD.decode(data["payload"].as_str().ok_or("Missing payload")?)?;
    let mut raw = Vec::new();
    GzDecoder::new(compressed.as_slice()).take(2 * 1024 * 1024).read_to_end(&mut raw)?;
    let rows: Vec<Transaction> = serde_json::from_slice(&raw)?;
    if rows.len() as u64 != data["rows"].as_u64().ok_or("Missing row count")?
        || rows.iter().enumerate().any(|(i, row)| row.0 != i as u64) { return Err("Record identities changed".into()); }
    let ledger = block_on(reconcile(&rows))?;
    if serde_json::json!(ledger.totals) != data["totals"] || serde_json::json!(ledger.weighted) != data["weighted"]
        || serde_json::json!(ledger.accepted) != data["accepted"] || serde_json::json!(ledger.rejected) != data["rejected"]
        || serde_json::json!(ledger.refunds) != data["refunds"] { return Err("Cross-language reconciliation failed".into()); }
    let mut metrics = data["metrics"].as_array().ok_or("Missing metrics")?.clone();
    metrics.push(serde_json::json!({"stage":"rust", "processing_ms":started.elapsed().as_secs_f64()*1000.0,
        "raw_bytes":raw.len(), "compressed_bytes":compressed.len()}));
    Ok(serde_json::json!({"status":"PASS", "rows":rows.len(), "seed":data["seed"],
        "accepted":ledger.accepted,"rejected":ledger.rejected,"refunds":ledger.refunds,
        "totals":ledger.totals,"total_cents":ledger.totals.iter().sum::<i64>(),"weighted":ledger.weighted,
        "sha256":data["sha256"],"merchants":data["merchants"],"first_day":data["first_day"],"last_day":data["last_day"],
        "metrics":metrics}))
}
