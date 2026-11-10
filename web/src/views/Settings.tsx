import { useEffect, useState } from "react";
import { api, fmtBytes } from "../api";
import { ErrorBox } from "../components";

export function SettingsView({ onError }: { onError: (e: unknown) => void }) {
  const [info, setInfo] = useState<Record<string, string | number | boolean>>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    api
      .info()
      .then(setInfo)
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [onError]);

  return (
    <>
      <h1>Settings</h1>
      <ErrorBox error={error} />
      {info && (
        <div className="card">
          <dl className="facts">
            <dt>Version</dt>
            <dd>
              {String(info.version)} <span className="muted small">({String(info.commit)}, {String(info.go)})</span>
            </dd>
            <dt>Data directory</dt>
            <dd className="mono">{String(info.data_dir)}</dd>
            <dt>Free space</dt>
            <dd>{typeof info.free_bytes === "number" ? fmtBytes(info.free_bytes) : "—"}</dd>
            <dt>Metadata schema</dt>
            <dd>v{String(info.metadata_schema ?? "?")}</dd>
            <dt>Docker</dt>
            <dd>{String(info.docker ?? "—")}</dd>
            <dt>Transport</dt>
            <dd>{info.tls ? "HTTPS" : "HTTP (loopback only)"}</dd>
          </dl>
        </div>
      )}
      <div className="card muted small">
        Jobs, retention and notifications are configured in job files and run with <code>unth run</code> (systemd
        timer or cron). The panel is read-only except for starting verifications. See docs/SCHEDULING.md.
      </div>
    </>
  );
}
