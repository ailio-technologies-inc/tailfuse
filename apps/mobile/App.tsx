import { HostGrotesk_400Regular, HostGrotesk_500Medium } from "@expo-google-fonts/host-grotesk";
import { JetBrainsMono_400Regular } from "@expo-google-fonts/jetbrains-mono";
import { useFonts } from "expo-font";
import * as SplashScreen from "expo-splash-screen";
import { StatusBar } from "expo-status-bar";
import * as Linking from "expo-linking";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { RefreshControl, ScrollView, StyleSheet, View } from "react-native";
import { SafeAreaProvider, useSafeAreaInsets } from "react-native-safe-area-context";

import { Button, GearIcon, IconButton, Mark, Notice, PortIcon, PressableCard, SearchIcon, Switch, T } from "./src/components";
import * as haptics from "./src/haptics";
import { addNeedsUpgrade, findSyncLink, statusLine, type UpgradeResume } from "./src/logic";
import { deviceWord } from "./src/platform";
import { preview } from "./src/preview";
import { Sheet } from "./src/Sheet";
import { AboutSection, SettingsSection } from "./src/SettingsSections";
import { SyncSheet } from "./src/SyncSheet";
import { TailnetSheet } from "./src/TailnetSheet";
import { EmptyState, TailnetsSection } from "./src/TailnetsSection";
import { colors, MAX_W } from "./src/theme";
import { CheckSheet, PortsSheet } from "./src/ToolsSections";
import { UnlockSection } from "./src/UnlockSection";
import { UpgradeSheet } from "./src/UpgradeSheet";
import { useTailfuse } from "./src/useTailfuse";
import { useUnlock } from "./src/useUnlock";

SplashScreen.preventAutoHideAsync().catch(() => {});

type SheetKind = "settings" | "check" | "ports" | "tailnet" | "sync" | "upgrade" | null;

const initialSheet = (): SheetKind => {
  const s = preview.sheet;
  if (s === "settings" || s === "check" || s === "ports" || s === "upgrade") return s;
  if (s === "sync" || s === "sync-confirm" || s === "sync-done") return "sync";
  if (s === "ports-form") return "ports";
  if (s === "tailnet" || (s && !["permission", "empty", "upgrade"].includes(s))) return "tailnet";
  return null;
};

// Content is centred and never wider than MAX_W (iPad, web).
const Column = ({ children, side = 20 }: { children: ReactNode; side?: number }) => (
  <View style={{ width: "100%", maxWidth: MAX_W, alignSelf: "center", paddingHorizontal: side }}>{children}</View>
);

function Screen() {
  const insets = useSafeAreaInsets();
  const [sheet, setSheet] = useState<SheetKind>(initialSheet);
  // What the person was doing when the free limit stopped them; the upgrade modal carries on with it.
  const [resume, setResume] = useState<UpgradeResume>(null);
  const openUpgrade = useRef((r: UpgradeResume) => {
    setResume(r);
    setSheet("upgrade");
  });
  const tf = useTailfuse((r) => openUpgrade.current(r));
  const s = tf.state;
  const device = deviceWord();
  const unlock = useUnlock(tf.refresh);
  const [selected, setSelected] = useState<string | undefined>(undefined);
  const [refreshing, setRefreshing] = useState(false);
  const [previewPermission, setPreviewPermission] = useState(preview.sheet === "permission");

  const [syncLink, setSyncLink] = useState<string | null>(null);
  const close = useCallback(() => setSheet(null), []);

  // tailfuse://sync?... from the phone's camera or a tapped link, cold start or while running: open the
  // Sync sheet at its confirmation. Nothing is added until the person confirms.
  useEffect(() => {
    const take = (url: string | null) => {
      const link = url ? findSyncLink(url) : null;
      if (!link) return;
      setSyncLink(link);
      setSheet("sync");
    };
    Linking.getInitialURL().then(take).catch(() => {});
    const sub = Linking.addEventListener("url", ({ url }) => take(url));
    return () => sub.remove();
  }, []);
  const pull = async () => {
    setRefreshing(true);
    await tf.refresh();
    setRefreshing(false);
  };

  // Preview only: ?sheet=tailnet or ?sheet=<profile name> picks the tailnet for the detail sheet.
  const previewProfile = preview.sheet ? (s?.profiles.find((p) => p.name === preview.sheet) ?? s?.profiles[0]) : undefined;
  const profile = s?.profiles.find((p) => p.name === selected) ?? (selected === undefined ? previewProfile : undefined);
  const permissionOpen = tf.permissionOpen || previewPermission;
  const answer = (ok: boolean) => {
    setPreviewPermission(false);
    tf.answerPermission(ok);
  };
  // Only one sheet at a time: the permission sheet takes over while it is asking.
  const open = (k: Exclude<SheetKind, null>) => !permissionOpen && sheet === k;

  const add = useCallback(() => {
    if (s && addNeedsUpgrade(s)) openUpgrade.current("add");
    else void tf.addTailnet();
  }, [s, tf]);

  // The unlock is held: close the modal and carry on with what the person started.
  const finishUpgrade = useCallback(() => {
    haptics.success();
    unlock.clearNote();
    if (resume === "sync") setSheet("sync");
    else {
      setSheet(null);
      if (resume === "add") void tf.addTailnet();
    }
    setResume(null);
  }, [resume, tf, unlock]);
  const buy = async () => {
    if (await unlock.unlock()) finishUpgrade();
  };
  const restore = async () => {
    if (await unlock.restore()) finishUpgrade();
  };
  // An approval that lands while the modal is open (Ask to Buy) closes it; the person taps Add again.
  const owned = !!s?.unlocked;
  const [wasOwned, setWasOwned] = useState(owned);
  if (wasOwned !== owned) {
    setWasOwned(owned);
    if (owned && sheet === "upgrade") {
      setSheet(null);
      setResume(null);
    }
  }
  const openTailnet = useCallback((name: string) => {
    setSelected(name);
    setSheet("tailnet");
  }, []);

  return (
    <View style={styles.root}>
      <StatusBar style="light" />
      <View style={[styles.header, { paddingTop: insets.top }]}>
        <Column side={20 + Math.max(insets.left, insets.right)}>
          <View style={styles.headerRow}>
            <View style={styles.brand} accessible accessibilityRole="header" accessibilityLabel="Tailfuse">
              <Mark width={23} />
              <T v="h1">Tailfuse</T>
            </View>
            <View style={styles.power}>
              <T v="label" medium color={s?.up ? colors.ok : colors.muted}>{s?.up ? "On" : "Off"}</T>
              <Switch value={!!s?.up} label="Turn Tailfuse on or off" onValueChange={tf.setPower} disabled={!s || tf.powerBusy} />
              <IconButton onPress={() => setSheet("settings")} label="Settings">
                <GearIcon />
              </IconButton>
            </View>
          </View>
        </Column>
      </View>

      <ScrollView
        style={styles.flex}
        contentContainerStyle={{ paddingBottom: 24 + insets.bottom }}
        keyboardShouldPersistTaps="handled"
        refreshControl={<RefreshControl refreshing={refreshing} onRefresh={pull} tintColor={colors.muted} colors={[colors.accent]} progressBackgroundColor={colors.card} />}
      >
        <Column side={20 + Math.max(insets.left, insets.right)}>
          <View style={styles.body}>
            {s && s.profiles.length > 0 ? (
              <T v="status" accessibilityRole="header" accessibilityLiveRegion="polite">{statusLine(s)}</T>
            ) : null}
            {!s ? <T v="status">Loading your tailnets.</T> : null}

            {tf.error ? <Notice title={tf.error.title} text={tf.error.text} onDismiss={tf.dismissError} /> : null}

            {s && s.profiles.length === 0 ? (
              <EmptyState busy={tf.signingIn !== null} message={tf.message} onAdd={add} onSync={() => setSheet("sync")} />
            ) : null}

            {s && s.profiles.length > 0 ? (
              <>
                <TailnetsSection
                  state={s}
                  pending={tf.pending}
                  signingIn={tf.signingIn}
                  message={tf.message}
                  onAdd={add}
                  onOpen={openTailnet}
                  onSignIn={tf.signInTo}
                  onToggle={tf.setConnected}
                />
                <View style={styles.actions}>
                  <PressableCard onPress={() => setSheet("check")} label="Check an address" hint="Opens a form" style={styles.action}>
                    <SearchIcon size={20} color={colors.ink} />
                    <T v="small" medium numberOfLines={2} style={{ flexShrink: 1 }}>Check an address</T>
                  </PressableCard>
                  <PressableCard
                    onPress={() => setSheet("ports")}
                    label={s.tunnels.length ? `Local ports, ${s.tunnels.length}` : "Local ports"}
                    hint="Opens your local ports"
                    style={styles.action}
                  >
                    <PortIcon size={20} color={colors.ink} />
                    <T v="small" medium numberOfLines={2} style={{ flexShrink: 1 }}>{s.tunnels.length ? `Local ports · ${s.tunnels.length}` : "Local ports"}</T>
                  </PressableCard>
                </View>
              </>
            ) : null}
          </View>
        </Column>
      </ScrollView>

      <TailnetSheet
        visible={open("tailnet") && !!profile}
        profile={profile}
        up={!!s?.up}
        toggling={!!profile && tf.pending.has(profile.name)}
        onClose={close}
        onToggle={(on) => profile && tf.setConnected(profile.name, on)}
        onSignInAgain={() => {
          if (!profile) return;
          close();
          tf.signInTo(profile.name);
        }}
        onRemove={async () => {
          if (!profile) return;
          await tf.remove("profile", profile.name);
          close();
        }}
      />

      <SyncSheet
        visible={open("sync")}
        link={syncLink}
        onClose={() => {
          setSyncLink(null);
          close();
        }}
        onImported={() => void tf.refresh()}
        onUnlock={(link) => {
          setSyncLink(link || null);
          openUpgrade.current("sync");
        }}
      />

      <UpgradeSheet
        visible={open("upgrade")}
        price={unlock.price}
        busy={unlock.busy}
        note={unlock.note}
        onUnlock={() => void buy()}
        onRestore={() => void restore()}
        onClose={() => {
          unlock.clearNote();
          // Backing out of a sync's unlock returns to its confirmation.
          if (resume === "sync") setSheet("sync");
          else close();
          setResume(null);
        }}
      />

      <CheckSheet visible={open("check")} onClose={close} />

      <PortsSheet
        visible={open("ports")}
        onClose={close}
        profiles={s?.profiles ?? []}
        tunnels={s?.tunnels ?? []}
        onChanged={tf.refresh}
        onError={tf.showError}
        onRemove={(name) => tf.remove("tunnel", name)}
      />

      <Sheet visible={open("settings")} onClose={close} title="Settings" tall>
        <View style={{ gap: 24 }}>
          <UnlockSection owned={!!s?.unlocked} price={unlock.price} busy={unlock.busy} note={unlock.note} onUnlock={unlock.unlock} onRestore={unlock.restore} />
          <SettingsSection
            proxy={s?.proxy ?? ""}
            allApps={s?.all_apps}
            allAppsError={s?.all_apps_error}
            onError={tf.showError}
            onSync={() => setSheet("sync")}
            onChanged={() => void tf.refresh()}
          />
          <AboutSection />
        </View>
      </Sheet>

      <Sheet visible={permissionOpen} onClose={() => answer(false)}>
        <View style={{ gap: 12 }}>
          <Mark width={40} />
          <T v="h2" accessibilityRole="header" style={{ fontSize: 19, lineHeight: 24 }}>Tailfuse needs your permission</T>
          <T v="small" color={colors.muted} style={{ lineHeight: 21 }}>
            {s?.all_apps === false
              ? `To reach your tailnets, Tailfuse adds a VPN configuration to this ${device}. Apps that follow the system proxy setting, including your browsers, then send tailnet addresses through Tailfuse and everything else directly.`
              : `To reach your tailnets, Tailfuse adds a VPN configuration to this ${device}. Every app then reaches your tailnets by name and by address, and everything else goes out directly.`}
          </T>
          <T v="label" color={colors.muted}>
            {`Your ${device} asks you to allow it next. You only do this once. This ${device} can run only one VPN at a time, so the Tailscale app is disconnected while Tailfuse is on.`}
          </T>
        </View>
        <View style={{ gap: 10 }}>
          <Button variant="primary" block title="OK" onPress={() => answer(true)} />
          <Button block title="Not now" onPress={() => answer(false)} />
        </View>
      </Sheet>
    </View>
  );
}

export default function App() {
  const [loaded, failed] = useFonts({ HostGrotesk_400Regular, HostGrotesk_500Medium, JetBrainsMono_400Regular });
  const ready = loaded || !!failed;
  useEffect(() => {
    if (ready) SplashScreen.hideAsync().catch(() => {});
  }, [ready]);
  if (!ready) return null;
  return (
    <SafeAreaProvider>
      <Screen />
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: colors.bg },
  flex: { flex: 1 },
  header: { borderBottomWidth: 1, borderBottomColor: colors.line },
  headerRow: { minHeight: 52, flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12 },
  brand: { flexDirection: "row", alignItems: "center", gap: 8 },
  power: { flexDirection: "row", alignItems: "center", gap: 10, marginRight: -10 },
  body: { paddingTop: 16, gap: 22 },
  actions: { flexDirection: "row", gap: 8 },
  action: { flex: 1, minHeight: 56, flexDirection: "row", alignItems: "center", justifyContent: "center", gap: 10, paddingHorizontal: 12, paddingVertical: 8 },
});
