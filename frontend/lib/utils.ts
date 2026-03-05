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
