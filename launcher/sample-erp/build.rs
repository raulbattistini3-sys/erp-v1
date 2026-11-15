// rust-embed reads these folders when the crate compiles, but cargo only notices edits to files it
// already embedded. Watching the folders also picks up new and deleted files (a fresh vite build
// renames every hashed asset), so `cargo xtask dist` never ships a stale web app or migration list.
// The launcher sits in erp-v1/launcher: the ERP is ../sample-ERP/sample-ERP, the TCC ../tcc-v2/tcc-v1.
fn main() {
    for dir in [
        "../../sample-ERP/sample-ERP/web/dist",
        "../../sample-ERP/sample-ERP/api/drizzle",
        "../../sample-ERP/sample-ERP/docker/postgres/init",
        "../../tcc-v2/tcc-v1/demo-monorepo-workspace/apps/server/drizzle",
    ] {
        println!("cargo:rerun-if-changed={dir}");
    }
}
