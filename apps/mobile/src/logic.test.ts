import assert from "node:assert/strict";
import { test } from "node:test";

import { addSubtext, atLimit, checkResult, deviceAddress, deviceUrl, details, filterDevices, friendly, osLabel, showDeviceSearch, showStateLabel, sortDevices, needsSignIn, sentence, statusLine, storeErrorText, tailnetState, tailnetsHint, unlockLabel } from "./logic";
import type { Device, Profile } from "./types";

const p = (o: Partial<Profile> = {}): Profile => ({
  name: "work", account: "", tailnet: "", suffixes: [".work.ts.net"], hostname: "", ips: [], online: false, paused: false, error: "", auth_url: "", ...o,
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
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a" }), p({ name: "b" })] }), "Sign in to a and b to bring them online.");
  assert.equal(statusLine({ up: true, profiles: [p({ name: "a" })] }), "Sign in to a to bring it online.");
  assert.equal(statusLine({ up: true, profiles: [p({ error: "x" })] }), "work has a problem. See the note beside it.");
  assert.equal(statusLine({ up: true, profiles: [p({ paused: true })] }), "Every tailnet is switched off. Turn one on below.");
});

test("tailnetState and needsSignIn", () => {
  assert.deepEqual(tailnetState(p({ error: "x", online: true }), true), ["Problem", "bad"]);
  assert.deepEqual(tailnetState(p({ online: true }), true), ["Connected", "on"]);
  assert.deepEqual(tailnetState(p(), false), ["Off", ""]);
  assert.deepEqual(tailnetState(p({ paused: true }), true), ["Disconnected", ""]);
  assert.deepEqual(tailnetState(p(), true), ["Sign-in needed", "wait"]);
  assert.equal(needsSignIn(p(), true), true);
  assert.equal(needsSignIn(p(), false), false);
  assert.equal(needsSignIn(p({ paused: true }), true), false);
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

test("devices: sorting", () => {
  const list = [dev("zeta", true), dev("Beta", false), dev("alpha", false), dev("mu", true)];
  assert.deepEqual(sortDevices(list).map((d) => d.name), ["mu", "zeta", "alpha", "Beta"]);
  assert.equal(list[0].name, "zeta"); // the input is not changed
});

test("devices: filtering and the search threshold", () => {
  const list = [dev("nas", true, { os: "linux" }), dev("adas-macbook", true, { os: "macOS", ips: ["100.64.0.9"] }), dev("pixel", false, { os: "android" })];
  assert.equal(filterDevices(list, "").length, 3);
  assert.equal(filterDevices(list, "  ").length, 3);
  assert.deepEqual(filterDevices(list, "MAC").map((d) => d.name), ["adas-macbook"]);
  assert.deepEqual(filterDevices(list, "android").map((d) => d.name), ["pixel"]);
  assert.deepEqual(filterDevices(list, "100.64.0.9").map((d) => d.name), ["adas-macbook"]);
  assert.deepEqual(filterDevices(list, "nas.x.ts").map((d) => d.name), ["nas"]);
  assert.equal(filterDevices(list, "nothing").length, 0);
  assert.equal(showDeviceSearch(8), false);
  assert.equal(showDeviceSearch(9), true);
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
