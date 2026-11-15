import { useCallback, useEffect, useState } from "react";

import * as engine from "./engine";
import { storeErrorText } from "./logic";
import { accountWord } from "./platform";
import * as store from "./store";

// The store is the source of truth for the purchase; the engine only keeps the flag.
export function useUnlock(refresh: () => Promise<unknown>) {
  const [price, setPrice] = useState<string | null>(null);
  const [busy, setBusy] = useState<"purchase" | "restore" | null>(null);
  const [note, setNote] = useState("");

  // Asks the store and tells the engine. If the store can't be reached the flag stays as it was.
  const sync = useCallback(async () => {
    try {
      await engine.setUnlocked(await store.isOwned());
    } catch {
      // keep the saved answer
    }
    await refresh();
  }, [refresh]);

  useEffect(() => {
    let live = true;
    store.getProduct().then((p) => live && setPrice(p?.price ?? null)).catch(() => {});
    sync();
    store.onPurchaseCompleted(sync);
    return () => {
      live = false;
    };
  }, [sync]);

  const unlock = useCallback(async () => {
    setBusy("purchase");
    setNote("");
    try {
      const outcome = await store.purchase();
      if (outcome === "pending") setNote("Waiting for approval. Tailfuse unlocks as soon as the store confirms it.");
      if (outcome === "purchased") await sync();
    } catch (err) {
      setNote(storeErrorText(err));
    }
    setBusy(null);
  }, [sync]);

  const restore = useCallback(async () => {
    setBusy("restore");
    setNote("");
    try {
      if (await store.restore()) await sync();
      else setNote(`No earlier purchase was found on this ${accountWord()}.`);
    } catch (err) {
      setNote(storeErrorText(err));
    }
    setBusy(null);
  }, [sync]);

  return { price, busy, note, unlock, restore };
}
