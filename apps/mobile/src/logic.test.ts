import assert from "node:assert/strict";
import { test } from "node:test";

import { SAMPLE_SYNC_LINK } from "./mock";
import { DOMAIN_HINT, addSubtext, decodeSyncLink, domainRows, findSyncLink, isSyncLink, showSignIn, switchOn, isConnecting, stabilize, capList, deviceSummary, formatCount, devicesReducer, devicesInitial, hasMoreDevices, noDevicesText, showAllLabel, isLimitError, addNeedsUpgrade, upgradeBullets, upgradeIntro, suggestButton, suggestLine, syncButton, syncDoneText, syncFromLine, syncRows, syncUnlockNote, atLimit, checkResult, deviceAddress, deviceUrl, details, friendly, osLabel, showDeviceSearch, showStateLabel, sentence, statusLine, storeErrorText, tailnetState, tailnetsHint, unlockLabel } from "./logic";
import type { Device, DevicePage, Profile, State, SyncResult } from "./types";

const p = (o: Partial<Profile> = {}): Profile => ({
  name: "work", account: "", tailnet: "", suffixes: [".work.ts.net"], hostname: "", ips: [], online: false, paused: false, error: "", auth_url: "", needs_login: false, connecting: false, ...o,
});

test("sentence", () => {
  assert.equal(sentence("tailfuse is off"), "Tailfuse is off.");
  assert.equal(sentence(new Error("Error: bad thing!")), "Bad thing!");
  assert.equal(sentence(""), "");
});

test("friendly", () => {
  assert.equal(friendly(new Error("tailfuse is off"))[0], "Tailfuse is off");
  assert.equal(friendly("listen tcp: address already in use")[0], "Tailfuse's port is busy");
  assert.equal(friendly("sign-in not finished in time")[0], "The sign-in ran out of time");
  assert.equal(friendly("no profiles defined")[0], "Add a tailnet first");
  assert.equal(friendly("that needs the unlimited upgrade")[0], "That needs the unlock");
  assert.match(friendly("that needs the unlimited upgrade")[1], /Unlimited tailnets/);
  assert.deepEqual(friendly("boom"), ["Something went wrong", "Boom."]);
});

test("statusLine", () => {
  assert.equal(statusLine({ up: false, profiles: [] }), "Add a tailnet below to get started.");
  assert.equal(statusLine({ up: false, profiles: [p()] }), "Tailfuse is off. Turn it on to reach your tailnet.");
  assert.equal(statusLine({ up: false, profiles: [p(), p({ name: "b" })] }), "Tailfuse is off. Turn it on to reach your 2 tailnets.");
  assert.equal(statusLine({ up: true, profiles: [p({ online: true }), p({ name: "b", online: true })] }), "2 tailnets online. Open any of their addresses as usual.");
  assert.equal(statusLine({ up: true, profiles: [p({ online: true }), p({ name: "b", paused: true })] }), "1 tailnet online. 1 switched off. Open any of their addresses as usual.");
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a", needs_login: true }), p({ name: "b", needs_login: true })] }), "Sign in to a and b to bring them online.");
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a", needs_login: true })] }), "Sign in to a to bring it online.");
  // merely coming up is not a sign-in
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a", connecting: true })] }), "Connecting to a…");
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a", connecting: true }), p({ name: "b" })] }), "Connecting to 2 tailnets…");
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a", online: true }), p({ name: "b", connecting: true })] }), "Connecting to b…");
  assert.equal(statusLine({ up: false, profiles: [p({ name: "a" })] }), "Tailfuse is off. Turn it on to reach your tailnet.");
  assert.equal(statusLine({ up: true, profiles: [p({ error: "x" })] }), "work has a problem. See the note beside it.");
  assert.equal(statusLine({ up: true, profiles: [p({ paused: true })] }), "Every tailnet is switched off. Turn one on below.");
});

test("tailnetState: error, connected, sign-in, off, paused, connecting, in that order", () => {
  assert.deepEqual(tailnetState(p({ error: "x", online: true }), true), ["Problem", "bad"]);
  assert.deepEqual(tailnetState(p({ error: "x", needs_login: true }), true), ["Problem", "bad"]);
  assert.deepEqual(tailnetState(p({ online: true, needs_login: true }), true), ["Connected", "on"]);
  assert.deepEqual(tailnetState(p({ needs_login: true }), true), ["Sign-in needed", "wait"]);
  assert.deepEqual(tailnetState(p({ needs_login: true }), false), ["Sign-in needed", "wait"]); // imported, never signed in
  assert.deepEqual(tailnetState(p(), false), ["Off", ""]); // off, signed in before: no sign-in
  assert.deepEqual(tailnetState(p({ paused: true }), true), ["Disconnected", ""]);
  assert.deepEqual(tailnetState(p({ connecting: true }), true), ["Connecting…", "wait"]);
  assert.deepEqual(tailnetState(p(), true), ["Connecting…", "wait"]); // on, nothing known: still not a sign-in
});

test("showSignIn only when the engine says a sign-in is needed", () => {
  assert.equal(showSignIn(p()), false);
  assert.equal(showSignIn(p({ connecting: true })), false);
  assert.equal(showSignIn(p({ needs_login: true })), true);
  assert.equal(showSignIn(p({ needs_login: true, error: "x" })), false);
  assert.equal(showSignIn(p({ needs_login: true, online: true })), false);
});

test("the switch reads on while connecting", () => {
  assert.equal(switchOn(p({ online: true }), true), true);
  assert.equal(switchOn(p({ connecting: true }), true), true);
  assert.equal(switchOn(p(), false), false);
  assert.equal(switchOn(p({ paused: true }), true), false);
  assert.equal(switchOn(p({ needs_login: true }), true), false);
  assert.equal(isConnecting(p(), false), false);
});

test("details", () => {
  const rows = details(p({ account: "a@b.c", hostname: "adas-iphone", ips: ["100.64.0.7", "fd7a::7", ""] }), "iPhone");
  assert.deepEqual(rows, [["This iPhone's name", "adas-iphone"], ["IPv4", "100.64.0.7"], ["IPv6", "fd7a::7"], ["Name in Tailfuse", "work"]]);
  assert.deepEqual(details(p(), "phone"), []);
});

test("checkResult", () => {
  assert.equal(checkResult("nas.work.ts.net", "work"), "nas.work.ts.net goes to work.");
  assert.match(checkResult("example.com", null), /is on none of your tailnets/);
});

test("the free limit", () => {
  const two = [p(), p({ name: "b" })];
  assert.equal(atLimit({ profiles: two, unlocked: false, free_profiles: 2 }), true);
  assert.equal(atLimit({ profiles: two, unlocked: true, free_profiles: 2 }), false);
  assert.equal(atLimit({ profiles: [p()], unlocked: false }), false);
  assert.equal(atLimit({ profiles: two }), true); // free_profiles defaults to 2
  assert.equal(tailnetsHint({ profiles: [], unlocked: false }), "");
  assert.equal(tailnetsHint({ profiles: [p()], unlocked: false, free_profiles: 2 }), "1 of 2 free tailnets used");
  assert.equal(tailnetsHint({ profiles: two, unlocked: false, free_profiles: 2 }), "2 of 2 free tailnets used");
  assert.equal(tailnetsHint({ profiles: [...two, p({ name: "c" })], unlocked: false, free_profiles: 2 }), "3 tailnets");
  assert.equal(tailnetsHint({ profiles: [...two, p({ name: "c" })], unlocked: true }), "3 tailnets, unlocked");
  assert.equal(tailnetsHint({ profiles: [p()], unlocked: true }), "1 tailnet, unlocked");
  assert.match(addSubtext({ profiles: two, unlocked: false }), /A third tailnet needs the one-time unlock\.$/);
  assert.match(addSubtext({ profiles: [p()], unlocked: false }), /fill themselves in\.$/);
  assert.match(addSubtext({ profiles: two, unlocked: true }), /fill themselves in\.$/);
});

test("the unlock button and store errors", () => {
  assert.equal(unlockLabel("$4.99"), "Unlock for $4.99");
  assert.equal(unlockLabel(null), "Unlock");
  assert.equal(unlockLabel(""), "Unlock");
  assert.match(storeErrorText(new Error("Network request failed")), /reach the store/);
  assert.match(storeErrorText(new Error("E_SERVICE_ERROR")), /couldn't finish/);
});

const dev = (name: string, online: boolean, o: Partial<Device> = {}): Device => ({ name, dns_name: `${name}.x.ts.net.`, ips: ["100.64.0.1"], os: "linux", online, ...o });

test("devices: search threshold, summary, counts", () => {
  assert.equal(showDeviceSearch(8), false);
  assert.equal(showDeviceSearch(9), true);
  assert.equal(deviceSummary(1204, 37, "en-US"), "1,204 devices · 37 online");
  assert.equal(deviceSummary(1204, 37, "de-DE"), "1.204 devices · 37 online");
  assert.equal(deviceSummary(1, 1, "en-US"), "1 device · 1 online");
  assert.equal(formatCount(5000, "en-US"), "5,000");
  assert.equal(formatCount(5, "not a locale!!"), "5");
});

test("devices: empty and no-match text", () => {
  assert.equal(noDevicesText("", false), "No devices yet");
  assert.equal(noDevicesText("  nas ", false), "No devices match “nas”");
  assert.equal(noDevicesText("", true), "No devices are online");
});

test("capList: '+N more' and Show all", () => {
  assert.deepEqual(capList(["a", "b"], 2), { shown: ["a", "b"], more: "", hidden: 0 });
  assert.deepEqual(capList(["a", "b", "c", "d", "e"], 2), { shown: ["a", "b"], more: "+3 more", hidden: 3 });
  assert.equal(showAllLabel(12), "Show all (12)");
});

const page = (n: number, from = 0, o: Partial<DevicePage> = {}): DevicePage => ({
  devices: Array.from({ length: n }, (_, i) => dev(`d${from + i}`, true)), total: 120, online: 100, matched: 120, ...o,
});

test("devices reducer: first page, load more, the end", () => {
  let s = devicesReducer(devicesInitial, { type: "fetch", key: "work", req: 1, append: false });
  assert.equal(s.status, "loading");
  s = devicesReducer(s, { type: "loaded", req: 1, offset: 0, page: page(50) });
  assert.equal(s.status, "ready");
  assert.equal(s.devices.length, 50);
  assert.equal(hasMoreDevices(s), true);
  s = devicesReducer(s, { type: "fetch", key: "work", req: 2, append: true });
  assert.equal(s.loadingMore, true);
  assert.equal(s.devices.length, 50); // rows stay
  assert.equal(hasMoreDevices(s), false); // not while one is loading
  s = devicesReducer(s, { type: "loaded", req: 2, offset: 50, page: page(50, 50) });
  s = devicesReducer(s, { type: "fetch", key: "work", req: 3, append: true });
  s = devicesReducer(s, { type: "loaded", req: 3, offset: 100, page: page(20, 100) });
  assert.equal(s.devices.length, 120);
  assert.equal(hasMoreDevices(s), false);
  assert.equal(new Set(s.devices.map((d) => d.name)).size, 120);
});

test("devices reducer: out-of-order and stale replies are ignored", () => {
  let s = devicesReducer(devicesInitial, { type: "fetch", key: "work", req: 1, append: false });
  s = devicesReducer(s, { type: "loaded", req: 1, offset: 0, page: page(50) });
  // the person types: request 2, then 3; 3 answers first, 2 answers late
  s = devicesReducer(s, { type: "fetch", key: "work", req: 2, append: false });
  assert.equal(s.status, "ready"); // old rows stay while searching
  s = devicesReducer(s, { type: "fetch", key: "work", req: 3, append: false });
  s = devicesReducer(s, { type: "loaded", req: 3, offset: 0, page: page(2, 0, { matched: 2 }) });
  const before = s;
  assert.equal(devicesReducer(s, { type: "loaded", req: 2, offset: 0, page: page(50) }), before);
  assert.equal(devicesReducer(s, { type: "failed", req: 2, text: "late" }), before);
  assert.equal(s.devices.length, 2);
  // a page that no longer lines up is dropped too
  s = devicesReducer(s, { type: "fetch", key: "work", req: 4, append: true });
  assert.equal(devicesReducer(s, { type: "loaded", req: 4, offset: 50, page: page(5) }), s);
});

test("devices reducer: another tailnet starts empty; errors", () => {
  let s = devicesReducer(devicesInitial, { type: "fetch", key: "work", req: 1, append: false });
  s = devicesReducer(s, { type: "loaded", req: 1, offset: 0, page: page(10) });
  s = devicesReducer(s, { type: "fetch", key: "home", req: 2, append: false });
  assert.deepEqual([s.status, s.devices.length], ["loading", 0]);
  s = devicesReducer(s, { type: "failed", req: 2, text: "Timed out." });
  assert.deepEqual([s.status, s.error], ["error", "Timed out."]);
  // a failed next page keeps the rows and shows the error under them
  s = devicesReducer(s, { type: "fetch", key: "home", req: 3, append: false });
  s = devicesReducer(s, { type: "loaded", req: 3, offset: 0, page: page(50) });
  s = devicesReducer(s, { type: "fetch", key: "home", req: 4, append: true });
  s = devicesReducer(s, { type: "failed", req: 4, text: "x" });
  assert.deepEqual([s.status, s.devices.length, s.error, hasMoreDevices(s)], ["ready", 50, "x", false]);
  assert.equal(devicesReducer(s, { type: "reset" }).devices.length, 0);
});

const state = (o: Partial<State> = {}): State => ({ up: true, proxy: "", pac_url: "", match_domains: ["a.ts.net"], profiles: [p({ name: "a" }), p({ name: "b" })], tunnels: [], ...o });

test("stabilize keeps references when nothing changed", () => {
  const prev = state();
  assert.equal(stabilize(prev, JSON.parse(JSON.stringify(prev))), prev);
  const next = JSON.parse(JSON.stringify(prev)) as State;
  next.profiles[1].online = true;
  const out = stabilize(prev, next);
  assert.notEqual(out, prev);
  assert.equal(out.profiles[0], prev.profiles[0]); // unchanged row keeps its object
  assert.notEqual(out.profiles[1], prev.profiles[1]);
  assert.equal(out.match_domains, prev.match_domains);
  assert.equal(stabilize(null, next), next);
});

test("upgrade modal: when it opens, what it says", () => {
  assert.equal(addNeedsUpgrade({ profiles: [p(), p({ name: "b" })], unlocked: false }), true);
  assert.equal(addNeedsUpgrade({ profiles: [p(), p({ name: "b" })], unlocked: true }), false);
  assert.equal(addNeedsUpgrade({ profiles: [p()], unlocked: false }), false);
  assert.equal(isLimitError(new Error("that needs the unlimited upgrade")), true);
  assert.equal(isLimitError(new Error("tailfuse is off")), false);
  assert.match(upgradeIntro("Google account"), /on this Google account\.$/);
  assert.equal(upgradeBullets("Apple ID").length, 3);
  assert.equal(unlockLabel("$4.99"), "Unlock for $4.99");
  assert.equal(unlockLabel(null), "Unlock");
  assert.equal(unlockLabel(""), "Unlock");
});

test("devices: addresses, urls and OS names", () => {
  assert.equal(deviceAddress("nas.home-lab.ts.net."), "nas.home-lab.ts.net");
  assert.equal(deviceAddress("nas.home-lab.ts.net"), "nas.home-lab.ts.net");
  assert.equal(deviceUrl("nas.home-lab.ts.net."), "https://nas.home-lab.ts.net");
  assert.equal(osLabel("macOS"), "macOS");
  assert.equal(osLabel("linux"), "Linux");
  assert.equal(osLabel("iOS"), "iOS");
  assert.equal(osLabel("plan9"), "Plan9");
  assert.equal(osLabel(""), "");
});

test("the row's state label is only shown when it adds something", () => {
  assert.equal(showStateLabel("Connected"), false);
  assert.equal(showStateLabel("Off"), false);
  for (const l of ["Problem", "Disconnected", "Sign-in needed"]) assert.equal(showStateLabel(l), true);
});

const result = (o: Partial<SyncResult> = {}): SyncResult => ({ from: "Ada's MacBook", added: [], already: [], needs_unlock: [], tunnels_added: [], skipped: [], ...o });

test("sync links are recognised", () => {
  assert.ok(isSyncLink(SAMPLE_SYNC_LINK));
  assert.ok(isSyncLink(`  ${SAMPLE_SYNC_LINK}\n`));
  assert.equal(findSyncLink(`open this: ${SAMPLE_SYNC_LINK} please`), SAMPLE_SYNC_LINK);
  assert.equal(findSyncLink("https://tailfuse.com"), null);
  assert.equal(findSyncLink("tailfuse://sync?v=1"), null);
  assert.equal(isSyncLink("WIFI:S:home;T:WPA;P:secret;;"), false);
  assert.equal(isSyncLink(""), false);
});

test("the shared vector decodes", () => {
  const s = decodeSyncLink(SAMPLE_SYNC_LINK);
  assert.ok(s);
  assert.equal(s.from, "Ada's MacBook");
  assert.deepEqual(s.profiles.map((x) => x.name), ["personal", "work"]);
  assert.deepEqual(s.profiles[1].suffixes, [".work.ts.net", ".internal.example"]);
  assert.equal(s.tunnels[0].name, "prod-db");
  assert.equal(decodeSyncLink("tailfuse://sync?v=1&d=%%%"), null);
  assert.equal(decodeSyncLink("tailfuse://sync?v=1&d=e30"), null); // {} has no v
});

test("the confirmation rows and button", () => {
  const s = decodeSyncLink(SAMPLE_SYNC_LINK)!;
  const rows = syncRows(s, result({ added: ["personal"], needs_unlock: ["work"], tunnels_added: [] }));
  assert.deepEqual(rows.map((r) => [r.title, r.status, r.label]), [
    ["ada@example.com", "add", "Will be added"],
    ["ada@work.example", "unlock", "Needs the unlock"],
  ]);
  assert.equal(rows[1].detail, "*.work.ts.net  *.internal.example");
  assert.equal(syncRows(s, result({ already: ["personal", "work"] }))[0].label, "Already here");
  assert.equal(syncFromLine(s), "From Ada's MacBook");
  assert.equal(syncFromLine({ ...s, from: "" }), "From your computer");
  assert.deepEqual(syncButton(result({ added: ["a", "b"] })), { title: "Add 2 tailnets", disabled: false });
  assert.deepEqual(syncButton(result({ added: ["a"] })), { title: "Add 1 tailnet", disabled: false });
  assert.deepEqual(syncButton(result({ already: ["a"] })), { title: "Nothing new to add", disabled: true });
  assert.deepEqual(syncButton(result({ tunnels_added: ["x"] })), { title: "Add 1 local port", disabled: false });
  assert.match(syncUnlockNote(result({ needs_unlock: ["w"] })), /Unlimited tailnets/);
  assert.equal(syncUnlockNote(result()), "");
  assert.equal(syncDoneText(result({ added: ["a", "b"] })), "2 tailnets added. Sign in to each one to bring them online.");
  assert.equal(syncDoneText(result({ added: ["a"] })), "1 tailnet added. Sign in to it to bring it online.");
});

test("sync errors are friendly", () => {
  assert.equal(friendly("that is not a tailfuse sync code")[0], "That isn't a Tailfuse code");
  assert.equal(friendly("that code is from a newer tailfuse")[0], "That code is from a newer Tailfuse");
});

test("sign-in is offered for an imported tailnet even while off, never for one that is just off or connecting", () => {
  const fresh = p({ needs_login: true });
  assert.ok(showSignIn(fresh));
  assert.equal(tailnetState(fresh, false)[0], "Sign-in needed");
  const off = p({ account: "ada@work.example" });
  assert.equal(showSignIn(off), false);
  assert.equal(tailnetState(off, false)[0], "Off");
  assert.equal(showSignIn(p({ connecting: true })), false);
});

test("domains", () => {
  assert.deepEqual(domainRows(p({ suffixes: [".work.ts.net", ".ailio.dev"] })), [
    { suffix: ".work.ts.net", domain: "work.ts.net", builtIn: true },
    { suffix: ".ailio.dev", domain: "ailio.dev", builtIn: false },
  ]);
  assert.equal(suggestLine("ailio.dev", "work"), "ailio.dev points at a device on work.");
  assert.equal(suggestButton("ailio.dev", "work"), "Always send ailio.dev to work");
  assert.match(DOMAIN_HINT, /example\.com/);
  assert.equal(friendly('ailio.dev already belongs to the tailnet "home"; remove it there first')[0], "That domain is taken");
});
