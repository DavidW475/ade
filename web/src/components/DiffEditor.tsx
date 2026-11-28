// Monaco Diff Editor for the review view. This file is its own chunk (React.lazy): Monaco
// is large and only needed on the workspace page.
//
// Monaco is bundled locally instead of loaded from the CDN – the dashboard's CSP only allows
// 'self'. Only the editor worker is needed (diff computation); syntax highlighting comes
// from the Monarch grammars that Monaco loads per language.
import * as monaco from "monaco-editor/editor/editor.api";
import "monaco-editor/basic-languages/monaco.contribution";
// Icon font (arrows, folded regions), diff navigation and search (Ctrl+F).
import "monaco-editor/features/codicon/register";
import "monaco-editor/features/diffEditor/register";
import "monaco-editor/features/find/register";
import EditorWorker from "monaco-editor/editor/editor.worker?worker";
import { useEffect, useRef } from "react";

self.MonacoEnvironment = { getWorker: () => new EditorWorker() };

const dark = window.matchMedia("(prefers-color-scheme: dark)");

export interface DiffEditorProps {
  /** Path of the file; determines the language. */
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
    // Unique URIs: Monaco detects the language by the extension, models must not
    // overlap.
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
