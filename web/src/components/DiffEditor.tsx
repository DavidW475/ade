// Monaco Diff Editor für die Review-Ansicht. Diese Datei ist ein eigener Chunk (React.lazy):
// Monaco ist groß und wird nur auf der Workspace-Seite gebraucht.
//
// Monaco wird lokal gebündelt statt vom CDN geladen – die CSP des Dashboards erlaubt nur
// 'self'. Nur der Editor-Worker wird gebraucht (Diff-Berechnung); Syntaxhervorhebung kommt
// von den Monarch-Grammatiken, die Monaco je Sprache nachlädt.
import * as monaco from "monaco-editor/editor/editor.api";
import "monaco-editor/basic-languages/monaco.contribution";
// Icon-Schrift (Pfeile, eingeklappte Bereiche), Diff-Navigation und Suche (Strg+F).
import "monaco-editor/features/codicon/register";
import "monaco-editor/features/diffEditor/register";
import "monaco-editor/features/find/register";
import EditorWorker from "monaco-editor/editor/editor.worker?worker";
import { useEffect, useRef } from "react";

self.MonacoEnvironment = { getWorker: () => new EditorWorker() };

const dark = window.matchMedia("(prefers-color-scheme: dark)");

export interface DiffEditorProps {
  /** Pfad der Datei; bestimmt die Sprache. */
  path: string;
  original: string;
  modified: string;
  sideBySide: boolean;
}

export default function DiffEditor({ path, original, modified, sideBySide }: DiffEditorProps) {
  const host = useRef<HTMLDivElement>(null);
  const editor = useRef<monaco.editor.IStandaloneDiffEditor | null>(null);

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const e = monaco.editor.createDiffEditor(el, {
      readOnly: true,
      originalEditable: false,
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      renderOverviewRuler: true,
      hideUnchangedRegions: { enabled: true, contextLineCount: 3, minimumLineCount: 5, revealLineCount: 20 },
      useInlineViewWhenSpaceIsLimited: true,
      fontSize: 13,
      theme: dark.matches ? "vs-dark" : "vs",
    });
    editor.current = e;
    const onScheme = () => monaco.editor.setTheme(dark.matches ? "vs-dark" : "vs");
    dark.addEventListener("change", onScheme);
    return () => {
      dark.removeEventListener("change", onScheme);
      const model = e.getModel();
      e.dispose();
      model?.original.dispose();
      model?.modified.dispose();
      editor.current = null;
    };
  }, []);

  useEffect(() => {
    const e = editor.current;
    if (!e) return;
    // Eindeutige URIs: Monaco erkennt die Sprache an der Endung, Modelle dürfen sich nicht
    // überschneiden.
    const stamp = Date.now().toString(36) + Math.random().toString(36).slice(2);
    const uri = (side: string) => monaco.Uri.from({ scheme: "ade", path: `/${side}/${stamp}/${path}` });
    const next = {
      original: monaco.editor.createModel(original, undefined, uri("original")),
      modified: monaco.editor.createModel(modified, undefined, uri("modified")),
    };
    const prev = e.getModel();
    e.setModel(next);
    prev?.original.dispose();
    prev?.modified.dispose();
  }, [path, original, modified]);

  useEffect(() => {
    editor.current?.updateOptions({ renderSideBySide: sideBySide });
  }, [sideBySide]);

  return <div ref={host} className="diffeditor" />;
}
