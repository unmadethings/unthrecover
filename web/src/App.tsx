import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "./api";
import { Login } from "./views/Login";
import { OverviewView } from "./views/Overview";
import { BackupsView } from "./views/Backups";
import { BackupDetail } from "./views/BackupDetail";
import { ActivityView } from "./views/Activity";
import { SettingsView } from "./views/Settings";

type Route = { name: "overview" | "backups" | "backup" | "activity" | "settings"; id?: string; workload?: string };

function parse(hash: string): Route {
  const [path, query] = hash.replace(/^#\/?/, "").split("?");
  const parts = path.split("/").filter(Boolean);
  const params = new URLSearchParams(query ?? "");
  if (parts[0] === "backups" && parts[1]) return { name: "backup", id: decodeURIComponent(parts[1]) };
  if (parts[0] === "backups") return { name: "backups", workload: params.get("workload") ?? undefined };
  if (parts[0] === "activity") return { name: "activity" };
  if (parts[0] === "settings") return { name: "settings" };
  return { name: "overview" };
}

const NAV: { key: Route["name"]; label: string; href: string }[] = [
  { key: "overview", label: "Overview", href: "#/" },
  { key: "backups", label: "Backups & recovery tests", href: "#/backups" },
  { key: "activity", label: "Activity log", href: "#/activity" },
  { key: "settings", label: "Settings", href: "#/settings" },
];

export function App() {
  const [auth, setAuth] = useState<"unknown" | "yes" | "no">("unknown");
  const [version, setVersion] = useState("");
  const [route, setRoute] = useState<Route>(parse(location.hash));

  useEffect(() => {
    const on = () => setRoute(parse(location.hash));
    addEventListener("hashchange", on);
    return () => removeEventListener("hashchange", on);
  }, []);

  const check = useCallback(() => {
    api
      .session()
      .then((s) => {
        setVersion(s.version);
        setAuth("yes");
      })
      .catch(() => setAuth("no"));
  }, []);
  useEffect(check, [check]);

  const onAuthError = useCallback((e: unknown) => {
    if (e instanceof ApiError && e.status === 401) setAuth("no");
  }, []);

  if (auth === "unknown") return <div className="center muted">Loading…</div>;
  if (auth === "no") return <Login onLogin={check} />;

  const active = route.name === "backup" ? "backups" : route.name;
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="logo" aria-hidden>
            ◆
          </span>
          <div>
            <div className="brand-name">Unth Recover</div>
            <div className="brand-tag">Your backup is not your recovery.</div>
          </div>
        </div>
        <nav>
          {NAV.map((n) => (
            <a key={n.key} href={n.href} className={active === n.key ? "nav active" : "nav"}>
              {n.label}
            </a>
          ))}
        </nav>
        <div className="sidebar-foot">
          <span className="muted small">{version}</span>
          <button
            className="link"
            onClick={() =>
              api
                .logout()
                .catch(() => undefined)
                .then(() => setAuth("no"))
            }
          >
            Sign out
          </button>
        </div>
      </aside>
      <main className="content">
        {route.name === "overview" && <OverviewView onError={onAuthError} />}
        {route.name === "backups" && <BackupsView workload={route.workload} onError={onAuthError} />}
        {route.name === "backup" && route.id && <BackupDetail id={route.id} onError={onAuthError} />}
        {route.name === "activity" && <ActivityView onError={onAuthError} />}
        {route.name === "settings" && <SettingsView onError={onAuthError} />}
      </main>
    </div>
  );
}
