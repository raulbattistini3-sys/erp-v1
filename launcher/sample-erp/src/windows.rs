//! Windows-only process plumbing.

use std::{io, mem, ptr};
use windows_sys::Win32::{
    Foundation::CloseHandle,
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
/// closed here: the job lives as long as its process. The service sees the limit from inside with
/// QueryInformationJobObject(NULL, ...) (win-telemetry's jobInfo(): jobMemoryLimitBytes, peaks),
/// and its children (the API's IPC telemetry child) inherit the job.
pub fn limit_memory(pid: u32, mb: u64) -> io::Result<()> {
    unsafe {
        let process = OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid);
        if process.is_null() {
            return Err(io::Error::last_os_error());
        }
        let job = CreateJobObjectW(ptr::null(), ptr::null());
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
        CloseHandle(job);
        CloseHandle(process);
        result
    }
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
    use windows_sys::Win32::System::JobObjects::QueryInformationJobObject;

    /// The process memory limit of the job this process is in, as a service sees it from inside.
    fn own_limit() -> Option<usize> {
        unsafe {
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = mem::zeroed();
            let ok = QueryInformationJobObject(
                ptr::null_mut(),
                JobObjectExtendedLimitInformation,
                (&raw mut info).cast(),
                mem::size_of_val(&info) as u32,
                ptr::null_mut(),
            );
            (ok != 0 && info.BasicLimitInformation.LimitFlags & JOB_OBJECT_LIMIT_PROCESS_MEMORY != 0)
                .then_some(info.ProcessMemoryLimit)
        }
    }

    /// Run as a child by the test below: waits until the parent has limited it, prints the limit.
    #[test]
    fn probe() {
        if std::env::var_os("LIMIT_PROBE").is_none() {
            return;
        }
        for _ in 0..100 {
            if let Some(limit) = own_limit() {
                println!("LIMIT={limit}");
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        println!("LIMIT=none");
    }

    #[test]
    fn a_limited_child_sees_its_limit() {
        let child = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "windows::tests::probe", "--nocapture", "--test-threads=1"])
            .env("LIMIT_PROBE", "1")
            .stdout(std::process::Stdio::piped())
            .spawn()
            .unwrap();
        limit_memory(child.id(), 300).unwrap();
        let out = String::from_utf8(child.wait_with_output().unwrap().stdout).unwrap();
        assert!(out.contains(&format!("LIMIT={}", 300usize << 20)), "child said: {out}");
        // and the launcher's own job is untouched
        assert_eq!(own_limit(), None);
    }
}
