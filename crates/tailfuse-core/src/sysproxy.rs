//! System PAC configuration. Only touches settings tailfuse owns: refuses to
//! overwrite an existing PAC URL that isn't ours, and clears only its own.

use anyhow::Result;

#[cfg(target_os = "macos")]
mod imp {
    use super::*;
    use anyhow::{bail, Context};

    pub fn apply(pac_url: &str) -> Result<()> {
        let svc = primary_service()?;
        let out = std::process::Command::new("networksetup")
            .args(["-getautoproxyurl", &svc])
            .output()?;
        let current = String::from_utf8_lossy(&out.stdout)
            .lines()
            .find(|l| l.starts_with("URL:"))
            .map(|l| l.trim_start_matches("URL:").trim().to_string())
            .unwrap_or_default();
        if !current.is_empty() && !current.contains("/tailfuse.pac") {
            bail!("network service {svc:?} already has a PAC URL ({current}); refusing to overwrite — configure your browser manually");
        }
        let status = std::process::Command::new("networksetup")
            .args(["-setautoproxyurl", &svc, pac_url])
            .status()?;
        if !status.success() {
            bail!("networksetup -setautoproxyurl failed (admin rights?)");
        }
        let _ = std::process::Command::new("networksetup")
            .args(["-setautoproxystate", &svc, "on"])
            .status();
        Ok(())
    }

    pub fn clear() -> Result<()> {
        let svc = primary_service()?;
        let out = std::process::Command::new("networksetup")
            .args(["-getautoproxyurl", &svc])
            .output()?;
        let text = String::from_utf8_lossy(&out.stdout);
        // Only touch the setting if the configured URL is ours — never
        // disable someone else's proxy configuration.
        if text.contains("/tailfuse.pac") {
            let _ = std::process::Command::new("networksetup")
                .args(["-setautoproxystate", &svc, "off"])
                .status();
            let _ = std::process::Command::new("networksetup")
                .args(["-setautoproxyurl", &svc, "off"])
                .status();
        }
        Ok(())
    }

    fn primary_service() -> Result<String> {
        // Map the default-route interface to its network service name.
        let script = r#"
            dev=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
            [ -z "$dev" ] && exit 1
            networksetup -listnetworkserviceorder | awk -v dev="$dev" '
                /\(Hardware Port|Device: / {
                    if ($0 ~ "Device: " dev "$") {
                        getline line
                    }
                }
                /^\([0-9]+\)/ { name=$0 }
                $0 ~ "Device: " dev "$" {
                    sub(/^\([0-9]+\) /, "", name)
                    print name
                    exit
                }'
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
