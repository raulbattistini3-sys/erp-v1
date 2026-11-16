//! sample-erp: the whole stack without Docker.
//!
//!   sample-erp                 start Postgres, the network service, the importer, the vault, the API, billing (if configured),
//!                              and the TCC server (tcc-v2.exe, telemetry + dashboard) beside them
//!   sample-erp promote EMAIL   give an account the SU permission
//!
//! It mirrors docker-compose.yml: same start order, same environment, same database bootstrap. It lives in
//! erp-v1/launcher, above both applications: sample-ERP/sample-ERP (the ERP) and tcc-v2/tcc-v1 (the TCC).

mod certs;
mod config;
mod db;
mod polivisor;
mod web;
#[cfg(windows)]
mod windows;

use anyhow::{Context, Result, bail};
use config::{Config, Paths};
use polivisor::{Spec, Unit};
use std::{
    io::Write,
    net::{Ipv4Addr, TcpListener as StdListener},
    process::ExitCode,
    time::Duration,
};
use tokio::net::TcpStream;

/// One line of output, prefixed with where it came from.
pub fn log(source: &str, line: &str) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{source:>9} | {line}");
}

#[tokio::main]
async fn main() -> ExitCode {
    // One TLS crypto provider for the whole process, before any HTTP client exists. postgresql_archive
    // turns on reqwest's rustls-no-provider: without this,
    // building it panics on every launch that doesn't download Postgres first (the download installs one).
    // ring rather than aws-lc-rs, which needs cmake and NASM to build on Windows.
    let _ = rustls::crypto::ring::default_provider().install_default();
    let result = cli().await;
    if let Err(e) = &result {
        eprintln!("\nerror: {e:#}");
    }
    #[cfg(windows)]
    if result.is_err() {
        windows::pause_if_double_clicked();
    }
    if result.is_ok() { ExitCode::SUCCESS } else { ExitCode::FAILURE }
}

async fn cli() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let paths = Paths::discover()?;
    match args.iter().map(String::as_str).collect::<Vec<_>>().as_slice() {
        [] | ["run"] => run(&paths, Config::load(&paths)?).await,
        ["promote", email] => db::promote(&paths, &Config::load(&paths)?, email).await,
        ["help" | "--help" | "-h"] => {
            println!("{}", include_str!("usage.txt"));
            Ok(())
        }
        _ => bail!("unknown arguments {args:?}\n\n{}", include_str!("usage.txt")),
    }
}

async fn run(paths: &Paths, cfg: Config) -> Result<()> {
    #[cfg(windows)]
    windows::kill_children_with_launcher();

    for (bin, what) in [
        (paths.polivisor(), "the polivisor"),
        (paths.api(), "the API"),
        (paths.importer(), "the importer"),
        (paths.network(), "the network service"),
        (paths.vault(), "the vault"),
    ] {
        if !bin.exists() {
            bail!("{} is missing ({what}): build the bin folder with `cargo xtask dist`", bin.display());
        }
    }
    if !web::is_built() {
        bail!("this launcher was compiled without web/dist: build it with `cargo xtask dist`");
    }
    // the TCC is optional: an older bin/ without services/tcc, or ERP_TCC=off, runs the ERP alone
    let tcc = cfg.tcc && paths.tcc().exists();
    if cfg.tcc && !tcc {
        log("launcher", &format!("{} is missing: starting without the TCC (rebuild with `cargo xtask dist`)", paths.tcc().display()));
    }
    let billing = paths.billing_env().exists();
    if billing && !paths.billing_jar().exists() {
        bail!(
            "config/billing.env exists but services/billing does not: rebuild with `cargo xtask dist` (without --no-billing)"
        );
    }

    let mut ports = vec![
        ("ERP_DB_PORT", cfg.db_port),
        ("ERP_IMPORTER_PORT", cfg.importer_port),
        ("ERP_API_PORT", cfg.api_port),
        ("ERP_NETWORK_PORT", cfg.network_port),
        ("ERP_VAULT_PORT", cfg.vault_port),
        ("ERP_POLIVISOR_PORT", cfg.polivisor_port),
    ];
    if billing {
        ports.push(("ERP_BILLING_PORT", cfg.billing_port));
    }
    if tcc {
        ports.push(("ERP_TCC_PORT", cfg.tcc_port));
    }
    for (key, port) in ports {
        if StdListener::bind((Ipv4Addr::LOCALHOST, port)).is_err() {
            bail!("port {port} is taken (is another copy, or docker compose, running?); set {key} in config/launcher.env");
        }
    }
    // the gateway (network.exe): the only public port
    drop(StdListener::bind((cfg.gateway.bind, cfg.web_port)).with_context(|| {
        format!("port {} is taken on {}; set ERP_WEB_PORT in config/launcher.env", cfg.web_port, cfg.gateway.bind)
    })?);

    std::fs::create_dir_all(&paths.data)?;
    web::extract(&paths.data.join("web"))?;
    let pg = db::start(paths, &cfg).await?;
    let result = run_stack(paths, &cfg, billing, tcc, &pg).await;
    log("postgres", "stopping");
    if let Err(e) = pg.stop().await {
        // Ctrl+C reaches Postgres too, so it may already be down
        log("postgres", &format!("stop: {e}"));
    }
    result
}

async fn run_stack(
    paths: &Paths,
    cfg: &Config,
    billing: bool,
    tcc: bool,
    pg: &postgresql_embedded::PostgreSQL,
) -> Result<()> {
    db::prepare(pg, cfg, tcc).await?;

    let mut units: Vec<Unit> = Vec::new();
    let secrets = &cfg.secrets;
    let cwd = paths.data.clone();
    let owned = |pairs: &[(&str, String)]| -> Vec<(String, String)> {
        pairs.iter().map(|(k, v)| (k.to_string(), v.clone())).collect()
    };

    // the internal certificates (data/certs, made on first launch): mutual TLS between the API and the
    // importer, the vault and billing, and the API's HTTPS behind the gateway
    let tls = certs::load_or_create(&paths.data)?;
    let path = |p: &std::path::Path| p.to_string_lossy().into_owned();

    // network: the registry every service below announces itself on (by name, at the port it bound), and
    // the public gateway that routes /api, /billing and the web app to whatever is registered
    let network_url = format!("http://127.0.0.1:{}", cfg.network_port);
    let g = &cfg.gateway;
    let mut network_env = owned(&[
        ("NETWORK_ADDR", std::net::SocketAddr::new(g.bind, cfg.web_port).to_string()),
        ("NETWORK_CONTROL_ADDR", format!("127.0.0.1:{}", cfg.network_port)),
        ("NETWORK_TOKEN", secrets.network_token.clone()),
        // production: the public port never moves, it's the one printed below and given to Stripe
        ("NETWORK_ENV", "production".into()),
        ("NETWORK_WEB_DIR", paths.data.join("web").to_string_lossy().into_owned()),
        ("NETWORK_RATE", g.rate.to_string()),
        ("NETWORK_BURST", g.burst.to_string()),
        ("NETWORK_TRUST_PROXY", g.trust_proxy.to_string()),
        ("NETWORK_MAX_BODY_MB", g.max_body_mb.to_string()),
        ("NETWORK_MAX_IMPORT_MB", g.max_import_mb.to_string()),
        ("LOG_FORMAT", "text".into()),
        // the API and billing announce https:// addresses signed by the internal CA
        ("NETWORK_UPSTREAM_CA", path(&tls.ca)),
    ]);
    if let Some((cert, key)) = &g.tls {
        network_env.extend(owned(&[
            ("NETWORK_TLS_CERT", cert.to_string_lossy().into_owned()),
            ("NETWORK_TLS_KEY", key.to_string_lossy().into_owned()),
        ]));
    }
    // reCAPTCHA keys, when config/api.env has them, go to the gateway (it checks them, not the API)
    for (k, v) in config::read_env_file(&paths.config.join("api.env"))? {
        if k.starts_with("RECAPTCHA_") {
            network_env.push((k, v));
        }
    }
    // tier 0 with the TCC and the vault: everything registers on it, so it is critical
    units.push(Unit {
        spec: Spec { name: "network", program: paths.network(), args: vec![], env: network_env, cwd: cwd.clone() },
        depends_on: &[], after: &[], ready_port: Some(cfg.network_port), critical: true,
        memory_mb: 256, min_free_mb: 0, runtime: "go", heap_mb: 0,
    });
    let joins = |name: &str| -> Vec<(String, String)> {
        owned(&[
            ("NETWORK_URL", network_url.clone()),
            ("NETWORK_TOKEN", secrets.network_token.clone()),
            ("NETWORK_ADVERTISE_HOST", "127.0.0.1".into()),
            ("NETWORK_INSTANCE_ID", format!("{name}-launcher")),
        ])
    };

    // tcc: the thesis's telemetry server (tcc-v2.exe), a sibling of the ERP rather than one of its
    // services: it joins the registry as "tcc" (network token, like importer and billing), its own database (sample_erp_tcc), its own port. The API posts its events and
    // host metrics to it; the dashboard is served on the same port. Started before the API so the API's
    // first batches find it up.
    if tcc {
        let dir = paths.data.join("tcc");
        std::fs::create_dir_all(&dir)?;
        units.push(Unit {
            spec: Spec {
                name: "tcc",
                program: paths.tcc(),
                args: vec![],
                env: [joins("tcc"), owned(&[
                    ("NODE_ENV", "production".into()),
                    ("DATABASE_URL", cfg.tcc_database_url()),
                    // embedded: no TLS; tcc-server.env's database is verified with its DATABASE_CA_CERT
                    ("DATABASE_SSL", if cfg.tcc_db.is_some() { "verify" } else { "off" }.into()),
                    ("DATABASE_CA_CERT", cfg.tcc_ca()),
                    ("HOST", "127.0.0.1".into()),
                    ("PORT", cfg.tcc_port.to_string()),
                    ("TCC_TOKEN", secrets.tcc_token.clone()),
                    ("TCC_WEB_DIR", path(&paths.tcc_web())),
                    ("WEB_ORIGINS", format!("http://localhost:{0},http://127.0.0.1:{0}", cfg.tcc_port)),
                    // loopback only, so the dashboard reads without a token; writes still need it
                    ("PUBLIC_READS", "1".into()),
                    ("SPOOL_DIR", path(&dir.join("spool"))),
                    ("TELEMETRY_LOG_DIR", path(&dir.join("logs"))),
                    ("INSTANCE_ID", "tcc-launcher".into()),
                ])]
                .concat(),
                cwd: dir,
            },
            depends_on: &[], after: &[], ready_port: Some(cfg.tcc_port), critical: false,
            memory_mb: 512, min_free_mb: 128, runtime: "bun", heap_mb: 0,
        });
    }

    // importer: reads, validates and saves product spreadsheets queued in the importacoes table
    units.push(Unit {
        spec: Spec {
            name: "importer",
            program: paths.importer(),
            args: vec![],
            env: owned(&[
                ("DATABASE_URL", cfg.database_url()),
                ("DATABASE_CA_CERT", cfg.database_ca()),
                // loopback, and only a client with a certificate from data/certs/ca.crt gets past the
                // handshake: the API
                ("GRPC_HOST", "127.0.0.1".into()),
                ("GRPC_PORT", cfg.importer_port.to_string()),
                ("GRPC_TLS_CERT", path(&tls.importer_cert)),
                ("GRPC_TLS_KEY", path(&tls.importer_key)),
                ("GRPC_TLS_CLIENT_CA", path(&tls.ca)),
                ("IMPORT_WORKERS", cfg.import_workers.clone()),
                // development: no TLS to the embedded Postgres; an external one is verified with DATABASE_CA_CERT
                ("IMPORTER_ENV", "development".into()),
            ])
            .into_iter()
            .chain(joins("importer"))
            .collect(),
            cwd: cwd.clone(),
        },
        // the API triggers imports: it starts once the API is up
        depends_on: &["api"], after: &[], ready_port: Some(cfg.importer_port), critical: false,
        memory_mb: 512, min_free_mb: 256, runtime: "go", heap_mb: 0,
    });

    // vault: the usuarios' CPF and birth dates, AES-256-GCM under keys in data/vault-keys. On Windows the
    // keys are DPAPI blobs bound to this Windows account: the folder copied to another machine or user
    // won't open, so back up data/ and restore it as the same user. Loopback + mutual TLS; the API's token
    // is checked against its SHA-256
    let vault_url = format!("https://127.0.0.1:{}", cfg.vault_port);
    let token_sha256 = {
        use sha2::{Digest, Sha256};
        Sha256::digest(secrets.vault_token.as_bytes()).iter().map(|b| format!("{b:02x}")).collect::<String>()
    };
    units.push(Unit {
        spec: Spec {
            name: "vault",
            program: paths.vault(),
            args: vec!["serve".into()],
            env: owned(&[
                ("VAULT_HOST", "127.0.0.1".into()),
                ("VAULT_PORT", cfg.vault_port.to_string()),
                ("VAULT_DATABASE_URL", cfg.vault_database_url()),
                ("VAULT_KEY_DIR", path(&paths.data.join("vault-keys"))),
                // the first launch makes the KEK and the CPF index key
                ("VAULT_KEY_CREATE", "true".into()),
                ("VAULT_KEY_PROTECTION", if cfg!(windows) { "dpapi" } else { "file" }.into()),
                ("VAULT_TOKEN_SHA256", token_sha256),
                ("VAULT_TLS_CERT", path(&tls.vault_cert)),
                ("VAULT_TLS_KEY", path(&tls.vault_key)),
                ("VAULT_TLS_CLIENT_CA", path(&tls.ca)),
                // development: no TLS to the embedded Postgres; an external one is verified with DATABASE_CA_CERT
                ("VAULT_ENV", "development".into()),
            ]),
            cwd: cwd.clone(),
        },
        depends_on: &[], after: &[], ready_port: Some(cfg.vault_port), critical: true,
        memory_mb: 128, min_free_mb: 0, runtime: "go", heap_mb: 0,
    });

    // api: defaults, then config/api.env, then what the launcher owns (as in docker-compose.yml, the
    // database, host/port, URLs and CORS always win over the file)
    let mut api_env = owned(&[
        ("NODE_ENV", "production".into()),
        // Stripe status and cobranças go through billing; without it the pages say it isn't running
        ("BILLING_URL", if billing { format!("https://127.0.0.1:{}", cfg.billing_port) } else { String::new() }),
        ("JWT_SECRET", secrets.jwt_secret.clone()),
        // the first superuser, created only while the database has no active SU
        ("BOOTSTRAP_SU_NAME", cfg.admin.name.clone()),
        ("BOOTSTRAP_SU_EMAIL", cfg.admin.email.clone()),
        ("BOOTSTRAP_SU_PASSWORD", cfg.admin.password.clone()),
    ]);
    api_env.extend(config::read_env_file(&paths.config.join("api.env"))?);
    api_env.extend(owned(&[
        // development: no TLS to the embedded Postgres; an external one is verified with DATABASE_CA_CERT; a copied api/.env may say production
        ("API_ENV", "development".into()),
        ("DATABASE_URL", cfg.database_url()),
                ("DATABASE_CA_CERT", cfg.database_ca()),
        ("APP_HOST", "127.0.0.1".into()),
        ("PORT", cfg.api_port.to_string()),
        // the browser calls /api on the gateway's own origin, so CORS only matters for these
        ("CORS_ORIGINS", format!("{0},{1}", cfg.web_url(), cfg.web_url().replace("localhost", "127.0.0.1"))),
        ("WEB_URL", cfg.web_url()),
        // OAuth callbacks come back through the gateway: <web>/api/integracoes/<provider>/callback
        ("API_PUBLIC_URL", format!("{}/api", cfg.web_url())),
        // with this set the API hands imports to the Go importer instead of forking worker processes,
        // which a single compiled executable can't do
        ("IMPORTER_GRPC_URL", format!("127.0.0.1:{}", cfg.importer_port)),
        ("IMPORTER_TLS_CA", path(&tls.ca)),
        ("IMPORTER_TLS_CERT", path(&tls.api_cert)),
        ("IMPORTER_TLS_KEY", path(&tls.api_key)),
        ("IMPORTER_TLS_SERVER_NAME", "importer".into()),
        ("VAULT_URL", vault_url.clone()),
        ("VAULT_TOKEN", secrets.vault_token.clone()),
        ("VAULT_TLS_CA", path(&tls.ca)),
        ("VAULT_TLS_CERT", path(&tls.api_cert)),
        ("VAULT_TLS_KEY", path(&tls.api_key)),
        ("VAULT_TLS_SERVER_NAME", "vault".into()),
        // HTTPS, TLS 1.3 only (the gateway reaches it with NETWORK_UPSTREAM_CA); also the client
        // certificate towards billing, whose own certificate API_TLS_CA checks
        ("API_TLS_CERT", path(&tls.api_cert)),
        ("API_TLS_KEY", path(&tls.api_key)),
        ("API_TLS_CA", path(&tls.ca)),
        ("BILLING_TLS_SERVER_NAME", "billing".into()),
    ]));
    // the web's search timings (products, Empresas), the API's own events and its host metrics go to the
    // TCC server (api/src/utils/telemetry); without the TCC the API sends nothing
    if tcc {
        api_env.extend(owned(&[
            ("TCC_TELEMETRY_URL", format!("{}/telemetry/host", cfg.tcc_url())),
            ("TCC_TOKEN", secrets.tcc_token.clone()),
            ("TELEMETRY_INSTANCE_ID", "api-launcher".into()),
        ]));
    }
    api_env.extend(joins("api"));
    // bun-compiled (JavaScriptCore): V8 heap flags don't apply, the job cap is its limit
    units.push(Unit {
        spec: Spec { name: "api", program: paths.api(), args: vec![], env: api_env, cwd: cwd.clone() },
        depends_on: &["network", "vault"], after: &["tcc"], ready_port: Some(cfg.api_port), critical: true,
        memory_mb: 1024, min_free_mb: 384, runtime: "bun", heap_mb: 0,
    });

    // billing: Stripe webhooks; its Flyway migrations run in the billing schema of the same database
    if billing {
        let mut env: Vec<(String, String)> = config::read_env_file(&paths.billing_env())?.into_iter().collect();
        env.extend(owned(&[
            ("JDBC_DATABASE_URL", cfg.jdbc_database_url()?),
            ("DATABASE_CA_CERT", cfg.database_ca()),
            ("ERP_API_URL", format!("https://127.0.0.1:{}", cfg.api_port)),
            // mutual TLS with the API, both ways; no shared token
            ("BILLING_TLS_CERT", path(&tls.billing_cert)),
            ("BILLING_TLS_KEY", path(&tls.billing_key)),
            ("BILLING_TLS_CA", path(&tls.ca)),
            ("BILLING_PORT", cfg.billing_port.to_string()),
            ("BILLING_ENV", "development".into()),
            ("SERVER_ADDRESS", "127.0.0.1".into()),
            // served under /billing by the gateway: honour its X-Forwarded-Prefix in links and redirects
            ("SERVER_FORWARD_HEADERS_STRATEGY", "framework".into()),
        ]));
        env.extend(joins("billing"));
        units.push(Unit {
            spec: Spec {
                name: "billing",
                program: paths.billing_java(),
                args: vec!["-jar".into(), paths.billing_jar().to_string_lossy().into_owned()],
                env,
                cwd: cwd.clone(),
            },
            depends_on: &["api"], after: &[], ready_port: Some(cfg.billing_port), critical: false,
            memory_mb: 896, min_free_mb: 512, runtime: "java", heap_mb: 512,
        });
    }

    let banner = {
        let api = cfg.api_port;
        let web_url = cfg.web_url();
        let open = cfg.open_browser;
        let admin_line = format!(
            "
  first sign-in  {} / {}  (asked to choose a new password; ignore once done)",
            cfg.admin.email, cfg.admin.password
        );
        let billing_line = if billing {
            format!(
                "\n  billing  {0}/billing/docs   (Stripe: forward webhooks to {0}/billing/webhooks/stripe)",
                cfg.web_url()
            )
        } else {
            String::new()
        };
        let docs = format!("{}/api/docs", cfg.web_url());
        let tcc_line = if tcc {
            format!("\n  tcc      http://localhost:{}/   (telemetry dashboard)", cfg.tcc_port)
        } else {
            String::new()
        };
        let billing_line = format!("{billing_line}{tcc_line}");
        tokio::spawn(async move {
            if !wait_for_port(api, Duration::from_secs(90)).await {
                log("launcher", "the API is not answering yet; check its output above");
                return;
            }
            println!(
                "\n  sample-erp is up on one port\n  web      {web_url}\n  api      {docs}{billing_line}{admin_line}\n  Ctrl+C stops everything.\n"
            );
            if open {
                open_browser(&web_url);
            }
        })
    };

    let config = paths.data.join("polivisor.json");
    polivisor::write_config(&config, cfg.polivisor_port, 20, &units)?;
    log(
        "launcher",
        &format!("handing {} services to the polivisor (status: polivisor status --addr 127.0.0.1:{})", units.len(), cfg.polivisor_port),
    );
    let result = polivisor::run(&paths.polivisor(), &config).await;
    banner.abort();
    result
}

async fn wait_for_port(port: u16, limit: Duration) -> bool {
    let deadline = tokio::time::Instant::now() + limit;
    while tokio::time::Instant::now() < deadline {
        if TcpStream::connect((Ipv4Addr::LOCALHOST, port)).await.is_ok() {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    false
}

fn open_browser(url: &str) {
    #[cfg(windows)]
    let result = {
        use std::os::windows::process::CommandExt;
        // out of the launcher's job object, or a browser it starts would close along with it
        std::process::Command::new("cmd")
            .args(["/C", "start", "", url])
            .creation_flags(windows::CREATE_BREAKAWAY_FROM_JOB)
            .spawn()
    };
    #[cfg(not(windows))]
    let result = if cfg!(target_os = "macos") {
        std::process::Command::new("open").arg(url).spawn()
    } else {
        std::process::Command::new("xdg-open").arg(url).spawn()
    };
    if let Err(e) = result {
        log("launcher", &format!("could not open a browser ({e}); go to {url}"));
    }
}
