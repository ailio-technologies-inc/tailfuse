fn main() {
    #[cfg(target_os = "macos")]
    tunnel_control();
    tauri_build::build()
}

/// Compiles `apps/desktop/macos/TunnelControl` (Swift) into a static library and links it, with
/// the frameworks it uses and the Swift runtime (in the OS since macOS 12.3; found at run time
/// through the /usr/lib/swift rpath). See `src/tunnel.rs`.
#[cfg(target_os = "macos")]
fn tunnel_control() {
    use std::{env, path::PathBuf, process::Command};
    let src = PathBuf::from("../macos/TunnelControl/TunnelControl.swift");
    println!("cargo:rerun-if-changed={}", src.display());
    let out = PathBuf::from(env::var("OUT_DIR").unwrap());
    let arch = match env::var("CARGO_CFG_TARGET_ARCH").unwrap().as_str() {
        "aarch64" => "arm64",
        other => other,
    }
    .to_string();
    let lib = out.join("libtunnelcontrol.a");
    let status = Command::new("xcrun")
        .args([
            "swiftc",
            "-O",
            "-parse-as-library",
            "-module-name",
            "TunnelControl",
            "-emit-library",
            "-static",
        ])
        .arg("-target")
        .arg(format!("{arch}-apple-macos13.0"))
        .arg("-o")
        .arg(&lib)
        .arg(&src)
        .status()
        .expect("xcrun swiftc (Xcode's command line tools) must be installed to build the Mac app");
    assert!(status.success(), "TunnelControl.swift did not compile");
    let toolchain = String::from_utf8(
        Command::new("xcrun")
            .args(["--find", "swiftc"])
            .output()
            .unwrap()
            .stdout,
    )
    .unwrap();
    let toolchain_lib = PathBuf::from(toolchain.trim())
        .parent()
        .unwrap()
        .parent()
        .unwrap()
        .join("lib/swift/macosx");
    let sdk = String::from_utf8(
        Command::new("xcrun")
            .args(["--sdk", "macosx", "--show-sdk-path"])
            .output()
            .unwrap()
            .stdout,
    )
    .unwrap();
    println!("cargo:rustc-link-search=native={}", out.display());
    println!("cargo:rustc-link-search=native={}", toolchain_lib.display());
    println!(
        "cargo:rustc-link-search=native={}/usr/lib/swift",
        sdk.trim()
    );
    println!("cargo:rustc-link-lib=static=tunnelcontrol");
    println!("cargo:rustc-link-lib=framework=NetworkExtension");
    println!("cargo:rustc-link-lib=framework=SystemExtensions");
    println!("cargo:rustc-link-arg=-Wl,-rpath,/usr/lib/swift");
}
