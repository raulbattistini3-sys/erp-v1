//! Hands the services to the polivisor (erp-v1/polivisor, bin/polivisor.exe): the launcher only bootstraps
//! (Postgres, migrations, certificates, secrets, the web build), describes each service as a unit with its
//! dependencies, readiness port and memory cap, writes data/polivisor.json and runs the polivisor until it
//! exits. Ordering, readiness gating, restarts, Job Object limits and shutdown live there.

use anyhow::{Context, Result};
use serde_json::{Value, json};
use std::{collections::BTreeMap, path::{Path, PathBuf}, process::Stdio};
use tokio::process::Command;

pub struct Spec {
    pub name: &'static str,
    pub program: PathBuf,
    pub args: Vec<String>,
    pub env: Vec<(String, String)>,
    pub cwd: PathBuf,
}

/// How a service sits in the stack.
pub struct Unit {
    pub spec: Spec,
    /// must be ready before this one starts; if they give up, this one is not started
    pub depends_on: &'static [&'static str],
    /// started after these settle, but runs even if they failed
    pub after: &'static [&'static str],
    /// loopback port that answers once the service is ready
    pub ready_port: Option<u16>,
    /// giving up on it stops the whole stack
    pub critical: bool,
    /// hard cap on the whole process tree (Job Object), MB; 0 = none
    pub memory_mb: u32,
    /// wait for this much free RAM before (re)starting, MB
    pub min_free_mb: u32,
    /// node | java | bun | go — tells the polivisor which heap flags apply
    pub runtime: &'static str,
    pub heap_mb: u32,
}

pub fn write_config(file: &Path, control_port: u16, grace_secs: u64, units: &[Unit]) -> Result<()> {
    let services: Vec<Value> = units
        .iter()
        .map(|u| {
            let env: BTreeMap<_, _> = u.spec.env.iter().cloned().collect();
            let mut s = json!({
                "name": u.spec.name,
                "program": u.spec.program,
                "args": u.spec.args,
                "env": env,
                "cwd": u.spec.cwd,
                "depends_on": u.depends_on,
                "after": u.after,
                "critical": u.critical,
                "memory_mb": u.memory_mb,
                "min_available_mb": u.min_free_mb,
                "runtime": { "kind": u.runtime, "heap_mb": u.heap_mb },
                "restart": { "max": 5, "window": "5m", "backoff_min": "2s", "backoff_max": "30s" },
            });
            if let Some(p) = u.ready_port {
                s["ready"] = json!({ "tcp": format!("127.0.0.1:{p}"), "timeout": "120s" });
            }
            s
        })
        .collect();
    let doc = json!({
        "control": format!("127.0.0.1:{control_port}"),
        "grace": format!("{grace_secs}s"),
        "services": services,
    });
    // holds secrets (tokens, JWT key) like config/: keep it in data/, never in the bin folder
    std::fs::write(file, serde_json::to_vec_pretty(&doc)?).with_context(|| format!("writing {}", file.display()))
}

/// Runs the polivisor in the foreground with this console. Ctrl+C reaches it directly (same console
/// group); the launcher keeps waiting so Postgres is only stopped after every service is down.
pub async fn run(exe: &Path, config: &Path) -> Result<()> {
    let mut child = Command::new(exe)
        .args(["run", "--config"])
        .arg(config)
        .stdin(Stdio::null())
        .spawn()
        .with_context(|| format!("starting {}", exe.display()))?;
    loop {
        tokio::select! {
            status = child.wait() => {
                let status = status?;
                if status.success() {
                    return Ok(());
                }
                anyhow::bail!("the polivisor stopped the stack ({status})");
            }
            _ = tokio::signal::ctrl_c() => {
                crate::log("launcher", "Ctrl+C: waiting for the polivisor to stop the services");
            }
        }
    }
}
