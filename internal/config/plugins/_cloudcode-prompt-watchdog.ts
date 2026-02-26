/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 监控 system prompt 的完整性和变化情况，通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. 将 system prompt 按段分割并计算 hash
 * 3. 首次调用时发送 "开始监控" 报告（含 prompt 指纹和段落数）
 * 4. 后续调用对比段落 hash，检测频繁变化的段落并告警
 * 5. session 空闲时通过 event hook 发送 "监控报告"
 *
 * 环境变量：
 * - CC_TELEGRAM_BOT_TOKEN: Telegram Bot API token
 * - CC_TELEGRAM_CHAT_ID: 目标 chat/group ID
 * - CC_PROMPT_WATCHDOG_DISABLED: 设为 "true" 可禁用此 plugin
 */

export const CloudCodePromptWatchdog = async (input: any) => {
  const token = process.env.CC_TELEGRAM_BOT_TOKEN
  const chatId = process.env.CC_TELEGRAM_CHAT_ID
  const disabled = process.env.CC_PROMPT_WATCHDOG_DISABLED === "true"

  if (!token || !chatId || disabled) return {}

  const instanceName = process.env.CC_INSTANCE_NAME || ""
  const host = process.env.HOSTNAME || "unknown"
  const tag = instanceName ? `\`${instanceName}\`` : `\`${host}\``

  // --- 简易 hash 函数（无需引入 crypto 依赖） ---
  const simpleHash = (str: string): string => {
    let h = 0
    for (let i = 0; i < str.length; i++) {
      const ch = str.charCodeAt(i)
      h = ((h << 5) - h + ch) | 0
    }
    return (h >>> 0).toString(36)
  }

  // --- Telegram 发送（与 _cloudcode-telegram.ts 保持一致的模式） ---
  const send = async (text: string) => {
    try {
      await fetch(`https://api.telegram.org/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text, parse_mode: "Markdown" }),
      })
    } catch {}
  }

  // --- 状态存储 ---
  // sessionSegments: 每个 session 上一次的 system prompt 段落 hash 列表
  const sessionSegments: Map<string, string[]> = new Map()
  // sessionFirstHash: 每个 session 首次的完整 prompt hash（用于最终报告对比）
  const sessionFirstHash: Map<string, string> = new Map()
  // changeCounter: 记录每个段落 hash 变化的次数，key = "sessionID:segmentIndex"
  const changeCounter: Map<string, number> = new Map()
  // notifiedSegments: 已通知过的段落，避免重复告警，key = "sessionID:segmentIndex"
  const notifiedSegments: Set<string> = new Set()
  // sessionCallCount: 每个 session 的调用次数
  const sessionCallCount: Map<string, number> = new Map()
  // sessionTotalChanges: 每个 session 累计变化段落数
  const sessionTotalChanges: Map<string, number> = new Map()
  // sessionLastHash: 每个 session 最后一次的完整 prompt hash（用于最终报告对比）
  const sessionLastHash: Map<string, string> = new Map()
  // reportedSessions: 已发送过结束报告的 session，避免重复
  const reportedSessions: Set<string> = new Set()

  // 频繁变化阈值：同一段落在一个 session 内变化超过此次数则告警
  const CHANGE_THRESHOLD = 2

  /**
   * 将 system prompt 分割成有意义的段落。
   */
  const segmentize = (systemParts: string[]): string[] => {
    const fullText = systemParts.join("\n---PART_BOUNDARY---\n")
    const segments = fullText.split(/\n{3,}/)
    return segments.map((s) => s.trim()).filter((s) => s.length > 0)
  }

  const diffSegments = (
    prev: string[],
    curr: string[],
    prevHashes: string[],
    currHashes: string[]
  ): { index: number; type: "changed" | "added" | "removed"; preview: string }[] => {
    const changes: { index: number; type: "changed" | "added" | "removed"; preview: string }[] = []
    const maxLen = Math.max(prevHashes.length, currHashes.length)
    for (let i = 0; i < maxLen; i++) {
      if (i >= prevHashes.length) {
        changes.push({ index: i, type: "added", preview: truncate(curr[i], 200) })
      } else if (i >= currHashes.length) {
        changes.push({ index: i, type: "removed", preview: truncate(prev[i], 200) })
      } else if (prevHashes[i] !== currHashes[i]) {
        changes.push({ index: i, type: "changed", preview: truncate(curr[i], 200) })
      }
    }
    return changes
  }

  const truncate = (str: string, maxLen: number): string => {
    if (str.length <= maxLen) return str
    return str.slice(0, maxLen) + "..."
  }

  const escapeMarkdown = (str: string): string => {
    return str.replace(/[_*[\]()~`>#+\-=|{}.!\\]/g, "\\$&")
  }

  /**
   * 发送 session 结束时的监控报告。
   * 汇总本次 session 内 system prompt 的变化情况。
   */
  const sendSessionReport = async (sessionID: string) => {
    if (reportedSessions.has(sessionID)) return
    // 仅对 watchdog 实际监控过的 session 发报告
    if (!sessionCallCount.has(sessionID)) return
    reportedSessions.add(sessionID)

    const calls = sessionCallCount.get(sessionID) || 0
    const totalChanges = sessionTotalChanges.get(sessionID) || 0
    const firstHash = sessionFirstHash.get(sessionID) || "?"
    const lastHash = sessionLastHash.get(sessionID) || "?"
    const drifted = firstHash !== lastHash

    const statusEmoji = totalChanges === 0 ? "✅" : drifted ? "⚠️" : "🔄"
    const statusText =
      totalChanges === 0
        ? "System prompt 无变化"
        : drifted
          ? `System prompt 发生漂移 (${totalChanges} 处变化)`
          : `System prompt 有临时波动但最终一致`

    const lines = [
      `🐕 *Prompt Watchdog Report*`,
      `🖥 ${tag}`,
      `📊 共 ${calls} 次 LLM 调用`,
      `🔑 指纹: \`${firstHash}\` → \`${lastHash}\``,
      `${statusEmoji} ${statusText}`,
    ]

    await send(lines.join("\n"))
  }

  return {
    // --- session 事件：在 session idle 时发送结束报告 ---
    event: async ({ event }: { event: { type: string; properties: any } }) => {
      const isIdle =
        event.type === "session.idle" ||
        (event.type === "session.status" && event.properties?.status?.type === "idle")

      if (isIdle) {
        const sessionID = event.properties?.sessionID
        if (sessionID) {
          await sendSessionReport(sessionID)
        }
      }
    },

    // --- system prompt 变化检测 ---
    "experimental.chat.system.transform": async (
      inputData: { sessionID?: string; model: any },
      output: { system: string[] }
    ): Promise<void> => {
      const sessionID = inputData.sessionID
      if (!sessionID || !output.system || output.system.length === 0) return

      const callCount = (sessionCallCount.get(sessionID) || 0) + 1
      sessionCallCount.set(sessionID, callCount)

      const segments = segmentize(output.system)
      const hashes = segments.map(simpleHash)
      const fullHash = simpleHash(hashes.join(":"))

      // 记录最新 hash（用于结束报告对比）
      sessionLastHash.set(sessionID, fullHash)

      const prevHashes = sessionSegments.get(sessionID)

      // 首次调用：记录基线，发送 "开始监控" 通知
      if (!prevHashes) {
        sessionSegments.set(sessionID, hashes)
        sessionFirstHash.set(sessionID, fullHash)

        const totalChars = output.system.reduce((sum, s) => sum + s.length, 0)
        const lines = [
          `🐕 *Prompt Watchdog Active*`,
          `🖥 ${tag}`,
          `🔑 指纹: \`${fullHash}\``,
          `📐 ${segments.length} 段 / ${totalChars} 字符 / ${output.system.length} parts`,
        ]
        await send(lines.join("\n"))
        return
      }

      // 对比变化
      const prevSegments = segmentize(output.system)
      const changes = diffSegments(prevSegments, segments, prevHashes, hashes)

      if (changes.length > 0) {
        // 累计变化数
        const prev = sessionTotalChanges.get(sessionID) || 0
        sessionTotalChanges.set(sessionID, prev + changes.length)

        for (const change of changes) {
          const counterKey = `${sessionID}:${change.index}`
          const count = (changeCounter.get(counterKey) || 0) + 1
          changeCounter.set(counterKey, count)

          // 达到阈值且未通知过 → 发送告警
          if (count >= CHANGE_THRESHOLD && !notifiedSegments.has(counterKey)) {
            notifiedSegments.add(counterKey)

            const typeLabel =
              change.type === "changed" ? "🔄 内容变化" :
              change.type === "added" ? "➕ 新增段落" :
              "➖ 移除段落"

            const lines = [
              `🐕 *Prompt Watchdog Alert*`,
              `🖥 ${tag}`,
              `📊 Session 内第 ${callCount} 次 LLM 调用`,
              `${typeLabel} (段落 #${change.index + 1}, 已变化 ${count} 次)`,
              ``,
              `\`\`\``,
              escapeMarkdown(change.preview),
              `\`\`\``,
            ]

            await send(lines.join("\n"))
          }
        }
      }

      // 更新基线
      sessionSegments.set(sessionID, hashes)
    },
  }
}
