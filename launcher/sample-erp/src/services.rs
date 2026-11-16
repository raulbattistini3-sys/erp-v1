//! Runs each service as a child process, prefixes its output with its name, and restarts it when it
//! dies (docker-compose.yml's `restart: unless-stopped`).

use std::{
    path::PathBuf,
    process::Stdio,
    time::{Duration, Instant},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncRead, BufReader},
    process::Command,
    sync::watch,
    time::{sleep, timeout},
};

pub struct Spec {
    pub name: &'static str,
    pub program: PathBuf,
    pub args: Vec<String>,
    pub env: Vec<(String, String)>,
    pub cwd: PathBuf,
    /// Memory limit of the process in MB (0: none): on Windows a job object of its own with
    /// JOB_OBJECT_LIMIT_PROCESS_MEMORY, nested in the launcher's kill-on-close job. An allocation over
    /// it fails, the service dies and is restarted like any other crash.
    pub memory_mb: u64,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum State {
    Running,
    /// Ctrl+C: the console delivered it to every child too, so they get time to finish their work.
    Interrupted,
    /// The launcher is giving up for another reason; the children never heard of it.
    Aborted,
}

const GRACE: Duration = Duration::from_secs(20);

pub async fn supervise(spec: Spec, mut state: watch::Receiver<State>) {
    let mut backoff = Duration::from_secs(2);
    #[allow(unused_mut, unused_variables)]
    let mut limited = false;
    #[allow(unused_mut, unused_variables)]
    let mut starts = 0u32;
    loop {
        if *state.borrow() != State::Running {
            return;
        }
        // the memory-limit job's name, handed to the service so its telemetry can read the job
        #[cfg(windows)]
        let job = (spec.memory_mb > 0).then(|| crate::windows::job_name(spec.name, starts));
        #[cfg(not(windows))]
        let job: Option<String> = None;
        starts += 1;
        let mut command = Command::new(&spec.program);
        // limited: started suspended, resumed once it is in its job (see windows::CREATE_SUSPENDED)
        #[cfg(windows)]
        if spec.memory_mb > 0 {
            command.creation_flags(crate::windows::CREATE_SUSPENDED);
        }
        let mut child = match command
            .args(&spec.args)
            .envs(spec.env.iter().map(|(k, v)| (k, v)))
            .envs(job.iter().map(|n| ("WIN_TELEMETRY_JOB", n)))
            .current_dir(&spec.cwd)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true)
            .spawn()
        {
            Ok(child) => child,
            Err(e) => {
                crate::log(spec.name, &format!("could not start {}: {e}", spec.program.display()));
                return;
            }
        };
        forward(spec.name, child.stdout.take());
        forward(spec.name, child.stderr.take());
        // held until the child is gone: the job's name (WIN_TELEMETRY_JOB) lasts only while a handle does
        #[cfg(windows)]
        let _job = match child.id().filter(|_| spec.memory_mb > 0) {
            None => None,
            Some(pid) => match crate::windows::limit_memory(pid, spec.memory_mb, job.as_deref()) {
                Err(e) => {
                    crate::log(spec.name, &format!("memory limit of {} MB not applied: {e}", spec.memory_mb));
                    None
                }
                Ok(handle) => {
                    if !limited {
                        crate::log(spec.name, &format!("memory limit {} MB (job object)", spec.memory_mb));
                        limited = true;
                    }
                    Some(handle)
                }
            },
        };
        #[cfg(windows)]
        if let Some(pid) = child.id().filter(|_| spec.memory_mb > 0) {
            if let Err(e) = crate::windows::resume(pid) {
                crate::log(spec.name, &format!("could not resume the process: {e}"));
                let _ = child.kill().await;
            }
        }
        let started = Instant::now();

        tokio::select! {
            status = child.wait() => {
                if *state.borrow() != State::Running {
                    return;
                }
                if started.elapsed() > Duration::from_secs(60) {
                    backoff = Duration::from_secs(2);
                }
                let status = status.map(|s| s.to_string()).unwrap_or_else(|e| e.to_string());
                crate::log(spec.name, &format!("stopped ({status}), restarting in {}s", backoff.as_secs()));
                tokio::select! {
                    _ = sleep(backoff) => {}
                    _ = state.changed() => return,
                }
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
            _ = state.changed() => {
                let graceful = *state.borrow() == State::Interrupted;
                if !graceful || timeout(GRACE, child.wait()).await.is_err() {
                    let _ = child.kill().await;
                }
                return;
            }
        }
    }
}

fn forward(name: &'static str, stream: Option<impl AsyncRead + Unpin + Send + 'static>) {
    let Some(stream) = stream else { return };
    tokio::spawn(async move {
        let mut lines = BufReader::new(stream).split(b'\n');
        while let Ok(Some(line)) = lines.next_segment().await {
            let line = String::from_utf8_lossy(&line);
            let line = line.trim_end_matches('\r');
            if name == "polyvisor" && line.contains(" | ") {
                // already "service | line" from polyvisor
                println!("{line}");
            } else {
                crate::log(name, line);
            }
        }
    });
}
