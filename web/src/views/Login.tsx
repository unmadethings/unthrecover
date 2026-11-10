import { useState, type FormEvent } from "react";
import { api, ApiError } from "../api";
import { ErrorBox } from "../components";

export function Login({ onLogin }: { onLogin: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.login(token);
      setToken("");
      onLogin();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Login failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="center">
      <form className="card login" onSubmit={submit}>
        <h1>Unth Recover</h1>
        <p className="muted">Your backup is not your recovery.</p>
        <label htmlFor="token">Admin token</label>
        <input
          id="token"
          type="password"
          autoComplete="current-password"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          autoFocus
          required
        />
        <ErrorBox error={error} />
        <button className="primary" disabled={busy || !token}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
        <p className="muted small">The token is configured on the server (UNTH_UI_TOKEN or --token-file).</p>
      </form>
    </div>
  );
}
