import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState, type FormEvent, type KeyboardEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { api, isActive, type Session, type SessionEvent } from "../api";
import { sessionTitle } from "../sessions";
import { SessionApprovals } from "../components/approvals";
import { IdeButton } from "../components/ide";
import { SandboxBadge } from "../components/sandbox";
import { Terminal } from "../components/Terminal";
import { Timeline } from "../components/Timeline";
import { Notice, SessionStatusBadge } from "../components/ui";
import { errorMessage, formatTime, formatTokens, formatUSD } from "../format";
import { useSessionEvents } from "../hooks/useSessionEvents";

export function SessionPage() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const session = useQuery({
    queryKey: ["session", id],
    queryFn: () => api.session(id),
    refetchInterval: (q) => (q.state.data && isActive(q.state.data.status) ? 5000 : false),
  });
  const project = useQuery({
    queryKey: ["project", session.data?.project_id],
    queryFn: () => api.project(session.data!.project_id),
    enabled: !!session.data,
  });
  const workspace = useQuery({
    queryKey: ["workspace", session.data?.workspace_id],
    queryFn: () => api.workspace(session.data!.workspace_id),
    enabled: !!session.data,
  });

  if (session.isPending) return <p className="muted">Loading session …</p>;
  if (session.isError) return <Notice tone="bad">{errorMessage(session.error)}</Notice>;
  const s = session.data;
  const refresh = () => void qc.invalidateQueries({ queryKey: ["session", id] });

  return (
    <>
      <div className="crumbs">
        <Link to="/">Projects</Link> / <Link to={`/projects/${s.project_id}`}>{project.data?.name ?? s.project_id}</Link> /{" "}
        <Link to={`/workspaces/${s.workspace_id}`} className="mono">
          {s.workspace_id}
        </Link>{" "}
        / {s.id} {workspace.data && <SandboxBadge workspace={workspace.data} />}
      </div>
      <SessionHeader session={s} />
      {s.live && <SessionApprovals sessionId={s.id} />}
      {s.capabilities.terminal ? <Terminal key={s.id} sessionId={s.id} onExit={refresh} /> : <AgentView key={s.id} session={s} />}
    </>
  );
}

function SessionHeader({ session: s }: { session: Session }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const stop = useMutation({
    mutationFn: () => api.stopSession(s.id),
    onSuccess: (v) => {
      qc.setQueryData(["session", s.id], v);
      void qc.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
  const shell = useMutation({
    mutationFn: () => api.startAgent({ project_id: s.project_id, workspace_id: s.workspace_id, adapter: "pty" }),
    onSuccess: (v) => void navigate(`/sessions/${v.id}`),
  });
  return (
    <>
      <div className="pagehead">
        <h1 className="sessiontitle">{sessionTitle(s)}</h1>
        <div className="actions">
          <IdeButton workspaceId={s.workspace_id} />
          {!s.capabilities.terminal && (
            <button type="button" onClick={() => shell.mutate()} disabled={shell.isPending} title="New terminal in the same workspace">
              Terminal in the workspace
            </button>
          )}
          {s.live && (
            <button type="button" className="danger" onClick={() => stop.mutate()} disabled={stop.isPending}>
              {stop.isPending ? "Stopping …" : "Stop"}
            </button>
          )}
        </div>
      </div>
      {stop.isError && <Notice tone="bad">{errorMessage(stop.error)}</Notice>}
      {shell.isError && <Notice tone="bad">{errorMessage(shell.error)}</Notice>}
      <dl className="facts panel">
        <dt>Status</dt>
        <dd>
          <SessionStatusBadge status={s.status} terminal={s.capabilities.terminal} />
          {s.exit_code !== undefined && s.exit_code !== 0 && <span className="muted"> exit code {s.exit_code}</span>}
        </dd>
        <dt>Adapter</dt>
        <dd>
          {s.adapter}
          {s.model && (
            <span className="muted">
              {" "}
              · model {s.model}
              {s.model_id && ` (${s.model_id})`}
            </span>
          )}
          {s.permission_mode && <span className="muted"> · {s.permission_mode}</span>}
        </dd>
        {(!s.capabilities.terminal || s.cost_usd > 0) && (
          <>
            <dt>Costs</dt>
            <dd>
              <SessionCost session={s} />
            </dd>
          </>
        )}
        <dt>Started</dt>
        <dd>
          {formatTime(s.created_at)}
          {s.ended_at && <span className="muted"> · ended {formatTime(s.ended_at)}</span>}
        </dd>
      </dl>
      {s.status === "paused" ? (
        <Notice tone="warn" title="Budget exceeded">
          {s.error}
          <p>
            The agent is paused. Under <Link to="/approvals">Approvals</Link> you can raise the budget; then it continues.
          </p>
        </Notice>
      ) : (
        s.error && (
          <Notice tone={s.status === "interrupted" ? "warn" : s.status === "failed" ? "bad" : "info"}>
            {s.error}
            {s.resumable && <p>The session can be resumed below with an instruction.</p>}
          </Notice>
        )
      )}
    </>
  );
}

function SessionCost({ session: s }: { session: Session }) {
  const tokens = s.input_tokens + s.output_tokens + s.cache_read_tokens + s.cache_write_tokens;
  const ratio = s.budget_usd ? Math.min(s.cost_usd / s.budget_usd, 1) : 0;
  return (
    <>
      {formatUSD(s.cost_usd)}
      {s.budget_usd ? <span className="muted"> of {formatUSD(s.budget_usd)} budget</span> : null}
      {tokens > 0 && (
        <span
          className="muted"
          title={`input ${s.input_tokens}, output ${s.output_tokens}, cache read ${s.cache_read_tokens}, cache written ${s.cache_write_tokens}`}
        >
          {" "}
          · {formatTokens(s.input_tokens + s.cache_read_tokens + s.cache_write_tokens)} in / {formatTokens(s.output_tokens)} out
        </span>
      )}
      {s.metered && <span className="muted"> · measured at the egress proxy</span>}
      {s.unpriced && <span className="muted"> · price for a model unknown (pricing in config.yaml)</span>}
      {s.budget_usd ? (
        <div className="budgetbar" role="meter" aria-valuemin={0} aria-valuemax={s.budget_usd} aria-valuenow={s.cost_usd} aria-label="Budget">
          <span className={ratio >= 1 ? "over" : ratio >= 0.8 ? "near" : ""} style={{ width: `${ratio * 100}%` }} />
        </div>
      ) : null}
    </>
  );
}

function AgentView({ session: s }: { session: Session }) {
  const qc = useQueryClient();
  // Status change in the stream → reload session data (status, costs, live).
  const onEvent = useCallback(
    (e: SessionEvent) => {
      if (e.type === "status" || e.type === "cost" || e.type === "usage") void qc.invalidateQueries({ queryKey: ["session", s.id] });
    },
    [qc, s.id],
  );
  const { events, state } = useSessionEvents(s.id, onEvent);
  return (
    <>
      <div className="sectionhead">
        <h2>History</h2>
        <span className="muted streamstate">
          <span className={`dot ${state === "live" ? "ok" : "warn"}`} aria-hidden />{" "}
          {state === "live" ? "live" : state === "reconnecting" ? "reconnecting …" : "connecting …"}
        </span>
      </div>
      <Timeline events={events} live={s.live} />
      <InputBox session={s} />
    </>
  );
}

function InputBox({ session: s }: { session: Session }) {
  const qc = useQueryClient();
  const [text, setText] = useState("");
  const send = useMutation({
    mutationFn: () => api.sendInput(s.id, text),
    onSuccess: (v) => {
      setText("");
      qc.setQueryData(["session", s.id], v);
      void qc.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
  const canSend = (s.live && s.capabilities.input && s.status !== "starting") || s.resumable;
  const submit = (e: FormEvent | KeyboardEvent) => {
    e.preventDefault();
    if (canSend && !send.isPending && (text.trim() || s.resumable)) send.mutate();
  };

  if (!canSend && !s.live)
    return (
      <p className="muted inputhint">
        {s.capabilities.resume ? "This session cannot be resumed (no session ID of the CLI)." : "Session ended."}
      </p>
    );

  return (
    <form className="inputbox" onSubmit={submit}>
      <textarea
        rows={2}
        aria-label="Instruction to the agent"
        placeholder={
          s.resumable
            ? "Instruction to resume (empty: continue where the agent left off)"
            : s.status === "running"
              ? "Follow-up instruction – handled after the current step"
              : "Follow-up instruction"
        }
        value={text}
        disabled={!canSend}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) submit(e);
        }}
      />
      <button type="submit" className="primary" disabled={!canSend || send.isPending || (!text.trim() && !s.resumable)}>
        {send.isPending ? "Sending …" : s.resumable ? "Resume" : "Send"}
      </button>
      {send.isError && <Notice tone="bad">{errorMessage(send.error)}</Notice>}
    </form>
  );
}
