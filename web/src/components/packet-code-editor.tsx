"use client";

import * as React from "react";
import { Annotation, Compartment, EditorState, Transaction } from "@codemirror/state";
import { Decoration, drawSelection, EditorView, highlightActiveLine, keymap, lineNumbers, MatchDecorator, ViewPlugin, type DecorationSet, type ViewUpdate } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { defaultHighlightStyle, HighlightStyle, StreamLanguage, syntaxHighlighting } from "@codemirror/language";
import { tags } from "@lezer/highlight";

const externalValue = Annotation.define<boolean>();
const packetHighlight = HighlightStyle.define([
  { tag: tags.keyword, color: "#d95656", fontWeight: "700" },
  { tag: tags.string, color: "#0284c7" },
  { tag: tags.variableName, color: "#286dcc", fontWeight: "650" },
  { tag: tags.typeName, color: "#6b7280" },
  { tag: tags.number, color: "#c026d3" },
  { tag: tags.bool, color: "#e11d48" },
]);
const httpLanguage = StreamLanguage.define({
  token(stream) {
    if (stream.match(/^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|CONNECT|TRACE)\b/)) return "keyword";
    if (stream.match(/^HTTP\/\d(?:\.\d)?\b/)) return "typeName";
    if (stream.match(/^[A-Za-z][A-Za-z0-9-]*(?=:)/)) return "variableName";
    if (stream.match(/^https?:\/\/\S+/)) return "string";
    if (stream.match(/^(?:\d{3})(?:\s+[^\n]*)?/)) return "number";
    if (stream.match(/^(?:true|false|null)\b/)) return "bool";
    stream.next();
    return null;
  },
});
const packetTheme = EditorView.theme({
  "&": { height: "100%", maxHeight: "inherit", backgroundColor: "transparent", color: "var(--foreground)" },
  ".cm-scroller": { overflow: "auto", maxHeight: "inherit", fontFamily: "var(--font-mono)", fontSize: "0.75rem", fontWeight: "500", lineHeight: "1.65rem" },
  ".cm-content": { minHeight: "100%", padding: "0.5rem" },
  ".cm-gutters": { backgroundColor: "transparent", color: "var(--muted-foreground)", border: "0", borderRight: "1px solid var(--border)" },
  ".cm-lineNumbers .cm-gutterElement": { minWidth: "2rem", padding: "0 0.5rem" },
  ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "transparent" },
  ".cm-selectionBackground, ::selection": { backgroundColor: "#c9d2e3" },
  ".cm-cursor": { borderLeftColor: "var(--foreground)" },
  ".cm-searchMatch": { backgroundColor: "#fde047", color: "#111827", outline: "1px solid #eab308" },
});

// The built-in search decoration follows the built-in search panel. This
// viewer has its own search input, so render its matches independently.
function queryHighlight(query: string) {
  const needle = query.trim();
  if (!needle) return [];
  const escaped = needle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const matcher = new MatchDecorator({
    regexp: new RegExp(escaped, "gi"),
    decoration: Decoration.mark({ class: "cm-searchMatch" }),
  });
  return ViewPlugin.fromClass(class {
    decorations: DecorationSet;
    constructor(view: EditorView) { this.decorations = matcher.createDeco(view); }
    update(update: ViewUpdate) { this.decorations = matcher.updateDeco(update, this.decorations); }
  }, { decorations: (instance) => instance.decorations });
}

export function PacketCodeEditor({
  value, onChange, readOnly = false, wrapLines = true, ariaLabel = "HTTP 报文代码", searchQuery = "",
}: {
  value: string;
  onChange?: (value: string) => void;
  readOnly?: boolean;
  wrapLines?: boolean;
  ariaLabel?: string;
  searchQuery?: string;
}) {
  const host = React.useRef<HTMLDivElement>(null);
  const viewRef = React.useRef<EditorView | null>(null);
  const onChangeRef = React.useRef(onChange);
  onChangeRef.current = onChange;
  const initial = React.useRef({ value, readOnly, wrapLines, ariaLabel });
  const compartments = React.useMemo(() => ({ wrapping: new Compartment(), access: new Compartment(), label: new Compartment(), matches: new Compartment() }), []);

  React.useEffect(() => {
    if (!host.current) return;
    const first = initial.current;
    const state = EditorState.create({
      doc: first.value,
      extensions: [
        lineNumbers(), drawSelection(), highlightActiveLine(), history(),
        keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
        httpLanguage, syntaxHighlighting(defaultHighlightStyle), syntaxHighlighting(packetHighlight), packetTheme,
        compartments.matches.of([]),
        compartments.wrapping.of(first.wrapLines ? EditorView.lineWrapping : []),
        compartments.access.of([EditorView.editable.of(!first.readOnly), EditorState.readOnly.of(first.readOnly)]),
        compartments.label.of(EditorView.contentAttributes.of({ role: "textbox", "aria-label": first.ariaLabel, "aria-multiline": "true", "aria-readonly": String(first.readOnly) })),
        EditorView.updateListener.of((update) => {
          if (update.docChanged && !update.transactions.some((item) => item.annotation(externalValue))) onChangeRef.current?.(update.state.doc.toString());
        }),
      ],
    });
    const view = new EditorView({ state, parent: host.current });
    viewRef.current = view;
    return () => { viewRef.current = null; view.destroy(); };
  }, [compartments]);

  // Reconfigure in place: toggling wrapping/read-only must preserve undo,
  // selection and scroll instead of destroying the editing session.
  React.useEffect(() => {
    viewRef.current?.dispatch({ effects: [
      compartments.wrapping.reconfigure(wrapLines ? EditorView.lineWrapping : []),
      compartments.access.reconfigure([EditorView.editable.of(!readOnly), EditorState.readOnly.of(readOnly)]),
      compartments.label.reconfigure(EditorView.contentAttributes.of({ role: "textbox", "aria-label": ariaLabel, "aria-multiline": "true", "aria-readonly": String(readOnly) })),
    ] });
  }, [ariaLabel, compartments, readOnly, wrapLines]);
  React.useEffect(() => {
    const view = viewRef.current;
    if (!view || view.state.doc.toString() === value) return;
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value }, annotations: [externalValue.of(true), Transaction.addToHistory.of(false)] });
  }, [value]);
  React.useEffect(() => {
    viewRef.current?.dispatch({ effects: compartments.matches.reconfigure(queryHighlight(searchQuery)) });
  }, [compartments, searchQuery]);
  return <div ref={host} className="h-full min-h-0 w-full max-h-[inherit]" />;
}
