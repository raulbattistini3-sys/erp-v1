//! polyvisor (erp-v1/supervisor): the cross-runtime supervisor the launcher hands the stack to.
//!
//! The launcher still decides *what* runs (programs, arguments, environment, ports, memory limits; the
//! `Spec`s built in main.rs); polyvisor decides *how*: one job object over the whole application, a named
//! job per service created suspended (so a child it forks, such as the API's tcc-v2.exe ingest process, is
//! inside the limit from its first instruction), start order by readiness, restarts, memory-pressure events
//! and admission. Without polyvisor.exe (beside sample-erp.exe) the launcher supervises the Specs itself.

use crate::config::{Config, Paths};
use crate::services::Spec;
use anyhow::Result;
use serde_json::{Map, Value, json};
use std::path::PathBuf;

/// Room for a service the launcher gives no memory limit (network, importer, vault: small Go services),
/// counted into the application's budget.
const UNLIMITED_ALLOWANCE_MB: u64 = 256;

/// The config polyvisor runs (data/polyvisor.json), written on every launch from the Specs.
pub fn write_config(paths: &Paths, cfg: &Config, specs: &[Spec]) -> Result<PathBuf> {
    let names: Vec<&str> = specs.iter().map(|s| s.name).collect();
    let mut services = Map::new();
    let mut budget_mb = 0;
    for s in specs {
        let env: Map<String, Value> = s.env.iter().map(|(k, v)| (k.clone(), Value::String(v.clone()))).collect();
        let mut svc = json!({
            "command": s.program.to_string_lossy(),
            "args": s.args,
            "env": env,
            "cwd": s.cwd.to_string_lossy(),
            // the stack is usable without the telemetry server, the cache and billing: those may wait when
            // the machine is short of memory (polyvisor's admission), the rest may not
            "required": matches!(s.name, "network" | "vault" | "importer" | "api"),
            "dependsOn": depends_on(s.name, &names),
            "restart": { "policy": "always", "backoff": "2s", "maxBackoff": "30s" },
            // the launcher waits 20 s for polyvisor itself; each service gets less, so a clean stop is the norm
            "stop": { "grace": "15s" },
        });
        // bun services shut down on CTRL_BREAK sent to their own process group (SIGBREAK); without a
        // signal polyvisor can only kill the tree on Windows
        if matches!(s.name, "api" | "worker" | "tcc") {
            svc["stop"]["signal"] = json!("break");
        }
        if s.memory_mb > 0 {
            budget_mb += s.memory_mb;
            // pressure is reported (and the runtime sheds) from 80%; the job object kills at the hard limit
            svc["memory"] = json!({ "softLimit": format!("{}MB", s.memory_mb * 4 / 5), "hardLimit": format!("{}MB", s.memory_mb) });
        } else {
            budget_mb += UNLIMITED_ALLOWANCE_MB;
        }
        if let Some(port) = ready_port(cfg, s.name) {
            svc["ready"] = json!({ "tcp": port.to_string(), "timeout": if s.name == "billing" { "180s" } else { "120s" } });
        }
        if s.name == "importer" {
            svc["scheduling"] = json!({ "priority": "background" });
        }
        if s.name == "worker" {
            // POLYVISOR_MAX_CONCURRENCY: jobs at once per queue; pressure pauses it at critical
            svc["scheduling"] = json!({ "priority": "background", "maxConcurrency": 3 });
        }
        services.insert(s.name.to_string(), svc);
    }
    let config = json!({
        "application": {
            "name": "sample-erp",
            "resources": { "memoryBudget": format!("{budget_mb}MB"), "reserveForSystem": "256MB" },
            "runtime": paths.polyvisor_runtime().to_string_lossy(),
            "events": paths.data.join("logs").join("polyvisor-events.ndjson").to_string_lossy(),
            "services": services,
        }
    });
    std::fs::create_dir_all(paths.data.join("logs"))?;
    let file = paths.data.join("polyvisor.json");
    std::fs::write(&file, serde_json::to_vec_pretty(&config)?)?;
    Ok(file)
}

/// The Spec the launcher supervises in place of the services: polyvisor running that config. No memory
/// limit of its own: it puts the services in their jobs (a job around polyvisor would cap them all at once).
pub fn spec(paths: &Paths, config: PathBuf) -> Spec {
    Spec {
        name: "polyvisor",
        program: paths.polyvisor(),
        args: vec!["run".into(), "-c".into(), config.to_string_lossy().into_owned()],
        env: vec![],
        cwd: paths.data.clone(),
        memory_mb: 0,
    }
}

/// Start order: everything registers with the network service, so it comes first; the API's telemetry goes
/// to the TCC from its first batch; the cache fronts the API.
fn depends_on(name: &str, present: &[&str]) -> Vec<&'static str> {
    let wanted: &[&'static str] = match name {
        "network" => &[],
        "api" => &["network", "tcc", "vault", "importer"],
        "cache" => &["network", "api"],
        // after the API: the queues' schema is created by the API's pg-boss start
        "worker" => &["network", "vault", "api"],
        _ => &["network"],
    };
    wanted.iter().copied().filter(|d| present.contains(d)).collect()
}

fn ready_port(cfg: &Config, name: &str) -> Option<u16> {
    Some(match name {
        "network" => cfg.network_port,
        "tcc" => cfg.tcc_port,
        "importer" => cfg.importer_port,
        "vault" => cfg.vault_port,
        "api" => cfg.api_port,
        "cache" => cfg.cache_port,
        "billing" => cfg.billing_port,
        _ => return None,
    })
}
