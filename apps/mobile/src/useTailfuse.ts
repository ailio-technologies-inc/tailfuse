// All the screen's state and actions: polling, the add-a-tailnet sign-in, the permission sheet,
// errors. The UI files only render what this returns.
import AsyncStorage from "@react-native-async-storage/async-storage";
import * as WebBrowser from "expo-web-browser";
import { useCallback, useEffect, useRef, useState } from "react";
import { AppState } from "react-native";

import * as engine from "./engine";
import * as haptics from "./haptics";
import { friendly, isLimitError, stabilize, type UpgradeResume } from "./logic";
import type { State } from "./types";

const ASKED_KEY = "tailfuse.permission.asked.v1";
const SIGN_IN_TIMEOUT_MS = 10 * 60 * 1000;
const REFRESH_MS = 3000;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export type ErrorNote = { title: string; text: string };

// `onLimit` is told when the engine refuses a tailnet past the free limit (instead of showing a note).
export function useTailfuse(onLimit?: (resume: UpgradeResume) => void) {
  const limitRef = useRef(onLimit);
  useEffect(() => {
    limitRef.current = onLimit;
  });
  const [state, setState] = useState<State | null>(null);
  const [error, setError] = useState<ErrorNote | null>(null);
  const [powerBusy, setPowerBusy] = useState(false);
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());
  // Which tailnet's sign-in is running ("" while a new one is being added), or null.
  const [signingIn, setSigningIn] = useState<string | null>(null);
  const [message, setMessage] = useState("");
  const [permissionOpen, setPermissionOpen] = useState(false);
  const permissionResolve = useRef<((ok: boolean) => void) | null>(null);
  const alive = useRef(true);
  const inflight = useRef(false);
  const lastPollError = useRef("");

  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const showError = useCallback((err: unknown) => {
    if (isLimitError(err) && limitRef.current) {
      limitRef.current(null);
      return;
    }
    const [title, text] = friendly(err);
    haptics.warning();
    setError({ title, text });
  }, []);

  const refresh = useCallback(async (): Promise<State | null> => {
    if (inflight.current) return null;
    inflight.current = true;
    try {
      const s = await engine.getState();
      if (alive.current) setState((prev) => stabilize(prev, s));
      lastPollError.current = "";
      return s;
    } catch (err) {
      // A failing poll would otherwise re-raise the note every 3 s after Dismiss.
      const msg = String((err as Error)?.message ?? err);
      if (msg !== lastPollError.current) showError(err);
      lastPollError.current = msg;
      return null;
    } finally {
      inflight.current = false;
    }
  }, [showError]);

  // Refresh every 3 s in the foreground and on the engine's change event; pause in the background.
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null;
    const startPolling = () => {
      if (timer) return;
      refresh();
      timer = setInterval(refresh, REFRESH_MS);
    };
    const stopPolling = () => {
      if (timer) clearInterval(timer);
      timer = null;
    };
    if (AppState.currentState === "active") startPolling();
    const sub = AppState.addEventListener("change", (s) => (s === "active" ? startPolling() : stopPolling()));
    const off = engine.onChange(() => {
      if (AppState.currentState === "active") refresh();
    });
    return () => {
      sub.remove();
      off();
      stopPolling();
    };
  }, [refresh]);

  // ----- the permission sheet: before the first start() only -----

  const askPermission = useCallback(async (): Promise<boolean> => {
    let asked = false;
    try {
      asked = (await AsyncStorage.getItem(ASKED_KEY)) === "1";
    } catch {
      // storage unavailable: ask again, which is harmless
    }
    if (asked) return true;
    setPermissionOpen(true);
    const ok = await new Promise<boolean>((resolve) => (permissionResolve.current = resolve));
    if (ok) AsyncStorage.setItem(ASKED_KEY, "1").catch(() => {});
    return ok;
  }, []);

  const answerPermission = useCallback((ok: boolean) => {
    setPermissionOpen(false);
    permissionResolve.current?.(ok);
    permissionResolve.current = null;
  }, []);

  // Turns Tailfuse on (sheet first, then the system's own prompt). False if the person said no.
  const ensureOn = useCallback(async (): Promise<boolean> => {
    if (state?.up) return true;
    if (!(await askPermission())) return false;
    await engine.start();
    return true;
  }, [state?.up, askPermission]);

  const setPower = useCallback(async (on: boolean) => {
    setPowerBusy(true);
    try {
      if (on) await ensureOn();
      else await engine.stop();
    } catch (err) {
      showError(err);
    }
    setPowerBusy(false);
    refresh();
  }, [ensureOn, refresh, showError]);

  // ----- tailnets -----

  const setConnected = useCallback(async (name: string, on: boolean) => {
    setPending((p) => new Set(p).add(name));
    try {
      await engine.setConnected(name, on);
      if (on) haptics.success();
    } catch (err) {
      showError(err);
    }
    setPending((p) => {
      const n = new Set(p);
      n.delete(name);
      return n;
    });
    refresh();
  }, [refresh, showError]);

  const remove = useCallback(async (kind: "profile" | "tunnel", name: string) => {
    try {
      await (kind === "profile" ? engine.removeProfile(name) : engine.removeTunnel(name));
    } catch (err) {
      showError(err);
    }
    refresh();
  }, [refresh, showError]);

  // Polls until `name` is online, or gone because the tailnet already existed; opens its sign-in
  // page once it has one. Returns the name to report, or null when the wait ended without success.
  const waitForSignIn = useCallback(async (name: string, before: Set<string>, isNew: boolean): Promise<string | null> => {
    const deadline = Date.now() + SIGN_IN_TIMEOUT_MS;
    let opened = "";
    while (alive.current && Date.now() < deadline) {
      await sleep(1000);
      const s = await engine.getState().catch(() => null);
      if (!s) continue;
      if (alive.current) setState((prev) => stabilize(prev, s));
      if (!s.up) return null; // switched off meanwhile
      const p = s.profiles.find((x) => x.name === name);
      if (!p) {
        // Renamed after the tailnet, or dropped because it already existed.
        const fresh = s.profiles.find((x) => !before.has(x.name));
        return fresh?.name ?? "Your tailnet";
      }
      if (p.error) return null;
      if (p.auth_url && p.auth_url !== opened) {
        opened = p.auth_url;
        // On iOS this resolves when the sheet closes; polling carries on either way.
        WebBrowser.openBrowserAsync(p.auth_url, { toolbarColor: "#18191b", controlsColor: "#ff4a1f" }).catch(() => {});
      }
      if (p.online) return isNew ? p.name : name;
    }
    if (alive.current && Date.now() >= deadline) showError(new Error("sign-in not finished in time"));
    return null;
  }, [showError]);

  const finishSignIn = useCallback((name: string | null) => {
    if (!name) return;
    try {
      // iOS only; elsewhere the browser stays until the person returns.
      WebBrowser.dismissBrowser();
    } catch {
      // not supported on this platform
    }
    haptics.success();
    setMessage(`${name} is signed in and coming online.`);
  }, []);

  const addTailnet = useCallback(async () => {
    if (signingIn !== null) return;
    setMessage("");
    setSigningIn("");
    try {
      if (!(await ensureOn())) return;
      const before = new Set((state?.profiles ?? []).map((p) => p.name));
      const { name } = await engine.addProfile();
      finishSignIn(await waitForSignIn(name, before, true));
    } catch (err) {
      if (isLimitError(err) && limitRef.current) limitRef.current("add");
      else showError(err);
    } finally {
      if (alive.current) setSigningIn(null);
      refresh();
    }
  }, [signingIn, ensureOn, state?.profiles, waitForSignIn, finishSignIn, refresh, showError]);

  const signInTo = useCallback(async (name: string) => {
    if (signingIn !== null) return;
    setMessage("");
    setSigningIn(name);
    try {
      // A tailnet set up from a computer's code can be signed in to while Tailfuse is off: turn it on first.
      if (!(await ensureOn())) return;
      await engine.login(name);
      finishSignIn(await waitForSignIn(name, new Set(), false));
    } catch (err) {
      showError(err);
    } finally {
      if (alive.current) setSigningIn(null);
      refresh();
    }
  }, [signingIn, ensureOn, waitForSignIn, finishSignIn, refresh, showError]);

  return {
    state,
    error,
    dismissError: () => setError(null),
    showError,
    refresh,
    powerBusy,
    setPower,
    pending,
    setConnected,
    remove,
    signingIn,
    message,
    addTailnet,
    signInTo,
    permissionOpen,
    answerPermission,
  };
}
