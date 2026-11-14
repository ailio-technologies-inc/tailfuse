//! Automatic updates. The app checks the latest GitHub release's `latest.json`
//! (signed per platform with the updater key; the release lanes write it) a
//! minute after launch and then every six hours, downloads and installs a newer
//! version, and relaunches into it as soon as Tailfuse is off, so a running
//! session is never cut. Nothing here talks to Ailio's own servers.

use std::time::Duration;
use tauri::{AppHandle, Runtime};
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_updater::UpdaterExt;

const FIRST_CHECK: Duration = Duration::from_secs(60);
const EVERY: Duration = Duration::from_secs(6 * 60 * 60);

/// Start the background checker. `is_running` says whether Tailfuse is on.
pub fn spawn<R, F, Fut>(app: AppHandle<R>, is_running: F)
where
    R: Runtime,
    F: Fn() -> Fut + Send + Sync + 'static,
    Fut: std::future::Future<Output = bool> + Send,
{
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(FIRST_CHECK).await;
        loop {
            match check_and_install(&app).await {
                Ok(Some(version)) => {
                    // Installed: relaunch once Tailfuse is off, so nobody loses a connection.
                    if is_running().await {
                        let _ = app
                            .notification()
                            .builder()
                            .title("Tailfuse update ready")
                            .body(format!(
                                "Version {version} is installed. It starts the next time Tailfuse is off."
                            ))
                            .show();
                        while is_running().await {
                            tokio::time::sleep(Duration::from_secs(60)).await;
                        }
                    }
                    app.restart();
                }
                Ok(None) => {}
                Err(e) => tracing::warn!("update check: {e:#}"),
            }
            tokio::time::sleep(EVERY).await;
        }
    });
}

/// Install a newer version if there is one; returns its version.
async fn check_and_install<R: Runtime>(app: &AppHandle<R>) -> anyhow::Result<Option<String>> {
    let Some(update) = app.updater()?.check().await? else {
        return Ok(None);
    };
    let version = update.version.clone();
    tracing::info!("installing update {version}");
    update.download_and_install(|_, _| {}, || {}).await?;
    Ok(Some(version))
}
