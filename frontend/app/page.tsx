"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import Link from "next/link";
import { api, instanceProxyUrl, Instance } from "@/lib/api";
import AnsiLog from "@/components/AnsiLog";
import { statusColor, statusLabel } from "@/lib/utils";

// ---- Log modal -------------------------------------------------------------

function LogModal({
  instanceId,
  onClose,
}: {
  instanceId: string;
  onClose: () => void;
}) {
  return (
    <div
      className="fixed inset-0 bg-black/70 z-50 flex items-center justify-center p-4"
      onClick={(e) => e.target === e.currentTarget && onClose()}
    >
      <div className="bg-slate-900 border border-slate-700 rounded-lg w-full max-w-4xl max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between px-4 py-3 border-b border-slate-700">
          <span className="font-semibold text-slate-200">
            Logs — {instanceId}
          </span>
          <button
            onClick={onClose}
            className="text-slate-400 hover:text-white text-xl leading-none"
          >
            ×
          </button>
        </div>
        <AnsiLog
          wsUrl={`/instances/${instanceId}/logs/ws`}
          className="flex-1 min-h-0"
        />
      </div>
    </div>
  );
}

// ---- Instance Card ---------------------------------------------------------

function InstanceCard({
  instance,
  onDeleted,
  onUpdated,
}: {
  instance: Instance;
  onDeleted: (id: string) => void;
  onUpdated: (inst: Instance) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [showLogs, setShowLogs] = useState(false);
  const [error, setError] = useState("");

  const doAction = async (action: "start" | "stop" | "restart") => {
    setBusy(true);
    setError("");
    try {
      const updated = await api.instances[action](instance.id);
      onUpdated(updated);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!confirm(`Delete instance "${instance.name}"? This is irreversible.`))
      return;
    setBusy(true);
    setError("");
    try {
      await api.instances.delete(instance.id);
      onDeleted(instance.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  const isRunning = instance.status === "running";
  const isStopped =
    instance.status === "stopped" ||
    instance.status === "exited" ||
    instance.status === "created";

  return (
    <>
      {showLogs && (
        <LogModal
          instanceId={instance.id}
          onClose={() => setShowLogs(false)}
        />
      )}
      <div className="bg-slate-800 border border-slate-700 rounded-xl p-5 flex flex-col gap-3 hover:border-slate-500 transition-colors">
        {/* Header */}
        <div className="flex items-start justify-between gap-2">
          <div>
            <div className="flex items-center gap-2">
              <span
                className={`inline-block w-2.5 h-2.5 rounded-full ${statusColor(instance.status)}`}
              />
              <Link
                href={`/instances/${instance.id}`}
                className="font-semibold text-white hover:text-blue-400 transition-colors"
              >
                {instance.name}
              </Link>
            </div>
            <div className="text-xs text-slate-400 mt-0.5 ml-[18px]">
              {statusLabel(instance.status)} · port {instance.port} · ID{" "}
              {instance.id}
            </div>
          </div>
          {isRunning && (
            <a
              href={instanceProxyUrl(instance.id)}
              target="_blank"
              rel="noopener noreferrer"
              className="shrink-0 px-3 py-1 text-xs bg-blue-600 hover:bg-blue-500 rounded text-white font-medium transition-colors"
            >
              Open ↗
            </a>
          )}
        </div>

        {/* Resource info */}
        <div className="flex gap-3 text-xs text-slate-400">
          <span>
            RAM:{" "}
            {instance.memory_mb === 0
              ? "unlimited"
              : `${instance.memory_mb} MB`}
          </span>
          <span>
            CPU:{" "}
            {instance.cpu_cores === 0
              ? "unlimited"
              : `${instance.cpu_cores} cores`}
          </span>
        </div>

        {error && (
          <div className="text-xs text-red-400 bg-red-950/50 rounded px-3 py-1.5">
            {error}
          </div>
        )}

        {/* Actions */}
        <div className="flex gap-2 flex-wrap">
          {isRunning && (
            <button
              disabled={busy}
              onClick={() => doAction("stop")}
              className="px-3 py-1 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-200 disabled:opacity-50"
            >
              Stop
            </button>
          )}
          {isStopped && (
            <button
              disabled={busy}
              onClick={() => doAction("start")}
              className="px-3 py-1 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-200 disabled:opacity-50"
            >
              Start
            </button>
          )}
          <button
            disabled={busy}
            onClick={() => doAction("restart")}
            className="px-3 py-1 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-200 disabled:opacity-50"
          >
            Restart
          </button>
          <button
            disabled={busy}
            onClick={() => setShowLogs(true)}
            className="px-3 py-1 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-200 disabled:opacity-50"
          >
            Logs
          </button>
          <Link
            href={`/instances/${instance.id}/terminal`}
            className="px-3 py-1 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-200"
          >
            Terminal
          </Link>
          <button
            disabled={busy}
            onClick={doDelete}
            className="px-3 py-1 text-xs bg-red-900/60 hover:bg-red-800 rounded text-red-300 disabled:opacity-50 ml-auto"
          >
            Delete
          </button>
        </div>
      </div>
    </>
  );
}

// ---- Dashboard page --------------------------------------------------------

export default function DashboardPage() {
  const [instances, setInstances] = useState<Instance[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const loadInstances = useCallback(async () => {
    try {
      const data = await api.instances.list();
      setInstances(data);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadInstances();
  }, [loadInstances]);

  // Single batch poller for all instances — one request every 10 s instead of
  // one per card. instancesRef keeps a stable reference so the poll closure
  // always sees current state without being added to the effect deps.
  const instancesRef = useRef<Instance[]>([]);
  useEffect(() => {
    instancesRef.current = instances;
  }, [instances]);

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      const current = instancesRef.current;
      if (current.length === 0) {
        timer = setTimeout(poll, 10000);
        return;
      }
      const statuses: Record<string, string> = {};
      for (const inst of current) statuses[inst.id] = inst.status;

      try {
        const changed = await api.instances.pollAllStatus(statuses);
        setInstances((prev) => {
          let next = prev;
          for (const [id, updated] of Object.entries(changed)) {
            if (updated === null) {
              next = next.filter((i) => i.id !== id);
            } else {
              next = next.map((i) => (i.id === id ? updated : i));
            }
          }
          return next;
        });
      } catch {
        // ignore transient poll errors
      }
      timer = setTimeout(poll, 10000);
    };
    timer = setTimeout(poll, 10000);
    return () => clearTimeout(timer);
  }, []);

  const handleDeleted = useCallback((id: string) => {
    setInstances((prev) => prev.filter((i) => i.id !== id));
  }, []);

  const handleUpdated = useCallback((inst: Instance) => {
    setInstances((prev) => prev.map((i) => (i.id === inst.id ? inst : i)));
  }, []);

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-white">Instances</h1>
          <p className="text-slate-400 text-sm mt-1">
            Manage your Claude Code containers
          </p>
        </div>
        <Link
          href="/instances/new"
          className="px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white text-sm font-medium rounded-lg transition-colors"
        >
          + New Instance
        </Link>
      </div>

      {loading && (
        <div className="text-slate-400 text-center py-16">Loading…</div>
      )}

      {!loading && error && (
        <div className="text-red-400 bg-red-950/50 rounded-lg px-4 py-3 mb-4">
          Failed to load instances: {error}
          <button
            onClick={loadInstances}
            className="ml-4 underline hover:text-red-300"
          >
            Retry
          </button>
        </div>
      )}

      {!loading && !error && instances.length === 0 && (
        <div className="text-center py-24 text-slate-500">
          <div className="text-5xl mb-4">📦</div>
          <div className="text-lg font-medium text-slate-400">
            No instances yet
          </div>
          <div className="text-sm mt-2">
            <Link
              href="/instances/new"
              className="text-blue-400 hover:text-blue-300 underline"
            >
              Create your first instance
            </Link>
          </div>
        </div>
      )}

      {!loading && instances.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
          {instances.map((inst) => (
            <InstanceCard
              key={inst.id}
              instance={inst}
              onDeleted={handleDeleted}
              onUpdated={handleUpdated}
            />
          ))}
        </div>
      )}
    </div>
  );
}
