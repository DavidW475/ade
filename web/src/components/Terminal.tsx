import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { wsURL } from "../api";

type TermState = "connecting" | "live" | "ended" | "reconnecting";

/**
 * Terminal einer pty-Session über WS /ws/sessions/{id}/pty.
 * Binärnachrichten sind Terminal-Daten, Textnachrichten Steuerbefehle (resize, exit).
 * Ein neu verbundener Client bekommt zuerst die letzte Ausgabe.
 */
export function Terminal({ sessionId, onExit }: { sessionId: string; onExit?: () => void }) {
  const box = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<TermState>("connecting");
  const onExitRef = useRef(onExit);
  useEffect(() => {
    onExitRef.current = onExit;
  });

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    const term = new XTerm({
      cursorBlink: true,
      fontFamily: 'ui-monospace, "Cascadia Code", "SF Mono", Menlo, Consolas, monospace',
      fontSize: 13,
      scrollback: 10_000,
      theme: dark ? { background: "#0b0d11", foreground: "#e6e8ec" } : { background: "#1a1d23", foreground: "#e6e8ec" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el);
    fit.fit();

    let ws: WebSocket | undefined;
    let stopped = false;
    let ended = false;
    let retry: number | undefined;
    const enc = new TextEncoder();

    const sendResize = () => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };

    const connect = () => {
      ws = new WebSocket(wsURL(`/ws/sessions/${encodeURIComponent(sessionId)}/pty`));
      ws.binaryType = "arraybuffer";
      let first = true;
      ws.onopen = () => {
        setState("live");
        sendResize();
      };
      ws.onmessage = (msg) => {
        if (typeof msg.data === "string") {
          const ctl = JSON.parse(msg.data) as { type: string };
          if (ctl.type === "exit") {
            ended = true;
            setState("ended");
            onExitRef.current?.();
          }
          return;
        }
        if (first) {
          // Replay nach einer Neuverbindung: alten Inhalt ersetzen statt doppeln.
          term.reset();
          first = false;
        }
        term.write(new Uint8Array(msg.data as ArrayBuffer));
      };
      ws.onclose = () => {
        if (stopped || ended) return;
        setState("reconnecting");
        retry = window.setTimeout(connect, 1000);
      };
    };
    connect();

    const input = term.onData((d) => {
      if (ws?.readyState === WebSocket.OPEN && !ended) ws.send(enc.encode(d));
    });
    const resize = term.onResize(sendResize);
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(el);
    term.focus();

    return () => {
      stopped = true;
      window.clearTimeout(retry);
      ro.disconnect();
      input.dispose();
      resize.dispose();
      ws?.close();
      term.dispose();
    };
  }, [sessionId]);

  return (
    <div className="terminal">
      <div className="termbar">
        <span className={`dot ${state === "live" ? "ok" : state === "ended" ? "" : "warn"}`} aria-hidden />
        {state === "live" ? "verbunden" : state === "ended" ? "Prozess beendet" : state === "reconnecting" ? "verbinde neu …" : "verbinde …"}
      </div>
      <div className="termbox" ref={box} />
    </div>
  );
}
