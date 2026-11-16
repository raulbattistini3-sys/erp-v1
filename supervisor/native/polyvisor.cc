// polyvisor.node: what a service needs to know about its own resources, read from the OS directly, so the
// JS runtime doesn't depend on its engine's idea of memory (Bun's heap statistics report the machine's RAM
// as the heap limit, and its event-loop utilization is always 0).
//
//   processMemory() → { privateBytes, workingSetBytes, peakWorkingSetBytes, pagefileBytes }
//   job(name?)      → the job this process is in (the innermost: the service's own under polyvisor), or a
//                     named one: { inJob, jobMemoryLimitBytes, processMemoryLimitBytes, jobMemoryBytes,
//                     peakJobMemoryBytes, peakProcessMemoryBytes, activeProcesses, cpuNanos } (null: no job)
//   systemMemory()  → { totalBytes, availableBytes, loadPercent, commitLimitBytes, commitAvailableBytes }
//   cpuTimes()      → { userNanos, kernelNanos } of this process
//
// Node-API only, no node.lib: the napi_* functions are looked up in the host (Node or Bun) when it loads.
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <psapi.h>

#include <cstdint>
#include <string>

#ifndef NAPI_VERSION
#define NAPI_VERSION 8
#endif
#include <node_api.h>

namespace {

// Node-API, resolved from the host process (node.exe, libnode.dll or a Bun executable) at load time
#define NAPI_FUNCS(X)   X(napi_create_object) X(napi_create_double) X(napi_get_boolean) X(napi_get_null) X(napi_set_named_property)   X(napi_get_cb_info) X(napi_typeof) X(napi_get_value_string_utf16) X(napi_define_properties) X(napi_throw_error)

struct Api {
#define DECLARE(name) decltype(&::name) name;
  NAPI_FUNCS(DECLARE)
#undef DECLARE
};
Api N;

bool resolveApi(std::string& missing) {
  HMODULE hosts[] = {GetModuleHandleW(nullptr), GetModuleHandleW(L"libnode.dll"), GetModuleHandleW(L"node.dll")};
  auto find = [&](const char* name) -> FARPROC {
    for (HMODULE h : hosts)
      if (h)
        if (FARPROC p = GetProcAddress(h, name)) return p;
    return nullptr;
  };
#define RESOLVE(name)   N.name = reinterpret_cast<decltype(&::name)>(find(#name));   if (!N.name) missing += (missing.empty() ? "" : ", ") + std::string(#name);
  NAPI_FUNCS(RESOLVE)
#undef RESOLVE
  return missing.empty();
}

// JobObjectMemoryUsageInformation (Windows 10+): the job's committed memory right now
constexpr int kJobObjectMemoryUsageInformation = 28;
struct JobMemoryUsage {
  ULONG64 JobMemory;
  ULONG64 PeakJobMemoryUsed;
};

void set(napi_env env, napi_value obj, const char* key, double v) {
  napi_value n;
  N.napi_create_double(env, v, &n);
  N.napi_set_named_property(env, obj, key, n);
}

void setBool(napi_env env, napi_value obj, const char* key, bool v) {
  napi_value b;
  N.napi_get_boolean(env, v, &b);
  N.napi_set_named_property(env, obj, key, b);
}

napi_value null(napi_env env) {
  napi_value n;
  N.napi_get_null(env, &n);
  return n;
}

napi_value ProcessMemory(napi_env env, napi_callback_info) {
  PROCESS_MEMORY_COUNTERS_EX m{};
  m.cb = sizeof(m);
  if (!GetProcessMemoryInfo(GetCurrentProcess(), reinterpret_cast<PROCESS_MEMORY_COUNTERS*>(&m), sizeof(m))) {
    return null(env);
  }
  napi_value out;
  N.napi_create_object(env, &out);
  set(env, out, "privateBytes", static_cast<double>(m.PrivateUsage));
  set(env, out, "workingSetBytes", static_cast<double>(m.WorkingSetSize));
  set(env, out, "peakWorkingSetBytes", static_cast<double>(m.PeakWorkingSetSize));
  set(env, out, "pagefileBytes", static_cast<double>(m.PagefileUsage));
  return out;
}

napi_value Job(napi_env env, napi_callback_info info) {
  size_t argc = 1;
  napi_value argv[1];
  N.napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);

  HANDLE job = nullptr;  // null: the calling process's (innermost) job
  bool opened = false;
  if (argc >= 1) {
    napi_valuetype t;
    N.napi_typeof(env, argv[0], &t);
    if (t == napi_string) {
      size_t len = 0;
      N.napi_get_value_string_utf16(env, argv[0], nullptr, 0, &len);
      std::u16string name(len, u'\0');
      N.napi_get_value_string_utf16(env, argv[0], name.data(), len + 1, &len);
      job = OpenJobObjectW(JOB_OBJECT_QUERY, FALSE, reinterpret_cast<const wchar_t*>(name.c_str()));
      if (!job) return null(env);
      opened = true;
    }
  }
  BOOL inJob = FALSE;
  if (!opened && (!IsProcessInJob(GetCurrentProcess(), nullptr, &inJob) || !inJob)) {
    return null(env);
  }

  napi_value out;
  N.napi_create_object(env, &out);
  setBool(env, out, "inJob", true);

  JOBOBJECT_EXTENDED_LIMIT_INFORMATION ext{};
  if (QueryInformationJobObject(job, JobObjectExtendedLimitInformation, &ext, sizeof(ext), nullptr)) {
    const DWORD flags = ext.BasicLimitInformation.LimitFlags;
    set(env, out, "jobMemoryLimitBytes", (flags & JOB_OBJECT_LIMIT_JOB_MEMORY) ? static_cast<double>(ext.JobMemoryLimit) : 0);
    set(env, out, "processMemoryLimitBytes",
        (flags & JOB_OBJECT_LIMIT_PROCESS_MEMORY) ? static_cast<double>(ext.ProcessMemoryLimit) : 0);
    set(env, out, "peakJobMemoryBytes", static_cast<double>(ext.PeakJobMemoryUsed));
    set(env, out, "peakProcessMemoryBytes", static_cast<double>(ext.PeakProcessMemoryUsed));
  }
  JobMemoryUsage usage{};
  if (QueryInformationJobObject(job, static_cast<JOBOBJECTINFOCLASS>(kJobObjectMemoryUsageInformation), &usage,
                                sizeof(usage), nullptr)) {
    set(env, out, "jobMemoryBytes", static_cast<double>(usage.JobMemory));
  }
  JOBOBJECT_BASIC_ACCOUNTING_INFORMATION acct{};
  if (QueryInformationJobObject(job, JobObjectBasicAccountingInformation, &acct, sizeof(acct), nullptr)) {
    set(env, out, "activeProcesses", acct.ActiveProcesses);
    set(env, out, "cpuNanos",
        (static_cast<double>(acct.TotalUserTime.QuadPart) + static_cast<double>(acct.TotalKernelTime.QuadPart)) * 100);
  }
  if (opened) CloseHandle(job);
  return out;
}

napi_value SystemMemory(napi_env env, napi_callback_info) {
  MEMORYSTATUSEX m{};
  m.dwLength = sizeof(m);
  if (!GlobalMemoryStatusEx(&m)) return null(env);
  napi_value out;
  N.napi_create_object(env, &out);
  set(env, out, "totalBytes", static_cast<double>(m.ullTotalPhys));
  set(env, out, "availableBytes", static_cast<double>(m.ullAvailPhys));
  set(env, out, "loadPercent", m.dwMemoryLoad);
  set(env, out, "commitLimitBytes", static_cast<double>(m.ullTotalPageFile));
  set(env, out, "commitAvailableBytes", static_cast<double>(m.ullAvailPageFile));
  return out;
}

double nanos(const FILETIME& t) {
  ULARGE_INTEGER v;
  v.LowPart = t.dwLowDateTime;
  v.HighPart = t.dwHighDateTime;
  return static_cast<double>(v.QuadPart) * 100;
}

napi_value CpuTimes(napi_env env, napi_callback_info) {
  FILETIME created, exited, kernel, user;
  if (!GetProcessTimes(GetCurrentProcess(), &created, &exited, &kernel, &user)) return null(env);
  napi_value out;
  N.napi_create_object(env, &out);
  set(env, out, "userNanos", nanos(user));
  set(env, out, "kernelNanos", nanos(kernel));
  return out;
}

napi_value Init(napi_env env, napi_value exports) {
  std::string missing;
  if (!resolveApi(missing)) {
    if (N.napi_throw_error) N.napi_throw_error(env, nullptr, ("polyvisor.node: host lacks " + missing).c_str());
    return nullptr;
  }
  napi_property_descriptor props[] = {
      {"processMemory", nullptr, ProcessMemory, nullptr, nullptr, nullptr, napi_default, nullptr},
      {"job", nullptr, Job, nullptr, nullptr, nullptr, napi_default, nullptr},
      {"systemMemory", nullptr, SystemMemory, nullptr, nullptr, nullptr, napi_default, nullptr},
      {"cpuTimes", nullptr, CpuTimes, nullptr, nullptr, nullptr, napi_default, nullptr},
  };
  N.napi_define_properties(env, exports, sizeof(props) / sizeof(props[0]), props);
  return exports;
}

}  // namespace

extern "C" __declspec(dllexport) napi_value napi_register_module_v1(napi_env env, napi_value exports) {
  return Init(env, exports);
}
extern "C" __declspec(dllexport) int32_t node_api_module_get_api_version_v1() { return NAPI_VERSION; }
