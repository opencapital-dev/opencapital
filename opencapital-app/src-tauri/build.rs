fn main() {
    // Expose the compile target triple to the crate so compute.rs can resolve
    // the `compute-<triple>` externalBin sidecar the Make stager produces.
    println!(
        "cargo:rustc-env=TARGET_TRIPLE={}",
        std::env::var("TARGET").unwrap_or_default()
    );
    tauri_build::build()
}
