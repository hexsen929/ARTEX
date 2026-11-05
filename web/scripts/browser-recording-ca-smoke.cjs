"use strict";
const assert = require("node:assert/strict");
const { X509Certificate, createHash } = require("node:crypto");
const fs = require("node:fs");
const https = require("node:https");
const os = require("node:os");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
if (!process.env.CI || !process.env.ARTEX_PLAYWRIGHT_DIR) throw new Error("Cloud regression only");
const { chromium } = require(path.resolve(process.env.ARTEX_PLAYWRIGHT_DIR));
const root = fs.mkdtempSync(path.join(os.tmpdir(), "recording-ca-"));
function openssl(args) { execFileSync("openssl", args, { cwd: root, stdio: "ignore" }); }
function read(name) { return fs.readFileSync(path.join(root, name)); }
function listen(server) { return new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", () => resolve(`https://127.0.0.1:${server.address().port}`)); }); }
(async () => {
  let browser, trustedServer, unrelatedServer;
  try {
    openssl(["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "ca.pem", "-days", "1", "-subj", "/CN=ARTEX fixture CA", "-addext", "basicConstraints=critical,CA:TRUE"]);
    openssl(["req", "-newkey", "rsa:2048", "-nodes", "-keyout", "leaf.key", "-out", "leaf.csr", "-subj", "/CN=127.0.0.1"]);
    fs.writeFileSync(path.join(root, "leaf.ext"), "subjectAltName=IP:127.0.0.1\nbasicConstraints=CA:FALSE\n");
    openssl(["x509", "-req", "-in", "leaf.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", "leaf.pem", "-days", "1", "-extfile", "leaf.ext"]);
    openssl(["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "other.key", "-out", "other.pem", "-days", "1", "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"]);
    const pin = createHash("sha256").update(new X509Certificate(read("ca.pem")).publicKey.export({ type: "spki", format: "der" })).digest("base64");
    trustedServer = https.createServer({ key: read("leaf.key"), cert: Buffer.concat([read("leaf.pem"), read("ca.pem")]) }, (_, res) => res.end("recording CA verified"));
    unrelatedServer = https.createServer({ key: read("other.key"), cert: read("other.pem") }, (_, res) => res.end("unrelated certificate"));
    const trusted = await listen(trustedServer), unrelated = await listen(unrelatedServer);
    browser = await chromium.launch({ headless: true, args: [`--ignore-certificate-errors-spki-list=${pin}`] });
    const page = await browser.newPage({ ignoreHTTPSErrors: false });
    await page.route("**/*", (route) => [trusted, unrelated].some((origin) => route.request().url().startsWith(origin + "/")) ? route.continue() : route.abort());
    await page.goto(trusted, { timeout: 10000 });
    assert.equal(await page.locator("body").innerText(), "recording CA verified");
    await assert.rejects(() => page.goto(unrelated, { timeout: 10000 }), /ERR_CERT_AUTHORITY_INVALID/);
    console.log("PASS Chromium trusts the recording CA key and rejects an unrelated certificate");
  } finally {
    if (browser) await browser.close();
    for (const server of [trustedServer, unrelatedServer]) if (server) { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)); }
    fs.rmSync(root, { recursive: true, force: true });
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
