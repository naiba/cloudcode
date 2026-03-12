import type { InstanceStatus } from "@/lib/api";

export function statusColor(s: InstanceStatus): string {
  switch (s) {
    case "running":
      return "bg-green-500";
    case "stopped":
    case "created":
      return "bg-yellow-500";
    case "exited":
      return "bg-slate-500";
    case "error":
      return "bg-red-500";
    case "removed":
      return "bg-slate-600";
    default:
      return "bg-slate-500";
  }
}

export function statusLabel(s: InstanceStatus): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** Format bytes as a human-readable string (e.g. "1.2 GB", "456 MB"). */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "N/A";
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(Math.max(0, Math.floor(Math.log(bytes) / Math.log(1024))), units.length - 1);
  const value = bytes / Math.pow(1024, i);
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[i]}`;
}
