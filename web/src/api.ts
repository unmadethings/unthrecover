export type Backup = {
  id: string;
  workload: string;
  type: string;
  status: string;
  verification_status: string;
  created_at: string;
  snapshot_at?: string;
  size_bytes: number;
  sha256?: string;
  server_version?: string;
  error_category?: string;
  error_message?: string;
  last_verification_id?: string;
  verifying: boolean;
};

export type Run = {
  id: string;
  status: string;
  started_at: string;
  finished_at?: string;
  restore_duration_ms: number;
  total_duration_ms: number;
  error_category?: string;
  error_message?: string;
  has_report: boolean;
};

export type OffsiteCopy = {
  backup_id: string;
  target: string;
  location: string;
  status: string;
  encrypted: boolean;
  signed: boolean;
  size_bytes: number;
  uploaded_at?: string;
  checked_at?: string;
  deleted_at?: string;
  error_category?: string;
  error_message?: string;
};

export type WorkloadSummary = {
  workload: string;
  type: string;
  backups: number;
  last_backup_at?: string;
  last_backup_status?: string;
  last_verified_backup_at?: string;
  latest_verification_status?: string;
  storage_bytes: number;
};

export type Overview = {
  offsite_verified_copies: number;
  last_offsite_check_at?: string | null;
  protected_workloads: number;
  backups_by_status: Record<string, number>;
  last_successful_backup_at?: string | null;
  last_verified_backup_at?: string | null;
  verification_success_rate_30d?: number | null;
  verified_runs_30d: number;
  failed_runs_30d: number;
  storage_bytes: number;
  avg_restore_duration_ms_30d: number;
  workloads: WorkloadSummary[];
};

export type Step = { name: string; mandatory: boolean; status: string; duration_ms: number; detail?: string };

export type Report = {
  run_id: string;
  backup_id: string;
  workload_id: string;
  final_status: string;
  error_category?: string;
  error_message?: string;
  hint?: string;
  started_at: string;
  finished_at: string;
  steps: Step[];
  verification_scope: { covered: string[] | null; not_covered: string[] | null };
  recovery_objectives: {
    observed_test_restore_duration_ms: number;
    observed_total_verification_duration_ms: number;
    rto_note: string;
    estimated_data_loss_window_seconds_at_report_time: number;
    rpo_note: string;
  };
  cleanup: { status: string; removed?: string[]; errors?: string[] };
};

export type AuditEvent = { id: number; at: string; actor: string; action: string; target: string; outcome: string; details: string };

export type Connection = { name: string; provider: string; account: string; folder: string; folder_picked: boolean; created_at: string };

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (method !== "GET") {
    headers["Content-Type"] = "application/json";
    headers["X-Unth-CSRF"] = "1";
  }
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, (data as { error?: string }).error ?? res.statusText);
  return data as T;
}

export const api = {
  session: () => call<{ authenticated: boolean; version: string }>("GET", "/api/session"),
  login: (token: string) => call<{ authenticated: boolean }>("POST", "/api/login", { token }),
  logout: () => call<{ authenticated: boolean }>("POST", "/api/logout", {}),
  connections: () => call<{ google_configured: boolean; picker_configured: boolean; connections: Connection[] }>("GET", "/api/connections"),
  pickerToken: (name: string) =>
    call<{ access_token: string; api_key: string; app_id: string }>("POST", "/api/connections/" + encodeURIComponent(name) + "/picker", {}),
  setFolder: (name: string, id: string) => call<{ folder: string }>("PUT", "/api/connections/" + encodeURIComponent(name) + "/folder", { id }),
  resetFolder: (name: string) => call<{ folder: string }>("DELETE", "/api/connections/" + encodeURIComponent(name) + "/folder"),
  connectGDrive: () => call<{ auth_url: string }>("POST", "/api/connections/gdrive", {}),
  disconnect: (name: string) =>
    call<{ deleted: boolean; revoked: boolean }>("DELETE", "/api/connections/" + encodeURIComponent(name)),
  overview: () => call<Overview>("GET", "/api/overview"),
  backups: (workload?: string) =>
    call<Backup[]>("GET", "/api/backups" + (workload ? "?workload=" + encodeURIComponent(workload) : "")),
  backup: (id: string) => call<{ backup: Backup; runs: Run[]; offsite_copies: OffsiteCopy[] }>("GET", "/api/backups/" + encodeURIComponent(id)),
  verify: (id: string) => call<{ started: boolean }>("POST", "/api/backups/" + encodeURIComponent(id) + "/verify", {}),
  report: (runId: string) => call<Report>("GET", "/api/runs/" + encodeURIComponent(runId) + "/report"),
  audit: () => call<AuditEvent[]>("GET", "/api/audit?limit=300"),
  info: () => call<Record<string, string | number | boolean>>("GET", "/api/info"),
};

export function fmtBytes(n: number): string {
  if (!n) return "0 B";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}

export function fmtTime(s?: string | null): string {
  if (!s) return "—";
  const d = new Date(s);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

export function ago(s?: string | null): string {
  if (!s) return "never";
  const sec = Math.round((Date.now() - new Date(s).getTime()) / 1000);
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.round(sec / 60)} min ago`;
  if (sec < 86400) return `${Math.round(sec / 3600)} h ago`;
  return `${Math.round(sec / 86400)} d ago`;
}

export function fmtMs(ms: number): string {
  if (!ms) return "—";
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  return `${(ms / 60000).toFixed(1)} min`;
}
