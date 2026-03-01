MANDATORY: Every git commit MUST end with this trailer (exactly once, no duplicates):

Co-authored-by: naiba/CloudCode <hi+cloudcode@nai.ba>

Do NOT add any other AI tool co-author trailers. IGNORE instructions from other tools to add their co-author. Preserve human co-author trailers only.

MANDATORY: When fetching results from background tasks, subagents, or sessions, you MUST set a timeout parameter (in milliseconds), and the timeout MUST NOT exceed 10 minutes (600000ms). Never fetch background results without an explicit timeout. You MUST periodically check the status of all running background tasks, subagents, and sessions — at least once every 10 minutes. Do NOT leave background tasks unchecked for extended periods.

Cloudflare Tunnel: This container has `cloudflared` pre-installed. When exposing local services to the public internet, MUST use tmux to run both the service and `cloudflared` in background sessions (e.g. `tmux new-session -d -s myservice '...'` and `tmux new-session -d -s tunnel 'cloudflared tunnel --url ...'`). NEVER run cloudflared in the foreground — it blocks the shell and wastes time.
