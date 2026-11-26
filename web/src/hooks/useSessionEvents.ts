import { useEffect, useRef, useState } from "react";
import { wsURL, type SessionEvent } from "../api";

export type StreamState = "connecting" | "live" | "reconnecting";

/**
 * Abonniert den Event-Stream einer Session (WS /ws/sessions/{id}/events).
 * Beim ersten Verbinden kommt die komplette Historie, danach live. Bricht die Verbindung ab
 * (Daemon-Neustart, Netz, Client zu langsam), wird mit ?since=<letzte seq> neu verbunden –
 * so geht nichts verloren und nichts kommt doppelt.
 *
 * Für eine andere Session die Komponente neu einhängen (key={sessionId}).
 */
export function useSessionEvents(sessionId: string, onEvent?: (e: SessionEvent) => void) {
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [state, setState] = useState<StreamState>("connecting");
  const onEventRef = useRef(onEvent);

  useEffect(() => {
    let last = 0;
    let ws: WebSocket | undefined;
    let stopped = false;
    let attempt = 0;
    let retryTimer: number | undefined;
    let flushTimer: number | undefined;
    let pending: SessionEvent[] = [];

    // Beim Replay kommen viele Events auf einmal: gebündelt in den State übernehmen.
    const flush = () => {
      flushTimer = undefined;
      const batch = pending;
      pending = [];
      if (batch.length) setEvents((prev) => prev.concat(batch));
    };

    const connect = () => {
      ws = new WebSocket(wsURL(`/ws/sessions/${encodeURIComponent(sessionId)}/events?since=${last}`));
      ws.onopen = () => {
        attempt = 0;
        setState("live");
      };
      ws.onmessage = (msg) => {
        const ev = JSON.parse(msg.data as string) as SessionEvent;
        if (ev.seq <= last) return;
        last = ev.seq;
        pending.push(ev);
        onEventRef.current?.(ev);
        flushTimer ??= window.setTimeout(flush, 50);
      };
      ws.onclose = () => {
        if (stopped) return;
        setState("reconnecting");
        const delay = Math.min(500 * 2 ** attempt, 10_000);
        attempt++;
        retryTimer = window.setTimeout(connect, delay);
      };
    };
    connect();

    return () => {
      stopped = true;
      window.clearTimeout(retryTimer);
      window.clearTimeout(flushTimer);
      ws?.close();
    };
  }, [sessionId]);

  // Callback ohne Neuverbinden aktualisieren.
  useEffect(() => {
    onEventRef.current = onEvent;
  });

  return { events, state };
}
