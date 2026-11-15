import { View } from "react-native";

import { Button, CheckIcon, Hint, LinkButton, Pane, T } from "./components";
import { unlockLabel } from "./logic";
import { accountWord } from "./platform";
import { colors, radius } from "./theme";

type Props = {
  owned: boolean;
  price: string | null;
  busy: "purchase" | "restore" | null;
  note: string;
  onUnlock: () => void;
  onRestore: () => void;
};

export function UnlockSection({ owned, price, busy, note, onUnlock, onRestore }: Props) {
  return (
    <Pane title="Unlimited tailnets">
      {owned ? (
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 14,
            padding: 14,
            paddingHorizontal: 16,
            borderWidth: 1,
            borderColor: "rgba(62,207,114,0.4)",
            borderRadius: radius.md,
            backgroundColor: "#1d2a22", // ok at 7% over bg
          }}
        >
          <View style={{ width: 32, height: 32, borderRadius: 16, alignItems: "center", justifyContent: "center", backgroundColor: "rgba(62,207,114,0.16)" }}>
            <CheckIcon size={18} color={colors.ok} strokeWidth={2.4} />
          </View>
          <View style={{ flex: 1, minWidth: 0, gap: 2 }}>
            <T v="small" medium>Unlimited tailnets</T>
            <T v="label" color={colors.muted}>{`Thank you. Purchased on this ${accountWord()}.`}</T>
          </View>
        </View>
      ) : (
        <>
          <Hint>{`Two tailnets are free. A one-time purchase adds as many as you need, on this ${accountWord()}.`}</Hint>
          <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", columnGap: 16, rowGap: 4 }}>
            <Button
              variant="primary"
              title={unlockLabel(price)}
              onPress={onUnlock}
              busy={busy === "purchase"}
              disabled={busy === "restore"}
              label={price ? `Unlock unlimited tailnets for ${price}` : "Unlock unlimited tailnets"}
            />
            <LinkButton title="Restore purchase" onPress={busy ? () => {} : onRestore} />
          </View>
        </>
      )}
      <Hint live>{busy === "purchase" ? "Waiting for the store." : busy === "restore" ? "Checking the store." : note}</Hint>
    </Pane>
  );
}
