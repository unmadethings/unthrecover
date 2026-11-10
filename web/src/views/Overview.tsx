import { useEffect, useState } from "react";
import { ago, api, fmtBytes, fmtMs, type Overview } from "../api";
import { Badge, ErrorBox, Stat } from "../components";

export function OverviewView({ onError }: { onError: (e: unknown) => void }) {
  const [data, setData] = useState<Overview>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    api
      .overview()
      .then(setData)
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [onError]);

  if (error) return <ErrorBox error={error} />;
  if (!data) return <p className="muted">Loading…</p>;

  const rate = data.verification_success_rate_30d;
  const rateText = rate == null ? "—" : `${Math.round(rate * 100)}%`;
  return (
    <>
      <h1>Overview</h1>
      <div className="stats">
        <Stat label="Protected workloads" value={String(data.protected_workloads)} />
        <Stat label="Last successful backup" value={ago(data.last_successful_backup_at)} />
        <Stat
          label="Last verified recovery"
          value={ago(data.last_verified_backup_at)}
          tone={data.last_verified_backup_at ? "" : "warn"}
          sub={data.last_verified_backup_at ? "backup restored and checked" : "no backup has been proven recoverable"}
        />
        <Stat
          label="Recovery success (30 d)"
          value={rateText}
          sub={`${data.verified_runs_30d} verified · ${data.failed_runs_30d} failed`}
          tone={data.failed_runs_30d > 0 ? "bad" : ""}
        />
        <Stat label="Backup storage" value={fmtBytes(data.storage_bytes)} />
        <Stat
          label="Offsite copies"
          value={String(data.offsite_verified_copies)}
          sub={
            data.last_offsite_check_at
              ? "downloaded and checked " + ago(data.last_offsite_check_at)
              : "no copy outside this server yet"
          }
          tone={data.offsite_verified_copies > 0 ? "" : "warn"}
        />
        <Stat label="Avg. test restore (30 d)" value={fmtMs(data.avg_restore_duration_ms_30d)} sub="observed, not an RTO" />
      </div>

      <h2>Workloads</h2>
      {data.workloads.length === 0 ? (
        <div className="card muted">
          No backups yet. Create one with <code>unth backup postgres --config job.yaml</code> or schedule{" "}
          <code>unth run</code>.
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Workload</th>
              <th>Type</th>
              <th>Last backup</th>
              <th>Latest verification</th>
              <th>Last verified</th>
              <th className="num">Backups</th>
              <th className="num">Storage</th>
            </tr>
          </thead>
          <tbody>
            {data.workloads.map((w) => (
              <tr key={w.workload}>
                <td>
                  <a href={"#/backups?workload=" + encodeURIComponent(w.workload)}>{w.workload}</a>
                </td>
                <td>{w.type}</td>
                <td>{ago(w.last_backup_at)}</td>
                <td>
                  <Badge value={w.latest_verification_status} />
                </td>
                <td className={w.last_verified_backup_at ? "" : "warn-text"}>{ago(w.last_verified_backup_at)}</td>
                <td className="num">{w.backups}</td>
                <td className="num">{fmtBytes(w.storage_bytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
