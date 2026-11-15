import Constants from "expo-constants";
import * as Clipboard from "expo-clipboard";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Linking, StyleSheet, View } from "react-native";

import { Button, Card, LinkButton, Pane, Switch, T } from "./components";
import * as engine from "./engine";
import { colors, fonts, radius } from "./theme";

function Row({ title, sub, children }: { title: ReactNode; sub?: string; children?: ReactNode }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", flexWrap: "wrap", gap: 12, paddingHorizontal: 16, paddingVertical: 12, minHeight: 56 }}>
      <View style={{ flex: 1, flexBasis: 180, minWidth: 0, gap: 2 }}>
        <T v="small" medium>{title}</T>
        {sub ? <T v="label" color={colors.muted}>{sub}</T> : null}
      </View>
      {children}
    </View>
  );
}

const Divider = () => <View style={{ height: StyleSheet.hairlineWidth, backgroundColor: colors.lineStrong, marginLeft: 16 }} />;

type SettingsProps = {
  proxy: string;
  /** state.all_apps; undefined before the first state. */
  allApps?: boolean;
  allAppsError?: string;
  onError: (e: unknown) => void;
  onSync: () => void;
  onChanged: () => void;
};

export function SettingsSection({ proxy, allApps, allAppsError, onError, onSync, onChanged }: SettingsProps) {
  const [alwaysOn, setAlwaysOn] = useState(false);
  // The switch follows the tap at once, until the engine's state (allApps) catches up.
  const [pending, setPending] = useState<{ next: boolean; was: boolean | undefined } | null>(null);
  const allAppsOn = pending && pending.was === allApps ? pending.next : (allApps ?? true);
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    engine.getAlwaysOn().then(setAlwaysOn).catch(() => {});
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  const toggle = async (next: boolean) => {
    setAlwaysOn(next);
    try {
      await engine.setAlwaysOn(next);
    } catch (err) {
      setAlwaysOn(!next);
      onError(err);
    }
  };
  const toggleAllApps = async (next: boolean) => {
    setPending({ next, was: allApps });
    try {
      await engine.setAllApps(next);
      onChanged();
    } catch (err) {
      setPending(null);
      onError(err);
    }
  };
  const copy = async () => {
    try {
      await Clipboard.setStringAsync(proxy);
    } catch {
      return;
    }
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Pane title="Settings">
      <Card>
        <Row title="Sync from your computer" sub="Scan the code Tailfuse shows on your Mac or PC to bring your tailnets across.">
          <Button title="Scan" label="Scan a code from your computer" onPress={onSync} />
        </Row>
        <Divider />
        <Row title="Keep Tailfuse on" sub="Reconnects on its own after a restart or a network change.">
          <Switch value={alwaysOn} onValueChange={toggle} label="Keep Tailfuse on" />
        </Row>
        <Divider />
        <Row
          title="All apps"
          sub={allAppsOn ? "Every app reaches your tailnets, by name and by address." : "Only apps that follow the system proxy, such as browsers."}
        >
          <Switch value={allAppsOn} onValueChange={toggleAllApps} label="All apps" />
        </Row>
        {allAppsOn && allAppsError ? (
          <View style={{ paddingHorizontal: 16, paddingBottom: 12, marginTop: -4 }}>
            <T v="label" color={colors.muted}>
              Not available right now, so only browsers reach your tailnets until Tailfuse restarts ({allAppsError}).
            </T>
          </View>
        ) : null}
        <Divider />
        <Row title="Proxy address" sub="For an app that ignores the system setting, give it this address.">
          <Button title={copied ? "Copied" : "Copy address"} label="Copy the proxy address" onPress={copy} disabled={!proxy} />
        </Row>
        {proxy ? (
          <View style={{ paddingHorizontal: 16, paddingBottom: 12, marginTop: -4, flexDirection: "row" }}>
            <T
              v="mono"
              selectable
              style={{ backgroundColor: colors.deep, borderRadius: radius.sm, paddingHorizontal: 8, paddingVertical: 2, fontFamily: fonts.mono }}
            >
              {proxy}
            </T>
          </View>
        ) : null}
      </Card>
    </Pane>
  );
}

const open = (url: string) => () => void Linking.openURL(url).catch(() => {});

export function AboutSection() {
  const version = Constants.expoConfig?.version ?? "";
  return (
    <Pane title="About">
      <Card>
        <Row
          title={
            <>
              Tailfuse <T v="small" color={colors.muted} style={{ fontFamily: fonts.regular }}>{version}</T>
            </>
          }
        />
        <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", justifyContent: "space-between", gap: 8, paddingHorizontal: 16, paddingBottom: 12, marginTop: -6 }}>
          <T v="small" color={colors.muted}>© Ailio Technologies Inc.</T>
          <View style={{ flexDirection: "row", flexWrap: "wrap", gap: 14 }}>
            <LinkButton title="tailfuse.com" onPress={open("https://tailfuse.com")} />
            <LinkButton title="Privacy" onPress={open("https://tailfuse.com/privacy")} />
            <LinkButton title="Release notes" onPress={open("https://github.com/ailio-technologies-inc/tailfuse/releases")} />
          </View>
        </View>
      </Card>
    </Pane>
  );
}
