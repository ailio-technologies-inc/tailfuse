//! tailfuse-core: connect to multiple Tailscale tailnets at once.
//!
//! One `tailscaled` subprocess per profile (userspace networking mode, so no
//! TUN/route/DNS conflicts), a loopback router that dispatches SOCKS5 and
//! HTTP CONNECT by hostname suffix, PAC generation, local TCP tunnels, and
//! offline license verification.

pub mod config;
pub mod daemon;
pub mod license;
pub mod pac;
pub mod router;
pub mod sysproxy;
pub mod tunnels;

pub use config::{Config, Profile, Tunnel};
