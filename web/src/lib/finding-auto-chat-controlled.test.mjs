import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(new URL("../components/finding-auto-chat.tsx", import.meta.url), "utf8");

test("finding workspace adapts React dispatch instead of narrowing it to a boolean callback", () => {
  assert.match(source, /const setWorkspaceOpen = React\.useCallback\(\(update: React\.SetStateAction<boolean>\)/);
  assert.match(source, /typeof update === "function" \? update\(openValue\.current\) : update/);
  assert.match(source, /setOpen: setWorkspaceOpen/);
  assert.doesNotMatch(source, /setOpen: onOpenChange/);
});

test("controlled and hook open state are synchronized without adding an effect loop", () => {
  assert.match(source, /openValue\.current = open/);
  assert.match(source, /openValue\.current = next;\s*side\.setOpen\(next && !!parent\);\s*onOpenChange\(next\)/);
  assert.match(source, /\[onOpenChange, parent, side\.setOpen\]/);
  assert.match(source, /side\.setOpen\(open && !!parent\);\s*\}, \[open, parent, side\.setOpen\]\)/);
  assert.doesNotMatch(source, /side\.setOpen\(\s*(?:\([^)]*\)|\w+)\s*=>/);
});
