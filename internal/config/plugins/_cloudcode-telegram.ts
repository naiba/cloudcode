export const CloudCodeTelegram = async (input: any) => {
  const token = process.env.CC_TELEGRAM_BOT_TOKEN
  const chatId = process.env.CC_TELEGRAM_CHAT_ID
  if (!token || !chatId) return {}

  const instanceName = process.env.CC_INSTANCE_NAME || ""
  const host = process.env.HOSTNAME || "unknown"
  const client = input?.client
  const tag = instanceName ? `\`${instanceName}\`` : `\`${host}\``

  const send = async (text: string) => {
    try {
      await fetch(`https://api.telegram.org/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text, parse_mode: "Markdown" }),
      })
    } catch {}
  }

  const formatCost = (cost: number) =>
    cost < 0.01 ? `$${cost.toFixed(4)}` : `$${cost.toFixed(2)}`

  const formatTokens = (tokens: {
    input: number
    output: number
    reasoning: number
    cache: { read: number; write: number }
  }) => {
    const parts = [`in:${tokens.input}`, `out:${tokens.output}`]
    if (tokens.reasoning > 0) parts.push(`reason:${tokens.reasoning}`)
    if (tokens.cache?.read) parts.push(`cache↓${tokens.cache.read}`)
    if (tokens.cache?.write) parts.push(`cache↑${tokens.cache.write}`)
    return parts.join(" | ")
  }

  const formatDuration = (seconds: number) => {
    if (seconds < 60) return `${Math.round(seconds)}s`
    const mins = Math.floor(seconds / 60)
    const secs = Math.round(seconds % 60)
    if (mins < 60) return secs > 0 ? `${mins}m${secs}s` : `${mins}m`
    const hours = Math.floor(mins / 60)
    const remainMins = mins % 60
    return remainMins > 0 ? `${hours}h${remainMins}m` : `${hours}h`
  }

  const getSession = async (sessionID: string) => {
    if (!client) return null
    const res = await client.session.get({ path: { id: sessionID } })
    return res?.data ?? res ?? null
  }

  const isChildSession = (session: any): boolean => {
    if (session.parentID || session.parent_id) return true
    const title = session.title || ""
    if (title.includes("subagent)") || title.startsWith("Child session - ")) return true
    return false
  }

  const getMessages = async (sessionID: string) => {
    if (!client) return []
    const res = await client.session.messages({ path: { id: sessionID } })
    const list = res?.data ?? res
    return Array.isArray(list) ? list : []
  }

  return {
    event: async ({ event }: { event: { type: string; properties: any } }) => {
      const isIdle =
        event.type === "session.idle" ||
        (event.type === "session.status" && event.properties?.status?.type === "idle")
      if (isIdle) {
        const sessionID = event.properties?.sessionID
        if (!sessionID) return

        try {
          const session = await getSession(sessionID)
          if (!session || isChildSession(session)) return

          const title = session.title || ""
          const createdAt = session.time?.created || 0
          const updatedAt = session.time?.updated || 0
          let duration = ""
          if (createdAt > 0 && updatedAt > 0) {
            const durationSec = (updatedAt - createdAt) / 1000
            if (durationSec > 0) duration = formatDuration(durationSec)
          }

          let totalCost = 0
          const totalTokens = { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } }
          const msgList = await getMessages(sessionID)
          for (const msg of msgList) {
            const info = msg.info || msg
            if (info.role !== "assistant") continue
            totalCost += info.cost || 0
            if (info.tokens) {
              totalTokens.input += info.tokens.input || 0
              totalTokens.output += info.tokens.output || 0
              totalTokens.reasoning += info.tokens.reasoning || 0
              totalTokens.cache.read += info.tokens.cache?.read || 0
              totalTokens.cache.write += info.tokens.cache?.write || 0
            }
          }

          const lines = [`✅ *Task Completed*`]
          if (title) lines.push(`📋 ${title}`)
          lines.push(`🖥 ${tag}`)
          if (duration) lines.push(`⏱ ${duration}`)
          if (totalCost > 0) lines.push(`💰 ${formatCost(totalCost)}`)
          if (totalTokens.input > 0 || totalTokens.output > 0) {
            lines.push(`🔢 ${formatTokens(totalTokens)}`)
          }

          await send(lines.join("\n"))
        } catch {}
      }

      if (event.type === "session.error") {
        const p = event.properties
        const sessionID = p?.sessionID

        if (sessionID) {
          try {
            const session = await getSession(sessionID)
            if (session && isChildSession(session)) return
          } catch {}
        }

        const errorName = p?.error?.name || "Unknown"
        const errorMsg = p?.error?.data?.message || p?.error?.data?.providerID || ""

        const lines = [`❌ *Session Error*`]
        lines.push(`🖥 ${tag}`)
        lines.push(`⚡ ${errorName}`)
        if (errorMsg) lines.push(`💬 ${errorMsg}`)

        await send(lines.join("\n"))
      }

      if (event.type === "permission.asked") {
        const p = event.properties
        const sessionID = p?.sessionID

        if (sessionID) {
          try {
            const session = await getSession(sessionID)
            if (session && isChildSession(session)) return
          } catch {}
        }

        const permission = p?.permission || "unknown"
        const metadata = p?.metadata || {}
        const tool = metadata?.tool || permission

        const lines = [`🔐 *Permission Required*`]
        lines.push(`🖥 ${tag}`)
        lines.push(`⚙️ ${tool}`)
        if (metadata?.path) lines.push(`📁 \`${metadata.path}\``)
        if (metadata?.command) lines.push(`💻 \`${metadata.command}\``)

        await send(lines.join("\n"))
      }

      if (event.type === "question.asked") {
        const p = event.properties
        const sessionID = p?.sessionID

        if (sessionID) {
          try {
            const session = await getSession(sessionID)
            if (session && isChildSession(session)) return
          } catch {}
        }

        const questions = p?.questions || []
        const lines = [`❓ *Input Required*`]
        lines.push(`🖥 ${tag}`)
        for (const q of questions) {
          if (q.header) lines.push(`📋 ${q.header}`)
          if (q.question) lines.push(`${q.question}`)
        }

        await send(lines.join("\n"))
      }
    },
  }
}
