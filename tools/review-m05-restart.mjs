// SC-M05-QA-002: verify acceptance criterion #2 (quit completely and re-enter -> identical state)
// against a REAL restart of the server process, using the real on-disk save file.
//
// usage: node tools/review-m05-restart.mjs <captured.json> <baseUrl>
// The caller is responsible for killing/restarting the server between the capture run and this one.
import { readFileSync } from "node:fs";

const capturedFile = process.argv[2];
const BASE = process.argv[3] || "http://127.0.0.1:18091";
const TOKEN = process.env.MALL_DEV_TOKEN;
const committed = JSON.parse(readFileSync(capturedFile, "utf8")).committed;

const res = await fetch(BASE + "/api/v1/mall", {
    headers: { Authorization: `Bearer ${TOKEN}` },
});
const after = await res.json();

const keys = [...new Set([...Object.keys(committed), ...Object.keys(after)])];
const diffs = [];
for (const k of keys) {
    // every field is compared, including revision and lastObservedAt
    const a = JSON.stringify(committed[k]);
    const b = JSON.stringify(after[k]);
    if (a !== b) diffs.push(`${k}: before=${a} after=${b}`);
}
console.log(`[restart] HTTP ${res.status}`);
console.log(`[restart] before(exit) coins=${committed.coins} served=${committed.visitorsServed} rev=${committed.revision}`);
console.log(`[restart] after(re-enter) coins=${after.coins} served=${after.visitorsServed} rev=${after.revision}`);
console.log(`[restart] prepared: ${committed.shops.map((s) => s.id + ":" + s.prepared).join(" ")}`);
console.log(`[restart] prepared: ${after.shops.map((s) => s.id + ":" + s.prepared).join(" ")}`);
if (diffs.length === 0) {
    console.log("PASS | criterion #2 all fields identical after a real process restart (except lastObservedAt)");
} else {
    console.log("FAIL | field drift after restart:");
    for (const d of diffs) console.log("   " + d);
    process.exitCode = 1;
}
