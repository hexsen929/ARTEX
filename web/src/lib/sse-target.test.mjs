import assert from "node:assert/strict";
import test from "node:test";

import { sseBaseURL, sseTicketURL, sseWithCredentials } from "./sse-target.ts";

test("production SSE stays same-origin behind a reverse proxy", () => {
  assert.equal(sseBaseURL(undefined, true, { protocol: "https:", hostname: "fixture.invalid" }), "");
  assert.equal(sseBaseURL(undefined, true), "");
});

test("development SSE bypasses buffering unless an explicit base is supplied", () => {
  const local = { protocol: "http:", hostname: "localhost" };
  assert.equal(sseBaseURL(undefined, false, local), "http://localhost:8787");
  assert.equal(sseBaseURL("", false, local), "");
  assert.equal(sseBaseURL("https://streams.fixture.invalid", true, local), "https://streams.fixture.invalid");
});

test("SSE URLs carry one-use tickets and preserve stream cursors", () => {
  assert.equal(sseTicketURL("", "/api/logs/stream?since=7", "fixture ticket"), "/api/logs/stream?since=7&ticket=fixture%20ticket");
  assert.equal(sseTicketURL("", "/api/update/stream", "fixture"), "/api/update/stream?ticket=fixture");
  assert.throws(() => sseTicketURL("", "/api/logs/stream", ""), /ticket/);
});

test("local cross-port streams keep gate cookies without credentialing remote hosts", () => {
  assert.equal(sseWithCredentials("http://localhost:8787/api/logs/stream", "http://localhost:3000/logs"), true);
  assert.equal(sseWithCredentials("http://127.0.0.1:8787/api/logs/stream", "http://localhost:3000/logs"), false);
  assert.equal(sseWithCredentials("http://[::1]:8787/api/logs/stream", "http://[::1]:3000/logs"), true);
  assert.equal(sseWithCredentials("https://streams.fixture.invalid/api/logs/stream", "https://app.fixture.invalid/logs"), false);
});
