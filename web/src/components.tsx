import type { Backup, Step } from "./api";

const TONE: Record<string, string> = {
  VERIFIED: "ok",
  BACKUP_CREATED: "info",
  FAILED: "bad",
  CANCELLED: "warn",
  EXPIRED: "muted",
  NOT_VERIFIED: "muted",
  RUNNING: "info",
  PENDING: "info",
  INTEGRITY_CHECKED: "info",
  RESTORE_TESTING: "info",
  PASS: "ok",
  FAIL: "bad",
  NOT_RUN: "muted",
  WARN: "warn",
};

export function Badge({ value }: { value?: string }) {
  if (!value) return <span className="muted">—</span>;
  return <span className={"badge " + (TONE[value] ?? "muted")}>{value.replace(/_/g, " ")}</span>;
}

const STAGES = ["Created", "Integrity checked", "Restore tested", "Verified"] as const;
const INTEGRITY = ["artifact_accessible", "manifest_valid", "checksum_sha256", "archive_readable"];
const RESTORE = ["isolated_environment", "restore"];

type StageState = "done" | "failed" | "pending" | "running";

// pipelineFromSteps derives CREATED → INTEGRITY CHECKED → RESTORE TESTED → VERIFIED
// from the steps of one verification report.
export function pipelineFromSteps(final: string, steps: Step[]): StageState[] {
  const st = (names: string[]): StageState => {
    const xs = steps.filter((s) => names.includes(s.name));
    if (xs.some((s) => s.status === "FAIL")) return "failed";
    if (xs.length && xs.every((s) => s.status === "PASS")) return "done";
    return "pending";
  };
  const integrity = st(INTEGRITY);
  const restore = integrity === "done" ? st(RESTORE) : "pending";
  const rest = steps.filter((s) => !INTEGRITY.includes(s.name) && !RESTORE.includes(s.name));
  let verified: StageState = final === "VERIFIED" ? "done" : "pending";
  if (final === "FAILED" && restore === "done" && rest.some((s) => s.status === "FAIL")) verified = "failed";
  return ["done", integrity, restore, verified];
}

// pipelineFromBackup is the coarse version used in tables (no report needed).
export function pipelineFromBackup(b: Backup): StageState[] {
  if (b.status !== "BACKUP_CREATED") return [b.status === "FAILED" ? "failed" : "pending", "pending", "pending", "pending"];
  switch (b.verification_status) {
    case "VERIFIED":
      return ["done", "done", "done", "done"];
    case "INTEGRITY_CHECKED":
      return ["done", "done", "running", "pending"];
    case "RESTORE_TESTING":
    case "RUNNING":
    case "PENDING":
      return ["done", "running", "pending", "pending"];
    case "FAILED":
      return ["done", "failed", "pending", "pending"];
    default:
      return ["done", "pending", "pending", "pending"];
  }
}

export function Pipeline({ states, compact }: { states: StageState[]; compact?: boolean }) {
  return (
    <ol className={compact ? "pipeline compact" : "pipeline"} aria-label="Recovery pipeline">
      {STAGES.map((label, i) => (
        <li key={label} className={"stage " + states[i]} title={`${label}: ${states[i]}`}>
          <span className="dot" aria-hidden />
          {!compact && <span className="stage-label">{label}</span>}
        </li>
      ))}
    </ol>
  );
}

export function Stat({ label, value, sub, tone }: { label: string; value: string; sub?: string; tone?: string }) {
  return (
    <div className={"stat " + (tone ?? "")}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  );
}

export function ErrorBox({ error }: { error?: string }) {
  if (!error) return null;
  return (
    <div className="error" role="alert">
      {error}
    </div>
  );
}
