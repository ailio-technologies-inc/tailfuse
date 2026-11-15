// Preview-only switches for screenshots in a browser: ?empty=1, ?sheet=settings|check|ports|ports-form|permission|tailnet|<profile>.
// They do nothing on a phone, where there is no location.search.
const q = typeof location !== "undefined" ? new URLSearchParams(location.search) : new URLSearchParams();
export const preview = { empty: q.has("empty"), sheet: q.get("sheet") ?? "", result: q.get("result") ?? "" };
