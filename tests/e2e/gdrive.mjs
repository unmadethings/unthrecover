// Clicks "Connect Google Drive" in Chromium. The fake Google consent page
// redirects straight back (as if the user clicked "Allow").
//   node tests/e2e/gdrive.mjs <base-url> <token>
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
let playwright;
try {
  playwright = require("playwright"); // CommonJS resolution honours NODE_PATH (CI)
} catch {
  playwright = await import("playwright");
}
const { chromium } = playwright;

const [base, token] = process.argv.slice(2);
const browser = await chromium.launch();
const page = await browser.newPage();
const errors = [];
page.on("pageerror", (e) => errors.push(String(e)));
try {
  await page.goto(base + "/");
  await page.getByLabel("Admin token").fill(token);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByRole("heading", { name: "Overview" }).waitFor();
  await page.getByRole("link", { name: "Settings" }).click();
  await page.getByRole("heading", { name: "Connections" }).waitFor();
  await page.getByRole("button", { name: "Connect Google Drive" }).click();
  await page.getByText(/Google Drive connected as "gdrive-\d+"/).waitFor({ timeout: 15000 });
  await page.getByRole("cell", { name: "user@example.com" }).first().waitFor();
  if (!page.url().endsWith("#/settings")) throw new Error("result not cleared from URL: " + page.url());
  if (errors.length) throw new Error("page errors: " + errors.join("; "));
  console.log("GDRIVE E2E: PASS");
} catch (e) {
  console.log("GDRIVE E2E: FAIL " + e);
  process.exitCode = 1;
} finally {
  await browser.close();
}
