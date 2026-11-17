import { memo, useCallback, useEffect, useReducer, useRef, useState } from "react";
import { ActivityIndicator, FlatList, Linking, Platform, Pressable, Share, View } from "react-native";

import { Button, Card, ChevronIcon, CopyIcon, Field, Hint, Pane, SearchField, SuffixLine, Switch, T } from "./components";
import { choose, confirmDestructive, hasNativePrompt, promptText } from "./dialogs";
import * as engine from "./engine";
import { DEVICE_PAGE, DEVICE_ROW_HEIGHT, DEVICE_SEARCH_DEBOUNCE_MS, DOMAIN_CAP, DOMAIN_HINT, deviceAddress, deviceSummary, deviceUrl, devicesInitial, devicesReducer, domainRows, friendly, hasMoreDevices, noDevicesText, osLabel, sentence, showAllLabel, showDeviceSearch, showSignIn, switchOn, tailnetState } from "./logic";
import { deviceWord } from "./platform";
import { Sheet } from "./Sheet";
import { colors, hairline } from "./theme";
import type { Device, Prefs, Profile } from "./types";
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

const ROW_H = DEVICE_ROW_HEIGHT;
const LINE_SCALE = 1.25; // device rows keep a fixed height: their text stops growing past this

function useDebounced<V>(value: V, ms: number): V {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

// One page of devices at a time from the engine. Every request gets an id; the reducer drops replies
// that are not from the latest one, so a slow earlier search can never overwrite a newer one.
function useDeviceList(name: string | undefined, enabled: boolean, query: string, onlineOnly: boolean) {
  const [state, dispatch] = useReducer(devicesReducer, devicesInitial);
  const [nonce, setNonce] = useState(0);
  const req = useRef(0);
  const asked = useRef(-1);
  const latest = useRef(state);
  useEffect(() => {
    latest.current = state;
  });

  const fetchPage = useCallback((offset: number, append: boolean) => {
    if (!name) return;
    const id = ++req.current;
    asked.current = append ? offset : -1;
    dispatch({ type: "fetch", key: name, req: id, append });
    engine
      .devices({ name, query: query.trim() || undefined, online: onlineOnly || undefined, limit: DEVICE_PAGE, offset })
      .then((page) => dispatch({ type: "loaded", req: id, offset, page }))
      .catch((e) => dispatch({ type: "failed", req: id, text: sentence(e) }));
  }, [name, query, onlineOnly]);

  useEffect(() => {
    if (enabled && name) fetchPage(0, false);
    else dispatch({ type: "reset" });
  }, [enabled, name, fetchPage, nonce]);

  const loadMore = useCallback(() => {
    const s = latest.current;
    if (hasMoreDevices(s) && asked.current !== s.devices.length) fetchPage(s.devices.length, true);
  }, [fetchPage]);
  const retry = useCallback(() => {
    const s = latest.current;
    if (s.devices.length) fetchPage(s.devices.length, true);
    else setNonce((n) => n + 1);
  }, [fetchPage]);
  // Anything not fetched yet for this tailnet shows as loading.
  const shown = state.key === name ? state : { ...devicesInitial, status: "loading" as const };
  return { state: shown, loadMore, retry };
}

function Skeleton() {
  return (
    <View accessibilityLabel="Loading devices" accessibilityLiveRegion="polite">
      {[0, 1, 2, 3, 4].map((i) => (
        <View key={i} style={{ height: ROW_H, flexDirection: "row", alignItems: "center", gap: 12, opacity: 1 - i * 0.17 }}>
          <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: colors.secondaryFill }} />
          <View style={{ flex: 1, gap: 6 }}>
            <View style={{ height: 12, width: `${48 + ((i * 13) % 30)}%`, borderRadius: 6, backgroundColor: colors.secondaryFill }} />
            <View style={{ height: 10, width: `${62 - ((i * 9) % 20)}%`, borderRadius: 5, backgroundColor: colors.secondaryFill }} />
          </View>
        </View>
      ))}
    </View>
  );
}

// A device: its name and address on one line each (the name ends in an ellipsis, the address in the
// middle so its tailnet suffix survives); tapping opens the platform's action sheet (open, copy, share).
const DeviceRow = memo(function DeviceRow({ d, copied, thisLabel, onPress }: { d: Device; copied: boolean; thisLabel: string; onPress: (d: Device) => void }) {
  return (
    <Pressable
      testID="device-row"
      accessibilityRole="button"
      accessibilityLabel={`${d.name}${d.self ? `, ${thisLabel}` : ""}, ${d.online ? "online" : "offline"}`}
      accessibilityHint="Opens actions for this device"
      onPress={() => onPress(d)}
      android_ripple={{ color: "rgba(239,240,241,0.10)" }}
      style={({ pressed }) => ({ height: ROW_H, flexDirection: "row", alignItems: "center", gap: 12, borderTopWidth: hairline, borderTopColor: colors.lineStrong, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
    >
      <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: d.online ? colors.ok : colors.dot }} />
      <View style={{ flex: 1, minWidth: 0, gap: 1 }}>
        <T v="small" medium numberOfLines={1} ellipsizeMode="tail" maxFontSizeMultiplier={LINE_SCALE}>{d.name}</T>
        <T v="mono" color={colors.muted} numberOfLines={1} ellipsizeMode="middle" maxFontSizeMultiplier={LINE_SCALE}>{copied ? "Copied" : deviceAddress(d.dns_name)}</T>
      </View>
      {d.self || d.os ? (
        <T v="label" color={colors.muted} numberOfLines={1} maxFontSizeMultiplier={LINE_SCALE} style={{ flexShrink: 0, maxWidth: 96 }}>{d.self ? thisLabel : osLabel(d.os)}</T>
      ) : null}
      <ChevronIcon />
    </Pressable>
  );
});

const getItemLayout = (_: ArrayLike<Device> | null | undefined, index: number) => ({ length: ROW_H, offset: ROW_H * index, index });
const keyOf = (d: Device) => d.dns_name || d.name;

// A tailnet's Tailscale settings. Keyed by tailnet, so each one starts from its own saved values.
function OptionsPane({ name, saved }: { name: string; saved: Prefs | undefined }) {
  const [opts, setOpts] = useState<Prefs>(saved ?? {});
  const [exitNode, setExitNode] = useState(saved?.exit_node ?? "");
  const [routes, setRoutes] = useState((saved?.advertise_routes ?? []).join(", "));
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await engine.setOptions(name, { ...opts, exit_node: exitNode.trim(), advertise_routes: routes.split(",").map((r) => r.trim()).filter(Boolean) });
      setNote("Saved");
    } catch (err) {
      const [title, text] = friendly(err);
      setNote(`${title}. ${text}`);
    } finally {
      setBusy(false);
    }
  }, [name, opts, exitNode, routes]);

  const toggles: [string, "accept_routes" | "advertise_exit_node" | "shields_up"][] = [
    ["Use subnet routes from other devices", "accept_routes"],
    ["Offer this device as an exit node", "advertise_exit_node"],
    ["Block incoming connections", "shields_up"],
  ];
  return (
    <Pane title="Tailscale options">
      <Field label="Exit node" value={exitNode} onChangeText={setExitNode} placeholder="A device's name, or empty for none" autoCapitalize="none" autoCorrect={false} returnKeyType="done" />
      <Field label="Subnets to share" value={routes} onChangeText={setRoutes} placeholder="10.0.0.0/24, 192.168.1.0/24" autoCapitalize="none" autoCorrect={false} returnKeyType="done" />
      {toggles.map(([label, key]) => (
        <View key={key} style={{ minHeight: 44, flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12 }}>
          <T v="small" style={{ flex: 1 }}>{label}</T>
          <Switch value={!!opts[key]} label={label} onValueChange={(v) => setOpts((o) => ({ ...o, [key]: v }))} />
        </View>
      ))}
      <Button variant="primary" title="Save options" busy={busy} onPress={() => void save()} />
      {note ? <T v="label" color={note === "Saved" ? colors.ok : colors.bad} accessibilityLiveRegion="polite">{note}</T> : null}
      <Hint>Shared subnets and exit nodes also need approval in the Tailscale admin console.</Hint>
    </Pane>
  );
}

export function TailnetSheet({ visible, profile: p, up, toggling, onClose, onToggle, onSignInAgain, onRemove }: Props) {
  const device = deviceWord();
  const { copied, copy } = useCopied();
  const [query, setQuery] = useState("");
  const [onlineOnly, setOnlineOnly] = useState(false);
  const [allDomains, setAllDomains] = useState(false);
  const { state: dev, loadMore, retry } = useDeviceList(p?.name, !!p?.online && visible, useDebounced(query, DEVICE_SEARCH_DEBOUNCE_MS), onlineOnly);
  const [removing, setRemoving] = useState(false);
  const [adding, setAdding] = useState(false);
  const [domain, setDomain] = useState("");
  const [domainNote, setDomainNote] = useState("");

  const [wasVisible, setWasVisible] = useState(visible);
  if (wasVisible !== visible) {
    setWasVisible(visible);
    if (!visible) {
      setQuery("");
      setOnlineOnly(false);
      setAllDomains(false);
      setAdding(false);
      setDomain("");
      setDomainNote("");
    }
  }

  const remove = useCallback(async () => {
    if (!p) return;
    if (!(await confirmDestructive(`Remove ${p.name}?`, "Its sign-in and its local ports are deleted from this device. You can add it again later.", "Remove tailnet"))) return;
    setRemoving(true);
    try {
      await onRemove();
    } finally {
      setRemoving(false);
    }
  }, [p, onRemove]);

  const deviceActions = useCallback(async (d: Device) => {
    const address = deviceAddress(d.dns_name);
    const names = ["Open in browser", "Copy address", ...(Platform.OS === "ios" ? ["Share address"] : [])];
    const i = await choose(d.name, names, address);
    if (i === 0) void Linking.openURL(deviceUrl(d.dns_name)).catch(() => {});
    else if (i === 1) copy(`dev:${d.dns_name}`, address);
    else if (i === 2) void Share.share({ message: address }).catch(() => {});
  }, [copy]);

  const changeDomain = useCallback(async (kind: "add" | "remove", value: string) => {
    if (!p) return;
    try {
      if (kind === "add") await engine.addDomain(p.name, value);
      else await engine.removeDomain(p.name, value);
      setDomainNote("");
      setDomain("");
      setAdding(false);
    } catch (err) {
      const [title, text] = friendly(err);
      setDomainNote(`${title}. ${text}`);
    }
  }, [p]);

  const removeDomain = useCallback(async (d: string) => {
    if (!p) return;
    if (await confirmDestructive(`Stop sending ${d} to ${p.name}?`, "Tailfuse will leave that domain alone, and your apps reach it directly.", "Remove domain")) await changeDomain("remove", d);
  }, [p, changeDomain]);

  const addDomain = useCallback(async () => {
    if (hasNativePrompt) {
      const v = await promptText("Add a domain", DOMAIN_HINT, "Add");
      if (v?.trim()) await changeDomain("add", v.trim());
    } else setAdding((a) => !a);
  }, [changeDomain]);

  if (!p) return <Sheet visible={false} onClose={onClose}>{null}</Sheet>;
  const [label, tone] = tailnetState(p, up);
  const stateColor = { on: colors.ok, bad: colors.bad, wait: colors.accent, "": colors.muted }[tone];
  const rows: [string, string][] = [];
  if (p.hostname) rows.push(["Name", p.hostname]);
  for (const ip of p.ips.filter(Boolean)) rows.push([ip.includes(":") ? "IPv6" : "IPv4", ip]);
  const domains = domainRows(p);
  const shownDomains = allDomains ? domains : domains.slice(0, DOMAIN_CAP);
  const thisLabel = `This ${device}`;
  const searching = showDeviceSearch(dev.total) || query !== "" || onlineOnly;
  const copiedDev = typeof copied === "string" && copied.startsWith("dev:") ? copied : "";

  const header = (
    <View style={{ gap: 16, paddingBottom: 12 }}>
      <Card style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12, padding: 14, minHeight: 64 }}>
        <View style={{ flex: 1, gap: 1 }}>
          <T v="small" medium>{showSignIn(p) ? "Sign-in needed" : "Connect"}</T>
          <T v="label" medium color={stateColor}>{label}</T>
        </View>
        {showSignIn(p) ? (
          <Button variant="primary" title="Sign in" label={`Sign in to ${p.name}`} onPress={onSignInAgain} />
        ) : (
          <Switch value={switchOn(p, up)} label={`${p.name} connected`} onValueChange={onToggle} disabled={toggling} />
        )}
      </Card>
      {p.error ? <T v="small" color={colors.bad}>{p.error}</T> : null}

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
                style={({ pressed }) => ({ minHeight: 48, flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12, paddingHorizontal: 14, borderTopWidth: i ? hairline : 0, borderTopColor: colors.lineStrong, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
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

      <Pane title="Domains">
        <Card>
          {shownDomains.map((r, i) => (
            <View key={r.suffix} style={{ minHeight: 48, flexDirection: "row", alignItems: "center", gap: 12, paddingLeft: 14, paddingRight: 8, borderTopWidth: i ? hairline : 0, borderTopColor: colors.lineStrong }}>
              <View style={{ flex: 1, minWidth: 0 }}>
                <T v="mono" numberOfLines={1}>{`*${r.suffix}`}</T>
              </View>
              {r.builtIn ? (
                <T v="label" color={colors.muted}>Built in</T>
              ) : (
                <Button destructive title="Remove" label={`Stop sending ${r.domain} to ${p.name}`} onPress={() => void removeDomain(r.domain)} />
              )}
            </View>
          ))}
          {domains.length > DOMAIN_CAP && !allDomains ? (
            <Pressable
              accessibilityRole="button"
              accessibilityLabel={showAllLabel(domains.length)}
              onPress={() => setAllDomains(true)}
              android_ripple={{ color: "rgba(239,240,241,0.10)" }}
              style={({ pressed }) => ({ minHeight: 48, justifyContent: "center", paddingHorizontal: 14, borderTopWidth: hairline, borderTopColor: colors.lineStrong, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
            >
              <T v="small" medium color={colors.muted}>{showAllLabel(domains.length)}</T>
            </Pressable>
          ) : null}
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Add a domain"
            onPress={() => void addDomain()}
            android_ripple={{ color: "rgba(239,240,241,0.10)" }}
            style={({ pressed }) => ({ minHeight: 48, justifyContent: "center", paddingHorizontal: 14, borderTopWidth: hairline, borderTopColor: colors.lineStrong, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
          >
            <T v="small" medium color={colors.accent}>Add a domain</T>
          </Pressable>
        </Card>
        {adding ? (
          <View style={{ gap: 8 }}>
            <Field label="Domain" hideLabel value={domain} onChangeText={setDomain} placeholder="example.com" keyboardType="url" returnKeyType="done" onSubmitEditing={() => domain.trim() && void changeDomain("add", domain.trim())} />
            <Button variant="primary" title="Add domain" onPress={() => void changeDomain("add", domain.trim())} disabled={!domain.trim()} />
          </View>
        ) : null}
        {domainNote ? <T v="label" color={colors.bad} accessibilityLiveRegion="polite">{domainNote}</T> : null}
        <Hint>{DOMAIN_HINT}</Hint>
      </Pane>

      <OptionsPane key={p.name} name={p.name} saved={p.prefs} />

      <View style={{ flexDirection: "row", gap: 10 }}>
        <View style={{ flex: 1 }}>
          <Button title="Sign in again" onPress={onSignInAgain} block />
        </View>
        <View style={{ flex: 1 }}>
          <Button destructive title="Remove" label={`Remove ${p.name}`} busy={removing} onPress={() => void remove()} block />
        </View>
      </View>

      <Pane title="Devices" hint={p.online && dev.status === "ready" ? deviceSummary(dev.total, dev.online) : undefined}>
        {searching && p.online ? (
          <View style={{ gap: 6 }}>
            <SearchField label="Search devices" value={query} onChangeText={setQuery} placeholder="Search devices" />
            <View style={{ minHeight: 44, flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 12 }}>
              <T v="small">Online only</T>
              <Switch value={onlineOnly} label="Show only online devices" onValueChange={setOnlineOnly} />
            </View>
          </View>
        ) : null}
      </Pane>
    </View>
  );

  const empty = !p.online ? (
    <Hint>Turn this tailnet on to see its devices.</Hint>
  ) : dev.status === "loading" || dev.status === "idle" ? (
    <Skeleton />
  ) : dev.status === "error" ? (
    <View style={{ gap: 8 }}>
      <Hint live>{dev.error}</Hint>
      <View style={{ flexDirection: "row" }}>
        <Button title="Try again" onPress={retry} />
      </View>
    </View>
  ) : (
    <Hint live>{noDevicesText(query, onlineOnly)}</Hint>
  );

  const footer = dev.loadingMore ? (
    <View style={{ height: 48, alignItems: "center", justifyContent: "center" }}>
      <ActivityIndicator color={colors.muted} accessibilityLabel="Loading more devices" />
    </View>
  ) : dev.status === "ready" && dev.error ? (
    <View style={{ gap: 8, paddingTop: 8, flexDirection: "row", alignItems: "center", justifyContent: "space-between" }}>
      <Hint live>{dev.error}</Hint>
      <Button title="Try again" onPress={retry} />
    </View>
  ) : undefined;

  return (
    <Sheet
      visible={visible}
      onClose={onClose}
      title={p.account || p.name}
      subtitle={
        <>
          {p.account ? <T v="label" color={colors.muted}>{p.name}</T> : null}
          <SuffixLine suffixes={p.suffixes} />
        </>
      }
      tall
      list={(ref) => (
        <FlatList
          ref={ref}
          data={dev.devices}
          extraData={copiedDev}
          keyExtractor={keyOf}
          getItemLayout={getItemLayout}
          renderItem={({ item }) => <DeviceRow d={item} copied={copiedDev === `dev:${item.dns_name}`} thisLabel={thisLabel} onPress={deviceActions} />}
          ListHeaderComponent={header}
          ListEmptyComponent={empty}
          ListFooterComponent={footer}
          onEndReached={loadMore}
          onEndReachedThreshold={0.6}
          initialNumToRender={10}
          maxToRenderPerBatch={10}
          updateCellsBatchingPeriod={50}
          windowSize={5}
          removeClippedSubviews={Platform.OS === "android"}
          nestedScrollEnabled
          keyboardShouldPersistTaps="handled"
          keyboardDismissMode="interactive"
          showsVerticalScrollIndicator={false}
          contentContainerStyle={{ paddingHorizontal: 20, paddingTop: 8, paddingBottom: 28 }}
        />
      )}
    />
  );
}
