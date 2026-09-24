// Zero-dependency Chrome DevTools Protocol driver for the e2e harness: spawns
// headless Chromium, talks to it over Node's global WebSocket, and exposes the
// handful of page helpers the checks need. Needs a Chromium on $CHROME
// (default `chromium` on PATH, e.g. `nix shell nixpkgs#chromium`).
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const MOD = { alt: 1, ctrl: 2, meta: 4, shift: 8 };

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Every child and temp dir registers a synchronous undo here; it runs on
// normal exit, on an uncaught error and on SIGINT/SIGTERM/SIGHUP, so no
// browser or server outlives the run.
const cleanups = [];

export function onCleanup(fn) {
  cleanups.push(fn);
}

export function runCleanups() {
  while (cleanups.length > 0) {
    const fn = cleanups.pop();
    try {
      fn();
    } catch {
      // best effort: the remaining cleanups must still run
    }
  }
}

process.on("exit", runCleanups);
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    runCleanups();
    process.exit(130);
  });
}

// spawnGroup starts a child in its own process group so killGroup takes its
// whole tree (Chromium forks a zygote, GPU and renderer processes).
export function spawnGroup(cmd, args, opts = {}) {
  const child = spawn(cmd, args, { ...opts, detached: true, stdio: ["ignore", "pipe", "pipe"] });
  const kill = () => killGroup(child);
  onCleanup(kill);
  return { child, kill };
}

export function killGroup(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  try {
    process.kill(-child.pid, "SIGKILL");
  } catch {
    // already gone
  }
}

export function tempDir(prefix) {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  onCleanup(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// waitForLine resolves with the first match of re on the stream, or rejects
// when the child exits or the timeout passes.
export function waitForLine(child, stream, re, timeoutMs, what) {
  return new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => done(new Error(what + ": timed out after " + timeoutMs + " ms\n" + buf)), timeoutMs);
    const onData = (chunk) => {
      buf += chunk.toString();
      const m = buf.match(re);
      if (m) done(null, m);
    };
    const onExit = (code) => done(new Error(what + ": exited with " + code + "\n" + buf));
    function done(err, m) {
      clearTimeout(timer);
      stream.off("data", onData);
      child.off("exit", onExit);
      if (err) reject(err);
      else resolve(m);
    }
    stream.on("data", onData);
    child.on("exit", onExit);
  });
}

class Connection {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.calls = new Map();
    this.listeners = new Set();
    ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id !== undefined) {
        const call = this.calls.get(msg.id);
        if (!call) return;
        this.calls.delete(msg.id);
        if (msg.error) call.reject(new Error(call.method + ": " + msg.error.message));
        else call.resolve(msg.result);
        return;
      }
      for (const fn of this.listeners) fn(msg);
    });
    ws.addEventListener("close", () => {
      for (const call of this.calls.values()) call.reject(new Error(call.method + ": connection closed"));
      this.calls.clear();
    });
  }

  send(method, params = {}, sessionId) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.calls.set(id, { resolve, reject, method });
      this.ws.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

export class Browser {
  static async launch({ width = 1600, height = 1000 } = {}) {
    const chrome = process.env.CHROME || "chromium";
    const profile = tempDir("hookyard-e2e-chrome-");
    const { child, kill } = spawnGroup(chrome, [
      "--headless=new",
      "--remote-debugging-port=0",
      "--user-data-dir=" + profile,
      `--window-size=${width},${height}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-background-networking",
      "--disable-component-update",
      "--disable-sync",
      "--disable-background-timer-throttling",
      "--disable-backgrounding-occluded-windows",
      "--disable-renderer-backgrounding",
      "--mute-audio",
      "--password-store=basic",
      "about:blank",
    ], { env: { ...process.env, TMPDIR: profile } }); // its singleton socket dir lands in the profile, removed with it
    child.stdout.resume();
    const m = await waitForLine(child, child.stderr, /DevTools listening on (ws:\/\/\S+)/, 30000, "chromium (" + chrome + ")");
    child.stderr.resume();
    const ws = new WebSocket(m[1]);
    await new Promise((resolve, reject) => {
      ws.addEventListener("open", resolve, { once: true });
      ws.addEventListener("error", () => reject(new Error("chromium: devtools websocket failed")), { once: true });
    });
    return new Browser(new Connection(ws), kill, { width, height });
  }

  constructor(conn, kill, size) {
    this.conn = conn;
    this.kill = kill;
    this.size = size;
  }

  async newPage() {
    const { targetId } = await this.conn.send("Target.createTarget", { url: "about:blank" });
    const { sessionId } = await this.conn.send("Target.attachToTarget", { targetId, flatten: true });
    const page = new Page(this.conn, sessionId);
    await page.init(this.size);
    return page;
  }

  async close() {
    try {
      await Promise.race([this.conn.send("Browser.close"), sleep(2000)]);
    } catch {
      // the kill below is the guarantee
    }
    this.conn.ws.close();
    this.kill();
  }
}

function describeConsole(params) {
  return params.args.map((a) => (a.value !== undefined ? String(a.value) : a.description ?? a.type)).join(" ");
}

export class Page {
  constructor(conn, sessionId) {
    this.conn = conn;
    this.sessionId = sessionId;
    // Every console error, uncaught exception and error-level log entry
    // since the page opened; checks slice it by index.
    this.errors = [];
    conn.listeners.add((msg) => {
      if (msg.sessionId !== sessionId) return;
      const p = msg.params;
      if (msg.method === "Runtime.exceptionThrown") {
        const d = p.exceptionDetails;
        this.errors.push("exception: " + (d.exception?.description ?? d.text));
      } else if (msg.method === "Runtime.consoleAPICalled" && (p.type === "error" || p.type === "assert")) {
        this.errors.push("console." + p.type + ": " + describeConsole(p));
      } else if (msg.method === "Log.entryAdded" && p.entry.level === "error") {
        this.errors.push("log: " + p.entry.text + (p.entry.url ? " (" + p.entry.url + ")" : ""));
      }
    });
  }

  send(method, params) {
    return this.conn.send(method, params, this.sessionId);
  }

  async init({ width, height }) {
    await this.send("Page.enable");
    await this.send("Runtime.enable");
    await this.send("Log.enable");
    await this.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: false });
  }

  // addInitScript runs source in every document this page loads, before the
  // page's own scripts.
  addInitScript(source) {
    return this.send("Page.addScriptToEvaluateOnNewDocument", { source });
  }

  // navigate loads url and waits for the new document's load event. The
  // marker on the old window tells a stale document apart from the new one
  // (app.js rewrites the query string, so the href can't be compared).
  async navigate(url) {
    await this.evaluate("window.__e2eOld = true");
    const res = await this.send("Page.navigate", { url });
    if (res.errorText) throw new Error("navigate " + url + ": " + res.errorText);
    await this.waitFor("!window.__e2eOld && document.readyState === 'complete'", 15000, "load " + url);
  }

  async evaluate(expr) {
    const res = await this.send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (res.exceptionDetails) {
      const d = res.exceptionDetails;
      throw new Error("evaluate failed: " + (d.exception?.description ?? d.text) + "\n  in: " + expr);
    }
    return res.result.value;
  }

  // waitFor polls expr until it is truthy and returns that value.
  async waitFor(expr, timeoutMs = 10000, what = expr, intervalMs = 50) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const v = await this.evaluate(expr);
      if (v) return v;
      if (Date.now() > deadline) throw new Error("timed out after " + timeoutMs + " ms waiting for: " + what);
      await sleep(intervalMs);
    }
  }

  mouse(type, x, y, { button = "left", buttons = 0, clickCount = 0, modifiers = 0, deltaX = 0, deltaY = 0 } = {}) {
    const params = { type, x, y, modifiers };
    if (type === "mouseWheel") Object.assign(params, { deltaX, deltaY });
    else Object.assign(params, { button: type === "mouseMoved" && buttons === 0 ? "none" : button, buttons, clickCount });
    return this.send("Input.dispatchMouseEvent", params);
  }

  move(x, y, opts) {
    return this.mouse("mouseMoved", x, y, opts);
  }

  async press(x, y, { modifiers = 0 } = {}) {
    await this.move(x, y, { modifiers });
    await this.mouse("mousePressed", x, y, { buttons: 1, clickCount: 1, modifiers });
  }

  release(x, y, { modifiers = 0 } = {}) {
    return this.mouse("mouseReleased", x, y, { buttons: 0, clickCount: 1, modifiers });
  }

  async click(x, y, opts) {
    await this.press(x, y, opts);
    await this.release(x, y, opts);
  }

  async drag(x0, y0, x1, y1, steps = 10) {
    await this.press(x0, y0);
    for (let i = 1; i <= steps; i++) {
      await this.move(x0 + ((x1 - x0) * i) / steps, y0 + ((y1 - y0) * i) / steps, { buttons: 1 });
      await sleep(16);
    }
    await this.release(x1, y1);
  }

  async wheel(x, y, deltaY) {
    await this.move(x, y);
    await this.mouse("mouseWheel", x, y, { deltaY });
  }

  // key sends a keyDown/keyUp pair; spec is { key, code, keyCode, text }.
  async key(spec, modifiers = 0) {
    const base = { key: spec.key, code: spec.code, windowsVirtualKeyCode: spec.keyCode, modifiers };
    await this.send("Input.dispatchKeyEvent", { type: "keyDown", ...base, text: spec.text });
    await this.send("Input.dispatchKeyEvent", { type: "keyUp", ...base });
  }

  keyDown(spec, modifiers = 0) {
    return this.send("Input.dispatchKeyEvent", {
      type: "rawKeyDown", key: spec.key, code: spec.code, windowsVirtualKeyCode: spec.keyCode, modifiers,
    });
  }

  keyUp(spec, modifiers = 0) {
    return this.send("Input.dispatchKeyEvent", {
      type: "keyUp", key: spec.key, code: spec.code, windowsVirtualKeyCode: spec.keyCode, modifiers,
    });
  }

  async screenshot(path) {
    const { data } = await this.send("Page.captureScreenshot", { format: "png" });
    writeFileSync(path, Buffer.from(data, "base64"));
  }
}

export const KEYS = {
  enter: { key: "Enter", code: "Enter", keyCode: 13, text: "\r" },
  shift: { key: "Shift", code: "ShiftLeft", keyCode: 16 },
};
