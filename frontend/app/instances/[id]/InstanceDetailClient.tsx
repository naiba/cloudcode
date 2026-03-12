"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import { useParams, useRouter } from "next/navigation";
import Link from "next/link";
import { api, instanceOpenUrl, Instance } from "@/lib/api";
import AnsiLog from "@/components/AnsiLog";
import { statusColor, formatBytes } from "@/lib/utils";

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-xs text-slate-500 uppercase tracking-wide">
        {label}
      </span>
      <span className="text-sm text-slate-200 font-mono break-all">
        {value || "—"}
      </span>
    </div>
  );
}

// ---- Token field with copy and regenerate ------------------------------------

function TokenField({ instanceId, token }: { instanceId: string; token: string }) {
  const [copied, setCopied] = useState(false);
  const [regenerating, setRegenerating] = useState(false);
  const [currentToken, setCurrentToken] = useState(token);

  useEffect(() => {
    setCurrentToken(token);
  }, [token]);

  const handleCopy = () => {
    navigator.clipboard.writeText(currentToken).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  };

  const handleRegenerate = async () => {
    if (
      !confirm(
        "Regenerate the access token? The current token will stop working immediately. You will need to restart the instance for the new token to take effect inside the OpenCode server."
      )
    )
      return;
    setRegenerating(true);
    try {
      const res = await api.instances.regenerateToken(instanceId);
      setCurrentToken(res.access_token);
    } catch (e) {
      alert(e instanceof Error ? e.message : String(e));
    } finally {
      setRegenerating(false);
    }
  };

  return (
    <div className="col-span-2 flex flex-col gap-1.5">
      <span className="text-xs text-slate-500 uppercase tracking-wide">
        Access Token
      </span>
      <div className="flex items-center gap-2 flex-wrap">
        <code className="flex-1 min-w-0 bg-slate-900 rounded-lg px-3 py-2 text-sm font-mono text-green-400 break-all select-all">
          {currentToken}
        </code>
        <div className="flex gap-1.5 shrink-0">
          <button
            onClick={handleCopy}
            className="px-3 py-2 bg-slate-700 hover:bg-slate-600 text-slate-300 text-xs rounded-lg transition-colors"
          >
            {copied ? "Copied!" : "Copy"}
          </button>
          <button
            onClick={handleRegenerate}
            disabled={regenerating}
            className="px-3 py-2 bg-amber-900/60 hover:bg-amber-800 text-amber-300 text-xs rounded-lg transition-colors disabled:opacity-50"
          >
            {regenerating ? "…" : "Regenerate"}
          </button>
        </div>
      </div>
      <p className="text-xs text-slate-600">
        SDK:{" "}
        <code className="text-slate-500">
          opencode attach http://localhost:8080/instance/{instanceId}/ --password {currentToken}
        </code>
      </p>
    </div>
  );
}

// ---- Env vars editor --------------------------------------------------------

interface EnvEntry {
  key: string;
  value: string;
}

function EnvVarsEditor({
  instanceId,
  initialEnvVars,
  onSaved,
}: {
  instanceId: string;
  initialEnvVars: Record<string, string>;
  onSaved: (updated: Instance) => void;
}) {
  const [entries, setEntries] = useState<EnvEntry[]>(() =>
    Object.entries(initialEnvVars).map(([k, v]) => ({ key: k, value: v }))
  );
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [saveOk, setSaveOk] = useState(false);

  // Re-sync rows when the parent fetches an updated instance (e.g. after status poll).
  // Only add/remove rows for keys that appeared/disappeared; don't overwrite
  // values the user is currently editing.
  useEffect(() => {
    setEntries((prev) => {
      const existingKeys = new Set(prev.map((e) => e.key));
      const incomingKeys = new Set(Object.keys(initialEnvVars));
      // Remove rows for keys that no longer exist
      const kept = prev.filter((e) => e.key === "" || incomingKeys.has(e.key));
      // Add rows for new keys (with real values from the API)
      const added = Object.entries(initialEnvVars)
        .filter(([k]) => !existingKeys.has(k))
        .map(([k, v]) => ({ key: k, value: v }));
      return [...kept, ...added];
    });
  }, [initialEnvVars]);

  const addRow = () => setEntries((prev) => [...prev, { key: "", value: "" }]);

  const removeRow = (i: number) =>
    setEntries((prev) => prev.filter((_, idx) => idx !== i));

  const updateRow = (i: number, field: "key" | "value", val: string) =>
    setEntries((prev) =>
      prev.map((e, idx) => (idx === i ? { ...e, [field]: val } : e))
    );

  const handleSave = async () => {
    const envKeyRe = /^[A-Za-z_][A-Za-z0-9_]*$/;
    for (const { key } of entries) {
      if (key === "") continue;
      if (!envKeyRe.test(key)) {
        setSaveError(
          `Invalid key "${key}". Keys must start with a letter or underscore followed by letters, digits, or underscores.`
        );
        return;
      }
    }
    // Deduplicate: last entry for a given key wins
    const envVars: Record<string, string> = {};
    for (const { key, value } of entries) {
      if (key.trim()) envVars[key.trim()] = value;
    }

    setSaving(true);
    setSaveError("");
    setSaveOk(false);
    try {
      const updated = await api.instances.updateEnvVars(instanceId, envVars);
      onSaved(updated);
      // Sync rows to what the server confirms (removes blanks / dupes), with real values
      setEntries(
        Object.entries(updated.env_vars)
          .sort(([a], [b]) => a.localeCompare(b))
          .map(([k, v]) => ({ key: k, value: v }))
      );
      setSaveOk(true);
      setTimeout(() => setSaveOk(false), 3000);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <p className="text-xs text-slate-500">
          Override global Settings vars for this instance. Applied on next
          restart.
        </p>
        <button
          type="button"
          onClick={addRow}
          className="text-xs text-blue-400 hover:text-blue-300 transition-colors whitespace-nowrap ml-4"
        >
          + Add variable
        </button>
      </div>

      {entries.length === 0 ? (
        <p className="text-xs text-slate-600 italic">
          No instance-specific variables.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {entries.map((entry, i) => (
            <div key={i} className="flex gap-2 items-center">
              <input
                type="text"
                value={entry.key}
                onChange={(e) => updateRow(i, "key", e.target.value)}
                placeholder="VARIABLE_NAME"
                className="w-2/5 bg-slate-900 border border-slate-600 rounded-lg px-2.5 py-1.5 text-white placeholder-slate-600 focus:outline-none focus:ring-2 focus:ring-blue-500 text-xs font-mono"
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
              />
              <input
                type="text"
                value={entry.value}
                onChange={(e) => updateRow(i, "value", e.target.value)}
                placeholder="value"
                className="flex-1 bg-slate-900 border border-slate-600 rounded-lg px-2.5 py-1.5 text-white placeholder-slate-600 focus:outline-none focus:ring-2 focus:ring-blue-500 text-xs font-mono"
                spellCheck={false}
                autoCapitalize="off"
                autoCorrect="off"
              />
              <button
                type="button"
                onClick={() => removeRow(i)}
                className="text-slate-500 hover:text-red-400 transition-colors text-lg leading-none px-1"
                aria-label="Remove"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      )}

      {saveError && (
        <p className="text-xs text-red-400 bg-red-950/50 rounded-lg px-3 py-2">
          {saveError}
        </p>
      )}

      <div className="flex items-center gap-3">
        <button
          type="button"
          onClick={handleSave}
          disabled={saving}
          className="px-4 py-1.5 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 text-white text-xs font-medium rounded-lg transition-colors"
        >
          {saving ? "Saving…" : "Save Env Vars"}
        </button>
        {saveOk && (
          <span className="text-xs text-green-400">
            Saved — restart instance to apply.
          </span>
        )}
      </div>
    </div>
  );
}

// ---- Inline log panel -------------------------------------------------------

function LogPanel({ instanceId }: { instanceId: string }) {
  return (
    <AnsiLog
      wsUrl={`/instances/${instanceId}/logs/ws`}
      className="h-64"
    />
  );
}

// ---- Page ------------------------------------------------------------------

export default function InstanceDetailPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [instance, setInstance] = useState<Instance | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState("");

  const currentStatusRef = useRef<string>("");
  useEffect(() => {
    if (instance) currentStatusRef.current = instance.status;
  }, [instance]);

  const cancelledRef = useRef(false);

  // Initial fetch
  useEffect(() => {
    api.instances
      .get(id)
      .then(setInstance)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [id]);

  // Status polling
  useEffect(() => {
    if (loading) return;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (cancelledRef.current) return;
      try {
        const result = await api.instances.pollStatus(id, currentStatusRef.current);
        if (cancelledRef.current) return;
        if (result === null) {
          // unchanged
        } else if ("deleted" in result) {
          cancelledRef.current = true;
          router.push("/");
          return;
        } else {
          // Preserve disk_usage_bytes from prior fetch since poll doesn't include it.
          setInstance((prev) => ({
            ...result,
            disk_usage_bytes: result.disk_usage_bytes ?? prev?.disk_usage_bytes,
          }));
        }
      } catch {
        // ignore
      }
      timer = setTimeout(poll, 10000);
    };
    timer = setTimeout(poll, 10000);
    return () => {
      clearTimeout(timer);
    };
  }, [id, loading, router]);

  const doAction = async (action: "start" | "stop" | "restart") => {
    setBusy(true);
    setActionError("");
    try {
      const updated = await api.instances[action](id);
      // Preserve disk_usage_bytes since action responses don't include it.
      setInstance((prev) => ({
        ...updated,
        disk_usage_bytes: updated.disk_usage_bytes ?? prev?.disk_usage_bytes,
      }));
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!confirm(`Delete instance "${instance?.name}"? This is irreversible.`))
      return;
    setBusy(true);
    try {
      await api.instances.delete(id);
      router.push("/");
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  if (loading)
    return <div className="text-slate-400 text-center py-16">Loading…</div>;

  if (error)
    return (
      <div className="text-red-400 bg-red-950/50 rounded-lg px-4 py-3">
        {error}
        <Link href="/" className="ml-4 underline hover:text-red-300">
          Back
        </Link>
      </div>
    );

  if (!instance) return null;

  const isRunning = instance.status === "running";
  const isStopped =
    instance.status === "stopped" ||
    instance.status === "exited" ||
    instance.status === "created";

  const envVars = instance.env_vars ?? {};

  return (
    <div className="max-w-3xl mx-auto">
      {/* Breadcrumb */}
      <div className="text-sm text-slate-400 mb-6">
        <Link href="/" className="hover:text-white">
          Instances
        </Link>{" "}
        / {instance.name}
      </div>

      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div className="flex items-center gap-3">
          <span
            className={`w-3 h-3 rounded-full ${statusColor(instance.status)}`}
          />
          <h1 className="text-2xl font-bold text-white">{instance.name}</h1>
          <span className="text-sm text-slate-400 capitalize">
            {instance.status}
          </span>
        </div>
        {isRunning && instance.access_token && (
          <a
            href={instanceOpenUrl(instance.id, instance.access_token)}
            target="_blank"
            rel="noopener noreferrer"
            className="px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white text-sm font-medium rounded-lg transition-colors"
          >
            Open Web UI ↗
          </a>
        )}
      </div>

      {/* Actions */}
      <div className="flex gap-2 flex-wrap mb-6">
        {isRunning && (
          <button
            disabled={busy}
            onClick={() => doAction("stop")}
            className="px-4 py-2 text-sm bg-slate-700 hover:bg-slate-600 rounded-lg text-slate-200 disabled:opacity-50"
          >
            Stop
          </button>
        )}
        {isStopped && (
          <button
            disabled={busy}
            onClick={() => doAction("start")}
            className="px-4 py-2 text-sm bg-slate-700 hover:bg-slate-600 rounded-lg text-slate-200 disabled:opacity-50"
          >
            Start
          </button>
        )}
        {(isRunning || isStopped) && (
          <button
            disabled={busy}
            onClick={() => doAction("restart")}
            className="px-4 py-2 text-sm bg-slate-700 hover:bg-slate-600 rounded-lg text-slate-200 disabled:opacity-50"
          >
            Restart
          </button>
        )}
        {isRunning ? (
          <Link
            href={`/instances/${id}/terminal`}
            className="px-4 py-2 text-sm bg-slate-700 hover:bg-slate-600 rounded-lg text-slate-200"
          >
            Terminal
          </Link>
        ) : (
          <span className="px-4 py-2 text-sm bg-slate-800 rounded-lg text-slate-600 cursor-not-allowed">
            Terminal
          </span>
        )}
        <button
          disabled={busy}
          onClick={doDelete}
          className="px-4 py-2 text-sm bg-red-900/60 hover:bg-red-800 rounded-lg text-red-300 disabled:opacity-50"
        >
          Delete
        </button>
      </div>

      {actionError && (
        <div className="text-red-400 bg-red-950/50 rounded-lg px-4 py-2.5 text-sm mb-4">
          {actionError}
        </div>
      )}

      {/* Details */}
      <div className="bg-slate-800 border border-slate-700 rounded-xl p-6 grid grid-cols-2 gap-4 mb-6">
        <Field label="ID" value={instance.id} />
        <Field
          label="Container ID"
          value={instance.container_id?.slice(0, 16) ?? ""}
        />
        <Field label="Work Dir" value={instance.work_dir} />
        <Field label="Status" value={instance.status} />
        <Field
          label="Memory"
          value={
            instance.memory_mb === 0
              ? "unlimited"
              : `${instance.memory_mb} MB`
          }
        />
        <Field
          label="CPU"
          value={
            instance.cpu_cores === 0
              ? "unlimited"
              : `${instance.cpu_cores} cores`
          }
        />
        <Field
          label="Disk Usage"
          value={
            instance.disk_usage_bytes !== undefined
              ? formatBytes(instance.disk_usage_bytes)
              : "..."
          }
        />
        <Field
          label="Created"
          value={new Date(instance.created_at).toLocaleString()}
        />
        <Field
          label="Updated"
          value={new Date(instance.updated_at).toLocaleString()}
        />
        {instance.error_msg && (
          <div className="col-span-2">
            <Field label="Error" value={instance.error_msg} />
          </div>
        )}
        <TokenField instanceId={instance.id} token={instance.access_token} />
      </div>

      {/* Environment Variables editor */}
      <div className="bg-slate-800 border border-slate-700 rounded-xl p-6 mb-6">
        <h2 className="text-sm font-semibold text-slate-300 uppercase tracking-wide mb-1">
          Instance Env Vars
        </h2>
        <p className="text-xs text-slate-500 mb-4">
          These override global Settings env vars for this instance only.
        </p>
        <EnvVarsEditor
          instanceId={instance.id}
          initialEnvVars={envVars}
          onSaved={setInstance}
        />
      </div>

      {/* Logs */}
      <div>
        <h2 className="text-sm font-semibold text-slate-300 uppercase tracking-wide mb-3">
          Live Logs
        </h2>
        <LogPanel instanceId={id} />
      </div>
    </div>
  );
}
