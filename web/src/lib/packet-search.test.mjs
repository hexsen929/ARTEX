import assert from "node:assert/strict";
import test from "node:test";
import { countPacketMatches } from "./packet-search.ts";

test("packet search returns a numeric count at EOF and with a short suffix", () => {
  assert.equal(countPacketMatches("MATCH", "MATCH"), 1);
  assert.equal(countPacketMatches("prefix-MATCH", "MATCH"), 1);
  assert.equal(countPacketMatches("prefix-MATCH-x", "MATCH"), 1);
  assert.equal(countPacketMatches("MATCH-MATCH", "MATCH"), 2);
});

test("packet search returns zero for empty, absent and longer queries", () => {
  assert.equal(countPacketMatches("", "MATCH"), 0);
  assert.equal(countPacketMatches("abc", "abcdef"), 0);
  assert.equal(countPacketMatches("HTTP/1.1 200 OK", "absent"), 0);
  assert.equal(countPacketMatches("anything", ""), 0);
  assert.equal(countPacketMatches("anything", "   "), 0);
});

test("packet search is literal, trimmed, case-insensitive and non-overlapping", () => {
  assert.equal(countPacketMatches("marker MARKER Marker", " marker "), 3);
  assert.equal(countPacketMatches(".*[x](a) regex-not-a-match .*[x](a)", ".*[x](a)"), 2);
  assert.equal(countPacketMatches("aaaaa", "aa"), 2);
  assert.equal(countPacketMatches("漏洞依据：漏洞", "漏洞"), 2);
});
