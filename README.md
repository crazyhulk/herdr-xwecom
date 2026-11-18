# WeCom Agents for Herdr

Control [Herdr](https://herdr.dev) agents from 企业微信 (WeCom) through a WeCom AI bot
(智能机器人, long-connection / 长连接 mode). Same idea as
`herdr-telegram-agents` / `herdr-notifications`, built for WeCom's protocol.

- **Herdr → WeCom**: when an agent goes **blocked** (dialog/approval), **done** or **idle**
  after working, the bot DMs you the agent's screen. Numbered dialogs also get a
  button card; pressing a button answers the dialog.
- **WeCom → Herdr**: chat with the bot to prompt agents, answer dialogs, send keys,
  view screens, and start new agents.

## How it works

```
 WeCom client ──► wss://openws.work.weixin.qq.com ◄── herdr-wecom daemon ──► ~/.config/herdr/herdr.sock
                 (aibot_subscribe, one WS per bot)        (Go, detached)        (agent.list / read / prompt / send_keys …)
```

- `[[startup]]` runs `bin/herdr-wecom startup`, which spawns a detached daemon when
  configured (pid + log in the plugin state dir).
- The daemon polls `agent.list` every second to detect status transitions (the same
  model as herdr-notifications), and reads screens with `agent.read`.
- WeCom wire details reuse what the xwecom Hermes plugin measured: `aibot_respond_msg`
  replies on the inbound `req_id` (FIFO acks), `aibot_send_msg` for proactive DMs,
  `button_interaction` template cards + `aibot_respond_update_msg` to update them after a
  click, 30 msgs/min per chat, and `disconnected_event` when another connection for the same bot
  takes over. **One bot = one WebSocket.** Don't run Hermes' wecom/xwecom adapter on the same bot.

## Install

```bash
make build                      # needs Go
herdr plugin link "$PWD"
herdr plugin action invoke crazyhulk.wecom.setup   # enter Bot ID / Secret, get a pairing code
```

In WeCom, DM the bot `/pair <code>`. Paired users can control Herdr and receive notifications.
Alternatively write the config directly:

```bash
HERDR_PLUGIN_CONFIG_DIR="$(herdr plugin config-dir crazyhulk.wecom)" \
  bin/herdr-wecom configure --bot-id ID --secret SECRET --allow <your-userid>
herdr plugin action invoke crazyhulk.wecom.start
```

Send `/whoami` to the bot to learn your userid (unauthorized users get it in the refusal too).

`config.json` (config dir, re-read on every event, so no restart is needed):

```json
{
  "bot_id": "…", "secret": "…",
  "allow_users": ["userid"],
  "home_users": [],
  "notify": {"blocked": true, "done": true, "idle": true, "min_work_seconds": 0, "screen_lines": 30},
  "default_kind": "claude"
}
```

`home_users` (who receives notifications) defaults to every allowed + paired user.

## Using it in WeCom

Every agent has a stable number `#N` that is shown in each notification.

| You send | Effect |
|---|---|
| plain text | prompt to the current agent (`agent.prompt`) |
| `#2 text` | prompt to agent #2, which also becomes current |
| quote-reply a notification | goes to the agent that notification is about |
| `1`..`9`, `y`, `n`, `enter`, `esc` while blocked | sent as keys (answers the dialog) |
| free text to a Claude question with "Type something" | picks that entry and types the text |
| card button | sends that option number |
| `/list` | all agents with status; 👈 marks the current one |
| `/use N` or `#N` | switch current agent |
| `/screen [N] [lines]` | screen capture |
| `/keys [#N] esc enter …` | raw keys |
| `/stop [N]` / `/interrupt [N]` | `esc` / `ctrl+c` |
| `/focus [N]` | focus it in Herdr |
| `/new [kind] [dir \| wN]` | new tab (or a workspace at `dir`) and start an agent |
| `/ws` | workspaces |
| `/mute` `/unmute` | pause notifications |
| other `/cmd` (e.g. `/clear`, `/compact`) | passed to the current agent |

If no agent is selected, messages go to the only agent, or to the only blocked agent.

## Herdr actions

`setup`, `pair` (new one-time code), `start`, `stop`, `restart`, `status`, `send-test`, `logs`:

```bash
herdr plugin action invoke crazyhulk.wecom.<action>
```

After changing code: `make build && herdr plugin action invoke crazyhulk.wecom.restart`.
The `startup` hook only runs when the Herdr server starts.

## Layout

```
cmd/herdr-wecom/      entrypoints: daemon, startup, actions, setup/logs panes, configure
internal/wecom/       WeCom AI-bot WebSocket client (auth, heartbeat, reconnect, acks, cards, rate limit)
internal/herdr/       Herdr socket client (NDJSON, one request per connection)
internal/bridge/      transition detection, notifications, routing, commands
internal/screen/      Claude input-frame trimming, numbered-option detection
internal/config/      config.json / state.json (pairing, targets, agent numbers)
internal/proc/        detached daemon + pid file
```

## Tests

```bash
make test
```
