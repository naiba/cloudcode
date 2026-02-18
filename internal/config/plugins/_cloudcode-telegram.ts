export const CloudCodeTelegram = async () => {
  const token = process.env.CC_TELEGRAM_BOT_TOKEN
  const chatId = process.env.CC_TELEGRAM_CHAT_ID
  if (!token || !chatId) return {}

  const send = async (text: string) => {
    try {
      await fetch(`https://api.telegram.org/bot${token}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text, parse_mode: "Markdown" }),
      })
    } catch {}
  }

  return {
    event: async ({ event }: { event: { type: string } }) => {
      if (event.type === "session.idle") {
        const host = process.env.HOSTNAME || "unknown"
        await send(`✅ *CloudCode* — Task completed\nInstance: \`${host}\``)
      }
      if (event.type === "session.error") {
        const host = process.env.HOSTNAME || "unknown"
        await send(`❌ *CloudCode* — Session error\nInstance: \`${host}\``)
      }
    },
  }
}
