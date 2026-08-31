// File overview: Durable send queue status, SMTP/Sent-copy progress, and recovery controls.

import { useCallback, useEffect, useState } from "react";
import { api } from "../../api";
import type { AddToast } from "../../appTypes";
import { Icon } from "../../components/Icon";
import type { OutboxJob, OutboxResponse, OutboxSummary } from "../../types";

export function OutboxView({
  csrf,
  summary,
  navigate,
  addToast
}: {
  csrf: string;
  summary: OutboxSummary;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const [data, setData] = useState<OutboxResponse>({ jobs: [], summary });
  const [loading, setLoading] = useState(true);
  const [workingID, setWorkingID] = useState(0);

  const refresh = useCallback(async () => {
    try {
      setData(await api.outbox());
    } catch (err) {
      addToast(errorMessage(err), "error");
    } finally {
      setLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    void refresh();
  }, [refresh, summary.latest_id, summary.active, summary.needs_attention]);

  useEffect(() => {
    if (data.summary.active === 0) return;
    const timer = window.setInterval(() => void refresh(), 4000);
    return () => window.clearInterval(timer);
  }, [data.summary.active, refresh]);

  async function act(job: OutboxJob, action: "retry" | "cancel" | "acknowledge") {
    if (workingID) return;
    if (action === "cancel" && !window.confirm("Cancel this queued message? Its pending Sent copy will be removed.")) return;
    let retryAnyway = false;
    if (action === "retry" && job.retry_may_duplicate) {
      retryAnyway = window.confirm(
        "The SMTP connection ended after delivery began, so this message may already have been sent. Retry anyway and accept the risk of a duplicate?"
      );
      if (!retryAnyway) return;
    }
    setWorkingID(job.id);
    try {
      if (action === "retry") await api.retryOutbox(csrf, job.id, retryAnyway);
      if (action === "cancel") await api.cancelOutbox(csrf, job.id);
      if (action === "acknowledge") await api.acknowledgeOutbox(csrf, job.id);
      addToast(action === "retry" ? "Send queued for another attempt." : action === "cancel" ? "Queued send canceled." : "Notification dismissed.");
      await refresh();
    } catch (err) {
      addToast(errorMessage(err), "error");
    } finally {
      setWorkingID(0);
    }
  }

  const active = data.jobs.filter((job) => !job.completed_at && job.delivery_state !== "canceled");
  const history = data.jobs.filter((job) => job.completed_at || job.delivery_state === "canceled");

  return (
    <main className="outbox-page">
      <header className="outbox-heading">
        <div>
          <span className="outbox-eyebrow">Delivery center</span>
          <h1><Icon name="send" weight="duotone" /> Outbox</h1>
          <p>Messages appear in Sent as soon as they are safely queued. SMTP delivery continues here in the background.</p>
        </div>
        <button className="secondary" type="button" onClick={() => void refresh()} disabled={loading}>
          <Icon name="sync" /> Refresh
        </button>
      </header>

      <section className="outbox-summary" aria-label="Outbox summary">
        <SummaryCard value={data.summary.active} label="In progress" tone="active" />
        <SummaryCard value={data.summary.needs_attention} label="Needs attention" tone={data.summary.needs_attention ? "danger" : "quiet"} />
        <SummaryCard value={data.jobs.filter((job) => Boolean(job.completed_at)).length} label="Recently completed" tone="quiet" />
      </section>

      {loading && data.jobs.length === 0 ? <div className="panel muted">Loading delivery status…</div> : null}
      {!loading && data.jobs.length === 0 ? (
        <section className="outbox-empty panel">
          <span><Icon name="mail_open" weight="duotone" /></span>
          <h2>Nothing waiting to send</h2>
          <p>New messages will briefly appear here while Rolltop hands them to your SMTP server.</p>
        </section>
      ) : null}

      {active.length > 0 ? (
        <section className="outbox-section">
          <div className="outbox-section-title">
            <h2>Current sends</h2>
            <span>{active.length.toLocaleString()}</span>
          </div>
          <div className="outbox-list">
            {active.map((job) => (
              <OutboxCard key={job.id} job={job} working={workingID === job.id} navigate={navigate} act={act} />
            ))}
          </div>
        </section>
      ) : null}

      {history.length > 0 ? (
        <section className="outbox-section">
          <div className="outbox-section-title">
            <h2>Recent history</h2>
          </div>
          <div className="outbox-list compact">
            {history.map((job) => (
              <OutboxCard key={job.id} job={job} working={workingID === job.id} navigate={navigate} act={act} />
            ))}
          </div>
        </section>
      ) : null}
    </main>
  );
}

function SummaryCard({ value, label, tone }: { value: number; label: string; tone: string }) {
  return (
    <div className={`outbox-summary-card ${tone}`}>
      <strong>{value.toLocaleString()}</strong>
      <span>{label}</span>
    </div>
  );
}

function OutboxCard({
  job,
  working,
  navigate,
  act
}: {
  job: OutboxJob;
  working: boolean;
  navigate: (url: string) => void;
  act: (job: OutboxJob, action: "retry" | "cancel" | "acknowledge") => Promise<void>;
}) {
  const status = jobStatus(job);
  const steps = jobSteps(job);
  const canOpen = job.delivery_state !== "canceled" && job.message_id > 0;
  return (
    <article className={`outbox-card ${status.tone}`}>
      <div className="outbox-card-main">
        {canOpen ? (
          <button className="outbox-subject" type="button" onClick={() => navigate(`/messages/${job.message_id}`)}>
            {job.subject || "(no subject)"}
          </button>
        ) : <strong className="outbox-subject">{job.subject || "(no subject)"}</strong>}
        <div className={`outbox-state ${status.tone}`}>
          <span className="outbox-state-dot" />
          {status.label}
        </div>
        <p>{status.detail}</p>
        <div className="outbox-meta">
          <span>{formatBytes(job.raw_size)}</span>
          <span>Queued {formatDate(job.created_at)}</span>
          {job.attempt_count > 0 ? <span>{job.attempt_count} SMTP {job.attempt_count === 1 ? "attempt" : "attempts"}</span> : null}
          {job.filing_attempt_count > 0 ? <span>{job.filing_attempt_count} Sent-copy {job.filing_attempt_count === 1 ? "attempt" : "attempts"}</span> : null}
        </div>
      </div>

      <ol className="outbox-steps" aria-label="Delivery steps">
        {steps.map((step) => (
          <li key={step.label} className={step.state}>
            <span>{step.state === "done" ? "✓" : step.state === "error" ? "!" : ""}</span>
            <small>{step.label}</small>
          </li>
        ))}
      </ol>

      {job.last_error ? (
        <div className="outbox-error">
          <Icon name="report" />
          <div><strong>{job.retry_may_duplicate ? "Delivery is uncertain" : "Rolltop could not finish this send"}</strong><p>{job.last_error}</p></div>
        </div>
      ) : null}

      <div className="outbox-actions">
        {canOpen ? <button className="secondary" type="button" onClick={() => navigate(`/messages/${job.message_id}`)}>Open in Sent</button> : null}
        {job.can_retry ? <button type="button" disabled={working} onClick={() => void act(job, "retry")}>{job.retry_may_duplicate ? "Retry anyway…" : "Retry"}</button> : null}
        {job.can_cancel ? <button className="danger secondary" type="button" disabled={working} onClick={() => void act(job, "cancel")}>Cancel send</button> : null}
        {job.needs_attention ? <button className="ghost" type="button" disabled={working} onClick={() => void act(job, "acknowledge")}>Dismiss alert</button> : null}
      </div>
    </article>
  );
}

function jobStatus(job: OutboxJob): { label: string; detail: string; tone: string } {
  if (job.delivery_state === "delivery_unknown") {
    return { label: "Delivery uncertain", detail: "The connection ended after SMTP delivery began. Rolltop will not retry automatically because that could send a duplicate.", tone: "danger" };
  }
  if (job.delivery_state === "failed") {
    return { label: "Send failed", detail: "Automatic delivery stopped. Review the error and retry when the account is ready.", tone: "danger" };
  }
  if (job.delivery_state === "canceled") {
    return { label: "Canceled", detail: "This message was canceled before SMTP accepted it.", tone: "quiet" };
  }
  if (job.delivery_state === "accepted" && job.filing_state === "complete") {
    return { label: "Sent", detail: "SMTP accepted the message and its server-side Sent copy was confirmed.", tone: "complete" };
  }
  if (job.delivery_state === "accepted") {
    const attention = job.filing_state === "needs_attention";
    return {
      label: attention ? "Sent · copy needs attention" : "Sent · saving copy",
      detail: attention
        ? "SMTP accepted the message, but Rolltop could not confirm its server-side Sent copy."
        : "SMTP accepted the message. Rolltop is confirming the matching copy in your Sent folder.",
      tone: attention ? "warning" : "active"
    };
  }
  if (job.delivery_state === "retry_wait") {
    return { label: "Retry scheduled", detail: `The SMTP server was unavailable. Rolltop will try again${job.next_attempt_at ? ` ${formatDate(job.next_attempt_at)}` : " automatically"}.`, tone: "warning" };
  }
  if (job.delivery_state === "smtp_in_flight") {
    return { label: "Sending", detail: "The immutable queued message is being streamed to your SMTP server.", tone: "active" };
  }
  return { label: "Queued", detail: "The message is safely stored locally and waiting for the delivery worker.", tone: "active" };
}

function jobSteps(job: OutboxJob): Array<{ label: string; state: string }> {
  const failed = job.delivery_state === "failed" || job.delivery_state === "delivery_unknown";
  const accepted = job.delivery_state === "accepted";
  return [
    { label: "Safely queued", state: "done" },
    {
      label: "SMTP delivery",
      state: failed ? "error" : accepted ? "done" : ["smtp_in_flight", "claimed"].includes(job.delivery_state) ? "current" : "pending"
    },
    { label: "Server accepted", state: failed ? "pending" : accepted ? "done" : "pending" },
    {
      label: "Sent copy confirmed",
      state: job.filing_state === "complete" ? "done" : job.filing_state === "needs_attention" ? "error" : accepted ? "current" : "pending"
    }
  ];
}

function formatDate(value: string): string {
  if (!value) return "";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : "Could not update the outbox.";
}
