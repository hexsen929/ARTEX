import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(
  new URL("../app/(main)/function/findings/_components/findings-table.tsx", import.meta.url),
  "utf8",
);
const tree = ts.createSourceFile("findings-table.tsx", source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const elements = [];
function visit(node) {
  if (ts.isJsxElement(node)) elements.push(node);
  ts.forEachChild(node, visit);
}
visit(tree);
const tag = (node) => (ts.isJsxElement(node) ? node.openingElement : node).tagName?.getText(tree);
const children = (node, name) =>
  node.children.filter((n) => (ts.isJsxElement(n) || ts.isJsxSelfClosingElement(n)) && tag(n) === name);
const rows = elements.filter((n) => tag(n) === "TableRow");

// Static structure complements Safari visual acceptance: fixed table layouts do
// not reserve room for badges added as extra inline children in a narrow cell.
test("every finding row and folder has a dedicated verification column", () => {
  const bodyRows = rows.map((row) => children(row, "TableCell")).filter((cells) => cells.length > 1);
  assert.equal(bodyRows.length, 2, "normal finding and merged-folder row");
  for (const cells of bodyRows) assert.equal(cells.length, 10);
  const finding = bodyRows.find((cells) =>
    cells.some((cell) => cell.getText(tree).includes("FindingVerificationBadge")),
  );
  assert.ok(finding[2].getText(tree).includes('domain="severity"'));
  assert.ok(!finding[2].getText(tree).includes("FindingVerificationBadge"));
  assert.ok(finding[4].getText(tree).includes("FindingVerificationBadge"));
  assert.ok(!finding[4].getText(tree).includes('domain="severity"'));
});

test("headers explicitly separate verification from processing status", () => {
  const heads = rows.map((row) => children(row, "TableHead")).find((cells) => cells.length);
  assert.equal(heads.length, 10);
  assert.match(heads[4].getText(tree), /验证状态/);
  assert.match(heads[6].getText(tree), /处理状态/);
});

test("nested and top-level tables share the same ten-column geometry", () => {
  const table = elements.find((n) => tag(n) === "Table");
  const groups = children(table, "colgroup");
  assert.equal(groups.length, 1, "colgroup must not depend on hideHeader");
  assert.equal(children(groups[0], "col").length, 10);
  assert.match(table.openingElement.getText(tree), /min-w-\[76rem\]/);
});

test("expanded details, folder members and empty rows span all ten columns", () => {
  assert.match(source, /const COLUMN_COUNT = 10;/);
  const spans = elements.filter(
    (node) => tag(node) === "TableCell" && node.openingElement.getText(tree).includes("colSpan"),
  );
  assert.equal(spans.length, 3);
  for (const cell of spans) assert.match(cell.openingElement.getText(tree), /colSpan=\{COLUMN_COUNT\}/);
});

test("merged folders do not imply all members are confirmed", () => {
  const folder = rows
    .map((row) => children(row, "TableCell"))
    .find((cells) => cells.some((cell) => cell.getText(tree).includes("FindingSeverityCounts")));
  assert.match(folder[4].getText(tree), /查看成员/);
});
