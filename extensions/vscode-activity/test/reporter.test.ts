import assert from "node:assert/strict";
import { test } from "node:test";

import { Reporter, type Report } from "../src/reporter.ts";

function setup(): { reporter: Reporter; sent: Report[]; advance: (ms: number) => void } {
  let now = 1_000_000;
  const sent: Report[] = [];
  const reporter = new Reporter((r) => sent.push(r), () => now, 5000, "p");
  return { reporter, sent, advance: (ms) => (now += ms) };
}

test("edits are reported at most once per interval", () => {
  const { reporter, sent, advance } = setup();
  reporter.edit();
  reporter.edit();
  advance(4999);
  reporter.edit();
  advance(1);
  reporter.edit();
  assert.deepEqual(sent, [{ kind: "edit" }, { kind: "edit" }]);
});

test("every save is reported", () => {
  const { reporter, sent } = setup();
  reporter.save();
  reporter.save();
  assert.deepEqual(sent, [{ kind: "save" }, { kind: "save" }]);
});

test("terminal commands are reported as they start and end", () => {
  const { reporter, sent } = setup();
  const build = {};
  const watch = {};
  reporter.commandStarted(build, "npm run build");
  reporter.commandStarted(watch, "npm run watch");
  reporter.reminder();
  reporter.commandEnded(build);
  reporter.commandEnded(build); // a second end for the same run is ignored
  reporter.reminder();
  reporter.commandEnded(watch);
  reporter.reminder(); // nothing running: nothing to say
  assert.deepEqual(sent, [
    { kind: "terminal-start", id: "p-1", command: "npm run build" },
    { kind: "terminal-start", id: "p-2", command: "npm run watch" },
    { kind: "terminal-running", ids: ["p-1", "p-2"] },
    { kind: "terminal-end", id: "p-1" },
    { kind: "terminal-running", ids: ["p-2"] },
    { kind: "terminal-end", id: "p-2" },
  ]);
});

test("an idle editor reports nothing", () => {
  const { reporter, sent, advance } = setup();
  for (let i = 0; i < 10; i++) {
    advance(60_000);
    reporter.reminder();
  }
  assert.deepEqual(sent, []);
});
