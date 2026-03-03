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
 * 4. 小幅 diff 告警：对过滤后的 prompt 做全局基线 diff，
 *    - diff < 10 行 → 替换旧基线，发送 git unified diff 风格告警（带 @@ hunk 头和 -/+ 前缀）
 *    - diff ≥ 10 行 → 视为大变更/全新 prompt，不替换基线、不告警
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
  // CC_TELEGRAM_WATCHDOG_THREAD_ID: 由平台注入，指定 Prompt Watchdog topic 的 thread ID
  const threadId = process.env.CC_TELEGRAM_WATCHDOG_THREAD_ID
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
        body: JSON.stringify({ chat_id: chatId, text: safeText, parse_mode: "Markdown", ...(threadId ? { message_thread_id: Number(threadId) } : {}) }),
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

  /**
   * 根据已知的被删除行号，在原始文本中生成 git-diff 风格的删除展示
   * 每个被删除的行以 - 前缀展示，加上前后各 1 行上下文
   *
   * BUG GUARD: 直接用被删除行信息构建 diff，而不是对比 rawText vs filteredText，
   * 因为简单的行号对齐 diff 会导致删除行下方所有内容错位
   */
  const formatRemovedLinesDiff = (
    allLines: string[],
    removedLineNums: number[]  // 1-based 行号
  ): string => {
    if (removedLineNums.length === 0) return ""
    const CONTEXT = 1
    const removedSet = new Set(removedLineNums)

    // 将相邻的删除行分组为 hunk（间隔 > 2*CONTEXT+1 则分开）
    const sorted = [...removedLineNums].sort((a, b) => a - b)
    const hunks: number[][] = []
    let currentHunk = [sorted[0]]
    for (let i = 1; i < sorted.length; i++) {
      if (sorted[i] - sorted[i - 1] <= 2 * CONTEXT + 1) {
        currentHunk.push(sorted[i])
      } else {
        hunks.push(currentHunk)
        currentHunk = [sorted[i]]
      }
    }
    hunks.push(currentHunk)

    const output: string[] = []
    for (const hunk of hunks) {
      const hunkStart = Math.max(0, hunk[0] - 1 - CONTEXT)  // 0-based
      const hunkEnd = Math.min(allLines.length - 1, hunk[hunk.length - 1] - 1 + CONTEXT)  // 0-based

      // @@ 头：old 包含所有行，new 排除删除行
      const oldStart = hunkStart + 1  // 1-based
      const oldCount = hunkEnd - hunkStart + 1
      const removedInHunk = hunk.length
      const newStart = oldStart  // 简化处理，不精确计算偏移
      const newCount = oldCount - removedInHunk

      output.push(`@@ -${oldStart},${oldCount} +${newStart},${newCount} @@`)

      for (let i = hunkStart; i <= hunkEnd; i++) {
        const lineNum = i + 1  // 1-based
        if (removedSet.has(lineNum)) {
          output.push(`-${truncate(allLines[i], 100)}`)
        } else {
          output.push(` ${truncate(allLines[i], 100)}`)
        }
      }
    }

    return output.join("\n")
  }

  /**
   * 生成 git unified diff 风格的文本输出
   * 格式类似 `git diff --unified=1`，带 @@ hunk 头和 -/+ 前缀
   *
   * BUG GUARD: 使用 oldLines/newLines 原始数组生成 diff，不依赖 LineDiff 的 lineNum，
   * 因为 changed 类型在 unified diff 中需要拆成一行 - 和一行 +
   */
  const formatUnifiedDiff = (oldText: string, newText: string): string => {
    const oldLines = oldText.split("\n")
    const newLines = newText.split("\n")
    const maxLen = Math.max(oldLines.length, newLines.length)

    // 标记每行的状态：equal / removed / added / changed
    interface DiffEntry {
      type: "equal" | "removed" | "added" | "changed"
      oldIdx: number  // 0-based index in oldLines (-1 if added)
      newIdx: number  // 0-based index in newLines (-1 if removed)
      oldLine?: string
      newLine?: string
    }

    const entries: DiffEntry[] = []
    for (let i = 0; i < maxLen; i++) {
      const ol = i < oldLines.length ? oldLines[i] : undefined
      const nl = i < newLines.length ? newLines[i] : undefined
      if (ol === nl) {
        entries.push({ type: "equal", oldIdx: i, newIdx: i, oldLine: ol, newLine: nl })
      } else if (ol === undefined) {
        entries.push({ type: "added", oldIdx: -1, newIdx: i, newLine: nl })
      } else if (nl === undefined) {
        entries.push({ type: "removed", oldIdx: i, newIdx: -1, oldLine: ol })
      } else {
        entries.push({ type: "changed", oldIdx: i, newIdx: i, oldLine: ol, newLine: nl })
      }
    }

    // 找出有变化的行索引，然后为每个变化区域生成 hunk（带 1 行上下文）
    const CONTEXT = 1
    const changedIndices = entries.map((e, i) => e.type !== "equal" ? i : -1).filter((i) => i >= 0)
    if (changedIndices.length === 0) return ""

    // 将连续的变化索引分组为 hunk（间隔 > 2*CONTEXT+1 则分开）
    const hunks: number[][] = []
    let currentHunk = [changedIndices[0]]
    for (let i = 1; i < changedIndices.length; i++) {
      if (changedIndices[i] - changedIndices[i - 1] <= 2 * CONTEXT + 1) {
        currentHunk.push(changedIndices[i])
      } else {
        hunks.push(currentHunk)
        currentHunk = [changedIndices[i]]
      }
    }
    hunks.push(currentHunk)

    const output: string[] = []
    for (const hunk of hunks) {
      const hunkStart = Math.max(0, hunk[0] - CONTEXT)
      const hunkEnd = Math.min(entries.length - 1, hunk[hunk.length - 1] + CONTEXT)

      // 计算 @@ 头中的行号范围
      let oldStart = 0, oldCount = 0, newStart = 0, newCount = 0
      let oldStartSet = false, newStartSet = false
      for (let i = hunkStart; i <= hunkEnd; i++) {
        const e = entries[i]
        if (e.type === "equal") {
          if (!oldStartSet) { oldStart = e.oldIdx + 1; oldStartSet = true }
          if (!newStartSet) { newStart = e.newIdx + 1; newStartSet = true }
          oldCount++; newCount++
        } else if (e.type === "removed") {
          if (!oldStartSet) { oldStart = e.oldIdx + 1; oldStartSet = true }
          if (!newStartSet) { newStart = (e.oldIdx < newLines.length ? e.oldIdx : newLines.length) + 1; newStartSet = true }
          oldCount++
        } else if (e.type === "added") {
          if (!oldStartSet) { oldStart = (e.newIdx < oldLines.length ? e.newIdx : oldLines.length) + 1; oldStartSet = true }
          if (!newStartSet) { newStart = e.newIdx + 1; newStartSet = true }
          newCount++
        } else if (e.type === "changed") {
          if (!oldStartSet) { oldStart = e.oldIdx + 1; oldStartSet = true }
          if (!newStartSet) { newStart = e.newIdx + 1; newStartSet = true }
          oldCount++; newCount++  // changed = 1 removed + 1 added
        }
      }

      output.push(`@@ -${oldStart},${oldCount} +${newStart},${newCount} @@`)

      for (let i = hunkStart; i <= hunkEnd; i++) {
        const e = entries[i]
        if (e.type === "equal") {
          output.push(` ${truncate(e.oldLine ?? "", 100)}`)
        } else if (e.type === "removed") {
          output.push(`-${truncate(e.oldLine ?? "", 100)}`)
        } else if (e.type === "added") {
          output.push(`+${truncate(e.newLine ?? "", 100)}`)
        } else if (e.type === "changed") {
          // changed 拆成一行 - 和一行 +，与 git diff 行为一致
          output.push(`-${truncate(e.oldLine ?? "", 100)}`)
          output.push(`+${truncate(e.newLine ?? "", 100)}`)
        }
      }
    }

    return output.join("\n")
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
        if (totalDiffLines > 0) statusParts.push(`${totalDiffLines} 行变化`)
        if (removedCount > 0) statusParts.push(`${removedCount} 行时间过滤`)
        if (alertCount > 0) statusParts.push(`${alertCount} 行时间告警`)
        const statusText = statusParts.length > 0 ? statusParts.join(", ") : "无变化"

        lines.push(`${statusEmoji} ${escapeMarkdown(modelID)} ×${calls} ${drifted ? `'${firstHash}'→'${lastHash}'` : `'${firstHash}'`} ${statusText}`)

        if (summaries.length > 0) {
          // 只展示最近一次 diff（git-diff 格式）
          const lastDiff = summaries[summaries.length - 1]
          lines.push("```")
          lines.push(lastDiff)
          lines.push("```")
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

          // Active 通知中用 git-diff 格式展示被过滤的时间行
          if (removedLines.length > 0) {
            const temporalDiff = formatRemovedLinesDiff(rawLines, removedLines.map((r) => r.lineNum))
            msgLines.push(`🧹 ${removedLines.length} 行时间数据已过滤:`)
            msgLines.push("```")
            msgLines.push(temporalDiff)
            msgLines.push("```")
          }

          // 长行告警：包含时间但未删除，仅简要提示行数
          if (temporalAlertLines.length > 0) {
            msgLines.push(`🔍 ${temporalAlertLines.length} 行含时间数据(未删除):`)
            msgLines.push("```")
            for (const a of temporalAlertLines.slice(0, 5)) {
              msgLines.push(`L${a.lineNum}: ${truncate(a.content, 100)}`)
            }
            if (temporalAlertLines.length > 5) {
              msgLines.push(`... 及其他 ${temporalAlertLines.length - 5} 行`)
            }
            msgLines.push("```")
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

          const temporalDiff = formatRemovedLinesDiff(rawLines, newRemovedToNotify.map((r) => r.lineNum))
          const msgLines = [
            `🐕 *Prompt Watchdog* ${tag}`,
            `🧹 ${escapeMarkdown(modelID)}: ${newRemovedToNotify.length} 行时间数据已过滤:`,
            "```",
            temporalDiff,
            "```",
          ]

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
            "```",
          ]
          for (const a of newAlertToNotify.slice(0, 5)) {
            msgLines.push(`L${a.lineNum}: ${truncate(a.content, 100)}`)
          }
          if (newAlertToNotify.length > 5) {
            msgLines.push(`... 及其他 ${newAlertToNotify.length - 5} 行`)
          }
          msgLines.push("```")

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

        // === 步骤 4：小幅 diff 告警（git-diff 风格）===
        // 只在 diff < 10 行时告警并替换基线，≥ 10 行视为大变更，不替换不告警
        // BUG GUARD: ≥ 10 行不替换基线，保持旧基线用于下次比对，
        // 这样如果后续 prompt 回到旧基线的近似版本，仍能正常检测到小变化
        const prevFiltered = globalPrevFilteredText.get(modelID)
        if (prevFiltered !== undefined && prevFiltered !== filteredText) {
          const structuralDiffs = diffLines(prevFiltered, filteredText)

          if (structuralDiffs.length > 0 && structuralDiffs.length < 10) {
            // 小幅变化：替换基线 + 发送 git-diff 风格告警
            globalPrevFilteredText.set(modelID, filteredText)

            const prevTotal = totalDiffLinesMap.get(trackKey) || 0
            totalDiffLinesMap.set(trackKey, prevTotal + structuralDiffs.length)

            if (!diffSummaryMap.has(trackKey)) {
              diffSummaryMap.set(trackKey, [])
            }
            const summaries = diffSummaryMap.get(trackKey)!

            const unifiedDiff = formatUnifiedDiff(prevFiltered, filteredText)
            const alertLines = [
              `🐕 *Prompt Watchdog Alert* ${tag}`,
              `⚠️ ${escapeMarkdown(modelID)} #${callCount}: ${structuralDiffs.length} 行变化`,
              "```",
              unifiedDiff,
              "```",
            ]

            // 存储 unified diff 文本用于 Report
            summaries.push(unifiedDiff)

            await send(alertLines.join("\n"))

            // 调试日志：记录 diff 决策
            if (debugLogPath) {
              const fs = await import("fs")
              const ts = new Date().toISOString()
              fs.appendFileSync(debugLogPath, `\n[DIFF] ${ts} model=${modelID} diffLines=${structuralDiffs.length} action=REPLACE_AND_ALERT\n${unifiedDiff}\n`)
            }
          } else if (structuralDiffs.length >= 10) {
            // 大幅变化（≥ 10 行）：不替换基线，不告警
            // BUG GUARD: 不更新 globalPrevFilteredText，保持旧基线不变
            if (debugLogPath) {
              const fs = await import("fs")
              const ts = new Date().toISOString()
              fs.appendFileSync(debugLogPath, `\n[DIFF] ${ts} model=${modelID} diffLines=${structuralDiffs.length} action=SKIP_LARGE_CHANGE\n`)
            }
          }
          // structuralDiffs.length === 0 理论上不会进入（prevFiltered !== filteredText 已判断）
        } else if (prevFiltered === undefined) {
          // 首次见到此 modelID 的过滤后文本，存储为基线
          globalPrevFilteredText.set(modelID, filteredText)
        }
        // 如果 prevFiltered === filteredText（无变化），不做任何操作
      } catch {}
    },
  }
}
