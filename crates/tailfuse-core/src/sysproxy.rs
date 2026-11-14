//! System PAC configuration. Only touches settings tailfuse owns: refuses to
//! overwrite an existing PAC URL that isn't ours, and clears only its own.

use anyhow::Result;

/// How an error from `apply` starts when the system wants an administrator.
pub const NEEDS_PERMISSION: &str = "permission needed";

/// True when `apply` failed only because an administrator has to allow it.
pub fn needs_permission(e: &anyhow::Error) -> bool {
    format!("{e:#}").starts_with(NEEDS_PERMISSION)
}

#[cfg(target_os = "macos")]
mod imp {
    use super::*;
    use anyhow::{bail, Context};

    /// The PAC URL set on a network service and whether it is switched on.
    fn current(svc: &str) -> (String, bool) {
        let out = std::process::Command::new("networksetup")
            .args(["-getautoproxyurl", svc])
            .output();
        let text = out
            .map(|o| String::from_utf8_lossy(&o.stdout).to_string())
            .unwrap_or_default();
        let url = text
            .lines()
            .find(|l| l.starts_with("URL:"))
            .map(|l| l.trim_start_matches("URL:").trim().to_string())
            // macOS prints "(null)" when no PAC URL is set.
            .filter(|u| u != "(null)")
            .unwrap_or_default();
        let on = text.lines().any(|l| l.trim() == "Enabled: Yes");
        (url, on)
    }

    /// Refuse to replace a PAC URL that is not ours.
    fn check_free(svc: &str, current: &str) -> Result<()> {
        if !current.is_empty() && !current.contains("/tailfuse.pac") {
            bail!("network service {svc:?} already uses another automatic proxy ({current}); Tailfuse will not replace it");
        }
        Ok(())
    }

    /// Point the system at the PAC URL. macOS needs an administrator for this,
    /// so the first time it fails with [`NEEDS_PERMISSION`]; once
    /// [`apply_with_prompt`] has set it, later calls find it in place.
    pub fn apply(pac_url: &str) -> Result<()> {
        let svc = primary_service()?;
        let (url, on) = current(&svc);
        if url == pac_url && on {
            return Ok(());
        }
        check_free(&svc, &url)?;
        let set = std::process::Command::new("networksetup")
            .args(["-setautoproxyurl", &svc, pac_url])
            .output()?;
        if !set.status.success() {
            bail!("{NEEDS_PERMISSION}: macOS asks for an administrator before the automatic proxy on {svc:?} can change");
        }
        let _ = std::process::Command::new("networksetup")
            .args(["-setautoproxystate", &svc, "on"])
            .status();
        Ok(())
    }

    /// Run two networksetup steps as an administrator; macOS shows its own
    /// password prompt. Arguments are passed to AppleScript, never spliced in.
    fn as_admin(prompt: &str, svc: &str, value: &str, state: &str) -> Result<()> {
        let script = r#"on run argv
set why to item 1 of argv
set svc to item 2 of argv
set pac to item 3 of argv
set onoff to item 4 of argv
do shell script "/usr/sbin/networksetup -setautoproxyurl " & quoted form of svc & " " & quoted form of pac & " && /usr/sbin/networksetup -setautoproxystate " & quoted form of svc & " " & quoted form of onoff with prompt why with administrator privileges
end run"#;
        let out = std::process::Command::new("osascript")
            .args(["-e", script, prompt, svc, value, state])
            .output()
            .context("asking macOS for permission")?;
        if !out.status.success() {
            let why = String::from_utf8_lossy(&out.stderr);
            if why.contains("-128") {
                bail!("you cancelled the password prompt");
            }
            bail!("macOS did not change the setting: {}", why.trim());
        }
        Ok(())
    }

    /// Like [`apply`], asking for an administrator password when needed.
    pub fn apply_with_prompt(pac_url: &str) -> Result<()> {
        let svc = primary_service()?;
        let (url, on) = current(&svc);
        if url == pac_url && on {
            return Ok(());
        }
        check_free(&svc, &url)?;
        as_admin(
            "Tailfuse wants to send your browsers to your tailnets.",
            &svc,
            pac_url,
            "on",
        )
    }

    /// Clear the setting if it is ours and no administrator is needed. It
    /// usually is needed, so the setting normally stays: with Tailfuse off the
    /// PAC file is unreachable and browsers go direct.
    pub fn clear() -> Result<()> {
        let svc = primary_service()?;
        let (url, _) = current(&svc);
        // Only touch the setting if the configured URL is ours — never
        // disable someone else's proxy configuration.
        if url.contains("/tailfuse.pac") {
            let _ = std::process::Command::new("networksetup")
                .args(["-setautoproxystate", &svc, "off"])
                .output();
            let _ = std::process::Command::new("networksetup")
                .args(["-setautoproxyurl", &svc, "off"])
                .output();
        }
        Ok(())
    }

    /// Remove our setting for good, asking for an administrator password.
    pub fn clear_with_prompt() -> Result<()> {
        let svc = primary_service()?;
        let (url, _) = current(&svc);
        if !url.contains("/tailfuse.pac") {
            return Ok(());
        }
        as_admin(
            "Tailfuse wants to stop sending your browsers to your tailnets.",
            &svc,
            " ",
            "off",
        )
    }

    fn primary_service() -> Result<String> {
        // Map the default-route interface to its network service name.
        let script = r#"
            dev=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
            [ -z "$dev" ] && exit 1
            networksetup -listnetworkserviceorder | awk -v dev="$dev" '
                /^\([0-9]+\)/ { name=$0; sub(/^\([0-9]+\) /, "", name) }
                index($0, "Device: " dev ")") { print name; exit }'
        "#;
        let out = std::process::Command::new("sh")
            .args(["-c", script])
            .output()
            .context("detecting primary network service")?;
        let svc = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !svc.is_empty() {
            return Ok(svc);
        }
        for candidate in ["Wi-Fi", "Ethernet", "USB 10/100/1000 LAN"] {
            if std::process::Command::new("networksetup")
                .args(["-getinfo", candidate])
                .output()
                .map(|o| o.status.success())
                .unwrap_or(false)
            {
                return Ok(candidate.to_string());
            }
        }
        bail!("could not determine primary network service")
    }
}

#[cfg(target_os = "windows")]
mod imp {
    use super::*;
    use anyhow::bail;

    const REG_PATH: &str = "HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings";

    pub fn apply(pac_url: &str) -> Result<()> {
        let ps = format!(
            "$cur = (Get-ItemProperty -Path '{REG_PATH}').AutoConfigURL; \
             if ($cur -and $cur -notlike '*tailfuse*') {{ exit 2 }}; \
             Set-ItemProperty -Path '{REG_PATH}' -Name AutoConfigURL -Value '{pac_url}'; \
             Set-ItemProperty -Path '{REG_PATH}' -Name AutoDetect -Value 0"
        );
        run_ps(&ps)?;
        refresh();
        Ok(())
    }

    pub fn clear() -> Result<()> {
        let ps = format!(
            "if ((Get-ItemProperty -Path '{REG_PATH}').AutoConfigURL -like '*tailfuse*') {{ \
                Remove-ItemProperty -Path '{REG_PATH}' -Name AutoConfigURL -ErrorAction SilentlyContinue }}"
        );
        let _ = run_ps(&ps);
        refresh();
        Ok(())
    }

    fn run_ps(script: &str) -> Result<()> {
        let status = std::process::Command::new("powershell")
            .args(["-NoProfile", "-NonInteractive", "-Command", script])
            .status()?;
        match status.code() {
            Some(2) => bail!("a non-tailfuse PAC URL is already configured; refusing to overwrite"),
            Some(0) => Ok(()),
            _ => bail!("powershell proxy configuration failed"),
        }
    }

    fn refresh() {
        let _ = std::process::Command::new("RunDll32.exe")
            .args(["inetcpl.cpl,ClearMyTracksByProcess", "8"])
            .status();
    }
}

#[cfg(not(any(target_os = "macos", target_os = "windows")))]
mod imp {
    use super::*;
    use anyhow::bail;
    pub fn apply(_pac_url: &str) -> Result<()> {
        bail!("system PAC configuration not supported on this platform — point your browser at the PAC URL manually (see `tailfuse serve-pac`)")
    }
    pub fn clear() -> Result<()> {
        Ok(())
    }
}

pub use imp::{apply, clear};

#[cfg(target_os = "macos")]
pub use imp::{apply_with_prompt, clear_with_prompt};

/// Other systems need no administrator: the prompt variants are the plain ones.
#[cfg(not(target_os = "macos"))]
pub fn apply_with_prompt(pac_url: &str) -> Result<()> {
    apply(pac_url)
}

#[cfg(not(target_os = "macos"))]
pub fn clear_with_prompt() -> Result<()> {
    clear()
}
