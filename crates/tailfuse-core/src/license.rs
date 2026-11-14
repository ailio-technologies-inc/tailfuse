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
const PUBKEY: &str = "x8npEBh1O8tGJrsosmTYcUomleSlPP9fB6Qa0TvzJNI";

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

/// Sign a license payload, returning a key string (tests; licgen has its own
/// vendored copy since the private key must never ship in the core crate).
#[cfg(test)]
pub fn sign(payload_json: &[u8], private_key_b64: &str) -> Result<String> {
    use ed25519_dalek::Signer;
    let sk_bytes = URL_SAFE_NO_PAD
        .decode(private_key_b64)
        .context("decoding private key")?;
    let sk = ed25519_dalek::SigningKey::from_bytes(
        &sk_bytes
            .try_into()
            .map_err(|_| anyhow::anyhow!("bad private key length"))?,
    );
    let sig = sk.sign(payload_json);
    Ok(format!(
        "tailfuse_{}__{}",
        URL_SAFE_NO_PAD.encode(payload_json),
        URL_SAFE_NO_PAD.encode(sig.to_bytes())
    ))
}

/// Generate a keypair as (private_b64, public_b64), unique per call.
#[cfg(test)]
pub fn generate_keypair() -> (String, String) {
    use std::sync::atomic::{AtomicU8, Ordering};
    static SEED: AtomicU8 = AtomicU8::new(1);
    let mut seed = [0u8; 32];
    seed[0] = SEED.fetch_add(1, Ordering::Relaxed);
    let sk = ed25519_dalek::SigningKey::from_bytes(&seed);
    (
        URL_SAFE_NO_PAD.encode(sk.to_bytes()),
        URL_SAFE_NO_PAD.encode(sk.verifying_key().to_bytes()),
    )
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

/// Named codes for groups we share Tailfuse with: typing the code is the same
/// as pasting its signed key, which is checked like any other.
const CODES: &[(&str, &str)] = &[(
    "CHANGE_ORG",
    "tailfuse_eyJlbWFpbCI6IkNoYW5nZS5vcmciLCJtYXhfcHJvZmlsZXMiOjAsInVwZGF0ZXNfdW50aWwiOiIyMDI3LTExLTEzIiwiaXNzdWVkX2F0IjoiMjAyNi0xMS0xNFQwMDowMDowMFoiLCJrZXlfaWQiOiJ0Zi1jaGFuZ2Utb3JnIn0__MYPXX30OzeDNt0U3i8Szq20ZVP5P0pHPhuOcI51osry8DYy6dpGlEypPs4QcEv_cjCQzjTb0lj02HnyGOHLABA",
)];

/// A named code's signed key, or the input unchanged.
fn expand(key: &str) -> String {
    let k = key.trim();
    let norm = k.to_ascii_uppercase().replace(['-', '.', ' '], "_");
    CODES
        .iter()
        .find(|(code, _)| *code == norm)
        .map(|(_, signed)| signed.to_string())
        .unwrap_or_else(|| k.to_string())
}

/// Parse and verify a key of the form `tailfuse_<payload_b64>__<sig_b64>`
/// against the embedded vendor key.
pub fn verify(key: &str) -> Result<License> {
    if PUBKEY == "PUBKEY_PLACEHOLDER" {
        bail!("this build has no vendor public key; licenses are not yet issuable");
    }
    verify_with(key, PUBKEY)
}

/// Verify against an explicit public key (tests, tooling).
pub fn verify_with(key: &str, pubkey: &str) -> Result<License> {
    let key = expand(key);
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
    let vk_bytes = URL_SAFE_NO_PAD.decode(pubkey).context("bad vendor key")?;
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
    check_with(n_profiles, key, PUBKEY)
}

/// Like [`check`] against an explicit vendor key (tests, tooling).
pub fn check_with(n_profiles: usize, key: &str, pubkey: &str) -> Result<()> {
    if n_profiles <= FREE_PROFILES {
        return Ok(());
    }
    if key.trim().is_empty() {
        bail!("{n_profiles} profiles requires a license (free tier: {FREE_PROFILES}) — activate one with `tailfuse license activate <key>`");
    }
    let lic = verify_with(key, pubkey).context("license invalid")?;
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn change_org_code_unlocks() {
        for code in ["CHANGE_ORG", "change_org", "change.org", " Change-Org "] {
            let lic = verify(code).expect("code verifies");
            assert_eq!(lic.email, "Change.org");
            assert_eq!(lic.max_profiles, 0);
            check(10, code).expect("unlimited profiles");
        }
        assert!(verify("CHANGE").is_err());
    }

    fn make_key(max_profiles: usize, email: &str) -> (String, String, String) {
        let (priv_b64, pub_b64) = generate_keypair();
        let payload = serde_json::to_vec(&License {
            email: email.to_string(),
            max_profiles,
            updates_until: "2099-01-01".into(),
            issued_at: "2026-01-01T00:00:00Z".into(),
            key_id: "test".into(),
        })
        .unwrap();
        let key = sign(&payload, &priv_b64).unwrap();
        (key, priv_b64, pub_b64)
    }

    #[test]
    fn roundtrip_verify() {
        let (key, _, pub_b64) = make_key(10, "user@example.com");
        let lic = verify_with(&key, &pub_b64).unwrap();
        assert_eq!(lic.email, "user@example.com");
        assert_eq!(lic.max_profiles, 10);
    }

    #[test]
    fn rejects_tampered_payload() {
        let (key, _, pub_b64) = make_key(10, "user@example.com");
        // Flip one character inside the base64 payload: decodes fine, but
        // the signature no longer matches.
        let mut chars: Vec<char> = key.chars().collect();
        chars[12] = if chars[12] == 'A' { 'B' } else { 'A' };
        let swapped: String = chars.into_iter().collect();
        assert!(verify_with(&swapped, &pub_b64).is_err());
    }

    #[test]
    fn rejects_wrong_key() {
        let (key, _, _) = make_key(10, "user@example.com");
        let (_, other_pub) = generate_keypair();
        assert!(verify_with(&key, &other_pub).is_err());
    }

    #[test]
    fn check_free_tier_and_limit() {
        assert!(check_with(2, "", "x").is_ok());
        assert!(check_with(3, "", "x").is_err());
        let (key, _, pub_b64) = make_key(5, "user@example.com");
        assert!(check_with(3, &key, &pub_b64).is_ok());
        assert!(check_with(6, &key, &pub_b64).is_err());
    }

    #[test]
    fn updates_expiry_is_warning_only() {
        let mut lic = License {
            email: "a@b.c".into(),
            max_profiles: 0,
            updates_until: "2020-01-01".into(),
            issued_at: String::new(),
            key_id: String::new(),
        };
        assert!(lic.updates_expired());
        lic.updates_until = "2099-01-01".into();
        assert!(!lic.updates_expired());
    }
}
