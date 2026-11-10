// Browser smoke test of the web panel (Playwright, Chromium).
//   node tests/e2e/panel.mjs <base-url> <token> <backup-id> <screenshot-dir>
// Exits non-zero on any failed expectation.
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
let playwright;
try {
  playwright = require("playwright");
} catch {
  playwright = await import("playwright");
}
const { chromium } = playwright;

const [base, token, backupId, shots] = process.argv.slice(2);
const fail = (m) => {
  console.error("FAIL:", m);
  process.exit(1);
};

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
const consoleErrors = [];
page.on("console", (m) => m.type() === "error" && consoleErrors.push(m.text()));
page.on("pageerror", (e) => consoleErrors.push(String(e)));

await page.goto(base + "/");
await page.getByLabel("Admin token").waitFor();

// wrong token
await page.getByLabel("Admin token").fill("not-the-right-token-at-all-xx");
await page.getByRole("button", { name: "Sign in" }).click();
await page.getByRole("alert").waitFor();
if (!/invalid token/i.test(await page.getByRole("alert").innerText())) fail("no invalid-token error");

// right token
await page.getByLabel("Admin token").fill(token);
await page.getByRole("button", { name: "Sign in" }).click();
await page.getByRole("heading", { name: "Overview" }).waitFor();
await page.getByText("Protected workloads").waitFor();
await page.getByText("Last verified recovery").waitFor();
if (shots) await page.screenshot({ path: shots + "/overview.png", fullPage: true });

// backups list -> detail
await page.getByRole("link", { name: "Backups & recovery tests" }).click();
await page.getByRole("link", { name: backupId }).click();
await page.getByRole("heading", { name: backupId }).waitFor();
await page.getByRole("button", { name: /Verify now|Verification running/ }).waitFor();
await page.getByText("Recovery report").waitFor({ timeout: 15000 });
await page.getByText("Not verified").waitFor();
const finalBadge = await page.locator(".report-head .badge").innerText();
if (!/VERIFIED/.test(finalBadge)) fail("report badge is " + finalBadge);
if ((await page.locator(".pipeline:not(.compact) .stage.done").count()) !== 4) fail("pipeline does not show 4 completed stages");
if (shots) await page.screenshot({ path: shots + "/backup.png", fullPage: true });

// activity + settings
await page.getByRole("link", { name: "Activity log" }).click();
await page.getByRole("heading", { name: "Activity log" }).waitFor();
await page.getByText("ui.login").first().waitFor();
await page.getByRole("link", { name: "Settings" }).click();
await page.getByText("Data directory").waitFor();

// sign out -> back to login, API locked
await page.getByRole("button", { name: "Sign out" }).click();
await page.getByLabel("Admin token").waitFor();
const status = await page.evaluate(async () => (await fetch("/api/overview")).status);
if (status !== 401) fail("API reachable after sign out: " + status);

// phone width renders without horizontal scroll
await page.setViewportSize({ width: 390, height: 800 });
const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
if (overflow) fail("horizontal overflow on login page at phone width");

if (consoleErrors.some((e) => !/401|Unauthorized/.test(e))) fail("console errors: " + consoleErrors.join(" | "));
await browser.close();
console.log("PANEL E2E: PASS");
