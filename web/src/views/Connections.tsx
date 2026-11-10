import { useCallback, useEffect, useState } from "react";
import { api, fmtTime, type Connection } from "../api";
import { ErrorBox } from "../components";

const RESULT: Record<string, string> = {
  denied: "Google Drive was not connected: access was not allowed on Google's page.",
  expired: "The Google sign-in expired or was already used. Click Connect again.",
  failed: "Google Drive could not be connected (Drive permission missing or Google refused). Try again.",
};

// The result of Google's sign-in comes back as #/settings?gdrive=<result>.
function readResult(): string | undefined {
  const q = location.hash.split("?")[1];
  return q ? new URLSearchParams(q).get("gdrive") ?? undefined : undefined;
}

export function Connections({ onError }: { onError: (e: unknown) => void }) {
  const [list, setList] = useState<Connection[]>([]);
  const [configured, setConfigured] = useState(true);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .connections()
      .then((d) => {
        setList(d.connections);
        setConfigured(d.google_configured);
      })
      .catch((e) => {
        onError(e);
        setError(String(e.message ?? e));
      });
  }, [onError]);
  useEffect(load, [load]);

  useEffect(() => {
    const r = readResult();
    if (!r) return;
    if (r.startsWith("connected:")) {
      setNotice(`Google Drive connected as "${r.slice(10)}". Use it in a job: offsite: { type: gdrive, connection: ${r.slice(10)} }`);
    } else {
      setError(RESULT[r] ?? "Google Drive was not connected.");
    }
    history.replaceState(null, "", "#/settings");
  }, []);

  const connect = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const { auth_url } = await api.connectGDrive();
      location.href = auth_url; // Google's own sign-in and consent page
    } catch (e) {
      onError(e);
      setError(String((e as Error).message ?? e));
      setBusy(false);
    }
  };

  const disconnect = async (name: string) => {
    if (!confirm(`Disconnect "${name}"? Jobs using it will fail until it is connected again. Copies already on Drive are kept.`)) return;
    try {
      const r = await api.disconnect(name);
      setNotice(r.revoked ? `"${name}" disconnected and access revoked at Google.` : `"${name}" removed. Also remove "Unth Recover" in your Google account's security settings.`);
      load();
    } catch (e) {
      onError(e);
      setError(String((e as Error).message ?? e));
    }
  };

  return (
    <>
      <h2>Connections</h2>
      <div className="card">
        <p>
          Keep encrypted copies of your verified backups in <b>your own Google Drive</b>. You sign in on Google's page
          and allow access; Unth Recover can only see the files it creates (folder <i>Unth Recover Backups</i>), never
          the rest of your Drive. The access token stays on this server.
        </p>
        {configured ? (
          <button className="primary" onClick={connect} disabled={busy}>
            {busy ? "Opening Google…" : "Connect Google Drive"}
          </button>
        ) : (
          <p className="muted small">
            This build has no Google OAuth client. Set UNTH_GOOGLE_CLIENT_ID and UNTH_GOOGLE_CLIENT_SECRET (see
            docs/OFFSITE.md).
          </p>
        )}
        {notice && <p className="notice">{notice}</p>}
        <ErrorBox error={error} />
      </div>
      {list.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Name (for job files)</th>
              <th>Service</th>
              <th>Account</th>
              <th>Connected</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((c) => (
              <tr key={c.name}>
                <td className="mono">{c.name}</td>
                <td>{c.provider === "gdrive" ? "Google Drive" : c.provider}</td>
                <td>{c.account}</td>
                <td>{fmtTime(c.created_at)}</td>
                <td className="num">
                  <button onClick={() => disconnect(c.name)}>Disconnect</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
