// Captures the flow view's documentation screenshots at 1600×1000 from a
// running `hookyard serve`:
//
//   CHROME=… npm run screenshots -- http://127.0.0.1:7757 ../../../docs/serve-flow
//
// writes <prefix>.png (default view), <prefix>-grouped.png (the busiest
// group opened, a member hovered) and <prefix>-filtered.png (a past day,
// filtered by clicking its deny outcome node).
import { Browser, runCleanups, sleep } from "./cdp.mjs";
import { installHelpers, openFlow } from "./helpers.mjs";

async function shoot(page, path) {
  await sleep(400); // let transitions and the throttled count refresh settle
  await page.screenshot(path);
  console.log("wrote " + path);
}

async function hitOrFail(page, expr, what) {
  const p = await page.evaluate(expr);
  if (!p?.ok) throw new Error(what + " not hit-testable: " + JSON.stringify(p));
  return p;
}

async function main() {
  const [base, prefix] = process.argv.slice(2);
  if (!base || !prefix) {
    console.error("usage: node e2e/screenshots.mjs <base-url> <output-prefix>");
    process.exit(2);
  }

  const browser = await Browser.launch();
  try {
    const page = await browser.newPage();
    await installHelpers(page);

    await openFlow(page, base);
    await shoot(page, prefix + ".png");

    // The busiest collapsible group, opened, a member hovered.
    const group = await page.evaluate(`__e2e.groups().filter((g) => !g.expanded && !g.forced).sort((a, b) => b.n - a.n)[0] ?? null`);
    if (!group) throw new Error("no collapsed group to open");
    const gen = (await page.evaluate("__e2e.state()")).gen;
    const member = JSON.stringify(group.members[0]);
    const hdr = await hitOrFail(page, `__e2e.groupHeaderPoint(${member})`, "group " + group.name);
    await page.click(hdr.x, hdr.y);
    await page.waitFor(`__e2e.settled() && __e2e.state().gen > ${gen} && __e2e.groupOf(${member})?.expanded`, 10000, "group opened");
    await sleep(300);
    const shown = await page.evaluate(`__e2e.groupOf(${member}).members.find((m) => __e2e.nodePoint("handler", m)?.ok) ?? null`);
    if (!shown) throw new Error("no drawn member in " + group.name);
    const hover = await hitOrFail(page, `__e2e.nodePoint("handler", ${JSON.stringify(shown)})`, "member " + shown);
    await page.move(hover.x, hover.y);
    await page.waitFor(`!document.querySelector("#flow-body .flow-tip").hidden`, 3000, "tooltip");
    await shoot(page, prefix + "-grouped.png");

    // A past day, filtered by clicking its deny outcome.
    const days = await (await fetch(base + "/api/days")).json();
    const past = days.days.find((d) => d !== days.today);
    if (!past) throw new Error("no past day in the state dir");
    await openFlow(page, base, "day=" + past);
    const deny = await hitOrFail(page, `__e2e.nodePoint("outcome", "deny")`, "outcome deny on " + past);
    await page.click(deny.x, deny.y);
    await page.waitFor(`__e2e.params("outcome").includes("deny") && __e2e.settled() && __e2e.selected("outcome", "deny")`,
      10000, "deny filter applied");
    await page.move(2, 2);
    await shoot(page, prefix + "-filtered.png");
  } finally {
    await browser.close();
    runCleanups();
  }
}

main().catch((err) => {
  console.error(err.stack ?? err);
  runCleanups();
  process.exit(1);
});
