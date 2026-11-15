// "Sync from your computer": scan the QR code Tailfuse shows on a Mac or PC (or paste its link), check
// what it would add, add it. Nothing is added before the confirmation step.
import * as Camera from "expo-camera";
import { useEffect, useRef, useState } from "react";
import { AppState, Linking, Platform, ScrollView, StyleSheet, View } from "react-native";

import { Button, Card, Field, Hint, Mark, T } from "./components";
import * as engine from "./engine";
import * as haptics from "./haptics";
import { SYNC_UNLOCK_BUTTON, decodeSyncLink, findSyncLink, friendly, syncButton, syncDoneText, syncFromLine, syncNote, syncPortsLine, syncRows, syncUnlockNote, type SyncSetup } from "./logic";
import { SAMPLE_SYNC_LINK } from "./mock";
import { deviceWord } from "./platform";
import { preview } from "./preview";
import { Sheet } from "./Sheet";
import { colors, radius } from "./theme";
import type { SyncResult } from "./types";

type Step = "scan" | "confirm" | "done";

type Props = {
  visible: boolean;
  /** A link that arrived by a deep link: open straight at the confirmation. */
  link: string | null;
  onClose: () => void;
  /** After the real import. */
  onImported: () => void;
  /** "Unlock to add the rest": gets the code so the sync can resume after the purchase. */
  onUnlock: (link: string) => void;
};

const cameraPossible = Platform.OS !== "web" && !!Camera.CameraView;
const SCAN_GAP_MS = 1500;

function Viewfinder({ children }: { children: React.ReactNode }) {
  const corner = (pos: object) => <View pointerEvents="none" style={[styles.corner, pos]} />;
  return (
    <View style={styles.finder}>
      {children}
      {corner({ top: 0, left: 0, borderTopWidth: 4, borderLeftWidth: 4, borderTopLeftRadius: 14 })}
      {corner({ top: 0, right: 0, borderTopWidth: 4, borderRightWidth: 4, borderTopRightRadius: 14 })}
      {corner({ bottom: 0, left: 0, borderBottomWidth: 4, borderLeftWidth: 4, borderBottomLeftRadius: 14 })}
      {corner({ bottom: 0, right: 0, borderBottomWidth: 4, borderRightWidth: 4, borderBottomRightRadius: 14 })}
    </View>
  );
}

function Scanner({ active, onLink, onNotTailfuse }: { active: boolean; onLink: (link: string) => void; onNotTailfuse: () => void }) {
  const [perm, ask] = Camera.useCameraPermissions();
  const [foreground, setForeground] = useState(AppState.currentState === "active");
  const last = useRef({ at: 0, text: "" });
  useEffect(() => {
    const sub = AppState.addEventListener("change", (s) => setForeground(s === "active"));
    return () => sub.remove();
  }, []);
  // Ask in context, the first time the scanner is on screen.
  useEffect(() => {
    if (active && perm && !perm.granted && perm.canAskAgain) void ask();
  }, [active, perm, ask]);

  if (!perm) return <Viewfinder>{null}</Viewfinder>;
  if (!perm.granted) {
    return (
      <View style={{ gap: 12 }}>
        <Viewfinder>
          <View style={styles.denied}>
            <T v="small" color={colors.muted} style={{ textAlign: "center" }}>
              {perm.canAskAgain ? "Tailfuse needs the camera to scan the code." : "The camera is off for Tailfuse. Turn it on in Settings to scan the code, or paste the link instead."}
            </T>
          </View>
        </Viewfinder>
        {perm.canAskAgain ? <Button block title="Allow the camera" onPress={() => void ask()} /> : <Button block title="Open Settings" onPress={() => void Linking.openSettings().catch(() => {})} />}
      </View>
    );
  }
  return (
    <Viewfinder>
      {active && foreground ? (
        <Camera.CameraView
          style={StyleSheet.absoluteFill}
          facing="back"
          barcodeScannerSettings={{ barcodeTypes: ["qr"] }}
          onBarcodeScanned={({ data }) => {
            const now = Date.now();
            if (data === last.current.text && now - last.current.at < SCAN_GAP_MS) return;
            last.current = { at: now, text: data };
            const link = findSyncLink(data);
            if (link) onLink(link);
            else onNotTailfuse();
          }}
        />
      ) : null}
    </Viewfinder>
  );
}

export function SyncSheet({ visible, link: incoming, onClose, onImported, onUnlock }: Props) {
  const device = deviceWord();
  const startConfirm = preview.sheet === "sync-confirm" || preview.sheet === "sync-done";
  const [step, setStep] = useState<Step>(startConfirm ? "confirm" : "scan");
  const [link, setLink] = useState(startConfirm ? SAMPLE_SYNC_LINK : "");
  const [setup, setSetup] = useState<SyncSetup | null>(startConfirm ? decodeSyncLink(SAMPLE_SYNC_LINK) : null);
  const [plan, setPlan] = useState<SyncResult | null>(null);
  const [done, setDone] = useState<SyncResult | null>(null);
  const [pasting, setPasting] = useState(!cameraPossible);
  const [paste, setPaste] = useState("");
  const [line, setLine] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);

  const reset = () => {
    setStep("scan");
    setLink("");
    setSetup(null);
    setPlan(null);
    setDone(null);
    setPaste("");
    setLine("");
    setPasting(!cameraPossible);
  };

  // Reset when the sheet closes; a link that arrives while closed opens it at the confirmation.
  const [wasVisible, setWasVisible] = useState(visible);
  if (wasVisible !== visible) {
    setWasVisible(visible);
    if (!visible) reset();
  }

  const check = async (text: string) => {
    if (busyRef.current) return;
    const found = findSyncLink(text);
    if (!found) {
      setLine("That isn't a Tailfuse code.");
      return;
    }
    busyRef.current = true;
    setBusy(true);
    try {
      const dry = await engine.importSetup(found, true);
      const decoded = decodeSyncLink(found);
      if (!decoded) throw new Error("that is not a tailfuse sync code");
      haptics.success();
      setLink(found);
      setSetup(decoded);
      setPlan(dry);
      setLine("");
      setStep("confirm");
    } catch (err) {
      const [title, text2] = friendly(err);
      setLine(`${title}. ${text2}`);
    }
    busyRef.current = false;
    setBusy(false);
  };

  const incomingRef = useRef("");
  useEffect(() => {
    if (visible && incoming && incoming !== incomingRef.current) {
      incomingRef.current = incoming;
      void check(incoming);
    }
    if (!visible) incomingRef.current = "";
  }, [visible, incoming]);

  // Preview: the confirmation and the done step without a real code.
  useEffect(() => {
    if (!visible || !startConfirm || plan) return;
    engine.importSetup(SAMPLE_SYNC_LINK, true).then(async (r) => {
      if (preview.sheet === "sync-done") {
        const real = await engine.importSetup(SAMPLE_SYNC_LINK, false);
        setDone(real);
        setStep("done");
      } else setPlan(r);
    }).catch(() => {});
  }, [visible, startConfirm, plan]);

  const add = async () => {
    if (busyRef.current || !link) return;
    busyRef.current = true;
    setBusy(true);
    try {
      const r = await engine.importSetup(link, false);
      haptics.success();
      setDone(r);
      setStep("done");
      onImported();
    } catch (err) {
      const [title, text] = friendly(err);
      setLine(`${title}. ${text}`);
    }
    busyRef.current = false;
    setBusy(false);
  };

  return (
    <Sheet visible={visible} onClose={onClose} title="Sync from your computer" tall={step === "confirm"}>
      {step === "scan" ? (
        <>
          {!pasting && cameraPossible ? <Scanner active={visible && step === "scan"} onLink={(l) => void check(l)} onNotTailfuse={() => setLine("That isn't a Tailfuse code.")} /> : null}
          <T v="small" color={colors.muted} style={{ textAlign: "center" }}>
            On your computer, open Tailfuse, then Settings, then Set up your phone.
          </T>
          {line ? <T v="label" color={colors.bad} accessibilityLiveRegion="polite" style={{ textAlign: "center" }}>{line}</T> : null}
          {pasting ? (
            <View style={{ gap: 12 }}>
              <Field label="Link" hideLabel value={paste} onChangeText={setPaste} placeholder="tailfuse://sync?..." multiline={false} returnKeyType="go" onSubmitEditing={() => void check(paste)} />
              <Button variant="primary" block title="Continue" onPress={() => void check(paste)} disabled={!paste.trim()} busy={busy} />
            </View>
          ) : (
            <View style={{ alignItems: "center" }}>
              <Button title="Paste a link instead" onPress={() => setPasting(true)} />
            </View>
          )}
        </>
      ) : null}

      {step === "confirm" && setup && plan ? (
        <>
          <T v="small" medium>{syncFromLine(setup)}</T>
          <ScrollView nestedScrollEnabled style={{ maxHeight: 320, borderRadius: radius.md }} showsVerticalScrollIndicator>
          <Card>
            {syncRows(setup, plan).map((r, i) => (
              <View key={r.name} style={{ flexDirection: "row", alignItems: "center", gap: 12, paddingHorizontal: 14, paddingVertical: 10, borderTopWidth: i ? StyleSheet.hairlineWidth : 0, borderTopColor: colors.lineStrong }}>
                <View style={{ width: 9, height: 9, borderRadius: 5, backgroundColor: r.status === "add" ? colors.accent : colors.dot }} />
                <View style={{ flex: 1, minWidth: 0, gap: 1 }}>
                  <T v="small" medium numberOfLines={1}>{r.title}</T>
                  {r.detail ? <T v="mono" color={colors.muted}>{r.detail}</T> : null}
                </View>
                <T v="label" medium color={r.status === "add" ? colors.accent : colors.muted}>{r.label}</T>
              </View>
            ))}
          </Card>
          </ScrollView>
          {syncPortsLine(plan) ? <T v="label" color={colors.muted}>{`${syncPortsLine(plan)} too.`}</T> : null}
          <Hint>{syncNote(device)}</Hint>
          {syncUnlockNote(plan) ? (
            <View style={{ gap: 8, alignItems: "flex-start" }}>
              <T v="label" color={colors.accent}>{syncUnlockNote(plan)}</T>
              <Button title={SYNC_UNLOCK_BUTTON} onPress={() => onUnlock(link)} />
            </View>
          ) : null}
          {line ? <T v="label" color={colors.bad} accessibilityLiveRegion="polite">{line}</T> : null}
          <View style={{ gap: 10 }}>
            <Button variant="primary" block title={syncButton(plan).title} disabled={syncButton(plan).disabled} busy={busy} onPress={() => void add()} />
            <Button block title="Cancel" onPress={onClose} />
          </View>
        </>
      ) : null}

      {step === "done" && done ? (
        <View style={{ gap: 16, alignItems: "center", paddingVertical: 8 }}>
          <Mark width={44} />
          <T v="body" style={{ textAlign: "center" }} accessibilityLiveRegion="polite">{syncDoneText(done)}</T>
          <Button variant="primary" block title="Done" onPress={onClose} />
        </View>
      ) : null}
    </Sheet>
  );
}

const styles = StyleSheet.create({
  finder: {
    alignSelf: "center",
    width: "78%",
    maxWidth: 300,
    aspectRatio: 1,
    borderRadius: radius.lg,
    backgroundColor: colors.deep,
    overflow: "hidden",
    justifyContent: "center",
  },
  corner: { position: "absolute", width: 34, height: 34, borderColor: colors.accent },
  denied: { flex: 1, alignItems: "center", justifyContent: "center", padding: 20 },
});
