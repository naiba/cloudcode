/**
 * CloudCode Prompt Watchdog Plugin
 *
 * 检测 system prompt 中频繁变化的部分（如注入的时间戳、动态内容），
 * 通过 Telegram 通知管理员。
 *
 * 工作原理：
 * 1. 通过 experimental.chat.system.transform hook 拦截每次 LLM 调用的 system prompt
 * 2. 将 system prompt 按行分段，对每段计算 hash
 * 3. 对比同一 session 内前后两次的段落 hash，找出变化的段落
 * 4. 如果检测到频繁变化的段落（同一段在短时间内多次变化），发送 Telegram 告警
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
  // changeCounter: 记录每个段落 hash 变化的次数，key = "sessionID:segmentIndex"
  const changeCounter: Map<string, number> = new Map()
  // notifiedSegments: 已通知过的段落，避免重复告警，key = "sessionID:segmentIndex"
  const notifiedSegments: Set<string> = new Set()
  // sessionCallCount: 每个 session 的调用次数，用于跳过首次调用（首次无法对比）
  const sessionCallCount: Map<string, number> = new Map()

  // 频繁变化阈值：同一段落在一个 session 内变化超过此次数则告警
  const CHANGE_THRESHOLD = 2

  /**
   * 将 system prompt 分割成有意义的段落。
   * 按 XML 标签块和空行分隔，保留段落间的结构关系。
   */
  const segmentize = (systemParts: string[]): string[] => {
    const fullText = systemParts.join("\n---PART_BOUNDARY---\n")
    // 按连续空行或 XML 标签边界分割
    const segments = fullText.split(/\n{3,}/)
    return segments.map((s) => s.trim()).filter((s) => s.length > 0)
  }

  /**
   * 对比两次段落列表，找出变化的段落。
   * 返回变化的段落索引和内容摘要。
   */
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
        // 新增的段落
        const preview = truncate(curr[i], 200)
        changes.push({ index: i, type: "added", preview })
      } else if (i >= currHashes.length) {
        // 被移除的段落
        const preview = truncate(prev[i], 200)
        changes.push({ index: i, type: "removed", preview })
      } else if (prevHashes[i] !== currHashes[i]) {
        // 内容变化的段落
        const preview = truncate(curr[i], 200)
        changes.push({ index: i, type: "changed", preview })
      }
    }

    return changes
  }

  const truncate = (str: string, maxLen: number): string => {
    if (str.length <= maxLen) return str
    return str.slice(0, maxLen) + "..."
  }

  // 转义 Markdown 特殊字符，避免 Telegram 解析失败
  const escapeMarkdown = (str: string): string => {
    return str.replace(/[_*[\]()~`>#+\-=|{}.!\\]/g, "\\$&")
  }

  return {
    "experimental.chat.system.transform": async (
      inputData: { sessionID?: string; model: any },
      output: { system: string[] }
    ): Promise<void> => {
      const sessionID = inputData.sessionID
      if (!sessionID || !output.system || output.system.length === 0) return

      // 更新调用次数
      const callCount = (sessionCallCount.get(sessionID) || 0) + 1
      sessionCallCount.set(sessionID, callCount)

      // 分段并计算 hash
      const segments = segmentize(output.system)
      const hashes = segments.map(simpleHash)

      const prevHashes = sessionSegments.get(sessionID)

      // 首次调用，仅记录基线
      if (!prevHashes) {
        sessionSegments.set(sessionID, hashes)
        return
      }

      // 对比变化
      const prevSegments = segmentize(output.system) // 用当前的分段逻辑重建，保证一致性
      const changes = diffSegments(prevSegments, segments, prevHashes, hashes)

      if (changes.length > 0) {
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
