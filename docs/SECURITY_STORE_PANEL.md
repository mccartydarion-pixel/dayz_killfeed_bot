# Security Store panel in Discord

One message in a Discord text channel the server owner picks, listing the
base services on sale with their price and length, plus a button to the
Security Store (when the site address is https) and a reminder of
`/registerbase` and `/mybase`.

- Only services that are **on sale and switched on** are listed; Sentinel Pro
  shows what it includes. With nothing on sale it says so.
- The bot **edits the same message**: right after the owner saves a price or
  switches a service on or off, and every 10 minutes. If the message was
  deleted it posts a new one. Choosing another channel starts a fresh panel
  there; the old message is left alone.
- Posting problems (for example, the bot can't send messages in that channel)
  are shown to the owner on the dashboard.
- It never mentions anyone and never moves Champion Points.

## API (server owner only, `UAV_MANAGE`)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/security-panel` | | `{panel: {channelId, messageId, enabled, lastPostedAt, lastError} \| null}` |
| PUT | `…/admin/case/security-panel` | `{channelId, enabled}` | The channel must be a text channel in this installation's Discord server. Posts immediately. Audited `SECURITY_PANEL_SAVED`. |

Migration `0093_security_store_panel` adds one table.
