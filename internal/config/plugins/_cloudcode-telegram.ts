export const CloudCodeTelegram = async (input: any) => {
  const token = process.env.CC_TELEGRAM_BOT_TOKEN
  const chatId = process.env.CC_TELEGRAM_CHAT_ID
  if (!token || !chatId) return {}

  const host = process.env.HOSTNAME || "unknown"
  const client = input?.client

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
    cache?: { read: number; write: number }
  }) => {
    const parts = [`in:${tokens.input}`, `out:${tokens.output}`]
    if (tokens.reasoning > 0) parts.push(`reason:${tokens.reasoning}`)
    if (tokens.cache?.read) parts.push(`cache↓${tokens.cache.read}`)
    if (tokens.cache?.write) parts.push(`cache↑${tokens.cache.write}`)
    return parts.join(" | ")
  }

  return {
    event: async ({ event }: { event: { type: string; properties: any } }) => {
      if (event.type === "session.idle") {
        const sessionID = event.properties?.sessionID
        let title = ""
        let cost = ""
        let tokens = ""
        let summary = ""

        if (client && sessionID) {
          try {
            const session = await client.sessions.retrieve(sessionID)
            title = session?.title || ""
            if (session?.summary) {
              const s = session.summary
              summary = `${s.files || 0} files | +${s.additions || 0} -${s.deletions || 0}`
            }

            const msgs = await client.sessions.messages.list(sessionID, { limit: 5 })
            const lastAssistant = [...(msgs?.data || [])].reverse().find(
              (m: any) => m.role === "assistant"
            )
            if (lastAssistant) {
              cost = formatCost(lastAssistant.cost || 0)
              if (lastAssistant.tokens) tokens = formatTokens(lastAssistant.tokens)
            }
          } catch {}
        }

        const lines = [`✅ *Task Completed*`]
        if (title) lines.push(`📋 ${title}`)
        lines.push(`🖥 \`${host}\``)
        if (summary) lines.push(`📊 ${summary}`)
        if (cost) lines.push(`💰 ${cost}`)
        if (tokens) lines.push(`🔢 ${tokens}`)

        await send(lines.join("\n"))
      }

      if (event.type === "session.error") {
        const p = event.properties
        const errorName = p?.error?.name || "Unknown"
        const errorMsg = p?.error?.data?.message || p?.error?.data?.providerID || ""

        const lines = [`❌ *Session Error*`]
        lines.push(`🖥 \`${host}\``)
        lines.push(`⚡ ${errorName}`)
        if (errorMsg) lines.push(`💬 ${errorMsg}`)

        await send(lines.join("\n"))
      }

      if (event.type === "permission.asked") {
        const p = event.properties
        const permission = p?.permission || "unknown"
        const metadata = p?.metadata || {}
        const tool = metadata?.tool || permission

        const lines = [`⚠️ *Action Required*`]
        lines.push(`🖥 \`${host}\``)
        lines.push(`🔐 ${tool}`)
        if (metadata?.path) lines.push(`📁 \`${metadata.path}\``)
        if (metadata?.command) lines.push(`💻 \`${metadata.command}\``)

        await send(lines.join("\n"))
      }
    },
  }
}
