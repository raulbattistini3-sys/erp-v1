//! `cargo xtask dist`: builds every service natively and assembles erp-v1/bin.
//!
//! The launcher sits in erp-v1/launcher, one level above both applications:
//!   erp-v1/sample-ERP/sample-ERP   the ERP (web, api, importer, vault, network, billing)
//!   erp-v1/tcc-v2/tcc-v1           the TCC (demo-monorepo-workspace: server + dashboard)
//!
//!   polivisor go build (erp-v1/polivisor) → polivisor.exe beside the launcher: starts, caps and restarts services/
//!   web      vite build (bun) → web/dist, embedded into the launcher
//!   api      bun build --compile → services/api.exe, Bun runtime included
//!   importer go build → services/importer.exe
//!   network  go build → services/network.exe (registry + public gateway)
//!   vault    go build → services/vault.exe (encrypted CPF / birth dates; keys under DPAPI)
//!   billing  mvnw package + jlink → services/billing/{billing.jar, runtime/} (a trimmed JRE)
//!   tcc      pnpm install, the C++ addon (optional), vite build of the dashboard, bun build --compile
//!            → services/tcc/{tcc-v2.exe, web/, win_telemetry.node}
//!   launcher cargo build --release → sample-erp.exe, with the web app, SQL and migrations (ERP and TCC) inside
//!
//! Options: --no-billing (skip Maven and the JDK), --no-tcc (skip the TCC), --out DIR (default: erp-v1/bin)

use anyhow::{Context, Result, bail};
use std::{
    env,
    ffi::OsStr,
    fs,
    path::{Path, PathBuf},
    process::Command,
};

const EXE: &str = env::consts::EXE_SUFFIX;
/// The web app calls the API on its own origin, through the launcher's gateway (no CORS, no address baked in).
const WEB_API_URL: &str = "/api";
/// The TCC workspace, from erp-v1
const TCC_WORKSPACE: &str = "tcc-v2/tcc-v1/demo-monorepo-workspace";
/// The ERP, from erp-v1
const ERP: &str = "sample-ERP/sample-ERP";
const USAGE: &str = "usage: cargo xtask dist [--no-billing] [--no-tcc] [--out DIR]";
/// What a Spring Boot 4 app with JPA, Flyway, Security and Actuator loads from the JDK.
const BILLING_MODULES: &str = "java.base,java.compiler,java.desktop,java.instrument,java.management,\
java.naming,java.net.http,java.prefs,java.rmi,java.scripting,java.security.jgss,java.security.sasl,\
java.sql,java.transaction.xa,java.xml,jdk.crypto.ec,jdk.management,jdk.naming.dns,jdk.net,\
jdk.unsupported,jdk.zipfs,jdk.localedata";

fn main() -> Result<()> {
    let args: Vec<String> = env::args().skip(1).collect();
    let Some((task, opts)) = args.split_first() else {
        bail!("{USAGE}");
    };
    if task != "dist" {
        bail!("unknown task {task}; {USAGE}");
    }
    // erp-v1: launcher/xtask/../..
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..").canonicalize()?;
    let root = PathBuf::from(root.to_string_lossy().trim_start_matches(r"\\?\"));
    let mut out = root.join("bin");
    let mut billing = true;
    let mut tcc = true;
    let mut opts = opts.iter();
    while let Some(opt) = opts.next() {
        match opt.as_str() {
            "--no-billing" => billing = false,
            "--no-tcc" => tcc = false,
            "--out" => out = PathBuf::from(opts.next().context("--out needs a folder")?),
            other => bail!("unknown option {other}; {USAGE}"),
        }
    }
    dist(&root, &out, billing, tcc)
}

fn dist(root: &Path, out: &Path, billing: bool, tcc: bool) -> Result<()> {
    let repo = &root.join(ERP);
    for tool in ["bun", "go"].into_iter().chain(billing.then_some("jlink")).chain(tcc.then_some("pnpm")) {
        if which(tool).is_none() {
            bail!("{tool} is not on PATH (needed to build {}); see launcher/README.md", match tool {
                "bun" => "the web app and the API",
                "go" => "the importer",
                "pnpm" => "the TCC; or pass --no-tcc",
                _ => "billing's runtime; or pass --no-billing",
            });
        }
    }
    let services = out.join("services");
    fs::create_dir_all(&services)?;
    fs::create_dir_all(out.join("config"))?;

    // the supervisor first: stdlib Go, no cgo, independent of the ERP checkout
    step("polivisor: go build");
    run(Command::new(tool("go"))
        .args(["build", "-trimpath", "-ldflags=-s -w", "-o"])
        .arg(out.join(format!("polivisor{EXE}")))
        .arg("./cmd/polivisor")
        .env("CGO_ENABLED", "0")
        .current_dir(root.join("polivisor")))?;

    step("web: vite build");
    let web = repo.join("web");
    run(Command::new(tool("bun")).args(["install", "--frozen-lockfile"]).current_dir(&web))?;
    run(Command::new(tool("bun"))
        .args(["run", "build"])
        .env("VITE_API_URL", WEB_API_URL)
        .current_dir(&web))?;

    step("api: bun build --compile");
    let api = repo.join("api");
    run(Command::new(tool("bun")).args(["install", "--frozen-lockfile"]).current_dir(&api))?;
    run(Command::new(tool("bun"))
        .args(["build", "--compile", "--minify-syntax", "src/index.ts", "--outfile"])
        .arg(services.join(format!("api{EXE}")))
        .current_dir(&api))?;

    step("importer: go build");
    run(Command::new(tool("go"))
        .args(["build", "-trimpath", "-ldflags=-s -w", "-o"])
        .arg(services.join(format!("importer{EXE}")))
        .arg("./importer/cmd/importer")
        .env("CGO_ENABLED", "0")
        .current_dir(repo))?;

    step("vault: go build");
    run(Command::new(tool("go"))
        .args(["build", "-trimpath", "-ldflags=-s -w", "-o"])
        .arg(services.join(format!("vault{EXE}")))
        .arg("./vault/cmd/vault")
        .env("CGO_ENABLED", "0")
        .current_dir(repo))?;

    step("network: go build");
    run(Command::new(tool("go"))
        .args(["build", "-trimpath", "-ldflags=-s -w", "-o"])
        .arg(services.join(format!("network{EXE}")))
        .arg("./network/cmd/network")
        .env("CGO_ENABLED", "0")
        .current_dir(repo))?;

    if billing {
        build_billing(repo, &services.join("billing"))?;
    } else {
        step("billing: skipped (--no-billing)");
    }

    if tcc {
        build_tcc(&root.join(TCC_WORKSPACE), &services.join("tcc"))?;
    } else {
        step("tcc: skipped (--no-tcc)");
    }

    step("launcher: cargo build --release");
    let launcher = root.join("launcher");
    run(Command::new(env::var("CARGO").unwrap_or_else(|_| "cargo".into()))
        .args(["build", "--release", "-p", "sample-erp"])
        .current_dir(&launcher))?;
    copy(
        &launcher.join("target/release").join(format!("sample-erp{EXE}")),
        &out.join(format!("sample-erp{EXE}")),
    )?;

    let examples = launcher.join("dist-config");
    for name in ["launcher.env.example", "api.env.example", "billing.env.example"] {
        copy(&examples.join(name), &out.join("config").join(name))?;
    }
    copy(&examples.join("README.txt"), &out.join("README.txt"))?;

    step(&format!("done: {}", out.display()));
    println!("  run {}", out.join(format!("sample-erp{EXE}")).display());
    Ok(())
}

fn build_billing(repo: &Path, dest: &Path) -> Result<()> {
    step("billing: mvnw package");
    let billing = repo.join("billing");
    // through sh on Unix: git has mvnw without its executable bit
    let mut mvnw = if cfg!(windows) {
        Command::new(billing.join("mvnw.cmd"))
    } else {
        let mut sh = Command::new("sh");
        sh.arg(billing.join("mvnw"));
        sh
    };
    // test.skip, not skipTests: the tests need Docker
    run(mvnw
        .args(["-B", "-q", "-Dmaven.test.skip=true", "package"])
        .current_dir(&billing))?;
    let jar = fs::read_dir(billing.join("target"))?
        .filter_map(|e| e.ok().map(|e| e.path()))
        .find(|p| {
            let name = p.file_name().and_then(OsStr::to_str).unwrap_or_default();
            name.starts_with("billing-") && name.ends_with(".jar")
        })
        .context("mvnw package left no billing-*.jar in billing/target")?;

    step("billing: jlink runtime");
    let runtime = dest.join("runtime");
    if runtime.exists() {
        fs::remove_dir_all(&runtime).with_context(|| format!("removing the old {}", runtime.display()))?;
    }
    fs::create_dir_all(dest)?;
    run(Command::new(tool("jlink"))
        .args(["--add-modules", BILLING_MODULES])
        .args(["--strip-debug", "--no-man-pages", "--no-header-files", "--compress=zip-6"])
        .arg("--output")
        .arg(&runtime))?;
    copy(&jar, &dest.join("billing.jar"))
}

/// The TCC server as one executable (Bun, bytecode compiled in), its dashboard beside it, and the Windows
/// counters addon when a C++ compiler is around. The server finds win_telemetry.node next to itself.
fn build_tcc(workspace: &Path, dest: &Path) -> Result<()> {
    step("tcc: pnpm install");
    run(Command::new(tool("pnpm")).args(["install", "--frozen-lockfile"]).current_dir(workspace))?;

    let addon = workspace.join("packages/win-telemetry/build/win_telemetry.node");
    if cfg!(windows) {
        step("tcc: win-telemetry addon (C++)");
        // optional: without g++ or cl the server runs without the win.* samples
        if let Err(e) = run(Command::new(tool("pnpm")).args(["build:native"]).current_dir(workspace)) {
            println!("  skipped: {e:#}");
        }
    }

    step("tcc: dashboard vite build");
    // "": the dashboard calls the server on its own origin (tcc-v2.exe serves both)
    run(Command::new(tool("pnpm"))
        .args(["--filter", "@tcc/web", "build"])
        .env("VITE_API_URL", "")
        .current_dir(workspace))?;

    step("tcc: bun build --compile");
    fs::create_dir_all(dest)?;
    run(Command::new(tool("bun"))
        .args(["build", "src/index.ts", "--compile", "--bytecode", "--minify", "--external", "pg-native", "--outfile"])
        .arg(dest.join(format!("tcc-v2{EXE}")))
        .current_dir(workspace.join("apps/server")))?;

    let web = dest.join("web");
    if web.exists() {
        fs::remove_dir_all(&web).with_context(|| format!("removing the old {}", web.display()))?;
    }
    copy_dir(&workspace.join("apps/web/dist"), &web)?;
    if addon.exists() {
        copy(&addon, &dest.join("win_telemetry.node"))?;
    }
    Ok(())
}

fn copy_dir(from: &Path, to: &Path) -> Result<()> {
    fs::create_dir_all(to)?;
    for entry in fs::read_dir(from).with_context(|| format!("reading {}", from.display()))? {
        let entry = entry?;
        let target = to.join(entry.file_name());
        if entry.file_type()?.is_dir() {
            copy_dir(&entry.path(), &target)?;
        } else {
            copy(&entry.path(), &target)?;
        }
    }
    Ok(())
}

fn step(message: &str) {
    println!("\n==> {message}");
}

fn run(cmd: &mut Command) -> Result<()> {
    let status = cmd.status().with_context(|| format!("starting {cmd:?}"))?;
    if !status.success() {
        bail!("{cmd:?} failed ({status})");
    }
    Ok(())
}

fn copy(from: &Path, to: &Path) -> Result<()> {
    fs::copy(from, to).with_context(|| format!("copying {} to {}", from.display(), to.display()))?;
    Ok(())
}

/// The full path of a tool on PATH. Command::new("bun") would find bun.exe on its own, but not a
/// .cmd shim, which is how some installers provide these.
fn tool(name: &str) -> PathBuf {
    which(name).unwrap_or_else(|| PathBuf::from(name))
}

fn which(name: &str) -> Option<PathBuf> {
    let exts: Vec<String> = if cfg!(windows) {
        env::var("PATHEXT").unwrap_or(".EXE;.CMD;.BAT".into()).split(';').map(str::to_lowercase).collect()
    } else {
        vec![String::new()]
    };
    env::split_paths(&env::var_os("PATH")?).find_map(|dir| {
        exts.iter().map(|ext| dir.join(format!("{name}{ext}"))).find(|p| p.is_file())
    })
}
