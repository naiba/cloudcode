/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 监控 system prompt 的完整性和变化情况，通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. system[] 实际是单元素数组（一个大字符串），对其做行级 diff 定位具体变化
 * 3. 首次调用时发送 "开始监控" 报告（含 prompt 指纹和字符数）
 * 4. 后续调用做行级对比，检测变化行并汇总告警
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

  const simpleHash = (str: string): string => {
    let h = 0
    for (let i = 0; i < str.length; i++) {
      const ch = str.charCodeAt(i)
      h = ((h << 5) - h + ch) | 0
    }
    return (h >>> 0).toString(36)
  }

  const send = async (text: string) => {
    try {
      // Telegram 单条消息上限 4096 字符，截断保护
      const safeText = text.length > 4000 ? text.slice(0, 4000) + "\n...(truncated)" : text
      await fetch(`https://api.telegram.org/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text: safeText, parse_mode: "Markdown" }),
      })
    } catch {}
  }

  // --- 行级 diff ---

  interface LineDiff {
    type: "added" | "removed" | "changed"
    lineNum: number
    oldLine?: string
    newLine?: string
  }

  /**
   * 简易行级 diff：逐行对比旧/新文本，返回变化的行。
   * 不是完整 LCS diff，但对于 system prompt 这种大部分不变、
   * 只有少量动态注入的场景足够高效准确。
   */
  const diffLines = (oldText: string, newText: string): LineDiff[] => {
    const oldLines = oldText.split("\n")
    const newLines = newText.split("\n")
    const diffs: LineDiff[] = []
    const maxLen = Math.max(oldLines.length, newLines.length)

    for (let i = 0; i < maxLen; i++) {
      const oldLine = i < oldLines.length ? oldLines[i] : undefined
      const newLine = i < newLines.length ? newLines[i] : undefined

      if (oldLine === newLine) continue

      if (oldLine === undefined) {
        diffs.push({ type: "added", lineNum: i + 1, newLine })
      } else if (newLine === undefined) {
        diffs.push({ type: "removed", lineNum: i + 1, oldLine })
      } else {
        diffs.push({ type: "changed", lineNum: i + 1, oldLine, newLine })
      }
    }
    return diffs
  }

  /**
   * 将连续变化行合并为区块，便于摘要展示。
   * 例如第 10-13 行连续变化 → 合并为一个区块。
   */
  interface DiffBlock {
    startLine: number
    endLine: number
    types: Set<LineDiff["type"]>
    lines: LineDiff[]
  }

  const groupDiffsIntoBlocks = (diffs: LineDiff[]): DiffBlock[] => {
    if (diffs.length === 0) return []
    const blocks: DiffBlock[] = []
    let current: DiffBlock = {
      startLine: diffs[0].lineNum,
      endLine: diffs[0].lineNum,
      types: new Set([diffs[0].type]),
      lines: [diffs[0]],
    }

    for (let i = 1; i < diffs.length; i++) {
      const diff = diffs[i]
      // 连续行（间隔 ≤ 2 行）合并为同一区块
      if (diff.lineNum - current.endLine <= 2) {
        current.endLine = diff.lineNum
        current.types.add(diff.type)
        current.lines.push(diff)
      } else {
        blocks.push(current)
        current = {
          startLine: diff.lineNum,
          endLine: diff.lineNum,
          types: new Set([diff.type]),
          lines: [diff],
        }
      }
    }
    blocks.push(current)
    return blocks
  }

  /**
   * 生成区块的摘要文本。
   */
  const summarizeBlock = (block: DiffBlock): string => {
    const range =
      block.startLine === block.endLine
        ? `L${block.startLine}`
        : `L${block.startLine}-${block.endLine}`

    const typeLabels: string[] = []
    if (block.types.has("added")) typeLabels.push("新增")
    if (block.types.has("removed")) typeLabels.push("移除")
    if (block.types.has("changed")) typeLabels.push("修改")

    // 取区块中第一个有内容的变化行作为预览
    const previewLine = block.lines.find((l) => l.newLine || l.oldLine)
    const preview = previewLine
      ? truncate((previewLine.newLine ?? previewLine.oldLine ?? "").trim(), 120)
      : ""

    return `${range} [${typeLabels.join("+")}] ${preview}`
  }

  const truncate = (str: string, maxLen: number): string => {
    if (str.length <= maxLen) return str
    return str.slice(0, maxLen) + "..."
  }

  const escapeMarkdown = (str: string): string => {
    return str.replace(/[_*[\]()~`>#+\-=|{}.!\\]/g, "\\$&")
  }

  // --- 状态存储 ---
  // sessionPrevText: 每个 session 上一次的完整 system prompt 文本
  const sessionPrevText: Map<string, string> = new Map()
  // sessionFirstHash: 首次完整指纹
  const sessionFirstHash: Map<string, string> = new Map()
  // sessionLastHash: 最新完整指纹
  const sessionLastHash: Map<string, string> = new Map()
  const sessionCallCount: Map<string, number> = new Map()
  // sessionTotalDiffLines: 累计变化行数
  const sessionTotalDiffLines: Map<string, number> = new Map()
  // sessionDiffSummary: 收集所有变化区块摘要（用于结束报告）
  const sessionDiffSummary: Map<string, string[]> = new Map()
  const reportedSessions: Set<string> = new Set()

  const sendSessionReport = async (sessionID: string) => {
    if (reportedSessions.has(sessionID)) return
    if (!sessionCallCount.has(sessionID)) return
    reportedSessions.add(sessionID)

    const calls = sessionCallCount.get(sessionID) || 0
    const totalDiffLines = sessionTotalDiffLines.get(sessionID) || 0
    const firstHash = sessionFirstHash.get(sessionID) || "?"
    const lastHash = sessionLastHash.get(sessionID) || "?"
    const drifted = firstHash !== lastHash
    const summaries = sessionDiffSummary.get(sessionID) || []

    const statusEmoji = totalDiffLines === 0 ? "✅" : drifted ? "⚠️" : "🔄"
    const statusText =
      totalDiffLines === 0
        ? "System prompt 无变化"
        : drifted
          ? `System prompt 发生漂移 (${totalDiffLines} 行变化)`
          : `System prompt 有临时波动但最终一致`

    const lines = [
      `🐕 *Prompt Watchdog Report*`,
      `🖥 ${tag}`,
      `📊 共 ${calls} 次 LLM 调用`,
      `🔑 指纹: \`${firstHash}\` → \`${lastHash}\``,
      `${statusEmoji} ${statusText}`,
    ]

    // 附上变化摘要（最多 10 条）
    if (summaries.length > 0) {
      lines.push(``)
      lines.push(`📝 *变化摘要:*`)
      const shown = summaries.slice(-10)
      for (const s of shown) {
        lines.push(`• ${escapeMarkdown(s)}`)
      }
      if (summaries.length > 10) {
        lines.push(`... 及其他 ${summaries.length - 10} 处`)
      }
    }

    await send(lines.join("\n"))
  }

  return {
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

    "experimental.chat.system.transform": async (
      inputData: { sessionID?: string; model: any },
      output: { system: string[] }
    ): Promise<void> => {
      const sessionID = inputData.sessionID
      if (!sessionID || !output.system || output.system.length === 0) return

      const callCount = (sessionCallCount.get(sessionID) || 0) + 1
      sessionCallCount.set(sessionID, callCount)

      // system[] 实际是单元素数组，拼接以防万一
      const currentText = output.system.join("\n")
      const fullHash = simpleHash(currentText)

      sessionLastHash.set(sessionID, fullHash)

      const prevText = sessionPrevText.get(sessionID)

      // 首次调用：记录基线，发送开始通知
      if (prevText === undefined) {
        sessionPrevText.set(sessionID, currentText)
        sessionFirstHash.set(sessionID, fullHash)

        const lineCount = currentText.split("\n").length
        const lines = [
          `🐕 *Prompt Watchdog Active*`,
          `🖥 ${tag}`,
          `🔑 指纹: \`${fullHash}\``,
          `📐 ${currentText.length} 字符 / ${lineCount} 行`,
        ]
        await send(lines.join("\n"))
        return
      }

      // 指纹相同则无需 diff
      const prevHash = simpleHash(prevText)
      if (fullHash === prevHash) return

      // 行级 diff
      const diffs = diffLines(prevText, currentText)
      if (diffs.length === 0) return

      // 累计统计
      const prevTotal = sessionTotalDiffLines.get(sessionID) || 0
      sessionTotalDiffLines.set(sessionID, prevTotal + diffs.length)

      // 合并为区块
      const blocks = groupDiffsIntoBlocks(diffs)

      // 收集摘要
      if (!sessionDiffSummary.has(sessionID)) {
        sessionDiffSummary.set(sessionID, [])
      }
      const summaries = sessionDiffSummary.get(sessionID)!

      // 构建告警消息
      const alertLines = [
        `🐕 *Prompt Watchdog Alert*`,
        `🖥 ${tag}`,
        `📊 第 ${callCount} 次调用, ${diffs.length} 行变化, ${blocks.length} 个区块`,
        ``,
      ]

      // 每个区块输出摘要（最多展示 5 个区块）
      const shownBlocks = blocks.slice(0, 5)
      for (const block of shownBlocks) {
        const summary = summarizeBlock(block)
        summaries.push(summary)
        alertLines.push(`• ${escapeMarkdown(summary)}`)
      }
      if (blocks.length > 5) {
        alertLines.push(`... 及其他 ${blocks.length - 5} 个区块`)
      }

      await send(alertLines.join("\n"))

      // 更新基线
      sessionPrevText.set(sessionID, currentText)
    },
  }
}
