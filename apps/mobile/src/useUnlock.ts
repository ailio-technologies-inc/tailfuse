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

  // Both resolve true when the unlock is now held, so a caller can carry on with what it was doing.
  const unlock = useCallback(async (): Promise<boolean> => {
    setBusy("purchase");
    setNote("");
    let ok = false;
    try {
      const outcome = await store.purchase();
      if (outcome === "pending") setNote("Waiting for approval. Tailfuse unlocks as soon as the store confirms it.");
      if (outcome === "purchased") {
        await sync();
        ok = true;
      }
    } catch (err) {
      setNote(storeErrorText(err));
    }
    setBusy(null);
    return ok;
  }, [sync]);

  const restore = useCallback(async (): Promise<boolean> => {
    setBusy("restore");
    setNote("");
    let ok = false;
    try {
      if (await store.restore()) {
        await sync();
        ok = true;
      } else setNote(`No earlier purchase was found on this ${accountWord()}.`);
    } catch (err) {
      setNote(storeErrorText(err));
    }
    setBusy(null);
    return ok;
  }, [sync]);

  const clearNote = useCallback(() => setNote(""), []);

  return { price, busy, note, unlock, restore, clearNote };
}
