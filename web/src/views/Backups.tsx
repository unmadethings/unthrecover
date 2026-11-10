import { useEffect, useState } from "react";
import { api, fmtBytes, fmtTime, type Backup } from "../api";
import { Badge, ErrorBox, Pipeline, pipelineFromBackup } from "../components";

export function BackupsView({ workload, onError }: { workload?: string; onError: (e: unknown) => void }) {
  const [rows, setRows] = useState<Backup[]>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    setRows(undefined);
    api
      .backups(workload)
      .then(setRows)
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [workload, onError]);

  return (
    <>
      <h1>
        Backups &amp; recovery tests {workload && <span className="muted">· {workload}</span>}
      </h1>
      {workload && (
        <p>
          <a href="#/backups">Show all workloads</a>
        </p>
      )}
      <p className="muted small legend">
        Pipeline: Created → Integrity checked → Restore tested → Verified. A backup is VERIFIED only after a real
        restore into an isolated environment passed every mandatory check.
      </p>
      <ErrorBox error={error} />
      {!rows && !error && <p className="muted">Loading…</p>}
      {rows && rows.length === 0 && <div className="card muted">No backups.</div>}
      {rows && rows.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Backup</th>
              <th>Workload</th>
              <th>Created</th>
              <th>Pipeline</th>
              <th>Verification</th>
              <th className="num">Size</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((b) => (
              <tr key={b.id}>
                <td>
                  <a href={"#/backups/" + encodeURIComponent(b.id)} className="mono">
                    {b.id}
                  </a>
                  {b.status !== "BACKUP_CREATED" && (
                    <>
                      {" "}
                      <Badge value={b.status} />
                    </>
                  )}
                </td>
                <td>{b.workload}</td>
                <td>{fmtTime(b.created_at)}</td>
                <td>
                  <Pipeline states={pipelineFromBackup(b)} compact />
                </td>
                <td>{b.verifying ? <Badge value="RUNNING" /> : <Badge value={b.verification_status} />}</td>
                <td className="num">{fmtBytes(b.size_bytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
