import { memo, useCallback, useEffect, useRef, useState } from "react";
import { Platform, Pressable, View } from "react-native";

import { Button, Card, ChevronIcon, Field, Hint, T, type InputRef } from "./components";
import { choose, confirmDestructive } from "./dialogs";
import * as engine from "./engine";
import * as haptics from "./haptics";
import { checkResult, friendly, sentence, suggestButton, suggestLine } from "./logic";
import { deviceWord } from "./platform";
import { Sheet } from "./Sheet";
import { colors } from "./theme";
import { preview } from "./preview";
import type { Profile, Tunnel } from "./types";

// "Check where an address goes", as a sheet: the field is focused when it opens.
export function CheckSheet({ visible, onClose }: { visible: boolean; onClose: () => void }) {
  const [host, setHost] = useState(preview.result === "suggest" ? "ailio.dev" : preview.result ? "nas.home-lab.ts.net" : "");
  const [result, setResult] = useState<{ text: string; owned: boolean; owner?: string; suggest?: string } | null>(
    preview.result === "owned"
      ? { text: checkResult("nas.home-lab.ts.net", "home-lab"), owned: true }
      : preview.result === "suggest"
        ? { text: checkResult("ailio.dev", "work"), owned: true, owner: "work", suggest: "ailio.dev" }
        : null,
  );
  const [suggestNote, setSuggestNote] = useState("");
  const [busy, setBusy] = useState(false);
  const input = useRef<InputRef>(null);

  useEffect(() => {
    if (!visible) return;
    const t = setTimeout(() => input.current?.focus(), 350);
    return () => clearTimeout(t);
  }, [visible]);

  const submit = async () => {
    const h = host.trim();
    if (!h || busy) return;
    setBusy(true);
    try {
      const a = await engine.testHost(h);
      setSuggestNote("");
      setResult({ text: checkResult(h, a.owner), owned: a.owner !== null, owner: a.owner ?? undefined, suggest: a.owner && a.suggest_domain ? a.suggest_domain : undefined });
    } catch (err) {
      haptics.warning();
      setResult({ text: sentence(err), owned: false });
    }
    setBusy(false);
  };

  return (
    <Sheet visible={visible} onClose={onClose} title="Check where an address goes">
      <Field
        ref={input}
        label="Address"
        hideLabel
        value={host}
        onChangeText={setHost}
        placeholder="nas.home-lab.ts.net"
        keyboardType="url"
        returnKeyType="go"
        onSubmitEditing={submit}
      />
      <Button variant="primary" block title="Check address" onPress={submit} disabled={!host.trim()} busy={busy} />
      {result ? (
        <Card style={{ flexDirection: "row", alignItems: "center", gap: 12, padding: 14 }} >
          {result.owned ? <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: colors.ok }} /> : null}
          <T v="small" style={{ flex: 1 }} accessibilityLiveRegion="polite">{result.text}</T>
        </Card>
      ) : null}
      {result?.suggest && result.owner ? (
        <View style={{ gap: 10 }}>
          <T v="small" accessibilityLiveRegion="polite">{suggestLine(result.suggest, result.owner)}</T>
          <Button
            title={suggestButton(result.suggest, result.owner)}
            onPress={async () => {
              try {
                await engine.addDomain(result.owner!, result.suggest!);
                haptics.success();
                setSuggestNote(`${result.suggest} now always goes to ${result.owner}.`);
                setResult({ ...result, suggest: undefined, text: checkResult(result.suggest!, result.owner!) });
              } catch (err) {
                const [title, text] = friendly(err);
                setSuggestNote(`${title}. ${text}`);
              }
            }}
          />
        </View>
      ) : null}
      {suggestNote ? <Hint live>{suggestNote}</Hint> : null}
    </Sheet>
  );
}

const TunnelRow = memo(function TunnelRow({ t, onRemove }: { t: Tunnel; onRemove: (t: Tunnel) => void }) {
  const device = deviceWord();
  return (
    <Card style={{ flexDirection: "row", alignItems: "center", gap: 12, padding: 12, paddingHorizontal: 14, flexWrap: "wrap" }}>
      <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: colors.accent }} />
      <View style={{ flex: 1, flexBasis: 160, minWidth: 0, gap: 1 }}>
        <T v="small" medium>{t.name}</T>
        <T v="label" color={colors.muted}>
          <T v="mono" color={colors.muted}>{t.listen}</T>
          {` on this ${device} leads to `}
          <T v="mono" color={colors.muted}>{t.target}</T>
        </T>
        <T v="label" color={colors.muted}>{`through ${t.profile}`}</T>
      </View>
      <Button destructive title="Remove" label={`Remove ${t.name}`} onPress={() => onRemove(t)} />
    </Card>
  );
});

// "Local ports", as a sheet. "Add a local port" swaps the sheet's content to the form.
export function PortsSheet({ visible, onClose, profiles, tunnels, onChanged, onError, onRemove }: {
  visible: boolean;
  onClose: () => void;
  profiles: Profile[];
  tunnels: Tunnel[];
  onChanged: () => void;
  onError: (e: unknown) => void;
  onRemove: (name: string) => void;
}) {
  const device = deviceWord();
  const removeTunnel = useCallback(async (t: Tunnel) => {
    if (await confirmDestructive(`Remove ${t.name}?`, `${t.listen} will stop leading to ${t.target}.`, "Remove local port")) onRemove(t.name);
  }, [onRemove]);
  const [form, setForm] = useState(preview.sheet === "ports-form");
  const [name, setName] = useState("");
  const [listen, setListen] = useState("");
  const [target, setTarget] = useState("");
  const [profile, setProfile] = useState("");
  const [busy, setBusy] = useState(false);
  const listenRef = useRef<InputRef>(null);
  const targetRef = useRef<InputRef>(null);
  const chosen = profiles.some((p) => p.name === profile) ? profile : profiles[0]?.name ?? "";

  const [wasVisible, setWasVisible] = useState(visible);
  if (wasVisible !== visible) {
    setWasVisible(visible);
    if (!visible) setForm(false);
  }

  const ready = !!name.trim() && !!listen.trim() && !!target.trim() && !!chosen;
  const submit = async () => {
    if (busy || !ready) return;
    setBusy(true);
    try {
      await engine.addTunnel({ name: name.trim(), listen: listen.trim(), target: target.trim(), profile: chosen });
      haptics.success();
      setName("");
      setListen("");
      setTarget("");
      setForm(false);
    } catch (err) {
      onError(err);
    }
    setBusy(false);
    onChanged();
  };

  if (form) {
    return (
      <Sheet visible={visible} onClose={onClose} title="Add a local port" tall>
        <Field label="Name" value={name} onChangeText={setName} placeholder="prod-db" returnKeyType="next" onSubmitEditing={() => listenRef.current?.focus()} />
        <Field
          ref={listenRef}
          label={`Port on this ${device}`}
          value={listen}
          onChangeText={setListen}
          placeholder="127.0.0.1:5432"
          keyboardType="numbers-and-punctuation"
          returnKeyType="next"
          onSubmitEditing={() => targetRef.current?.focus()}
        />
        <Field
          ref={targetRef}
          label="Service on the tailnet"
          value={target}
          onChangeText={setTarget}
          placeholder="db.work.ts.net:5432"
          keyboardType="url"
          returnKeyType="done"
          onSubmitEditing={submit}
        />
        <View style={{ gap: 4 }}>
          <T v="label" medium accessibilityRole="header">Tailnet</T>
          <Card>
            <Pressable
              accessibilityRole="button"
              accessibilityLabel={`Tailnet ${chosen}`}
              accessibilityHint={profiles.length > 1 ? "Choose another tailnet" : undefined}
              disabled={profiles.length < 2}
              onPress={async () => {
                if (Platform.OS === "android") {
                  const i = profiles.findIndex((p) => p.name === chosen);
                  setProfile(profiles[(i + 1) % profiles.length].name);
                  return;
                }
                const i = await choose("Tailnet", profiles.map((p) => p.name), "Which tailnet does this port lead into?");
                if (i !== null) setProfile(profiles[i].name);
              }}
              android_ripple={{ color: "rgba(239,240,241,0.10)" }}
              style={({ pressed }) => ({ minHeight: 48, flexDirection: "row", alignItems: "center", justifyContent: "space-between", paddingHorizontal: 14, borderTopWidth: 0, borderTopColor: colors.lineStrong, backgroundColor: pressed ? "#2a2c30" : "transparent" })}
            >
              <T v="small" medium>{chosen}</T>
              {profiles.length > 1 ? <ChevronIcon /> : null}
            </Pressable>
          </Card>
        </View>
        <View style={{ gap: 10, marginTop: 4 }}>
          <Button variant="primary" block title="Add local port" onPress={submit} disabled={!ready} busy={busy} />
          <Button block title="Cancel" onPress={() => setForm(false)} />
        </View>
      </Sheet>
    );
  }

  return (
    <Sheet visible={visible} onClose={onClose} title="Local ports">
      <Hint>{`A port on this ${device} that leads to one service on a tailnet, for apps that can't use a proxy, such as a database or SSH client.`}</Hint>
      {tunnels.length > 0 ? (
        <View style={{ gap: 8 }}>
          {tunnels.map((t) => (
            <TunnelRow
              key={t.name}
              t={t}
              onRemove={removeTunnel}
            />
          ))}
        </View>
      ) : null}
      <Button block title="Add a local port" disabled={profiles.length === 0} onPress={() => setForm(true)} />
    </Sheet>
  );
}
