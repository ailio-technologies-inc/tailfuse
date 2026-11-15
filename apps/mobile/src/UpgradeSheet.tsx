// Shown when someone tries to go past the two free tailnets: what the one-time purchase is, and the
// buttons to buy or restore it. The purchase logic itself lives in useUnlock / store.ts.
import { View } from "react-native";

import { Button, CheckIcon, Hint, LinkButton, Mark, T } from "./components";
import { unlockLabel, upgradeBullets, upgradeIntro } from "./logic";
import { accountWord } from "./platform";
import { Sheet } from "./Sheet";
import { colors } from "./theme";

type Props = {
  visible: boolean;
  price: string | null;
  busy: "purchase" | "restore" | null;
  /** A store problem in plain words ("" when none). A cancelled purchase leaves it empty. */
  note: string;
  onUnlock: () => void;
  onRestore: () => void;
  onClose: () => void;
};

export function UpgradeSheet({ visible, price, busy, note, onUnlock, onRestore, onClose }: Props) {
  const account = accountWord();
  return (
    <Sheet visible={visible} onClose={onClose}>
      <View style={{ gap: 12 }}>
        <Mark width={40} />
        <T v="h2" accessibilityRole="header" style={{ fontSize: 19, lineHeight: 24 }}>Unlimited tailnets</T>
        <T v="small" color={colors.muted} style={{ lineHeight: 21 }}>{upgradeIntro(account)}</T>
        <View style={{ gap: 8, paddingTop: 4 }}>
          {upgradeBullets(account).map((line) => (
            <View key={line} style={{ flexDirection: "row", alignItems: "flex-start", gap: 10 }}>
              <View style={{ paddingTop: 3 }}>
                <CheckIcon size={16} color={colors.ok} strokeWidth={2.4} />
              </View>
              <T v="small" style={{ flex: 1 }}>{line}</T>
            </View>
          ))}
        </View>
      </View>
      <View style={{ gap: 10 }}>
        <Button
          variant="primary"
          block
          title={unlockLabel(price)}
          onPress={onUnlock}
          busy={busy === "purchase"}
          disabled={busy === "restore"}
          label={price ? `Unlock unlimited tailnets for ${price}` : "Unlock unlimited tailnets"}
        />
        <View style={{ alignItems: "center", paddingVertical: 4 }}>
          <LinkButton title="Restore purchase" onPress={busy ? () => {} : onRestore} />
        </View>
        <Hint live>{busy === "purchase" ? "Waiting for the store." : busy === "restore" ? "Checking the store." : note}</Hint>
        <Button block title="Not now" onPress={onClose} disabled={busy === "purchase"} />
      </View>
    </Sheet>
  );
}
