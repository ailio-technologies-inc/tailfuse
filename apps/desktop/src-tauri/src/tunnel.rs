//! All-apps mode on the Mac: the packet-tunnel system extension
//! (`apps/desktop/macos/TailfuseTunnel`), which runs the phones' Go engine so every program on the
//! Mac reaches every running tailnet by name and address, with no proxy settings. This file is the
//! Rust face of `apps/desktop/macos/TunnelControl/TunnelControl.swift` (linked by `build.rs`).
//!
//! Each call blocks until the Swift side answers, so callers run it on a blocking thread
//! (`tauri::async_runtime::spawn_blocking`). Elsewhere than macOS every call says it is not
//! available.

#[cfg(target_os = "macos")]
mod imp {
    use std::ffi::{c_char, c_void, CStr, CString};
    use std::sync::mpsc::{sync_channel, SyncSender};
    use std::time::Duration;

    type Done = extern "C" fn(*mut c_void, i32, *const c_char);

    extern "C" {
        fn tf_tunnel_activate(ctx: *mut c_void, done: Done);
        fn tf_tunnel_start(device_name: *const c_char, ctx: *mut c_void, done: Done);
        fn tf_tunnel_stop(ctx: *mut c_void, done: Done);
        fn tf_tunnel_status(ctx: *mut c_void, done: Done);
        fn tf_tunnel_call(request: *const c_char, ctx: *mut c_void, done: Done);
    }

    type Answer = Result<String, String>;

    /// `ctx` is a leaked `SyncSender`; the Swift side may answer more than once (activation sends
    /// "needs_approval" first), so the sender is never freed by the callback. A few bytes per
    /// activation, bounded by how often a person installs the tunnel.
    extern "C" fn done(ctx: *mut c_void, ok: i32, text: *const c_char) {
        let tx = unsafe { &*(ctx as *const SyncSender<Answer>) };
        let text = unsafe { CStr::from_ptr(text) }
            .to_string_lossy()
            .into_owned();
        let _ = tx.try_send(if ok == 1 { Ok(text) } else { Err(text) });
    }

    fn wait<F: FnOnce(*mut c_void)>(call: F) -> std::sync::mpsc::Receiver<Answer> {
        let (tx, rx) = sync_channel::<Answer>(4);
        let ctx = Box::into_raw(Box::new(tx)) as *mut c_void;
        call(ctx);
        rx
    }

    fn one<F: FnOnce(*mut c_void)>(timeout: Duration, call: F) -> Answer {
        wait(call)
            .recv_timeout(timeout)
            .unwrap_or_else(|_| Err("the tunnel did not answer in time".into()))
    }

    /// Installs or updates the extension. `on_approval` runs when macOS wants the person to
    /// approve it in System Settings; the call keeps waiting (up to 10 minutes) for that.
    pub fn activate(on_approval: impl Fn()) -> Answer {
        let rx = wait(|ctx| unsafe { tf_tunnel_activate(ctx, done) });
        loop {
            match rx.recv_timeout(Duration::from_secs(600)) {
                Ok(Ok(s)) if s == "needs_approval" => on_approval(),
                Ok(answer) => return answer,
                Err(_) => return Err("the tunnel was not approved in time".into()),
            }
        }
    }

    pub fn start(device_name: &str) -> Answer {
        let name = CString::new(device_name.replace('\0', "")).unwrap_or_default();
        one(Duration::from_secs(45), |ctx| unsafe {
            tf_tunnel_start(name.as_ptr(), ctx, done)
        })
    }

    pub fn stop() -> Answer {
        one(Duration::from_secs(30), |ctx| unsafe {
            tf_tunnel_stop(ctx, done)
        })
    }

    pub fn status() -> Answer {
        one(Duration::from_secs(10), |ctx| unsafe {
            tf_tunnel_status(ctx, done)
        })
    }

    /// One engine request (`engines/mobile/API.md`): `{"cmd": …, "args": …}` in, the engine's
    /// `{"ok": …}` JSON out.
    pub fn call(request: &str) -> Answer {
        let req = CString::new(request.replace('\0', "")).unwrap_or_default();
        one(Duration::from_secs(90), |ctx| unsafe {
            tf_tunnel_call(req.as_ptr(), ctx, done)
        })
    }
}

#[cfg(not(target_os = "macos"))]
mod imp {
    const NO: &str = "all-apps mode is only on macOS so far";
    type Answer = Result<String, String>;
    pub fn activate(_on_approval: impl Fn()) -> Answer {
        Err(NO.into())
    }
    pub fn start(_device_name: &str) -> Answer {
        Err(NO.into())
    }
    pub fn stop() -> Answer {
        Err(NO.into())
    }
    pub fn status() -> Answer {
        Ok("none".into())
    }
    pub fn call(_request: &str) -> Answer {
        Err(NO.into())
    }
}

pub use imp::*;
