import * as Clipboard from "expo-clipboard";
import { useCallback, useEffect, useRef, useState } from "react";

import * as haptics from "./haptics";

// Copies text and reports which key was copied for 1.5 s, so its label can say "Copied".
export function useCopied(ms = 1500) {
  const [copied, setCopied] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => void (timer.current && clearTimeout(timer.current)), []);
  const copy = useCallback(async (key: string, text: string) => {
    try {
      await Clipboard.setStringAsync(text);
    } catch {
      return;
    }
    haptics.light();
    setCopied(key);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(null), ms);
  }, [ms]);
  return { copied, copy };
}
