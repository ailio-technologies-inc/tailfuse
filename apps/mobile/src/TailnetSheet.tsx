import { useCallback, useEffect, useState } from "react";
import { Linking, Pressable, View } from "react-native";

import { Button, Card, CopyIcon, Field, Hint, Pane, SearchIcon, Switch, T } from "./components";
import * as engine from "./engine";
import { deviceAddress, deviceUrl, filterDevices, needsSignIn, osLabel, showDeviceSearch, sentence, sortDevices, tailnetState } from "./logic";
import { deviceWord } from "./platform";
import { Sheet } from "./Sheet";
import { colors, radius } from "./theme";
import type { Device, Profile } from "./types";
import { useArmed } from "./useArmed";
import { useCopied } from "./useCopied";

type Props = {
  visible: boolean;
  profile: Profile | undefined;
  up: boolean;
  toggling: boolean;
  onClose: () => void;
  onToggle: (on: boolean) => void;
  onSignInAgain: () => void;
  onRemove: () => Promise<void>;
};

type Devices = { status: "idle" } | { status: "loading" } | { status: "error"; text: string } | { status: "ready"; list: Device[] };

function useDevices(name: string | undefined, online: boolean, active: boolean) {
  const [res, setRes] = useState<{ name: string; nonce: number; value: Devices } | null>(null);
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    if (!active || !name || !online) return;
    let live = true;
    engine
      .devices(name)
      .then((list) => live && setRes({ name, nonce, value: { status: "ready", list: sortDevices(list ?? []) } }))
      .catch((e) => live && setRes({ name, nonce, value: { status: "error", text: sentence(e) } }));
    return () => {
      live = false;
    };
  }, [name, online, active, nonce]);
  // Anything not fetched yet for this tailnet shows as loading; a refetch keeps the old list on screen.
  const devices: Devices = res && res.name === name && (res.value.status === "ready" || res.nonce === nonce) ? res.value : { status: "loading" };
  return { devices, retry: () => setNonce((n) => n + 1) };
}

function Skeleton() {
  return (
    <View accessibilityLabel="Loading devices" accessibilityLiveRegion="polite" style={{ gap: 8 }}>
      {[0, 1, 2].map((i) => (
        <View key={i} style={{ height: 56, backgroundColor: colors.secondaryFill, borderRadius: radius.md, opacity: 1 - i * 0.25 }} />
      ))}
      <Hint>Loading devices</Hint>
    </View>
  );
}

function DeviceRow({ d, open, copied, onPress, onCopy }: { d: Device; open: boolean; copied: boolean; onPress: () => void; onCopy: () => void }) {
  return (
    <View style={{ borderTopWidth: 1, borderTopColor: colors.line }}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`${d.name}, ${d.online ? "online" : "offline"}`}
        accessibilityState={{ expanded: open }}
        onPress={onPress}
        style={({ pressed }) => ({ minHeight: 56, flexDirection: "row", alignItems: "center", gap: 12, paddingVertical: 8, paddingHorizontal: 14, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
      >
        <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: d.online ? colors.ok : colors.dot }} />
        <View style={{ flex: 1, minWidth: 0, gap: 1 }}>
          <T v="small" medium numberOfLines={1}>{d.name}</T>
          <T v="mono" color={colors.muted} numberOfLines={1}>{deviceAddress(d.dns_name)}</T>
        </View>
        {d.os ? <T v="label" color={colors.muted}>{osLabel(d.os)}</T> : null}
      </Pressable>
      {open ? (
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: 8, paddingHorizontal: 14, paddingBottom: 12, paddingLeft: 35 }}>
          <Button title="Open in browser" label={`Open ${deviceAddress(d.dns_name)} in the browser`} onPress={() => void Linking.openURL(deviceUrl(d.dns_name)).catch(() => {})} />
          <Button title={copied ? "Copied" : "Copy address"} label={`Copy ${deviceAddress(d.dns_name)}`} onPress={onCopy} />
        </View>
      ) : null}
    </View>
  );
}

export function TailnetSheet({ visible, profile: p, up, toggling, onClose, onToggle, onSignInAgain, onRemove }: Props) {
  const device = deviceWord();
  const armed = useArmed();
  const { copied, copy } = useCopied();
  const { devices, retry } = useDevices(p?.name, !!p?.online, visible);
  const [query, setQuery] = useState("");
  const [openDevice, setOpenDevice] = useState<string | null>(null);
  const [removing, setRemoving] = useState(false);

  const [wasVisible, setWasVisible] = useState(visible);
  if (wasVisible !== visible) {
    setWasVisible(visible);
    if (!visible) {
      setQuery("");
      setOpenDevice(null);
    }
  }

  const remove = useCallback(async () => {
    if (!p || !armed.tap(`profile:${p.name}`)) return;
    setRemoving(true);
    try {
      await onRemove();
    } finally {
      setRemoving(false);
    }
  }, [p, armed, onRemove]);

  if (!p) return <Sheet visible={false} onClose={onClose}>{null}</Sheet>;
  const [label, tone] = tailnetState(p, up);
  const stateColor = { on: colors.ok, bad: colors.bad, wait: colors.accent, "": colors.muted }[tone];
  const rows: [string, string][] = [];
  if (p.hostname) rows.push(["Name", p.hostname]);
  for (const ip of p.ips.filter(Boolean)) rows.push([ip.includes(":") ? "IPv6" : "IPv4", ip]);
  const list = devices.status === "ready" ? devices.list : [];
  const shown = filterDevices(list, query);

  return (
    <Sheet
      visible={visible}
      onClose={onClose}
      title={p.account || p.name}
      subtitle={
        <>
          {p.account ? <T v="label" color={colors.muted}>{p.name}</T> : null}
          {p.suffixes.length > 0 ? <T v="mono" color={colors.muted}>{p.suffixes.map((x) => `*${x}`).join("  ")}</T> : null}
        </>
      }
      tall
    >
      <Card style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12, padding: 14, minHeight: 64 }}>
        <View style={{ flex: 1, gap: 1 }}>
          <T v="small" medium>{needsSignIn(p, up) ? "Sign-in needed" : "Connect"}</T>
          <T v="label" medium color={stateColor}>{label}</T>
        </View>
        {needsSignIn(p, up) ? (
          <Button variant="primary" title="Sign in" label={`Sign in to ${p.name}`} onPress={onSignInAgain} />
        ) : (
          <Switch value={p.online} label={`${p.name} connected`} onValueChange={onToggle} disabled={toggling} />
        )}
      </Card>
      {p.error ? <T v="small" color={colors.bad}>{p.error}</T> : null}

      {rows.length > 0 ? (
        <Pane title={`This ${device}`}>
          <Card>
            {rows.map(([k, v], i) => (
              <Pressable
                key={k}
                accessibilityRole="button"
                accessibilityLabel={`${k} ${v}`}
                accessibilityHint="Copies it"
                onPress={() => copy(k, v)}
                onLongPress={() => copy(k, v)}
                style={({ pressed }) => ({ minHeight: 48, flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12, paddingHorizontal: 14, borderTopWidth: i ? 1 : 0, borderTopColor: colors.line, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
              >
                <T v="small" color={colors.muted}>{k}</T>
                <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexShrink: 1 }}>
                  <T v={k === "Name" ? "small" : "mono"} style={{ flexShrink: 1, fontSize: k === "Name" ? 14 : 13 }}>{copied === k ? "Copied" : v}</T>
                  {copied === k ? null : <CopyIcon size={14} />}
                </View>
              </Pressable>
            ))}
          </Card>
        </Pane>
      ) : null}

      <Pane title="Devices">
        {!p.online ? (
          <Hint>Turn this tailnet on to see its devices.</Hint>
        ) : devices.status === "loading" || devices.status === "idle" ? (
          <Skeleton />
        ) : devices.status === "error" ? (
          <View style={{ gap: 8 }}>
            <Hint live>{devices.text}</Hint>
            <View style={{ flexDirection: "row" }}>
              <Button title="Try again" onPress={retry} />
            </View>
          </View>
        ) : list.length === 0 ? (
          <Hint>No other devices are on this tailnet yet.</Hint>
        ) : (
          <View style={{ gap: 10 }}>
            {showDeviceSearch(list.length) ? (
              <View>
                <Field label="Search devices" hideLabel value={query} onChangeText={setQuery} placeholder="Search devices" returnKeyType="search" style={{ paddingLeft: 40 }} />
                <View pointerEvents="none" style={{ position: "absolute", left: 14, top: 15 }}>
                  <SearchIcon size={18} />
                </View>
              </View>
            ) : null}
            {shown.length === 0 ? (
              <Hint live>{`No device matches "${query.trim()}".`}</Hint>
            ) : (
              <Card style={{ overflow: "hidden" }}>
                {shown.map((d, i) => (
                  <View key={d.dns_name || d.name} style={i === 0 ? { marginTop: -1 } : undefined}>
                    <DeviceRow
                      d={d}
                      open={openDevice === d.dns_name}
                      copied={copied === `dev:${d.dns_name}`}
                      onPress={() => setOpenDevice((cur) => (cur === d.dns_name ? null : d.dns_name))}
                      onCopy={() => copy(`dev:${d.dns_name}`, deviceAddress(d.dns_name))}
                    />
                  </View>
                ))}
              </Card>
            )}
          </View>
        )}
      </Pane>

      <View style={{ gap: 10, marginTop: 4 }}>
        <Button title="Sign in again" onPress={onSignInAgain} block />
        <Button
          title={armed.isArmed(`profile:${p.name}`) ? "Remove for good" : "Remove tailnet"}
          label={armed.isArmed(`profile:${p.name}`) ? `Confirm removing ${p.name}` : `Remove ${p.name}`}
          armed={armed.isArmed(`profile:${p.name}`)}
          busy={removing}
          onPress={remove}
          block
        />
      </View>
    </Sheet>
  );
}
