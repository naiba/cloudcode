/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 监控 system prompt 的完整性和变化情况，通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. system[] 实际是单元素数组（一个大字符串），对其做行级 diff 定位具体变化
 * 3. diff 前先用正则将已知的动态内容（日期/时间/数字等）替换为占位符，
 *    避免正常的时间戳变化触发误报
 * 4. 首次调用时发送 "开始监控" 报告，包含被替换的动态内容清单
 * 5. 后续调用做行级对比，仅对真正的结构性变化发送告警
 * 6. session 空闲时发送监控报告
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

  // --- 动态内容替换 ---
  // 每条规则: [正则, 占位符, 人类可读描述]
  // 规则按从具体到通用排序，防止通用规则先吃掉具体模式
  const DYNAMIC_PATTERNS: [RegExp, string, string][] = [
    // omo-env 块中的日期: "Current date: Thu, Feb 26, 2026"
    [
      /Current date:\s*.+$/gm,
      "Current date: {{DATE}}",
      "omo-env 当前日期",
    ],
    // omo-env 块中的时间: "Current time: 04:37:54 AM"
    [
      /Current time:\s*.+$/gm,
      "Current time: {{TIME}}",
      "omo-env 当前时间",
    ],
    // omo-env 块中的时区: "Timezone: UTC"
    [
      /Timezone:\s*\S+/gm,
      "Timezone: {{TZ}}",
      "omo-env 时区",
    ],
    // omo-env 块中的语言: "Locale: en-US"
    [
      /Locale:\s*\S+/gm,
      "Locale: {{LOCALE}}",
      "omo-env 语言区域",
    ],
    // OpenCode 原生注入的日期: "Today's date: Thu Feb 26 2026"
    [
      /Today's date:\s*.+$/gm,
      "Today's date: {{DATE}}",
      "OpenCode 当前日期",
    ],
    // 模型标识行: "You are powered by the model named xxx. The exact model ID is xxx"
    [
      /You are powered by the model named .+$/gm,
      "You are powered by the model named {{MODEL}}. The exact model ID is {{MODEL_ID}}",
      "模型标识",
    ],
    // 精确模型ID: "The exact model ID is song/claude-opus-4-6"
    [
      /The exact model ID is \S+/gm,
      "The exact model ID is {{MODEL_ID}}",
      "精确模型 ID",
    ],
  ]

  interface NormalizeResult {
    text: string
    replacements: { description: string; original: string }[]
  }

  /**
   * 将已知的动态内容替换为占位符。
   * 返回替换后的文本和被替换内容的清单。
   */
  const normalizeText = (rawText: string): NormalizeResult => {
    let text = rawText
    const replacements: { description: string; original: string }[] = []

    for (const [pattern, placeholder, description] of DYNAMIC_PATTERNS) {
      // 重置 lastIndex（因为用 /g 标志）
      pattern.lastIndex = 0
      const matches = text.match(pattern)
      if (matches) {
        for (const match of matches) {
          // 相同描述只记录一次
          if (!replacements.some((r) => r.description === description)) {
            replacements.push({ description, original: match.trim() })
          }
        }
        text = text.replace(pattern, placeholder)
      }
    }

    return { text, replacements }
  }

  // --- 行级 diff ---

  interface LineDiff {
    type: "added" | "removed" | "changed"
    lineNum: number
    oldLine?: string
    newLine?: string
  }

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

  const summarizeBlock = (block: DiffBlock): string => {
    const range =
      block.startLine === block.endLine
        ? `L${block.startLine}`
        : `L${block.startLine}-${block.endLine}`

    const typeLabels: string[] = []
    if (block.types.has("added")) typeLabels.push("新增")
    if (block.types.has("removed")) typeLabels.push("移除")
    if (block.types.has("changed")) typeLabels.push("修改")

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
  // sessionNormalizedText: 上一次 normalize 后的文本（用于 diff）
  const sessionNormalizedText: Map<string, string> = new Map()
  const sessionFirstHash: Map<string, string> = new Map()
  const sessionLastHash: Map<string, string> = new Map()
  const sessionCallCount: Map<string, number> = new Map()
  const sessionTotalDiffLines: Map<string, number> = new Map()
  const sessionDiffSummary: Map<string, string[]> = new Map()
  const reportedSessions: Set<string> = new Set()
  // 已报告过的动态内容替换描述（相同模式全局只报告一次）
  const reportedDynamicPatterns: Set<string> = new Set()

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
      try {
        const isIdle =
          event.type === "session.idle" ||
          (event.type === "session.status" && event.properties?.status?.type === "idle")

        if (isIdle) {
          const sessionID = event.properties?.sessionID
          if (sessionID) {
            await sendSessionReport(sessionID)
          }
        }
      } catch {}
    },

    "experimental.chat.system.transform": async (
      inputData: { sessionID?: string; model: any },
      output: { system: string[] }
    ): Promise<void> => {
      try {
        const sessionID = inputData.sessionID
        if (!sessionID || !output.system || output.system.length === 0) return

        const callCount = (sessionCallCount.get(sessionID) || 0) + 1
        sessionCallCount.set(sessionID, callCount)

        const rawText = output.system.join("\n")
        const { text: normalizedText, replacements } = normalizeText(rawText)
        const fullHash = simpleHash(normalizedText)

        sessionLastHash.set(sessionID, fullHash)

        // --- 将 output.system 中的动态内容替换为占位符 ---
        // 防止 LLM 把实际的时间戳等当作上下文去理解
        if (replacements.length > 0) {
          const normalizedParts = output.system.map((part) => {
            let result = part
            for (const [pattern, placeholder] of DYNAMIC_PATTERNS) {
              pattern.lastIndex = 0
              result = result.replace(pattern, placeholder)
            }
            return result
          })
          output.system.splice(0, output.system.length, ...normalizedParts)
        }

        const prevNormalized = sessionNormalizedText.get(sessionID)

        // 首次调用：记录基线，发送开始通知（含动态内容报告）
        if (prevNormalized === undefined) {
          sessionNormalizedText.set(sessionID, normalizedText)
          sessionFirstHash.set(sessionID, fullHash)

          const lineCount = normalizedText.split("\n").length
          const lines = [
            `🐕 *Prompt Watchdog Active*`,
            `🖥 ${tag}`,
            `🔑 指纹: \`${fullHash}\``,
            `📐 ${rawText.length} 字符 / ${lineCount} 行`,
          ]

          // 报告被替换的动态内容（相同模式只报告一次）
          if (replacements.length > 0) {
            lines.push(``)
            lines.push(`🧹 *已过滤动态内容:*`)
            for (const r of replacements) {
              if (!reportedDynamicPatterns.has(r.description)) {
                reportedDynamicPatterns.add(r.description)
                lines.push(`• ${r.description}: ${escapeMarkdown(truncate(r.original, 80))}`)
              }
            }
          }

          await send(lines.join("\n"))
          return
        }

        // 指纹相同则无需 diff（normalize 后相同 = 结构无变化）
        if (fullHash === simpleHash(prevNormalized)) return

        // 行级 diff（对 normalize 后的文本做 diff，排除已知动态变化）
        const diffs = diffLines(prevNormalized, normalizedText)
        if (diffs.length === 0) return

        const prevTotal = sessionTotalDiffLines.get(sessionID) || 0
        sessionTotalDiffLines.set(sessionID, prevTotal + diffs.length)

        const blocks = groupDiffsIntoBlocks(diffs)

        if (!sessionDiffSummary.has(sessionID)) {
          sessionDiffSummary.set(sessionID, [])
        }
        const summaries = sessionDiffSummary.get(sessionID)!

        const alertLines = [
          `🐕 *Prompt Watchdog Alert*`,
          `🖥 ${tag}`,
          `📊 第 ${callCount} 次调用, ${diffs.length} 行变化, ${blocks.length} 个区块`,
          ``,
        ]

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

        sessionNormalizedText.set(sessionID, normalizedText)
      } catch {}
    },
  }
}
