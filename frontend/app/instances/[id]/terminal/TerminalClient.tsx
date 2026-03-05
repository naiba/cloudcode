"use client";

// #37: xterm CSS imported from local npm package (not CDN)
import "@xterm/xterm/css/xterm.css";

import { useEffect, useRef } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { buildWsUrl } from "@/lib/api";

export default function TerminalPage() {
  const { id } = useParams<{ id: string }>();
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<import("@xterm/xterm").Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    // #29: guard against async init completing after unmount
    let aborted = false;
    let fitAddon: import("@xterm/addon-fit").FitAddon;

    const init = async () => {
      const { Terminal } = await import("@xterm/xterm");
      const { FitAddon } = await import("@xterm/addon-fit");
      const { WebLinksAddon } = await import("@xterm/addon-web-links");

      // #29: if cleanup ran before the async import resolved, bail out
      if (aborted || !containerRef.current) return;

      const term = new Terminal({
        theme: {
          background: "#0a0f1a",
          foreground: "#e2e8f0",
          cursor: "#60a5fa",
        },
        fontFamily: "ui-monospace, 'Cascadia Code', 'Fira Code', monospace",
        fontSize: 14,
        cursorBlink: true,
        scrollback: 5000,
      });

      fitAddon = new FitAddon();
      term.loadAddon(fitAddon);
      term.loadAddon(new WebLinksAddon());
      term.open(containerRef.current);
      fitAddon.fit();
      termRef.current = term;

      // Connect WebSocket (buildWsUrl fetches a one-time auth token for cross-origin WS)
      const ws = new WebSocket(await buildWsUrl(`/instances/${id}/terminal/ws`));
      // Assign to ref immediately so the outer cleanup function can close it
      // even if unmount races with this async init completing.
      wsRef.current = ws;
      ws.binaryType = "arraybuffer";

      // If unmount already ran, close the socket we just opened and bail out.
      if (aborted) { ws.close(); return; }

      ws.onopen = () => {
        if (aborted) { ws.close(); return; }
        ws.send(
          JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows })
        );
      };

      ws.onmessage = (e) => {
        if (e.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(e.data));
        } else {
          term.write(e.data as string);
        }
      };

      ws.onclose = () => {
        term.write("\r\n\x1b[31m[Connection closed]\x1b[0m\r\n");
      };

      ws.onerror = () => {
        term.write("\r\n\x1b[31m[Connection error]\x1b[0m\r\n");
      };

      term.onData((data) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(new TextEncoder().encode(data));
        }
      });

      term.onResize(({ cols, rows }) => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "resize", cols, rows }));
        }
      });

      // Handle window resize
      const onResize = () => fitAddon.fit();
      window.addEventListener("resize", onResize);

      return () => {
        window.removeEventListener("resize", onResize);
      };
    };

    const cleanup = init();

    return () => {
      aborted = true; // #29: prevent post-unmount init
      cleanup.then((fn) => fn && fn());
      wsRef.current?.close();
      termRef.current?.dispose();
    };
  }, [id]);

  return (
    <div className="flex flex-col h-[calc(100vh-64px)] -m-6">
      {/* Toolbar */}
      <div className="flex items-center gap-4 px-4 py-2 bg-slate-900 border-b border-slate-700 shrink-0">
        <Link
          href={`/instances/${id}`}
          className="text-slate-400 hover:text-white text-sm transition-colors"
        >
          ← Back
        </Link>
        <span className="text-slate-300 text-sm font-mono">
          Terminal — {id}
        </span>
      </div>

      {/* xterm container */}
      <div ref={containerRef} className="flex-1 overflow-hidden bg-[#0a0f1a]" />

      {/* xterm-specific overrides */}
      <style>{`
        .xterm { height: 100% !important; }
        .xterm-viewport { overflow-y: hidden !important; }
      `}</style>
    </div>
  );
}
