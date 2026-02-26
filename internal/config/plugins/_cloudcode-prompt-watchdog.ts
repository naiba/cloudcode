/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 监控 system prompt 的完整性和变化情况，通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. system[] 实际是单元素数组（一个大字符串），对其做行级 diff 定位具体变化
 * 3. 频繁变化检测（非首次变化即替换）：
 *    - 首次调用记录基线，不做任何替换
 *    - 后续调用逐行对比，发现变化行后 neutralize（日期/时间/数字→占位符）再比较
 *    - neutralize 后相同 → 判定为"动态微变"（如时间戳更新），累计该行变化次数
 *    - 变化次数达到阈值（DYNAMIC_CHANGE_THRESHOLD，默认2）后才开始替换该行为占位符版本
 *    - 未达阈值的行保持原样，可能只是一次性变化
 *    - neutralize 后仍不同 → 判定为"结构变化"，立即触发告警
 * 4. 同一 session 内不同 agent（如 title vs sisyphus）使用 sessionID:modelID 复合 key 独立追踪
 * 5. 同一 session 中同一行位置的动态替换达到阈值时只通知一次，避免重复告警
 * 6. 首次调用时发送 "开始监控" 报告
 * 7. session 空闲时发送监控总结报告
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

  // --- 动态分析核心：将一行中的日期/时间/数字替换为通用占位符 ---
  // 不使用静态规则列表，而是对任意行做通用的 neutralize 处理，
  // 让 diff 对比自动发现哪些行只是日期/时间/数字发生了变化
  const neutralizeLine = (line: string): string => {
    return (
      line
        // 时间格式: 04:37:54 AM, 16:30:00, 4:37 PM 等
        .replace(/\d{1,2}:\d{2}(:\d{2})?\s*(AM|PM|am|pm)?/g, "{{TIME}}")
        // ISO 日期时间: 2026-02-26T04:37:54Z, 2026-02-26 04:37 等
        .replace(/\d{4}-\d{2}-\d{2}[T ]\d{1,2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?/g, "{{DATETIME}}")
        // 日期格式: 2026-02-26, 02/26/2026, Feb 26 2026 等
        .replace(/\d{4}[-/]\d{1,2}[-/]\d{1,2}/g, "{{DATE}}")
        .replace(/\d{1,2}[-/]\d{1,2}[-/]\d{4}/g, "{{DATE}}")
        // 英文星期: Mon, Tue, Wed, ... Sunday, Monday ...
        .replace(/\b(Mon|Tue|Wed|Thu|Fri|Sat|Sun)(day|nesday|rsday|urday)?/gi, "{{DAY}}")
        // 英文月份: Jan, Feb, ... January, February ...
        .replace(/\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\w*/gi, "{{MONTH}}")
        // 4位年份（独立出现）
        .replace(/\b(19|20)\d{2}\b/g, "{{YEAR}}")
        // 剩余的独立数字序列（兜底：捕获所有纯数字变化）
        .replace(/\b\d+\b/g, "{{N}}")
    )
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

  // 频繁变化判定阈值：某行累计变化达到此次数后才开始替换为占位符
  // BUG GUARD: 阈值不能设为 1，否则退化为"首次变化即替换"，丧失一次性变化的容忍能力
  const DYNAMIC_CHANGE_THRESHOLD = 2

  // === 全局基线（按 modelID，跨 session 共享）===
  // 同一个 model 的 prompt 结构基本一致，跨 session 只有日期/时间等动态内容会变
  // 用全局基线来检测这些跨 session 的动态变化
  const globalPrevRawLines: Map<string, string[]> = new Map()
  const globalLineChangeCount: Map<string, Map<number, number>> = new Map()
  const globalNotifiedDynamic: Map<string, Set<number>> = new Map()

  // === Per-session 状态（用于 Report 统计）===
  // trackKey = "sessionID:modelID"
  const firstHashMap: Map<string, string> = new Map()
  const lastHashMap: Map<string, string> = new Map()
  const callCountMap: Map<string, number> = new Map()
  const totalDiffLinesMap: Map<string, number> = new Map()
  const diffSummaryMap: Map<string, string[]> = new Map()

  // 用于结束报告：记录每个 session 涉及的所有 trackKey
  const sessionTrackKeys: Map<string, Set<string>> = new Map()
  const reportedSessions: Set<string> = new Set()

  const buildTrackKey = (sessionID: string, modelID: string): string => {
    return `${sessionID}:${modelID}`
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
        const firstHash = firstHashMap.get(key) || "?"
        const lastHash = lastHashMap.get(key) || "?"
        const drifted = firstHash !== lastHash
        const summaries = diffSummaryMap.get(key) || []
        const dynamicCount = globalLineChangeCount.get(modelID)?.size || 0

        const statusEmoji = totalDiffLines === 0 && dynamicCount === 0 ? "✅" : drifted ? "⚠️" : "🔄"
        const statusParts: string[] = []
        if (totalDiffLines > 0) statusParts.push(`${totalDiffLines} 行结构变化`)
        if (dynamicCount > 0) statusParts.push(`${dynamicCount} 行动态过滤`)
        const statusText = statusParts.length > 0 ? statusParts.join(", ") : "无变化"

        lines.push(`${statusEmoji} ${escapeMarkdown(modelID)} ×${calls} ${drifted ? `\'${firstHash}\'→\'${lastHash}\'` : `\'${firstHash}\'`} ${statusText}`)

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

        // === 全局基线对比（跨 session 检测动态变化）===
        const globalPrev = globalPrevRawLines.get(modelID)
        if (!globalLineChangeCount.has(modelID)) {
          globalLineChangeCount.set(modelID, new Map())
        }
        const gChangeCounts = globalLineChangeCount.get(modelID)!
        if (!globalNotifiedDynamic.has(modelID)) {
          globalNotifiedDynamic.set(modelID, new Set())
        }
        const gNotified = globalNotifiedDynamic.get(modelID)!

        // 构建输出行：默认保持原样，只有全局达到阈值的动态行才替换
        const outputLines = [...rawLines]
        const structuralDiffs: LineDiff[] = []
        const newlyConfirmedDynamic: { lineNum: number; oldLine: string; newLine: string; neutralized: string }[] = []
        const pendingDynamic: { lineNum: number; count: number }[] = []

        if (globalPrev !== undefined) {
          // 有全局基线：逐行对比
          const maxLen = Math.max(globalPrev.length, rawLines.length)
          for (let i = 0; i < maxLen; i++) {
            const oldLine = i < globalPrev.length ? globalPrev[i] : undefined
            const newLine = i < rawLines.length ? rawLines[i] : undefined
            const lineNum = i + 1

            if (oldLine === newLine) continue

            // 行增删：属于结构变化
            if (oldLine === undefined || newLine === undefined) {
              if (oldLine === undefined) {
                structuralDiffs.push({ type: "added", lineNum, newLine })
              } else {
                structuralDiffs.push({ type: "removed", lineNum, oldLine })
              }
              continue
            }

            // 行内容变化：neutralize 后对比
            const neutralizedOld = neutralizeLine(oldLine)
            const neutralizedNew = neutralizeLine(newLine)

            if (neutralizedOld === neutralizedNew) {
              // 动态微变：日期/时间/数字变了但结构不变，累计全局变化次数
              const prevCount = gChangeCounts.get(lineNum) || 0
              const newCount = prevCount + 1
              gChangeCounts.set(lineNum, newCount)

              if (newCount >= DYNAMIC_CHANGE_THRESHOLD) {
                // BUG GUARD: 达到阈值才替换为占位符，确认是频繁变化而非一次性变化
                outputLines[i] = neutralizedNew
                if (newCount === DYNAMIC_CHANGE_THRESHOLD) {
                  newlyConfirmedDynamic.push({ lineNum, oldLine, newLine, neutralized: neutralizedNew })
                }
              } else {
                // 未达阈值：保持原样，可能只是一次性变化
                pendingDynamic.push({ lineNum, count: newCount })
              }
            } else {
              // 真正的结构变化
              structuralDiffs.push({ type: "changed", lineNum, oldLine, newLine })
            }
          }
        } else {
          // 全局首次见到这个 model，已达阈值的行仍需替换（处理进程重启不会发生，但逻辑完整性）
        }

        // 更新全局基线
        globalPrevRawLines.set(modelID, rawLines)

        // 将替换后的内容写回 output.system
        output.system.splice(0, output.system.length, outputLines.join("\n"))

        // === Per-session 统计（用于 Report）===
        const isFirstCallInSession = !firstHashMap.has(trackKey)
        const neutralizedLines = outputLines.map(neutralizeLine)
        const fullHash = simpleHash(neutralizedLines.join("\n"))
        if (isFirstCallInSession) {
          firstHashMap.set(trackKey, fullHash)
        }
        lastHashMap.set(trackKey, fullHash)

        // 首次见到这个 model 且本 session 首次调用 → 发送 Active 通知
        if (globalPrev === undefined && isFirstCallInSession) {
          const lineCount = rawLines.length
          const lines = [
            `\ud83d\udc15 *Prompt Watchdog* ${tag}`,
            `\ud83d\udce6 ${escapeMarkdown(modelID)} (${rawText.length} chars / ${lineCount} lines)`,
            `\ud83d\udd11 \'${fullHash}\'`,
          ]
          await send(lines.join("\n"))
          // 全局首次无基线可比，直接返回
          return
        }
        // === 通知逻辑 ===

        // 1. 动态微变通知：只有刚达到全局阈值且未通知过的行才发送
        const toNotify = newlyConfirmedDynamic.filter((d) => !gNotified.has(d.lineNum))
        if (toNotify.length > 0) {
          for (const d of toNotify) {
            gNotified.add(d.lineNum)
          }

          const lines = [
            `🐕 *Prompt Watchdog* ${tag}`,
            `🧹 ${escapeMarkdown(modelID)}: ${toNotify.length} 行频繁变化已替换为占位符 (≥${DYNAMIC_CHANGE_THRESHOLD}次)`,
          ]
          const shown = toNotify.slice(0, 5)
          for (const d of shown) {
            lines.push(`  L${d.lineNum}: ${escapeMarkdown(truncate(d.newLine.trim(), 60))} → \'...\'`)
          }
          if (toNotify.length > 5) {
            lines.push(`  ... 及其他 ${toNotify.length - 5} 处`)
          }
          if (pendingDynamic.length > 0) {
            lines.push(`🕒 ${pendingDynamic.length} 行观察中`)
          }

          await send(lines.join("\n"))
        }

        // 2. 结构变化告警
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
      } catch {}
    },
  }
}
