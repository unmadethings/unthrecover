import { useEffect, useState } from "react";
import { api, fmtTime, type AuditEvent } from "../api";
import { ErrorBox } from "../components";

export function ActivityView({ onError }: { onError: (e: unknown) => void }) {
  const [rows, setRows] = useState<AuditEvent[]>();
  const [error, setError] = useState<string>();
  const [filter, setFilter] = useState("");

  useEffect(() => {
    api
      .audit()
      .then(setRows)
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [onError]);

  const shown = (rows ?? []).filter((e) =>
    filter ? `${e.action} ${e.target} ${e.outcome} ${e.details}`.toLowerCase().includes(filter.toLowerCase()) : true,
  );
  return (
    <>
      <h1>Activity log</h1>
      <input className="filter" placeholder="Filter…" value={filter} onChange={(e) => setFilter(e.target.value)} />
      <ErrorBox error={error} />
      {!rows && !error && <p className="muted">Loading…</p>}
      {rows && (
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Actor</th>
              <th>Action</th>
              <th>Target</th>
              <th>Outcome</th>
              <th>Details</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((e) => (
              <tr key={e.id}>
                <td className="nowrap">{fmtTime(e.at)}</td>
                <td>{e.actor}</td>
                <td className="mono small">{e.action}</td>
                <td className="mono small">{e.target}</td>
                <td className={e.outcome === "failed" ? "bad-text" : ""}>{e.outcome}</td>
                <td className="small">{e.details}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
