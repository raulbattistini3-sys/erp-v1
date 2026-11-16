//! Where things live next to the executable, and the settings the launcher hands to each service.
//!
//! bin/
//!   sample-erp.exe              this launcher, with the web app, SQL scripts and migrations embedded
//!   services/                   network.exe, importer.exe, api.exe, billing/ (runtime/ + billing.jar),
//!                               tcc/ (tcc-v2.exe, its dashboard in web/, win_telemetry.node),
//!                               erp-cache.exe + cache.json (the read cache in front of the API and the TCC)
//!   config/launcher.env         ports and options (optional, see launcher.env.example)
//!   config/api.env              the API's optional secrets: Resend, Jira/HubSpot OAuth, ... (optional)
//!   config/billing.env          Stripe keys; billing only starts when this file exists
//!   data/                       created on first launch: Postgres, generated secrets (secrets.env),
//!                               the api <-> importer mTLS certificates (certs/)

use anyhow::{Context, Result, bail};
use rand::{Rng, distr::Alphanumeric};
use std::{
    collections::HashMap,
    env, fs,
    path::{Path, PathBuf},
};

pub const EXE: &str = env::consts::EXE_SUFFIX;

pub struct Paths {
    pub services: PathBuf,
    pub config: PathBuf,
    pub data: PathBuf,
}

impl Paths {
    /// The folder holding the launcher, or ERP_HOME (handy for `cargo run` against ../bin).
    pub fn discover() -> Result<Self> {
        let root = match env::var_os("ERP_HOME") {
            Some(home) => PathBuf::from(home),
            None => env::current_exe()?
                .parent()
                .context("the launcher has no parent folder")?
                .to_path_buf(),
        };
        let root = fs::canonicalize(&root).with_context(|| format!("{} does not exist", root.display()))?;
        // canonicalize gives \\?\C:\... on Windows, which some child programs (java) dislike
        let root = PathBuf::from(root.to_string_lossy().trim_start_matches(r"\\?\"));
        Ok(Self {
            services: root.join("services"),
            config: root.join("config"),
            data: root.join("data"),
        })
    }

    pub fn api(&self) -> PathBuf {
        self.services.join(format!("api{EXE}"))
    }

    /// The network service (network/, Go): service registry plus the public gateway.
    pub fn network(&self) -> PathBuf {
        self.services.join(format!("network{EXE}"))
    }

    /// The vault (vault/, Go): the encrypted CPF and birth dates, in its own database.
    pub fn vault(&self) -> PathBuf {
        self.services.join(format!("vault{EXE}"))
    }

    pub fn importer(&self) -> PathBuf {
        self.services.join(format!("importer{EXE}"))
    }

    pub fn billing_java(&self) -> PathBuf {
        self.services.join("billing").join("runtime").join("bin").join(format!("java{EXE}"))
    }

    pub fn billing_jar(&self) -> PathBuf {
        self.services.join("billing").join("billing.jar")
    }

    pub fn billing_env(&self) -> PathBuf {
        self.config.join("billing.env")
    }

    /// The TCC server (tcc-v2/tcc-v1, Bun-compiled): the telemetry the API and its own host metrics go to,
    /// plus its dashboard. A sibling of the ERP's services, not one of them.
    pub fn tcc(&self) -> PathBuf {
        self.services.join("tcc").join(format!("tcc-v2{EXE}"))
    }

    /// erp-cache (erp-v1/cache, Go): the read cache in front of the API and the TCC server
    pub fn cache(&self) -> PathBuf {
        self.services.join(format!("erp-cache{EXE}"))
    }

    /// Its sites, routes and tables (cache.example.json); ports and URLs come from the launcher
    pub fn cache_config(&self) -> PathBuf {
        self.services.join("cache.json")
    }

    /// The TCC dashboard (apps/web/dist, built with VITE_API_URL=""), served by tcc-v2.exe at /
    pub fn tcc_web(&self) -> PathBuf {
        self.services.join("tcc").join("web")
    }
}

/// Secrets generated on first launch and kept in data/secrets.env. The Postgres password is fixed
/// when the cluster is created, so that file must stay with data/postgres.
pub struct Secrets {
    pub db_password: String,
    pub jwt_secret: String,
    /// Signs the services' calls to the network registry (NETWORK_TOKEN).
    pub network_token: String,
    /// The API's bearer token for the vault (VAULT_TOKEN); the vault is given only its SHA-256.
    pub vault_token: String,
    /// The bearer token for the TCC server (TCC_TOKEN): the API posts its telemetry with it.
    pub tcc_token: String,
}

pub struct Config {
    pub web_port: u16,
    pub api_port: u16,
    pub db_port: u16,
    pub importer_port: u16,
    pub billing_port: u16,
    /// The network registry (control) port, loopback only.
    pub network_port: u16,
    /// The vault's HTTPS port, loopback only.
    pub vault_port: u16,
    /// The TCC server's port (API ingest + dashboard), loopback only.
    pub tcc_port: u16,
    /// ERP_TCC=off: don't start the TCC even when services/tcc is there.
    pub tcc: bool,
    /// erp-cache's sites (in front of the API and the TCC) and its control port, loopback only.
    pub cache_port: u16,
    pub cache_tcc_port: u16,
    pub cache_control_port: u16,
    /// ERP_CACHE=off: don't start erp-cache even when services/erp-cache.exe is there; /api then goes
    /// straight to the API.
    pub cache: bool,
    /// Per-service memory limits in MB (ERP_<SERVICE>_MEMORY_MB; 0 = none), enforced by a job object
    /// per service. The Bun executables ignore --max-old-space-size, so this is their only bound.
    pub memory_mb: HashMap<&'static str, u64>,
    pub import_workers: String,
    pub open_browser: bool,
    pub gateway: GatewayConfig,
    /// The first superuser, created by the API when the database has none (BOOTSTRAP_SU_*). Its
    /// password is a known one, so the web app makes it pick a new one at first sign-in.
    pub admin: Admin,
    pub secrets: Secrets,
    /// The ERP's own database when it isn't the embedded one: DATABASE_URL (+ DATABASE_CA_CERT) from
    /// config/sample-erp.env, or erp-v1/secrets/sample-erp.env beside bin/. The same one network dev
    /// uses, so a user registered in either is there for both. The vault and the TCC stay embedded.
    pub erp_db: Option<ExternalDb>,
    /// The TCC server's database from tcc-server.env; embedded when absent
    pub tcc_db: Option<ExternalDb>,
}

pub struct ExternalDb {
    pub url: String,
    pub ca: Option<PathBuf>,
    /// a session-mode URL for migrations when DATABASE_URL is a transaction pooler
    pub migration_url: Option<String>,
}

impl ExternalDb {
    /// the launcher's own connection: migration URL if any, server verified with the CA
    fn launcher_url(&self) -> String {
        let url = self.migration_url.as_ref().unwrap_or(&self.url);
        match &self.ca {
            Some(ca) => {
                let sep = if url.contains('?') { '&' } else { '?' };
                format!("{url}{sep}sslmode=verify-full&sslrootcert={}", ca.to_string_lossy().replace('\\', "/"))
            }
            None => url.clone(),
        }
    }
}

/// config/sample-erp.env, else <erp-v1>/secrets/sample-erp.env (bin/ sits in erp-v1)
fn load_external_db(paths: &Paths) -> Result<Option<ExternalDb>> {
    let root = paths.config.parent().map(Path::to_path_buf).unwrap_or_default();
    let erp = root.join("..");
    load_db_file(&[
        paths.config.join("sample-erp.env"),
        erp.join("secrets").join("sample-erp.env"),
        erp.join("sample-ERP").join("sample-ERP").join("api").join(".env"),
    ])
}

/// The TCC server's database: config/tcc-server.env, else <erp-v1>/secrets/tcc-server.env
fn load_tcc_db(paths: &Paths) -> Result<Option<ExternalDb>> {
    let root = paths.config.parent().map(Path::to_path_buf).unwrap_or_default();
    load_db_file(&[paths.config.join("tcc-server.env"), root.join("..").join("secrets").join("tcc-server.env")])
}

/// The first file with a DATABASE_URL; DATABASE_CA_CERT is relative to that file
fn load_db_file(candidates: &[PathBuf]) -> Result<Option<ExternalDb>> {
    for file in candidates {
        if !file.exists() {
            continue;
        }
        let vars = read_env_file(file)?;
        let Some(url) = vars.get("DATABASE_URL").filter(|u| !u.is_empty()).cloned() else { continue };
        let dir = file.parent().map(Path::to_path_buf).unwrap_or_default();
        let ca = vars.get("DATABASE_CA_CERT").filter(|c| !c.is_empty()).map(|c| {
            let p = PathBuf::from(c);
            if p.is_absolute() { p } else { dir.join(p) }
        });
        if let Some(ca) = &ca {
            if !ca.exists() {
                bail!("{}: DATABASE_CA_CERT {} does not exist", file.display(), ca.display());
            }
        }
        let migration_url = vars.get("DATABASE_MIGRATION_URL").filter(|u| !u.is_empty()).cloned();
        crate::log("launcher", &format!("database from {}", file.display()));
        return Ok(Some(ExternalDb { url, ca, migration_url }));
    }
    Ok(None)
}

/// The public port's behaviour (see gateway/mod.rs).
pub struct GatewayConfig {
    /// 127.0.0.1 by default; 0.0.0.0 to serve other machines (put it behind HTTPS then)
    pub bind: std::net::IpAddr,
    pub rate: f64,
    pub burst: u32,
    pub trust_proxy: bool,
    pub max_body_mb: usize,
    pub max_import_mb: usize,
    /// both set: the gateway serves HTTPS with this certificate and key (PEM)
    pub tls: Option<(PathBuf, PathBuf)>,
}

pub struct Admin {
    pub name: String,
    pub email: String,
    pub password: String,
}

impl Config {
    /// config/launcher.env, then ERP_* variables from the environment on top.
    pub fn load(paths: &Paths) -> Result<Self> {
        let mut vars = read_env_file(&paths.config.join("launcher.env"))?;
        vars.extend(env::vars().filter(|(k, _)| k.starts_with("ERP_")));

        let port = |key: &str, default: u16| -> Result<u16> {
            match vars.get(key) {
                Some(v) => v.parse().with_context(|| format!("{key}={v} is not a port")),
                None => Ok(default),
            }
        };
        Ok(Self {
            web_port: port("ERP_WEB_PORT", 8080)?,
            api_port: port("ERP_API_PORT", 3333)?,
            // not 5432, so it never collides with a Postgres already installed on the machine
            db_port: port("ERP_DB_PORT", 5433)?,
            importer_port: port("ERP_IMPORTER_PORT", 50051)?,
            billing_port: port("ERP_BILLING_PORT", 8081)?,
            network_port: port("ERP_NETWORK_PORT", 7070)?,
            vault_port: port("ERP_VAULT_PORT", 8200)?,
            // the same port as `pnpm dev` in the TCC workspace, so the extension's settings keep working
            tcc_port: port("ERP_TCC_PORT", 8787)?,
            tcc: flag(&vars, "ERP_TCC", true),
            cache_port: port("ERP_CACHE_PORT", 10080)?,
            cache_tcc_port: port("ERP_CACHE_TCC_PORT", 8788)?,
            cache_control_port: port("ERP_CACHE_CONTROL_PORT", 7071)?,
            cache: flag(&vars, "ERP_CACHE", true),
            // convo2.json: API heap 768 MB, TCC server 384 MB, billing -Xmx512m + metaspace + direct
            // buffers; the limit is the whole process (heap plus native), so it sits above the heap
            memory_mb: [("api", 1024.0), ("tcc", 512.0), ("billing", 1024.0), ("cache", 256.0), ("network", 0.0), ("importer", 0.0), ("vault", 0.0)]
                .into_iter()
                .map(|(name, default)| {
                    let key = format!("ERP_{}_MEMORY_MB", name.to_uppercase());
                    number(&vars, &key, default).map(|mb| (name, mb as u64))
                })
                .collect::<Result<_>>()?,
            import_workers: vars.get("ERP_IMPORT_WORKERS").cloned().unwrap_or_else(|| "2".into()),
            open_browser: flag(&vars, "ERP_OPEN_BROWSER", true),
            gateway: GatewayConfig {
                bind: match vars.get("ERP_BIND") {
                    Some(v) => v.parse().with_context(|| format!("ERP_BIND={v} is not an IP address"))?,
                    None => std::net::IpAddr::from([127, 0, 0, 1]),
                },
                rate: number(&vars, "ERP_RATE", 20.0)?,
                burst: number(&vars, "ERP_BURST", 40.0)? as u32,
                trust_proxy: flag(&vars, "ERP_TRUST_PROXY", false),
                max_body_mb: number(&vars, "ERP_MAX_BODY_MB", 1.0)? as usize,
                // product spreadsheets up to 15 MB, clientes/empresas up to 120 MB (api/src/middlewares/upload.middleware.ts)
                max_import_mb: number(&vars, "ERP_MAX_IMPORT_MB", 128.0)? as usize,
                tls: match (vars.get("ERP_TLS_CERT"), vars.get("ERP_TLS_KEY")) {
                    (Some(c), Some(k)) if !c.is_empty() && !k.is_empty() => Some((PathBuf::from(c), PathBuf::from(k))),
                    (None, None) => None,
                    (Some(c), Some(k)) if c.is_empty() && k.is_empty() => None,
                    _ => bail!("set both ERP_TLS_CERT and ERP_TLS_KEY, or neither"),
                },
            },
            admin: Admin {
                name: vars.get("ERP_ADMIN_NAME").cloned().unwrap_or_else(|| "John Doe".into()),
                email: vars.get("ERP_ADMIN_EMAIL").cloned().unwrap_or_else(|| "admin@sample-erp.local".into()),
                password: vars.get("ERP_ADMIN_PASSWORD").cloned().unwrap_or_else(|| "change-me".into()),
            },
            secrets: load_secrets(&paths.data)?,
            erp_db: load_external_db(paths)?,
            tcc_db: load_tcc_db(paths)?,
        })
    }

    /// The ERP's database as the services get it (external when configured, else the embedded one)
    pub fn database_url(&self) -> String {
        if let Some(db) = &self.erp_db {
            return db.url.clone();
        }
        self.embedded_database_url()
    }

    /// The same, for the launcher's own connections (migrations, promote): verifies the server with
    /// DATABASE_CA_CERT when there is one
    pub fn migration_url(&self) -> String {
        match &self.erp_db {
            Some(db) => db.launcher_url(),
            None => self.embedded_database_url(),
        }
    }

    /// billing's JDBC form of database_url(): user and password move into the query
    pub fn jdbc_database_url(&self) -> Result<String> {
        let u = url::Url::parse(&self.database_url()).context("DATABASE_URL is not a URL")?;
        let mut q: Vec<(String, String)> = u.query_pairs().map(|(k, v)| (k.into_owned(), v.into_owned())).collect();
        let decode = |s: &str| percent_decode(s);
        q.push(("user".into(), decode(u.username())));
        if let Some(p) = u.password() {
            q.push(("password".into(), decode(p)));
        }
        let query = url::form_urlencoded::Serializer::new(String::new()).extend_pairs(q).finish();
        let port = u.port().map(|p| format!(":{p}")).unwrap_or_default();
        Ok(format!("jdbc:postgresql://{}{}{}?{}", u.host_str().unwrap_or("localhost"), port, u.path(), query))
    }

    /// DATABASE_CA_CERT for the services: the external database's CA, empty for the embedded one
    pub fn database_ca(&self) -> String {
        self.erp_db.as_ref().and_then(|d| d.ca.as_ref()).map(|c| c.to_string_lossy().into_owned()).unwrap_or_default()
    }

    fn embedded_database_url(&self) -> String {
        format!(
            "postgresql://postgres:{}@127.0.0.1:{}/sample_erp?sslmode=disable",
            self.secrets.db_password, self.db_port
        )
    }

    /// The vault's own database, in the same embedded cluster (created on first launch)
    pub fn vault_database_url(&self) -> String {
        format!(
            "postgresql://postgres:{}@127.0.0.1:{}/{}?sslmode=disable",
            self.secrets.db_password,
            self.db_port,
            crate::db::VAULT_DATABASE
        )
    }

    /// The TCC server's database, in the same embedded cluster (created and migrated on launch)
    pub fn tcc_database_url(&self) -> String {
        if let Some(db) = &self.tcc_db {
            return db.url.clone();
        }
        format!(
            "postgresql://postgres:{}@127.0.0.1:{}/{}?sslmode=disable",
            self.secrets.db_password,
            self.db_port,
            crate::db::TCC_DATABASE
        )
    }

    /// The launcher's connection for TCC migrations
    pub fn tcc_migration_url(&self) -> String {
        match &self.tcc_db {
            Some(db) => db.launcher_url(),
            None => self.tcc_database_url(),
        }
    }

    pub fn tcc_ca(&self) -> String {
        self.tcc_db.as_ref().and_then(|d| d.ca.as_ref()).map(|c| c.to_string_lossy().into_owned()).unwrap_or_default()
    }

    /// The memory limit of a service in MB, 0 for none
    pub fn memory_mb(&self, service: &str) -> u64 {
        self.memory_mb.get(service).copied().unwrap_or(0)
    }

    pub fn tcc_url(&self) -> String {
        format!("http://127.0.0.1:{}", self.tcc_port)
    }

    /// The public address: the gateway, the only port a browser (or Stripe) talks to.
    pub fn web_url(&self) -> String {
        let scheme = if self.gateway.tls.is_some() { "https" } else { "http" };
        format!("{scheme}://localhost:{}", self.web_port)
    }
}

fn number(vars: &HashMap<String, String>, key: &str, default: f64) -> Result<f64> {
    match vars.get(key) {
        Some(v) => v.parse::<f64>().ok().filter(|n| *n >= 0.0).with_context(|| format!("{key}={v} is not a non-negative number")),
        None => Ok(default),
    }
}

fn flag(vars: &HashMap<String, String>, key: &str, default: bool) -> bool {
    match vars.get(key).map(|v| v.to_ascii_lowercase()).as_deref() {
        Some("0" | "false" | "no" | "off") => false,
        Some("1" | "true" | "yes" | "on") => true,
        _ => default,
    }
}

/// KEY=value lines (dotenv syntax), or nothing when the file is missing.
pub fn read_env_file(path: &Path) -> Result<HashMap<String, String>> {
    if !path.exists() {
        return Ok(HashMap::new());
    }
    dotenvy::from_path_iter(path)
        .with_context(|| format!("reading {}", path.display()))?
        .map(|item| item.with_context(|| format!("parsing {}", path.display())))
        .collect()
}

// BILLING_INTERNAL_TOKEN is gone: api <-> billing is mutual TLS (data/certs); an old line is ignored
// TCC_TOKEN joined later: a data/secrets.env from before it gets the line added on the next launch
const SECRET_KEYS: [&str; 5] = ["POSTGRES_PASSWORD", "JWT_SECRET", "NETWORK_TOKEN", "VAULT_TOKEN", "TCC_TOKEN"];

fn load_secrets(data: &Path) -> Result<Secrets> {
    let path = data.join("secrets.env");
    let mut vars = read_env_file(&path)?;
    let mut changed = false;
    for key in SECRET_KEYS {
        if !vars.contains_key(key) {
            if key == "POSTGRES_PASSWORD" && data.join("postgres").join("data").exists() {
                bail!(
                    "{} is missing but data/postgres already exists with the password it held; \
                     restore the file, or delete the data folder to start over with an empty database",
                    path.display()
                );
            }
            vars.insert(key.into(), random_secret());
            changed = true;
        }
    }
    if changed {
        fs::create_dir_all(data)?;
        let body: String = SECRET_KEYS
            .iter()
            .map(|k| format!("{k}={}\n", vars[*k]))
            .collect();
        let header = "# Generated by sample-erp on first launch. POSTGRES_PASSWORD belongs to data/postgres:\n\
                      # keep them together, or delete both to start over.\n";
        fs::write(&path, format!("{header}{body}")).with_context(|| format!("writing {}", path.display()))?;
    }
    Ok(Secrets {
        db_password: vars.remove("POSTGRES_PASSWORD").unwrap(),
        jwt_secret: vars.remove("JWT_SECRET").unwrap(),
        network_token: vars.remove("NETWORK_TOKEN").unwrap(),
        vault_token: vars.remove("VAULT_TOKEN").unwrap(),
        tcc_token: vars.remove("TCC_TOKEN").unwrap(),
    })
}

/// 43 alphanumeric characters: about 256 bits, the size of an HS256 key (what secrets/generate.ts makes
/// for docker compose), and safe unquoted in a .env file or a connection string.
fn random_secret() -> String {
    rand::rng().sample_iter(&Alphanumeric).take(43).map(char::from).collect()
}

fn percent_decode(s: &str) -> String {
    url::form_urlencoded::parse(format!("x={}", s.replace('+', "%2B")).as_bytes())
        .next()
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default()
}
