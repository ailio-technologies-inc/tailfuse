//! Offline license verification (Ed25519-signed keys).
//!
//! Free tier: [`FREE_PROFILES`] tailnet profiles. Paid licenses are JSON
//! payloads signed by the vendor key embedded below; verification needs no
//! network, so licensed features work fully offline.

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};

/// Number of profiles usable without a license.
pub const FREE_PROFILES: usize = 2;

/// Vendor public key (base64url, 32 bytes). Generate a pair with
/// `licgen --gen-keys` and paste the public half here before issuing keys.
const PUBKEY: &str = "wB4m22qwhcqVabZC3NeXs3JAkjjbMwFmHyawJM_wwLI";

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct License {
    pub email: String,
    /// 0 = unlimited.
    #[serde(default)]
    pub max_profiles: usize,
    /// YYYY-MM-DD; passing this date only blocks *updates*, never the app.
    pub updates_until: String,
    #[serde(default)]
    pub issued_at: String,
    #[serde(default)]
    pub key_id: String,
}

impl License {
    pub fn updates_expired(&self) -> bool {
        chrono_date(&self.updates_until)
            .map(|d| d < today())
            .unwrap_or(false)
    }
}

fn chrono_date(s: &str) -> Option<(i32, u32, u32)> {
    let mut it = s.split('-');
    let y = it.next()?.parse().ok()?;
    let m = it.next()?.parse().ok()?;
    let d = it.next()?.parse().ok()?;
    Some((y, m, d))
}

fn today() -> (i32, u32, u32) {
    let days = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() / 86400)
        .unwrap_or(0) as i64;
    civil_from_days(days)
}

// Howard Hinnant's civil_from_days, no chrono dependency in the core crate.
fn civil_from_days(z: i64) -> (i32, u32, u32) {
    let z = z + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = (z - era * 146097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { y + 1 } else { y } as i32, m as u32, d as u32)
}

/// Parse and verify a key of the form `tailfuse_<payload_b64>__<sig_b64>`.
pub fn verify(key: &str) -> Result<License> {
    if PUBKEY == "PUBKEY_PLACEHOLDER" {
        bail!("this build has no vendor public key; licenses are not yet issuable");
    }
    let body = key
        .trim()
        .strip_prefix("tailfuse_")
        .context("invalid license key")?;
    let (payload_b64, sig_b64) = body.split_once("__").context("invalid license key")?;
    let payload = URL_SAFE_NO_PAD
        .decode(payload_b64)
        .context("invalid license payload")?;
    let sig_bytes = URL_SAFE_NO_PAD
        .decode(sig_b64)
        .context("invalid license signature")?;
    let vk_bytes = URL_SAFE_NO_PAD
        .decode(PUBKEY)
        .context("bad vendor key in build")?;
    let vk = VerifyingKey::from_bytes(
        &vk_bytes
            .try_into()
            .map_err(|_| anyhow::anyhow!("bad vendor key"))?,
    )?;
    let sig = Signature::from_slice(&sig_bytes).context("bad signature")?;
    vk.verify(&payload, &sig)
        .context("license signature mismatch")?;
    serde_json::from_slice(&payload).context("invalid license payload")
}

/// Nil if `n_profiles` is permitted, else a human-readable error.
pub fn check(n_profiles: usize, key: &str) -> Result<()> {
    if n_profiles <= FREE_PROFILES {
        return Ok(());
    }
    if key.trim().is_empty() {
        bail!("{n_profiles} profiles requires a license (free tier: {FREE_PROFILES}) — activate one with `tailfuse license activate <key>`");
    }
    let lic = verify(key).context("license invalid")?;
    if lic.max_profiles > 0 && n_profiles > lic.max_profiles {
        bail!(
            "license allows {} profiles, config has {n_profiles} — upgrade your license",
            lic.max_profiles
        );
    }
    Ok(())
}

/// One-line summary for UIs.
pub fn status(n_profiles: usize, key: &str) -> String {
    if n_profiles > FREE_PROFILES {
        if !key.trim().is_empty() {
            match verify(key) {
                Ok(lic) => {
                    let mut s = format!("licensed to {}", lic.email);
                    if lic.updates_expired() {
                        s.push_str(&format!(
                            " (updates expired {} — upgrade to keep receiving updates)",
                            lic.updates_until
                        ));
                    }
                    return s;
                }
                Err(_) => return "license present but invalid".into(),
            }
        }
        return format!("unlicensed ({n_profiles}/{FREE_PROFILES} free profiles)");
    }
    if !key.trim().is_empty() {
        if let Ok(lic) = verify(key) {
            return format!("licensed to {}", lic.email);
        }
    }
    format!("free ({n_profiles}/{FREE_PROFILES} profiles)")
}
