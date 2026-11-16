//! Windows-only process plumbing.

use std::{io, mem, ptr};
use windows_sys::Win32::{
    Foundation::{CloseHandle, HANDLE},
    System::{
        Console::GetConsoleProcessList,
        JobObjects::{
            AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_BREAKAWAY_OK, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
            JOB_OBJECT_LIMIT_PROCESS_MEMORY, JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
            SetInformationJobObject,
        },
        Threading::{GetCurrentProcess, OpenProcess, PROCESS_SET_QUOTA, PROCESS_TERMINATE},
    },
};

/// Process creation flag for children that must outlive the launcher (the browser it opens).
pub const CREATE_BREAKAWAY_FROM_JOB: u32 = 0x0100_0000;

/// Puts the launcher in a job object that kills every process in it when the launcher goes away,
/// so closing the console window or a crash never leaves api.exe, java or postgres running. The
/// handle is deliberately leaked: it closes, and the job fires, when this process ends.
pub fn kill_children_with_launcher() {
    unsafe {
        let job = CreateJobObjectW(ptr::null(), ptr::null());
        if job.is_null() {
            return;
        }
        let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = mem::zeroed();
        info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | JOB_OBJECT_LIMIT_BREAKAWAY_OK;
        let ok = SetInformationJobObject(
            job,
            JobObjectExtendedLimitInformation,
            (&raw const info).cast(),
            mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
        );
        if ok != 0 {
            AssignProcessToJobObject(job, GetCurrentProcess());
        }
    }
}

/// Puts a child in a job object of its own whose processes may commit at most `mb` MB each. The job
/// nests inside the launcher's (Windows 8+), so the kill-on-close still reaches it. The handle is
/// returned: the job lives as long as its process, but its name only while a handle is open, so
/// the caller keeps the [`Job`] until the process is gone. Its children (the API's IPC telemetry
/// child) inherit the job.
///
/// `name` (see [`job_name`]) is how the service finds the job again: a Bun executable puts itself in
/// a kill-on-close job of its own, so the innermost job it sees with QueryInformationJobObject(NULL)
/// is Bun's, without the limit. The launcher passes the name down as WIN_TELEMETRY_JOB and
/// win-telemetry's jobInfo(name) opens it (JOB_OBJECT_QUERY) whatever the nesting.
pub fn limit_memory(pid: u32, mb: u64, name: Option<&str>) -> io::Result<Job> {
    unsafe {
        let process = OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid);
        if process.is_null() {
            return Err(io::Error::last_os_error());
        }
        let wide: Option<Vec<u16>> = name.map(|n| n.encode_utf16().chain(Some(0)).collect());
        let job = CreateJobObjectW(ptr::null(), wide.as_ref().map_or(ptr::null(), |w| w.as_ptr()));
        if job.is_null() {
            let e = io::Error::last_os_error();
            CloseHandle(process);
            return Err(e);
        }
        let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = mem::zeroed();
        info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_PROCESS_MEMORY;
        info.ProcessMemoryLimit = (mb as usize) << 20;
        let set = SetInformationJobObject(
            job,
            JobObjectExtendedLimitInformation,
            (&raw const info).cast(),
            mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
        );
        let result = if set == 0 || AssignProcessToJobObject(job, process) == 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        };
        CloseHandle(process);
        let job = Job(job);
        result.map(|()| job)
    }
}

/// Process creation flag: the child's first thread waits for [`resume`], so it is in its memory-limit
/// job before it runs a single instruction, and so is every process it starts (the API spawns its
/// telemetry ingest child within milliseconds; spawned before the job, that child would sit outside it
/// and see no limit).
pub const CREATE_SUSPENDED: u32 = 0x0000_0004;

#[link(name = "ntdll")]
unsafe extern "system" {
    fn NtResumeProcess(process: HANDLE) -> i32;
}

/// Resumes a child started with [`CREATE_SUSPENDED`] (all of its threads; there is only the first).
pub fn resume(pid: u32) -> io::Result<()> {
    const PROCESS_SUSPEND_RESUME: u32 = 0x0800;
    unsafe {
        let process = OpenProcess(PROCESS_SUSPEND_RESUME, 0, pid);
        if process.is_null() {
            return Err(io::Error::last_os_error());
        }
        let status = NtResumeProcess(process);
        CloseHandle(process);
        if status < 0 { Err(io::Error::other(format!("NtResumeProcess: NTSTATUS {status:#x}"))) } else { Ok(()) }
    }
}

/// A job object handle, closed on drop. Dropping it doesn't end the job (its processes keep it), but
/// it does take a named job out of the namespace.
pub struct Job(HANDLE);

// a kernel handle, usable from any thread
unsafe impl Send for Job {}
unsafe impl Sync for Job {}

impl Drop for Job {
    fn drop(&mut self) {
        unsafe { CloseHandle(self.0) };
    }
}

/// The name of a service's memory-limit job for one start: per launcher and per restart, so a
/// restarted service never finds its predecessor's job (still alive while that process exits).
pub fn job_name(service: &str, start: u32) -> String {
    format!("Local\\sample-erp-{service}-{}-{start}", std::process::id())
}

/// Started from Explorer (double-click), the console closes as soon as the process exits, taking
/// any error message with it. Only this process is attached to the console in that case.
pub fn pause_if_double_clicked() {
    let mut pids = [0u32; 4];
    let attached = unsafe { GetConsoleProcessList(pids.as_mut_ptr(), pids.len() as u32) };
    if attached == 1 {
        println!("\nPress Enter to close this window.");
        let _ = std::io::stdin().read_line(&mut String::new());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use windows_sys::Win32::System::JobObjects::{OpenJobObjectW, QueryInformationJobObject};

    const JOB_OBJECT_QUERY: u32 = 0x0004;

    /// The process memory limit of the job this process is in, or of the named job, as a service
    /// sees it from inside (win-telemetry's jobInfo does the same).
    fn limit_of(name: Option<&str>) -> Option<usize> {
        unsafe {
            let job = match name {
                None => ptr::null_mut(),
                Some(n) => {
                    let wide: Vec<u16> = n.encode_utf16().chain(Some(0)).collect();
                    let job = OpenJobObjectW(JOB_OBJECT_QUERY, 0, wide.as_ptr());
                    if job.is_null() {
                        return None;
                    }
                    job
                }
            };
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = mem::zeroed();
            let ok = QueryInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                (&raw mut info).cast(),
                mem::size_of_val(&info) as u32,
                ptr::null_mut(),
            );
            if !job.is_null() {
                CloseHandle(job);
            }
            (ok != 0 && info.BasicLimitInformation.LimitFlags & JOB_OBJECT_LIMIT_PROCESS_MEMORY != 0)
                .then_some(info.ProcessMemoryLimit)
        }
    }

    /// Run as a child by the tests below: waits until the parent has limited it, prints the limit of
    /// its own job, or of the job named in WIN_TELEMETRY_JOB.
    #[test]
    fn probe() {
        if std::env::var_os("LIMIT_PROBE").is_none() {
            return;
        }
        let name = std::env::var("WIN_TELEMETRY_JOB").ok();
        // LIMIT_PROBE_ONCE: one look, at start — the limit must already be there
        let tries = if std::env::var_os("LIMIT_PROBE_ONCE").is_some() { 1 } else { 100 };
        for _ in 0..tries {
            if let Some(limit) = limit_of(name.as_deref()) {
                println!("LIMIT={limit}");
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        println!("LIMIT=none");
    }

    fn run_probe(mb: u64, name: Option<&str>) -> String {
        let mut cmd = std::process::Command::new(std::env::current_exe().unwrap());
        cmd.args(["--exact", "windows::tests::probe", "--nocapture", "--test-threads=1"])
            .env("LIMIT_PROBE", "1")
            .env_remove("WIN_TELEMETRY_JOB")
            .stdout(std::process::Stdio::piped());
        if let Some(n) = name {
            cmd.env("WIN_TELEMETRY_JOB", n);
        }
        let child = cmd.spawn().unwrap();
        let _job = limit_memory(child.id(), mb, name).unwrap();
        String::from_utf8(child.wait_with_output().unwrap().stdout).unwrap()
    }

    #[test]
    fn a_limited_child_sees_its_limit() {
        let out = run_probe(300, None);
        assert!(out.contains(&format!("LIMIT={}", 300usize << 20)), "child said: {out}");
        // and the launcher's own job is untouched
        assert_eq!(limit_of(None), None);
    }

    /// The name reaches the job from inside the child while the launcher holds the handle.
    #[test]
    fn a_limited_child_finds_its_job_by_name() {
        let name = job_name("test", 7);
        let out = run_probe(200, Some(&name));
        assert!(out.contains(&format!("LIMIT={}", 200usize << 20)), "child said: {out}");
        // gone with its handle and process
        assert_eq!(limit_of(Some(&name)), None);
    }

    /// Started suspended, limited, then resumed (what supervise does): the child is in its job from its
    /// first instruction, so whatever it forks at start (the API's tcc-v2.exe ingest child) is too.
    #[test]
    fn a_suspended_child_is_limited_before_it_runs() {
        use std::os::windows::process::CommandExt;
        let child = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "windows::tests::probe", "--nocapture", "--test-threads=1"])
            .env("LIMIT_PROBE", "1")
            .env("LIMIT_PROBE_ONCE", "1")
            .env_remove("WIN_TELEMETRY_JOB")
            .stdout(std::process::Stdio::piped())
            .creation_flags(CREATE_SUSPENDED)
            .spawn()
            .unwrap();
        let _job = limit_memory(child.id(), 250, None).unwrap();
        resume(child.id()).unwrap();
        let out = String::from_utf8(child.wait_with_output().unwrap().stdout).unwrap();
        assert!(out.contains(&format!("LIMIT={}", 250usize << 20)), "child said: {out}");
    }
}
