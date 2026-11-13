//! licgen: sign tailfuse license keys. Vendor-only — the private key is
//! never distributed.
//!
//!   licgen --gen-keys                                  # one-time keypair
//!   licgen --email user@example.com --max-profiles 10  # sign a key

use anyhow::{Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use clap::Parser;
use ed25519_dalek::{Signer, SigningKey};
use rand::rngs::OsRng;
use tailfuse_core::license::License;

#[derive(Parser)]
#[command(name = "licgen", about = "tailfuse license key signer (vendor-only)")]
struct Args {
    /// Generate a fresh Ed25519 keypair and exit.
    #[arg(long)]
    gen_keys: bool,
    /// base64url private key (or env TAILFUSE_LICENSE_PRIVKEY).
    #[arg(long, env = "TAILFUSE_LICENSE_PRIVKEY")]
    private_key: Option<String>,
    #[arg(long)]
    email: Option<String>,
    /// Max profiles (0 = unlimited).
    #[arg(long, default_value_t = 0)]
    max_profiles: usize,
    /// Updates until, YYYY-MM-DD (default: 1 year from now).
    #[arg(long)]
    updates_until: Option<String>,
}

fn main() -> Result<()> {
    let args = Args::parse();

    if args.gen_keys {
        let sk = SigningKey::generate(&mut OsRng);
        println!(
            "public : {}",
            URL_SAFE_NO_PAD.encode(sk.verifying_key().to_bytes())
        );
        println!("private: {}", URL_SAFE_NO_PAD.encode(sk.to_bytes()));
        println!("\nPaste the public key into crates/tailfuse-core/src/license.rs (PUBKEY).");
        return Ok(());
    }

    let email = args.email.context("--email is required")?;
    let priv_b64 = args
        .private_key
        .context("--private-key is required (or run --gen-keys)")?;
    let sk_bytes = URL_SAFE_NO_PAD
        .decode(&priv_b64)
        .context("decoding private key")?;
    let sk = SigningKey::from_bytes(
        &sk_bytes
            .try_into()
            .map_err(|_| anyhow::anyhow!("bad private key length"))?,
    );

    let updates_until = args.updates_until.unwrap_or_else(|| {
        (chrono::Utc::now() + chrono::Duration::days(365))
            .format("%Y-%m-%d")
            .to_string()
    });
    let payload = serde_json::to_vec(&License {
        email,
        max_profiles: args.max_profiles,
        updates_until,
        issued_at: chrono::Utc::now().to_rfc3339(),
        key_id: format!("tf-{}", chrono::Utc::now().timestamp()),
    })?;
    let sig = sk.sign(&payload);
    println!(
        "tailfuse_{}__{}",
        URL_SAFE_NO_PAD.encode(&payload),
        URL_SAFE_NO_PAD.encode(sig.to_bytes())
    );
    Ok(())
}
