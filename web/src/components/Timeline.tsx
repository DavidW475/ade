import { useEffect, useLayoutEffect, useMemo, useRef } from "react";
import type { CostPayload, SessionEvent, StatusPayload, ToolCallPayload, ToolResultPayload } from "../api";
import { formatDuration, formatNumber, formatTime, formatUSD } from "../format";

type Item =
  | { kind: "event"; ev: SessionEvent }
  | { kind: "tool"; seq: number; ts: string; call: ToolCallPayload; result?: ToolResultPayload };

/** Combines tool_call and the matching tool_result into one entry. */
function buildItems(events: SessionEvent[]): Item[] {
  const items: Item[] = [];
  const tools = new Map<string, Extract<Item, { kind: "tool" }>>();
  for (const ev of events) {
    if (ev.type === "tool_call") {
      const item = { kind: "tool" as const, seq: ev.seq, ts: ev.ts, call: ev.payload };
      tools.set(ev.payload.id, item);
      items.push(item);
    } else if (ev.type === "tool_result") {
      const item = tools.get(ev.payload.id);
      if (item) item.result = ev.payload;
      else items.push({ kind: "event", ev });
    } else {
      items.push({ kind: "event", ev });
    }
  }
  return items;
}

export function Timeline({ events, live }: { events: SessionEvent[]; live: boolean }) {
  const items = useMemo(() => buildItems(events), [events]);
  const end = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  // Stick to the bottom as long as the user has not scrolled up.
  useEffect(() => {
    const onScroll = () => {
      stick.current = window.innerHeight + window.scrollY >= document.body.scrollHeight - 120;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);
  useLayoutEffect(() => {
    if (stick.current) end.current?.scrollIntoView({ block: "end" });
  }, [items.length]);

  if (items.length === 0) return <p className="muted">{live ? "Waiting for events …" : "No events."}</p>;
  return (
    <>
      <ol className="timeline">
        {items.map((it) =>
          it.kind === "tool" ? <ToolItem key={it.seq} call={it.call} result={it.result} ts={it.ts} /> : <EventItem key={it.ev.seq} ev={it.ev} />,
        )}
      </ol>
      <div ref={end} />
    </>
  );
}

const statusText: Record<StatusPayload["status"], string> = {
  starting: "Starting",
  running: "Agent working",
  idle: "Waiting for an instruction",
  done: "Ended",
  failed: "Failed",
  stopped: "Stopped",
  interrupted: "Interrupted",
  paused: "Paused",
};

function EventItem({ ev }: { ev: SessionEvent }) {
  switch (ev.type) {
    case "message":
      return (
        <li className={`tl-msg ${ev.payload.role}`}>
          <div className="tl-who">
            {ev.payload.role === "user" ? "You" : "Agent"} <time>{formatTime(ev.ts, true)}</time>
          </div>
          <div className="tl-text">{ev.payload.text}</div>
        </li>
      );
    case "thinking":
      return (
        <li className="tl-thinking">
          <details>
            <summary>Thinking</summary>
            <div className="tl-text">{ev.payload.text}</div>
          </details>
        </li>
      );
    case "status": {
      const p = ev.payload;
      if (p.status === "running" && !p.model && !p.reason) return null;
      const text = p.model ? `${statusText[p.status]} · model ${p.model}` : statusText[p.status];
      return (
        <li className={`tl-status ${p.status}`}>
          <span>
            {text}
            {p.reason && <span className="muted"> – {p.reason}</span>}
          </span>
          {p.exit_code !== undefined && p.exit_code !== 0 && <span className="muted"> (Exit-Code {p.exit_code})</span>}
          <time>{formatTime(ev.ts, true)}</time>
        </li>
      );
    }
    case "cost":
      return <CostItem cost={ev.payload} />;
    case "usage":
      // Do not show individual API calls (egress proxy); the sum is in the cost event.
      return null;
    case "error":
      return (
        <li className="tl-error" role="alert">
          <pre>{ev.payload.message}</pre>
        </li>
      );
    case "tool_result":
      // Result without a known call.
      return (
        <li className="tl-tool">
          <details>
            <summary>Result</summary>
            <pre className="tl-output">{ev.payload.output}</pre>
          </details>
        </li>
      );
    default: {
      const unknown = ev as { type: string; payload: unknown };
      return (
        <li className="tl-status">
          <span className="mono">{unknown.type}</span> <code>{JSON.stringify(unknown.payload)}</code>
        </li>
      );
    }
  }
}

function CostItem({ cost: c }: { cost: CostPayload }) {
  const tokens = (c.input_tokens ?? 0) + (c.output_tokens ?? 0) + (c.cache_read_tokens ?? 0) + (c.cache_write_tokens ?? 0);
  const parts = [`Turn ended`, formatUSD(c.turn_usd) + (c.estimated ? " (estimated)" : c.metered ? " (measured)" : "")];
  if (tokens) parts.push(`${formatNumber(tokens)} tokens`);
  if (c.duration_ms) parts.push(formatDuration(c.duration_ms));
  if (c.turns) parts.push(`${c.turns} ${c.turns === 1 ? "step" : "steps"}`);
  return (
    <li className="tl-cost">
      {parts.join(" · ")}{" "}
      <span className="muted">
        (total {formatUSD(c.total_usd)}
        {c.budget_usd ? ` of ${formatUSD(c.budget_usd)}` : ""})
      </span>
    </li>
  );
}

/** Short description of a tool call from its input. */
function toolSummary(call: ToolCallPayload): string {
  const input = (call.input ?? {}) as Record<string, unknown>;
  for (const key of ["command", "file_path", "path", "pattern", "url", "query", "description", "prompt"]) {
    const v = input[key];
    if (typeof v === "string" && v) return v.length > 160 ? `${v.slice(0, 160)} …` : v;
  }
  return "";
}

function ToolItem({ call, result, ts }: { call: ToolCallPayload; result?: ToolResultPayload; ts: string }) {
  const state = !result ? "running" : result.is_error ? "error" : "ok";
  const summary = toolSummary(call);
  return (
    <li className={`tl-tool ${state}${call.parent_id ? " nested" : ""}`}>
      <details>
        <summary>
          <span className="tl-icon" aria-label={state === "running" ? "running" : state === "error" ? "error" : "done"}>
            {state === "running" ? "…" : state === "error" ? "✗" : "✓"}
          </span>
          <strong>{call.tool}</strong>
          {summary && <span className="mono tl-summary">{summary}</span>}
          <time>{formatTime(ts, true)}</time>
        </summary>
        <div className="tl-tooldetail">
          <div className="tl-label">Input</div>
          <pre className="tl-output">{JSON.stringify(call.input ?? {}, null, 2)}</pre>
          {result && (
            <>
              <div className="tl-label">
                Result{result.truncated ? " (truncated)" : ""}
                {result.is_error ? " – error" : ""}
              </div>
              <pre className="tl-output">{result.output || "(leer)"}</pre>
            </>
          )}
        </div>
      </details>
    </li>
  );
}
