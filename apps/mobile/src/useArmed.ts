import { useCallback, useEffect, useRef, useState } from "react";

// The two-step Remove: the first tap arms an id for 4 s, a second tap inside that time confirms.
export function useArmed(ms = 4000) {
  const [armed, setArmed] = useState<ReadonlySet<string>>(new Set());
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  useEffect(() => () => timers.current.forEach(clearTimeout), []);

  const clear = useCallback((id: string) => {
    clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    setArmed((s) => {
      const n = new Set(s);
      n.delete(id);
      return n;
    });
  }, []);

  /** Returns true when this tap confirms (it was already armed); otherwise arms and returns false. */
  const tap = useCallback((id: string) => {
    if (timers.current.has(id)) {
      clear(id);
      return true;
    }
    timers.current.set(id, setTimeout(() => clear(id), ms));
    setArmed((s) => new Set(s).add(id));
    return false;
  }, [clear, ms]);

  return { isArmed: (id: string) => armed.has(id), tap };
}
