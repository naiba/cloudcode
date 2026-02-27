/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 监控 system prompt 的完整性和变化情况，通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. system[] 实际是单元素数组（一个大字符串），对其做行级处理
 * 3. 时间行处理（不做变化检测）：
 *    - 每次 hook 调用时扫描所有行
 *    - 判断行是否包含时间/日期/时间戳（正则匹配）
 *    - 如果包含且去除时间内容后剩余文本 ≤ 30 字符 → 直接删除该行，通知一次（带行号+内容）
 *    - 如果包含但去除时间内容后剩余文本 > 30 字符 → 不删除，但告警一次（带时间内容+前后上下文+行号）
 *    - 按 modelID 记录已通知的行内容签名，相同签名只通知一次
 * 4. 结构变化检测：全局基线 diff，检测非时间行的真正变化
 * 5. 首次调用时发送 "开始监控" 报告
 * 6. session 空闲时发送监控总结报告
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

  const debugLogPath = process.env.CC_WATCHDOG_DEBUG_LOG || ""
  const send = async (text: string) => {
    try {
      const safeText = text.length > 4000 ? text.slice(0, 4000) + "\n...(truncated)" : text
      // 调试模式：写入文件以便验证通知内容
      if (debugLogPath) {
        const fs = await import("fs")
        const ts = new Date().toISOString()
        fs.appendFileSync(debugLogPath, `\n--- ${ts} ---\n${safeText}\n`)
      }
      await fetch(`https://api.telegram.org/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text: safeText, parse_mode: "Markdown" }),
      })
    } catch {}
  }

  // --- 时间行判定 ---
  // BUG GUARD: 正则顺序很重要 — 长模式（ISO datetime）必须在短模式（date、time）之前，
  // 否则短模式会先匹配局部字符串，导致长模式无法完整匹配
  const temporalPatterns: RegExp[] = [
    // ISO 日期时间: 2026-02-26T04:37:54Z, 2026-02-26 04:37:54+08:00 等
    /\d{4}-\d{2}-\d{2}[T ]\d{1,2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?/g,
    // 时间格式: 04:37:54 AM, 16:30:00, 4:37 PM 等
    /\d{1,2}:\d{2}(:\d{2})?\s*(AM|PM|am|pm)?/g,
    // 日期格式: 2026-02-26, 02/26/2026
    /\d{4}[-/]\d{1,2}[-/]\d{1,2}/g,
    /\d{1,2}[-/]\d{1,2}[-/]\d{4}/g,
    // 英文星期: 只匹配 3 字母缩写（后跟非字母）或完整拼写
    // BUG GUARD: 不能用 \b(Mon)\w* 会误匹配 Monkey/Monitor 等普通单词
    /\b(Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday|Mon|Tue|Wed|Thu|Fri|Sat|Sun)\b,?/gi,
    // 英文月份: 只匹配 3 字母缩写（后跟非字母）或完整拼写
    // BUG GUARD: 不能用 \b(Mar)\w* 会误匹配 Marking/Market 等，只允许精确缩写或完整月份名
    // BUG GUARD: 不包含 May — 与英文助动词 may 完全同形，无法区分，误报率极高
    /\b(January|February|March|April|June|July|August|September|October|November|December|Jan|Feb|Mar|Apr|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\b,?/gi,
    // 4位年份（独立出现）
    /\b(19|20)\d{2}\b/g,
  ]

  interface TemporalAnalysis {
    hasTemporal: boolean
    /** 匹配到的时间/日期/时间戳片段 */
    matchedFragments: string[]
    /** 去除时间内容后的剩余文本 */
    strippedText: string
    /** 剩余文本是否 ≤ 30 字符（短行，应删除） */
    isShortLine: boolean
  }

  /**
   * 分析一行是否包含时间/日期/时间戳，返回匹配详情
   *
   * BUG GUARD: 必须先检查是否有时间匹配（hasTemporal），再检查剩余长度。
   * 如果跳过检查，任何 ≤ 30 字符的短行都会被误删。
   */
  const analyzeTemporalLine = (line: string): TemporalAnalysis => {
    let stripped = line
    let hasTemporal = false
    const matchedFragments: string[] = []
    for (const pattern of temporalPatterns) {
      // BUG GUARD: 必须重置 lastIndex，因为带 /g 的正则在 test/exec 后会保留状态
      pattern.lastIndex = 0
      const matches = stripped.match(pattern)
      if (matches) {
        hasTemporal = true
        matchedFragments.push(...matches)
        stripped = stripped.replace(pattern, "")
      }
    }
    return {
      hasTemporal,
      matchedFragments,
      strippedText: stripped.trim(),
      isShortLine: hasTemporal && stripped.trim().length <= 30,
    }
  }

  // --- 行级 diff（用于结构变化检测）---

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

  // Telegram Markdown (legacy) 模式只需转义 _ * ` [
  // BUG GUARD: 不要过度转义，否则 model 名中的 - 会变成 \- 影响可读性
  const escapeMarkdown = (str: string): string => {
    return str.replace(/[_*`\[\]]/g, "\\$&")
  }

  // --- 状态存储 ---

  // === 全局：按 modelID 记录已通知的行签名，相同签名不重复通知 ===
  // BUG GUARD: key 是 "行内容 trim 后的签名" 而非行号，因为行号可能因上方行增删而漂移，
  // 用内容签名更稳定（同一时间行内容结构不变，只有具体数值变化，trim 后签名一致）
  // 短行（删除）和长行（告警）使用不同前缀避免签名碰撞：
  //   短行签名: "removed:" + signature
  //   长行签名: "temporal-alert:" + signature
  const notifiedTemporalLines: Map<string, Set<string>> = new Map()

  // === 全局基线（按 modelID，用于 diff 过滤后的 prompt）===
  const globalPrevFilteredText: Map<string, string> = new Map()

  // === Per-session 状态（用于 Report 统计）===
  // trackKey = "sessionID:modelID"
  const firstHashMap: Map<string, string> = new Map()
  const lastHashMap: Map<string, string> = new Map()
  const callCountMap: Map<string, number> = new Map()
  const totalDiffLinesMap: Map<string, number> = new Map()
  const removedLineCountMap: Map<string, number> = new Map()
  const temporalAlertCountMap: Map<string, number> = new Map()
  const diffSummaryMap: Map<string, string[]> = new Map()

  // 用于结束报告：记录每个 session 涉及的所有 trackKey
  const sessionTrackKeys: Map<string, Set<string>> = new Map()
  const reportedSessions: Set<string> = new Set()

  const buildTrackKey = (sessionID: string, modelID: string): string => {
    return `${sessionID}:${modelID}`
  }

  /**
   * 生成时间行的内容签名：去除具体时间数值后的结构指纹
   * 例如 "  Current date: Thu, Feb 26, 2026" → "current date:"
   * 这样即使日期变了，签名仍然相同，避免重复通知
   */
  const temporalLineSignature = (line: string): string => {
    let sig = line
    for (const pattern of temporalPatterns) {
      pattern.lastIndex = 0
      sig = sig.replace(pattern, "")
    }
    return sig.trim().toLowerCase()
  }

  const sendSessionReport = async (sessionID: string) => {
    try {
      if (reportedSessions.has(sessionID)) return
      const trackKeys = sessionTrackKeys.get(sessionID)
      if (!trackKeys || trackKeys.size === 0) return
      reportedSessions.add(sessionID)

      const lines = [
        `🐕 *Prompt Watchdog Report* ${tag}`,
      ]

      // 按 agent 分别汇总
      for (const key of trackKeys) {
        const modelID = key.split(":").slice(1).join(":")
        const calls = callCountMap.get(key) || 0
        const totalDiffLines = totalDiffLinesMap.get(key) || 0
        const removedCount = removedLineCountMap.get(key) || 0
        const alertCount = temporalAlertCountMap.get(key) || 0
        const firstHash = firstHashMap.get(key) || "?"
        const lastHash = lastHashMap.get(key) || "?"
        const drifted = firstHash !== lastHash
        const summaries = diffSummaryMap.get(key) || []

        const statusEmoji = totalDiffLines === 0 && removedCount === 0 && alertCount === 0 ? "✅" : drifted ? "⚠️" : "🔄"
        const statusParts: string[] = []
        if (totalDiffLines > 0) statusParts.push(`${totalDiffLines} 行结构变化`)
        if (removedCount > 0) statusParts.push(`${removedCount} 行时间过滤`)
        if (alertCount > 0) statusParts.push(`${alertCount} 行时间告警`)
        const statusText = statusParts.length > 0 ? statusParts.join(", ") : "无变化"

        lines.push(`${statusEmoji} ${escapeMarkdown(modelID)} ×${calls} ${drifted ? `'${firstHash}'→'${lastHash}'` : `'${firstHash}'`} ${statusText}`)

        if (summaries.length > 0) {
          const shown = summaries.slice(-3)
          for (const s of shown) {
            lines.push(`  • ${escapeMarkdown(s)}`)
          }
          if (summaries.length > 3) {
            lines.push(`  ... 及其他 ${summaries.length - 3} 处`)
          }
        }
      }

      await send(lines.join("\n"))
    } catch {}
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
        // modelID 用于区分同一 session 内不同 agent 的 prompt
        const modelID = inputData.model?.id || "unknown"
        if (!sessionID || !output.system || output.system.length === 0) return

        // 调试模式：记录 hook 元信息到文件
        if (debugLogPath) {
          const fs = await import("fs")
          const ts = new Date().toISOString()
          fs.appendFileSync(debugLogPath, `\n[HOOK] ${ts} model=${modelID} len=${output.system[0]?.length || 0}\n`)
        }
        const trackKey = buildTrackKey(sessionID, modelID)

        // 记录 session → trackKey 映射
        if (!sessionTrackKeys.has(sessionID)) {
          sessionTrackKeys.set(sessionID, new Set())
        }
        sessionTrackKeys.get(sessionID)!.add(trackKey)

        const callCount = (callCountMap.get(trackKey) || 0) + 1
        callCountMap.set(trackKey, callCount)

        const rawText = output.system.join("\n")
        const rawLines = rawText.split("\n")

        // === 步骤 1：扫描所有行，区分短行（删除）和长行（告警）===
        const removedLines: { lineNum: number; content: string; signature: string }[] = []
        // 长行告警：包含时间/日期/时间戳但剩余文本 > 30 字符，不删除但需告警
        const temporalAlertLines: { lineNum: number; content: string; signature: string; matchedFragments: string[] }[] = []

        const filteredLines = rawLines.filter((line, i) => {
          const analysis = analyzeTemporalLine(line)
          if (!analysis.hasTemporal) return true // 无时间内容，保留

          const signature = temporalLineSignature(line)
          if (analysis.isShortLine) {
            // 剩余 ≤ 30 字符：删除该行
            removedLines.push({
              lineNum: i + 1,
              content: line.trim(),
              signature,
            })
            return false
          } else {
            // 剩余 > 30 字符：保留该行，但记录为告警
            temporalAlertLines.push({
              lineNum: i + 1,
              content: line.trim(),
              signature,
              matchedFragments: analysis.matchedFragments,
            })
            return true
          }
        })

        // 将过滤后的内容写回 output.system
        output.system.splice(0, output.system.length, filteredLines.join("\n"))

        // === 步骤 2：统计 + hash ===
        const filteredText = filteredLines.join("\n")
        const filteredHash = simpleHash(filteredText)
        const isFirstCallInSession = !firstHashMap.has(trackKey)
        if (isFirstCallInSession) {
          firstHashMap.set(trackKey, filteredHash)
        }
        lastHashMap.set(trackKey, filteredHash)

        // 累计删除行数和告警行数（用于 Report）
        const prevRemoved = removedLineCountMap.get(trackKey) || 0
        removedLineCountMap.set(trackKey, prevRemoved + removedLines.length)
        const prevAlert = temporalAlertCountMap.get(trackKey) || 0
        temporalAlertCountMap.set(trackKey, prevAlert + temporalAlertLines.length)

        // 初始化已通知集合
        if (!notifiedTemporalLines.has(modelID)) {
          notifiedTemporalLines.set(modelID, new Set())
        }
        const notifiedSet = notifiedTemporalLines.get(modelID)!

        // 首次见到这个 model 且本 session 首次调用 → 发送 Active 通知
        const isGlobalFirstForModel = !globalPrevFilteredText.has(modelID)
        if (isGlobalFirstForModel && isFirstCallInSession) {
          const lineCount = rawLines.length
          const msgLines = [
            `🐕 *Prompt Watchdog* ${tag}`,
            `📦 ${escapeMarkdown(modelID)} (${rawText.length} chars / ${lineCount} lines)`,
            `🔑 '${filteredHash}'`,
          ]

          // Active 通知中列出被删除的行（带行号+内容）
          if (removedLines.length > 0) {
            msgLines.push(`🧹 ${removedLines.length} 行时间数据已过滤:`)
            const shown = removedLines.slice(0, 5)
            for (const r of shown) {
              msgLines.push(`  L${r.lineNum}: ${escapeMarkdown(truncate(r.content, 80))}`)
            }
            if (removedLines.length > 5) {
              msgLines.push(`  ... 及其他 ${removedLines.length - 5} 行`)
            }
          }

          // Active 通知中列出长行告警（带行号+时间内容+上下文）
          if (temporalAlertLines.length > 0) {
            msgLines.push(`🔍 ${temporalAlertLines.length} 行含时间数据(未删除):`)
            const shown = temporalAlertLines.slice(0, 3)
            for (const a of shown) {
              msgLines.push(`  L${a.lineNum} [${escapeMarkdown(a.matchedFragments.join(", "))}]: ${escapeMarkdown(truncate(a.content, 80))}`)
            }
            if (temporalAlertLines.length > 3) {
              msgLines.push(`  ... 及其他 ${temporalAlertLines.length - 3} 行`)
            }
          }

          await send(msgLines.join("\n"))

          // 记录已通知的签名（短行和长行分别用前缀区分）
          for (const r of removedLines) {
            notifiedSet.add(`removed:${r.signature}`)
          }
          for (const a of temporalAlertLines) {
            notifiedSet.add(`temporal-alert:${a.signature}`)
          }

          // 存储基线
          globalPrevFilteredText.set(modelID, filteredText)
          return
        }

        // === 步骤 3：删除行通知（只通知新出现的、未通知过的）===
        // BUG GUARD: 用内容签名（而非行号）判断是否已通知，因为行号可能因 prompt 上方内容变化而漂移
        const newRemovedToNotify = removedLines.filter((r) => !notifiedSet.has(`removed:${r.signature}`))
        if (newRemovedToNotify.length > 0) {
          for (const r of newRemovedToNotify) {
            notifiedSet.add(`removed:${r.signature}`)
          }

          const msgLines = [
            `🐕 *Prompt Watchdog* ${tag}`,
            `🧹 ${escapeMarkdown(modelID)}: ${newRemovedToNotify.length} 行时间数据已过滤:`,
          ]
          const shown = newRemovedToNotify.slice(0, 5)
          for (const r of shown) {
            msgLines.push(`  L${r.lineNum}: ${escapeMarkdown(truncate(r.content, 80))}`)
          }
          if (newRemovedToNotify.length > 5) {
            msgLines.push(`  ... 及其他 ${newRemovedToNotify.length - 5} 行`)
          }

          await send(msgLines.join("\n"))
        }

        // === 步骤 3b：长行告警（包含时间但未删除，只通知新出现的）===
        const newAlertToNotify = temporalAlertLines.filter((a) => !notifiedSet.has(`temporal-alert:${a.signature}`))
        if (newAlertToNotify.length > 0) {
          for (const a of newAlertToNotify) {
            notifiedSet.add(`temporal-alert:${a.signature}`)
          }

          const msgLines = [
            `🐕 *Prompt Watchdog* ${tag}`,
            `🔍 ${escapeMarkdown(modelID)}: ${newAlertToNotify.length} 行含时间数据(未删除):`,
          ]
          const shown = newAlertToNotify.slice(0, 5)
          for (const a of shown) {
            // 格式：行号 [匹配到的时间片段]: 前后部分内容
            msgLines.push(`  L${a.lineNum} [${escapeMarkdown(a.matchedFragments.join(", "))}]: ${escapeMarkdown(truncate(a.content, 80))}`)
          }
          if (newAlertToNotify.length > 5) {
            msgLines.push(`  ... 及其他 ${newAlertToNotify.length - 5} 行`)
          }

          await send(msgLines.join("\n"))
        }

        // 调试日志：记录删除和告警详情
        if (debugLogPath && (removedLines.length > 0 || temporalAlertLines.length > 0)) {
          const fs = await import("fs")
          const ts = new Date().toISOString()
          const removedDetail = removedLines.map((r) => `  REMOVED L${r.lineNum}: ${r.content} [sig=${r.signature}]`).join("\n")
          const alertDetail = temporalAlertLines.map((a) => `  ALERT L${a.lineNum} [${a.matchedFragments.join(", ")}]: ${a.content} [sig=${a.signature}]`).join("\n")
          fs.appendFileSync(debugLogPath, `\n[TEMPORAL] ${ts} model=${modelID} removed=${removedLines.length}(new=${newRemovedToNotify.length}) alerts=${temporalAlertLines.length}(new=${newAlertToNotify.length})\n${removedDetail}\n${alertDetail}\n`)
        }

        // === 步骤 4：结构变化检测（对过滤后的文本做 diff）===
        const prevFiltered = globalPrevFilteredText.get(modelID)
        if (prevFiltered !== undefined && prevFiltered !== filteredText) {
          const structuralDiffs = diffLines(prevFiltered, filteredText)

          if (structuralDiffs.length > 0) {
            const prevTotal = totalDiffLinesMap.get(trackKey) || 0
            totalDiffLinesMap.set(trackKey, prevTotal + structuralDiffs.length)

            const blocks = groupDiffsIntoBlocks(structuralDiffs)

            if (!diffSummaryMap.has(trackKey)) {
              diffSummaryMap.set(trackKey, [])
            }
            const summaries = diffSummaryMap.get(trackKey)!

            const alertLines = [
              `🐕 *Prompt Watchdog Alert* ${tag}`,
              `⚠️ ${escapeMarkdown(modelID)} #${callCount}: ${structuralDiffs.length} 行结构变化`,
            ]

            const shownBlocks = blocks.slice(0, 5)
            for (const block of shownBlocks) {
              const summary = summarizeBlock(block)
              summaries.push(summary)
              alertLines.push(`  • ${escapeMarkdown(summary)}`)
            }
            if (blocks.length > 5) {
              alertLines.push(`  ... 及其他 ${blocks.length - 5} 个区块`)
            }

            await send(alertLines.join("\n"))
          }
        }

        // 更新全局基线（过滤后的文本）
        globalPrevFilteredText.set(modelID, filteredText)
      } catch {}
    },
  }
}
