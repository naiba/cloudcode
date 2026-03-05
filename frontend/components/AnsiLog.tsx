"use client";

// AnsiLog renders ANSI-escaped log output from a WebSocket stream.
// ansi-to-html converts ANSI escape codes to <span style="color:..."> markup.
// DOMPurify sanitizes the resulting HTML before it is written to the DOM,
// preventing XSS from crafted ANSI sequences or library output.

import { useEffect, useRef, useState } from "react";
import AnsiToHtml from "ansi-to-html";
import DOMPurify from "dompurify";
import { buildWsUrl } from "@/lib/api";

interface Props {
  wsUrl: string;
  className?: string;
}

export default function AnsiLog({ wsUrl, className }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [resolvedUrl, setResolvedUrl] = useState<string | null>(null);

  // Per-instance converter — stream:true keeps ANSI state across chunks (#28)
  const converterRef = useRef<AnsiToHtml | null>(null);
  if (!converterRef.current) {
    converterRef.current = new AnsiToHtml({
      fg: "#a3e635",
      bg: "#020617",
      newline: false,
      escapeXML: true,
      stream: true,
    });
  }

  // Resolve the authenticated WS URL (may fetch a one-time token) before connecting.
  useEffect(() => {
    let cancelled = false;
    buildWsUrl(wsUrl).then((url) => {
      if (!cancelled) setResolvedUrl(url);
    });
    return () => { cancelled = true; };
  }, [wsUrl]);

  useEffect(() => {
    if (!resolvedUrl) return;
    const container = containerRef.current;
    if (!container) return;

    // Show connecting placeholder
    const placeholder = document.createElement("span");
    placeholder.style.color = "#64748b";
    placeholder.textContent = "Connecting…";
    container.appendChild(placeholder);

    const ws = new WebSocket(resolvedUrl);
    ws.binaryType = "arraybuffer"; // ensure binary frames arrive as ArrayBuffer not Blob

    ws.onopen = () => {
      placeholder.remove();
    };

    ws.onmessage = (e) => {
      if (!container) return;
      const chunk = e.data instanceof ArrayBuffer
        ? new TextDecoder().decode(e.data)
        : (e.data as string);

      // Convert ANSI → HTML string, then sanitize before injecting into DOM.
      const raw = converterRef.current!.toHtml(chunk);
      const html = DOMPurify.sanitize(raw, {
        ALLOWED_TAGS: ["span"],
        ALLOWED_ATTR: ["style"],
      });

      const temp = document.createElement("div");
      temp.innerHTML = html;
      while (temp.firstChild) {
        container.appendChild(temp.firstChild);
      }

      // Auto-scroll to bottom
      container.scrollTop = container.scrollHeight;
    };

    ws.onerror = () => {
      if (!container) return;
      const errSpan = document.createElement("span");
      errSpan.style.color = "#f87171";
      errSpan.textContent = "[connection error]\n";
      container.appendChild(errSpan);
      container.scrollTop = container.scrollHeight;
    };

    ws.onclose = () => {
      // nothing — the WS closing is expected when navigating away
    };

    return () => {
      ws.close();
      if (container) container.innerHTML = "";
      // Reset the ANSI converter so stale escape-sequence state doesn't bleed
      // into the next connection.
      converterRef.current = new AnsiToHtml({
        fg: "#a3e635",
        bg: "#020617",
        newline: false,
        escapeXML: true,
        stream: true,
      });
    };
  }, [resolvedUrl]);

  return (
    <div
      ref={containerRef}
      className={`overflow-auto font-mono text-xs whitespace-pre-wrap bg-slate-950 rounded-lg p-4 ${className ?? ""}`}
    />
  );
}
