"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api, Settings, EnvVar, DirFile, AgentsSkill } from "@/lib/api";

// Stable ID for env var rows so React keys don't depend on array index (#34)
let _uidCounter = 0;
function nextUid() { return ++_uidCounter; }
type EnvVarRow = EnvVar & { _uid: number };

// ============================================================
// Helpers
// ============================================================

function SaveBtn({
  busy,
  saved,
  onClick,
}: {
  busy: boolean;
  saved: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      disabled={busy}
      className={`px-4 py-1.5 text-sm font-medium rounded-lg transition-colors disabled:opacity-50 ${
        saved
          ? "bg-green-700 text-green-100"
          : "bg-blue-600 hover:bg-blue-500 text-white"
      }`}
    >
      {busy ? "Saving…" : saved ? "Saved!" : "Save"}
    </button>
  );
}

// ============================================================
// Env Vars editor
// ============================================================

function toRows(vars: EnvVar[]): EnvVarRow[] {
  return vars.map((v) => ({ ...v, _uid: nextUid() }));
}

function EnvVarsEditor({
  initial,
  onSaved,
}: {
  initial: EnvVar[];
  onSaved: () => void;
}) {
  const [vars, setVars] = useState<EnvVarRow[]>(() => toRows(initial));
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  // #33: track whether user has made unsaved edits
  const dirtyRef = useRef(false);

  useEffect(() => {
    // #33: only reset form when there are no unsaved changes
    if (!dirtyRef.current) {
      setVars(toRows(initial));
    }
  }, [initial]);

  const addRow = () => {
    dirtyRef.current = true;
    setVars((v) => [...v, { key: "", value: "", _uid: nextUid() }]);
  };
  const removeRow = (uid: number) => {
    dirtyRef.current = true;
    setVars((v) => v.filter((row) => row._uid !== uid));
  };
  const updateRow = (uid: number, field: "key" | "value", val: string) => {
    dirtyRef.current = true;
    setVars((v) =>
      v.map((row) => (row._uid === uid ? { ...row, [field]: val } : row))
    );
  };

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await api.settings.saveEnv(vars.filter((v) => v.key.trim()));
      setSaved(true);
      dirtyRef.current = false; // #33: mark clean after successful save
      setTimeout(() => setSaved(false), 2000);
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-2">
        {vars.map((v) => (
          <div key={v._uid} className="flex gap-2"> {/* #34: stable uid key */}
            <input
              type="text"
              value={v.key}
              onChange={(e) => updateRow(v._uid, "key", e.target.value)}
              placeholder="KEY"
              className="w-48 bg-slate-900 border border-slate-600 rounded px-2.5 py-1.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500"
            />
            <input
              type="text"
              value={v.value}
              onChange={(e) => updateRow(v._uid, "value", e.target.value)}
              placeholder="value"
              className="flex-1 bg-slate-900 border border-slate-600 rounded px-2.5 py-1.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500"
            />
            <button
              onClick={() => removeRow(v._uid)}
              className="px-2.5 py-1 text-slate-400 hover:text-red-400 text-sm"
            >
              ×
            </button>
          </div>
        ))}
      </div>
      {error && <div className="text-red-400 text-xs">{error}</div>}
      <div className="flex gap-2">
        <button
          onClick={addRow}
          className="px-3 py-1.5 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-300"
        >
          + Add Variable
        </button>
        <SaveBtn busy={busy} saved={saved} onClick={save} />
      </div>
    </div>
  );
}

// ============================================================
// Config file editor
// ============================================================

function ConfigFileEditor({
  relPath,
  initialContent,
  agentsSkill,
}: {
  relPath: string;
  initialContent: string;
  /** When true, saves via the __agents-skill__ dir marker instead of the normal file write path */
  agentsSkill?: boolean;
}) {
  const [content, setContent] = useState(initialContent);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  // #33: track whether user has made unsaved edits
  const dirtyRef = useRef(false);

  useEffect(() => {
    // #33: only reset when there are no unsaved changes
    if (!dirtyRef.current) {
      setContent(initialContent);
    }
  }, [initialContent]);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      if (agentsSkill) {
        await api.settings.saveDirFile({ dir: "__agents-skill__", filename: relPath, content });
      } else {
        await api.settings.saveFile(relPath, content);
      }
      setSaved(true);
      dirtyRef.current = false; // #33: mark clean after save
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-2">
      <textarea
        value={content}
        onChange={(e) => { dirtyRef.current = true; setContent(e.target.value); }}
        rows={16}
        className="w-full bg-slate-950 border border-slate-700 rounded-lg px-3 py-2.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500 resize-y"
        spellCheck={false}
      />
      {error && <div className="text-red-400 text-xs">{error}</div>}
      <div>
        <SaveBtn busy={busy} saved={saved} onClick={save} />
      </div>
    </div>
  );
}

// ============================================================
// Startup script editor
// ============================================================

function StartupScriptEditor({
  initialScript,
  onSaved,
}: {
  initialScript: string;
  onSaved: () => void;
}) {
  const [script, setScript] = useState(initialScript);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  const dirtyRef = useRef(false);

  useEffect(() => {
    if (!dirtyRef.current) {
      setScript(initialScript);
    }
  }, [initialScript]);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await api.settings.saveStartupScript(script);
      setSaved(true);
      dirtyRef.current = false;
      setTimeout(() => setSaved(false), 2000);
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="text-sm text-slate-400">
        Shell script executed automatically on every container startup, before
        OpenCode launches. Runs as <code className="text-slate-300 bg-slate-900 px-1 py-0.5 rounded text-xs">bash</code>.
        Applied to all instances on next restart.
      </div>
      <textarea
        value={script}
        onChange={(e) => {
          dirtyRef.current = true;
          setScript(e.target.value);
        }}
        rows={20}
        placeholder={"#!/bin/bash\n# Example: install a global package\nnpm install -g some-tool\n"}
        className="w-full bg-slate-950 border border-slate-700 rounded-lg px-3 py-2.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500 resize-y"
        spellCheck={false}
      />
      {error && <div className="text-red-400 text-xs">{error}</div>}
      <div>
        <SaveBtn busy={busy} saved={saved} onClick={save} />
      </div>
    </div>
  );
}

// ============================================================
// Directory file manager
// ============================================================

function DirFileManager({
  dir,
  files: initialFiles,
}: {
  dir: "commands" | "agents" | "skills" | "plugins";
  files: DirFile[];
}) {
  const [files, setFiles] = useState<DirFile[]>(initialFiles);
  const [editing, setEditing] = useState<{
    filename: string;
    content: string;
    isNew: boolean;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    setFiles(initialFiles);
  }, [initialFiles]);

  const openNew = () => setEditing({ filename: "", content: "", isNew: true });

  const openEdit = async (f: DirFile) => {
    try {
      const result = await api.settings.readFile(f.rel_path);
      setEditing({ filename: f.name, content: result.content, isNew: false });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const saveEdit = async () => {
    if (!editing) return;
    if (!editing.filename.trim()) {
      setError("Filename is required");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await api.settings.saveDirFile({
        dir,
        filename: editing.filename,
        content: editing.content,
      });
      const updated = await api.settings.listDirFiles(dir);
      setFiles(updated);
      setEditing(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const deleteFile = async (f: DirFile) => {
    if (!confirm(`Delete "${f.name}"?`)) return;
    setBusy(true);
    try {
      await api.settings.deleteDirFile(f.rel_path);
      setFiles((prev) => prev.filter((x) => x.rel_path !== f.rel_path));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      {editing ? (
        <div className="flex flex-col gap-2">
          {editing.isNew && (
            <input
              type="text"
              value={editing.filename}
              onChange={(e) =>
                setEditing((prev) =>
                  prev ? { ...prev, filename: e.target.value } : null
                )
              }
              placeholder="filename.md"
              className="w-64 bg-slate-900 border border-slate-600 rounded px-2.5 py-1.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500"
            />
          )}
          {!editing.isNew && (
            <div className="text-xs font-mono text-slate-400">
              {editing.filename}
            </div>
          )}
          <textarea
            value={editing.content}
            onChange={(e) =>
              setEditing((prev) =>
                prev ? { ...prev, content: e.target.value } : null
              )
            }
            rows={14}
            className="w-full bg-slate-950 border border-slate-700 rounded-lg px-3 py-2.5 text-xs font-mono text-slate-200 focus:outline-none focus:ring-1 focus:ring-blue-500 resize-y"
            spellCheck={false}
          />
          {error && <div className="text-red-400 text-xs">{error}</div>}
          <div className="flex gap-2">
            <button
              disabled={busy}
              onClick={saveEdit}
              className="px-4 py-1.5 text-sm bg-blue-600 hover:bg-blue-500 text-white rounded-lg disabled:opacity-50"
            >
              {busy ? "Saving…" : "Save"}
            </button>
            <button
              onClick={() => {
                setEditing(null);
                setError("");
              }}
              className="px-4 py-1.5 text-sm bg-slate-700 hover:bg-slate-600 text-slate-300 rounded-lg"
            >
              Cancel
            </button>
          </div>
        </div>
      ) : (
        <>
          {files.length === 0 && (
            <div className="text-slate-500 text-sm">No files yet.</div>
          )}
          <div className="flex flex-col gap-1">
            {files.map((f) => (
              <div
                key={f.rel_path}
                className="flex items-center gap-2 group"
              >
                <button
                  onClick={() => openEdit(f)}
                  className="flex-1 text-left text-sm font-mono text-slate-300 hover:text-white py-1 px-2 rounded hover:bg-slate-700 transition-colors"
                >
                  {f.name}
                </button>
                <button
                  onClick={() => deleteFile(f)}
                  disabled={busy}
                  className="opacity-0 group-hover:opacity-100 px-2 py-0.5 text-xs text-red-400 hover:text-red-300 disabled:opacity-50"
                >
                  Delete
                </button>
              </div>
            ))}
          </div>
          {error && <div className="text-red-400 text-xs">{error}</div>}
          <button
            onClick={openNew}
            className="w-fit px-3 py-1.5 text-xs bg-slate-700 hover:bg-slate-600 rounded text-slate-300"
          >
            + New File
          </button>
        </>
      )}
    </div>
  );
}

// ============================================================
// Agents Skills panel
// ============================================================

function AgentsSkillsPanel({
  skills,
  onChanged,
}: {
  skills: AgentsSkill[];
  onChanged: () => void;
}) {
  const [editing, setEditing] = useState<{ relPath: string; skillName: string; content: string } | null>(null);
  const [loadError, setLoadError] = useState("");

  const openEdit = async (s: AgentsSkill) => {
    setLoadError("");
    try {
      const result = await api.settings.readFile(s.rel_path);
      setEditing({ relPath: s.rel_path, skillName: s.skill_name, content: result.content });
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : String(e));
    }
  };

  if (editing) {
    return (
      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-3">
          <button
            onClick={() => setEditing(null)}
            className="text-xs text-slate-400 hover:text-white"
          >
            ← Back
          </button>
          <span className="text-sm font-mono text-slate-300">{editing.skillName} / SKILL.md</span>
        </div>
        <ConfigFileEditor
          key={editing.relPath}
          relPath={editing.relPath}
          initialContent={editing.content}
          agentsSkill
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="text-sm text-slate-400">
        Skills installed via{" "}
        <code className="text-slate-300 bg-slate-900 px-1 py-0.5 rounded text-xs">skills.sh</code>
        . Shared across all instances.
      </div>
      {loadError && <div className="text-red-400 text-xs">{loadError}</div>}
      {skills.length === 0 ? (
        <div className="text-slate-500 text-sm">No skills installed.</div>
      ) : (
        <div className="flex flex-col gap-1">
          {skills.map((s) => (
            <div
              key={s.rel_path}
              className="flex items-center gap-3 group py-1 px-2 rounded hover:bg-slate-700"
            >
              <span className="flex-1 text-sm font-mono text-slate-300">{s.skill_name}</span>
              <span className="text-xs text-slate-500 font-mono">{s.rel_path}</span>
              <button
                onClick={() => openEdit(s)}
                className="opacity-0 group-hover:opacity-100 text-xs text-blue-400 hover:text-blue-300 px-2 py-0.5"
              >
                Edit
              </button>
              <button
                onClick={async () => {
                  if (!confirm(`Delete skill "${s.skill_name}"?`)) return;
                  try {
                    await api.settings.deleteAgentsSkill(s.skill_name);
                    onChanged();
                  } catch (e) {
                    alert(e instanceof Error ? e.message : String(e));
                  }
                }}
                className="opacity-0 group-hover:opacity-100 text-xs text-red-400 hover:text-red-300 px-2 py-0.5"
              >
                Delete
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ============================================================
// Settings page
// ============================================================

type TabKey =
  | "env"
  | "startup-script"
  | "config-files"
  | "commands"
  | "agents"
  | "skills"
  | "plugins"
  | "agents-skills"
  | "directory-mappings";

export default function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [activeTab, setActiveTab] = useState<TabKey>("env");
  // #35: track selected config file by rel_path (stable) instead of numeric index
  const [cfRelPath, setCfRelPath] = useState<string | null>(null);

  const loadSettings = useCallback(async () => {
    try {
      const data = await api.settings.get();
      setSettings(data);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadSettings();
  }, [loadSettings]);

  const tabs: { key: TabKey; label: string }[] = [
    { key: "env", label: "Env Vars" },
    { key: "startup-script", label: "Startup Script" },
    { key: "config-files", label: "Config Files" },
    { key: "commands", label: "Commands" },
    { key: "agents", label: "Agents" },
    { key: "skills", label: "Skills" },
    { key: "plugins", label: "Plugins" },
    { key: "agents-skills", label: "Agents Skills" },
    { key: "directory-mappings", label: "Dir Mappings" },
  ];

  const tabClass = (key: TabKey) =>
    `px-4 py-2 text-sm font-medium rounded-t-lg transition-colors ${
      activeTab === key
        ? "bg-slate-800 text-white border border-b-0 border-slate-700"
        : "text-slate-400 hover:text-white hover:bg-slate-800/50"
    }`;

  if (loading)
    return <div className="text-slate-400 text-center py-16">Loading…</div>;

  if (error)
    return (
      <div className="text-red-400 bg-red-950/50 rounded-lg px-4 py-3">
        {error}
        <button onClick={loadSettings} className="ml-4 underline">
          Retry
        </button>
      </div>
    );

  if (!settings) return null;

  return (
    <div>
      <h1 className="text-2xl font-bold text-white mb-6">Settings</h1>

      {/* Tab bar */}
      <div className="flex gap-1 flex-wrap border-b border-slate-700 mb-0">
        {tabs.map((t) => (
          <button
            key={t.key}
            className={tabClass(t.key)}
            onClick={() => setActiveTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* Panel */}
      <div className="bg-slate-800 border border-slate-700 border-t-0 rounded-b-xl rounded-tr-xl p-6">
        {/* --- Env Vars --- */}
        {activeTab === "env" && (
          <div>
            <div className="text-sm text-slate-400 mb-4">
              Environment variables injected into all new containers at startup.
              Changes take effect after restarting the container.
            </div>
            <EnvVarsEditor
              initial={settings.env_vars}
              onSaved={loadSettings}
            />
          </div>
        )}

        {/* --- Startup Script --- */}
        {activeTab === "startup-script" && (
          <StartupScriptEditor
            initialScript={settings.startup_script ?? ""}
            onSaved={loadSettings}
          />
        )}

        {/* --- Config Files --- */}
        {activeTab === "config-files" && (
          <div>
            <div className="flex gap-2 flex-wrap mb-4">
              {settings.config_files.map((cf) => {
                // #35: default to first file on first render
                const isActive = cfRelPath
                  ? cfRelPath === cf.rel_path
                  : settings.config_files[0]?.rel_path === cf.rel_path;
                return (
                  <button
                    key={cf.rel_path}
                    onClick={() => setCfRelPath(cf.rel_path)}
                    className={`px-3 py-1 text-xs rounded-full transition-colors ${
                      isActive
                        ? "bg-blue-600 text-white"
                        : "bg-slate-700 text-slate-300 hover:bg-slate-600"
                    }`}
                  >
                    {cf.name}
                  </button>
                );
              })}
            </div>
            {(() => {
              // #35: resolve by rel_path instead of index
              const activePath = cfRelPath ?? settings.config_files[0]?.rel_path;
              const cf = settings.config_files.find(
                (f) => f.rel_path === activePath
              );
              if (!cf) return null;
              return (
                <div>
                  <div className="text-xs text-slate-400 mb-3">
                    {cf.hint}
                    <span className="ml-2 font-mono text-slate-500">
                      {cf.rel_path}
                    </span>
                  </div>
                  <ConfigFileEditor
                    key={cf.rel_path}
                    relPath={cf.rel_path}
                    initialContent={cf.content}
                  />
                </div>
              );
            })()}
          </div>
        )}

        {/* --- Dir file tabs --- */}
        {(
          ["commands", "agents", "skills", "plugins"] as Array<
            "commands" | "agents" | "skills" | "plugins"
          >
        ).map(
          (dir) =>
            activeTab === dir && (
              <DirFileManager
                key={dir}
                dir={dir}
                files={settings.dirs[dir]}
              />
            )
        )}

        {/* --- Agents Skills --- */}
        {activeTab === "agents-skills" && (
          <AgentsSkillsPanel
            skills={settings.agents_skills}
            onChanged={loadSettings}
          />
        )}

        {/* --- Directory Mappings --- */}
        {activeTab === "directory-mappings" && (
          <div>
            <div className="text-sm text-slate-400 mb-4">
              Bind mounts injected into every container at creation time.
              Read-only reference — not editable here.
            </div>
            <div className="overflow-auto">
              <table className="w-full text-sm border-collapse">
                <thead>
                  <tr className="border-b border-slate-700">
                    <th className="text-left py-2 px-3 text-slate-400 font-medium">
                      Host Path
                    </th>
                    <th className="text-left py-2 px-3 text-slate-400 font-medium">
                      Container Path
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {settings.directory_mappings.map((m, i) => (
                    <tr
                      key={i}
                      className="border-b border-slate-700/50 hover:bg-slate-700/30"
                    >
                      <td className="py-2 px-3 font-mono text-xs text-slate-300 break-all">
                        {m.host}
                      </td>
                      <td className="py-2 px-3 font-mono text-xs text-slate-300 break-all">
                        {m.container}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="mt-4 text-xs text-slate-500 font-mono">
              Config dir: {settings.config_dir}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
