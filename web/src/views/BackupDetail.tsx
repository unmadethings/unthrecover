import { useCallback, useEffect, useState } from "react";
import { api, ApiError, fmtBytes, fmtMs, fmtTime, type Backup, type Report, type Run } from "../api";
import { Badge, ErrorBox, Pipeline, pipelineFromBackup, pipelineFromSteps } from "../components";

export function BackupDetail({ id, onError }: { id: string; onError: (e: unknown) => void }) {
  const [backup, setBackup] = useState<Backup>();
  const [runs, setRuns] = useState<Run[]>([]);
  const [selected, setSelected] = useState<string>();
  const [report, setReport] = useState<Report>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const load = useCallback(() => {
    api
      .backup(id)
      .then((d) => {
        setBackup(d.backup);
        setRuns(d.runs);
        setSelected((cur) => cur ?? d.runs.find((r) => r.has_report)?.id);
      })
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [id, onError]);
  useEffect(load, [load]);

  // Poll while a verification started from the UI is running.
  useEffect(() => {
    if (!backup?.verifying) return;
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  }, [backup?.verifying, load]);

  useEffect(() => {
    if (!selected) return;
    setReport(undefined);
    api.report(selected).then(setReport).catch(onError);
  }, [selected, onError]);

  async function verifyNow() {
    setNotice(undefined);
    setError(undefined);
    try {
      await api.verify(id);
      setNotice("Verification started: a fresh isolated environment is being created and the backup restored.");
      setSelected(undefined);
      load();
    } catch (e) {
      onError(e);
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  if (error && !backup) return <ErrorBox error={error} />;
  if (!backup) return <p className="muted">Loading…</p>;

  return (
    <>
      <p>
        <a href="#/backups">← Backups</a>
      </p>
      <h1 className="mono">{backup.id}</h1>
      <div className="card">
        <Pipeline states={report ? pipelineFromSteps(report.final_status, report.steps) : pipelineFromBackup(backup)} />
        <dl className="facts">
          <dt>Workload</dt>
          <dd>{backup.workload}</dd>
          <dt>Type</dt>
          <dd>{backup.type}</dd>
          <dt>Status</dt>
          <dd>
            <Badge value={backup.status} />
          </dd>
          <dt>Verification</dt>
          <dd>{backup.verifying ? <Badge value="RUNNING" /> : <Badge value={backup.verification_status} />}</dd>
          <dt>Created</dt>
          <dd>{fmtTime(backup.created_at)}</dd>
          <dt>Snapshot</dt>
          <dd>{fmtTime(backup.snapshot_at)}</dd>
          <dt>Size</dt>
          <dd>{fmtBytes(backup.size_bytes)}</dd>
          {backup.server_version && (
            <>
              <dt>Source version</dt>
              <dd>PostgreSQL {backup.server_version}</dd>
            </>
          )}
          {backup.sha256 && (
            <>
              <dt>SHA-256</dt>
              <dd className="mono small break">{backup.sha256}</dd>
            </>
          )}
          {backup.error_category && (
            <>
              <dt>Error</dt>
              <dd className="bad-text">
                {backup.error_category}: {backup.error_message}
              </dd>
            </>
          )}
        </dl>
        <div className="actions">
          <button
            className="primary"
            onClick={verifyNow}
            disabled={backup.verifying || backup.status !== "BACKUP_CREATED"}
            title="Restores this backup into a new isolated environment and checks it"
          >
            {backup.verifying ? "Verification running…" : "Verify now"}
          </button>
        </div>
        {notice && <p className="notice">{notice}</p>}
        <ErrorBox error={error} />
      </div>

      <h2>Verification runs</h2>
      {runs.length === 0 ? (
        <div className="card muted">This backup has never been verified — it is not proven recoverable.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Run</th>
              <th>Started</th>
              <th>Result</th>
              <th className="num">Test restore</th>
              <th className="num">Total</th>
              <th>Failure</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr
                key={r.id}
                className={r.id === selected ? "selected clickable" : r.has_report ? "clickable" : ""}
                onClick={() => r.has_report && setSelected(r.id)}
              >
                <td className="mono">{r.id}</td>
                <td>{fmtTime(r.started_at)}</td>
                <td>
                  <Badge value={r.status} />
                </td>
                <td className="num">{fmtMs(r.restore_duration_ms)}</td>
                <td className="num">{fmtMs(r.total_duration_ms)}</td>
                <td>{r.error_category ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {report && <ReportView report={report} />}
    </>
  );
}

function ReportView({ report }: { report: Report }) {
  const o = report.recovery_objectives;
  return (
    <>
      <h2>
        Recovery report <span className="mono small muted">{report.run_id}</span>
      </h2>
      <div className={"card report " + (report.final_status === "VERIFIED" ? "ok-border" : "bad-border")}>
        <div className="report-head">
          <Badge value={report.final_status} />
          <span className="muted small">tested {fmtTime(report.started_at)}</span>
        </div>
        {report.error_category && (
          <div className="failure">
            <strong>{report.error_category}</strong>: {report.error_message}
            {report.hint && (
              <div className="hint">
                <strong>Suggested fix:</strong> {report.hint}
              </div>
            )}
          </div>
        )}
        <table className="steps">
          <thead>
            <tr>
              <th>Step</th>
              <th>Result</th>
              <th className="num">Duration</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {report.steps.map((s) => (
              <tr key={s.name}>
                <td>
                  {s.name}
                  {s.mandatory && <span className="muted" title="mandatory for VERIFIED"> *</span>}
                </td>
                <td>
                  <Badge value={s.status} />
                </td>
                <td className="num">{fmtMs(s.duration_ms)}</td>
                <td className="small">{s.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="scope">
          <div>
            <h3>Verified</h3>
            <ul>{(report.verification_scope.covered ?? []).map((c) => <li key={c}>{c}</li>)}</ul>
          </div>
          <div>
            <h3>Not verified</h3>
            <ul className="muted">
              {(report.verification_scope.not_covered ?? []).map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
          </div>
        </div>
        <dl className="facts">
          <dt>Observed test restore</dt>
          <dd>
            {fmtMs(o.observed_test_restore_duration_ms)} <span className="muted small">— {o.rto_note}</span>
          </dd>
          <dt>Data-loss window</dt>
          <dd>
            {Math.round(o.estimated_data_loss_window_seconds_at_report_time / 60)} min{" "}
            <span className="muted small">— {o.rpo_note}</span>
          </dd>
          <dt>Cleanup</dt>
          <dd>
            <Badge value={report.cleanup.status} />
          </dd>
        </dl>
      </div>
    </>
  );
}
