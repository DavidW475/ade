import { useEffect, useRef, useState } from "react";
import { wsURL, type SessionEvent } from "../api";

export type StreamState = "connecting" | "live" | "reconnecting";

/**
 * Subscribes to the event stream of a session (WS /ws/sessions/{id}/events).
 * On the first connection the complete history arrives, then live events. If the connection
 * breaks (daemon restart, network, client too slow), it reconnects with ?since=<last seq> –
 * so nothing is lost and nothing arrives twice.
 *
 * For another session, remount the component (key={sessionId}).
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

    // During replay many events arrive at once: take them over into the state in batches.
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

  // Update the callback without reconnecting.
  useEffect(() => {
    onEventRef.current = onEvent;
  });

  return { events, state };
}
