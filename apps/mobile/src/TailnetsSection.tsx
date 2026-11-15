import { memo, useEffect, useState } from "react";
import { ActivityIndicator, Animated, Pressable, View } from "react-native";

import { Button, ChevronIcon, Hint, Mark, Pane, PlusIcon, PressableCard, SuffixLine, Switch, T, useReduceMotion } from "./components";
import { addSubtext, showSignIn, showStateLabel, switchOn, tailnetState, tailnetsHint } from "./logic";
import { colors, radius } from "./theme";
import type { Profile, State } from "./types";

const toneColor = { on: colors.ok, bad: colors.bad, wait: colors.accent, "": colors.muted } as const;

// One compact row: dot, who and what it owns, then the switch (or Sign in). The whole row opens
// the tailnet's sheet.
// Memoised: the 3-second poll hands back the same profile object when nothing changed, so a row only
// renders when its own tailnet does.
const TailnetRow = memo(function TailnetRow({ p, up, busy, signInBusy, onOpen, onSignIn, onToggle }: {
  p: Profile;
  up: boolean;
  busy: boolean;
  signInBusy: boolean;
  onOpen: (name: string) => void;
  onSignIn: (name: string) => void;
  onToggle: (name: string, on: boolean) => void;
}) {
  const [label, tone] = tailnetState(p, up);
  const title = p.account || p.name;
  return (
    <PressableCard onPress={() => onOpen(p.name)} label={`${title}, ${label}`} hint="Opens details" style={{ minHeight: 56, flexDirection: "row", alignItems: "center", gap: 12, paddingVertical: 10, paddingLeft: 14, paddingRight: 8 }}>
      <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: p.error ? colors.bad : p.online ? colors.ok : colors.dot }} />
      <View style={{ flex: 1, minWidth: 0, gap: 1 }}>
        <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "baseline", columnGap: 10 }}>
          <T v="small" medium numberOfLines={1} style={{ flexShrink: 1 }}>{title}</T>
          {p.account ? <T v="label" color={colors.muted} numberOfLines={1} style={{ flexShrink: 1 }}>{p.name}</T> : null}
        </View>
        <SuffixLine suffixes={p.suffixes} />
        {showStateLabel(label) ? (
          <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
            <T v="label" medium color={toneColor[tone]}>{label}</T>
            {label === "Connecting…" ? <ActivityIndicator size="small" color={colors.muted} accessibilityLabel="Connecting" /> : null}
          </View>
        ) : null}
        {p.error ? <T v="label" color={colors.bad}>{p.error}</T> : null}
      </View>
      {showSignIn(p) ? (
        <Button variant="primary" title="Sign in" label={`Sign in to ${p.name}`} onPress={() => onSignIn(p.name)} disabled={signInBusy} />
      ) : (
        <Switch value={switchOn(p, up)} label={`${p.name} connected`} onValueChange={(on) => onToggle(p.name, on)} disabled={busy} />
      )}
    </PressableCard>
  );
});

function AddCard({ count, sub: idle, busy, onPress }: { count: number; sub: string; busy: boolean; onPress: () => void }) {
  const reduce = useReduceMotion();
  const [pulse] = useState(() => new Animated.Value(1));
  useEffect(() => {
    if (!busy || reduce) {
      pulse.setValue(1);
      return;
    }
    const loop = Animated.loop(
      Animated.sequence([
        Animated.timing(pulse, { toValue: 0.45, duration: 700, useNativeDriver: true }),
        Animated.timing(pulse, { toValue: 1, duration: 700, useNativeDriver: true }),
      ]),
    );
    loop.start();
    return () => loop.stop();
  }, [busy, reduce, pulse]);

  const title = busy ? "Waiting for your browser" : count === 0 ? "Add your first tailnet" : "Add another tailnet";
  const sub = busy ? "Finish signing in on the Tailscale page that just opened. This screen updates when you're done." : idle;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={title}
      accessibilityHint={sub}
      accessibilityState={{ disabled: busy, busy }}
      disabled={busy}
      onPress={onPress}
      style={({ pressed }) => ({
        flexDirection: "row",
        alignItems: "center",
        gap: 14,
        padding: 14,
        paddingHorizontal: 16,
        minHeight: 64,
        borderWidth: 1,
        borderStyle: "dashed",
        borderCurve: "continuous",
        borderColor: pressed ? colors.accent : colors.lineStrong,
        backgroundColor: pressed ? colors.accentTint : "transparent",
        borderRadius: radius.md,
      })}
    >
      <Animated.View style={{ opacity: pulse, width: 34, height: 34, borderRadius: 17, backgroundColor: colors.accent, alignItems: "center", justifyContent: "center" }}>
        <PlusIcon />
      </Animated.View>
      <View style={{ flex: 1, minWidth: 0, gap: 2 }}>
        <T v="small" medium>{title}</T>
        <T v="label" color={colors.muted}>{sub}</T>
      </View>
      <ChevronIcon />
    </Pressable>
  );
}

// First run: no tailnets yet.
export function EmptyState({ busy, message, onAdd, onSync }: { busy: boolean; message: string; onAdd: () => void; onSync: () => void }) {
  return (
    <View style={{ alignItems: "center", gap: 16, paddingTop: 72, paddingBottom: 48 }}>
      <Mark width={64} />
      <T v="body" color={colors.muted} style={{ textAlign: "center", maxWidth: 360 }}>
        Tailfuse keeps several Tailscale tailnets online at once and sends each address to the tailnet that owns it.
      </T>
      <View style={{ alignSelf: "stretch", marginTop: 8 }}>
        <Button variant="primary" block busy={busy} title={busy ? "Waiting for your browser" : "Add your first tailnet"} onPress={onAdd} />
      </View>
      <View style={{ alignSelf: "stretch", marginTop: -4 }}>
        <Button block title="Sync from your computer" onPress={onSync} disabled={busy} />
      </View>
      <Hint live>{busy ? "Finish signing in on the Tailscale page that just opened." : message}</Hint>
    </View>
  );
}

type Props = {
  state: State;
  pending: ReadonlySet<string>;
  signingIn: string | null;
  message: string;
  onAdd: () => void;
  onOpen: (name: string) => void;
  onSignIn: (name: string) => void;
  onToggle: (name: string, on: boolean) => void;
};

export function TailnetsSection({ state, pending, signingIn, message, onAdd, onOpen, onSignIn, onToggle }: Props) {
  const { profiles, up } = state;
  return (
    <Pane title="Your tailnets" hint={tailnetsHint(state)}>
      <View style={{ gap: 8 }}>
        {profiles.map((p) => (
          <TailnetRow
            key={p.name}
            p={p}
            up={up}
            busy={pending.has(p.name)}
            signInBusy={signingIn !== null}
            onOpen={onOpen}
            onSignIn={onSignIn}
            onToggle={onToggle}
          />
        ))}
      </View>
      <AddCard count={profiles.length} sub={addSubtext(state)} busy={signingIn !== null} onPress={onAdd} />
      <Hint live>{message}</Hint>
    </Pane>
  );
}
