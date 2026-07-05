# gmtui

A terminal UI for [gmcli](https://github.com/fdsouvenir/gmcli): browse,
search, and send Google Messages from your terminal — with a human approval
queue for messages proposed by AI agents.

```
┌ chats ───────────────┐┌ Alice Chen ─────────────────────────────┐
│▍  Weekend Crew📌 2h  ││ Alice Chen  5m                          │
│ ● Alice Chen 1m      ││   hey! are we still on for pizza?       │
│   Dad 1d             ││ me  4m                                  │
│                      ││   yes! 7pm at the usual place           │
└──────────────────────┘└─────────────────────────────────────────┘
 ● connected · sends: approve · 1 pending approval — press a
```

gmtui is a thin client: it holds no protocol logic and no database. It talks
JSON-RPC over the unix socket exposed by `gmcli serve`, which owns the
Google Messages session, the SQLite archive, and the send policy.

## Run

```sh
gmcli auth     # once: pair with your phone (QR code)
gmtui          # that's it
```

If no daemon is listening, gmtui starts one automatically (`gmcli serve
--auto`: sends gated by the approval queue, exits ~10 minutes after the last
client disconnects). The `gmcli` binary must be on your PATH (or pass
`--gmcli <path>`). For an always-on daemon, run `gmcli serve` yourself —
gmtui will use it.

Flags: `--store <dir>` / `--socket <path>` to point at a non-default store,
`--offline` to browse an archive without a phone connection,
`--no-autostart` to fail instead of spawning a daemon.

`gmtui` finds the daemon socket the same way gmcli does
(`$XDG_STATE_HOME/gmcli/gmcli.sock`, falling back to
`~/.local/state/gmcli/gmcli.sock`).

## Keys

| Key       | Action                                             |
| --------- | -------------------------------------------------- |
| `j`/`k`   | move (arrows work too)                             |
| `enter`   | open conversation / open search hit in context     |
| `tab`     | switch pane                                        |
| `/`       | full-text search across all messages (FTS5 syntax) |
| `i`       | compose to the selected conversation               |
| `a`       | review pending agent send requests                 |
| `y` / `n` | approve (sends!) / deny the selected request       |
| `r`       | refresh                                            |
| `g`/`G`   | jump to top / bottom                               |
| `q`       | quit                                               |

## The approval queue

AI agents connected through `gmcli mcp` (or any RPC client) cannot send
messages directly under the default daemon policy. Their `send_text` calls
become *pending approvals*. gmtui surfaces them live — a status-bar warning
plus the `a` overlay — and nothing touches your phone until you press `y`.
Messages you compose inside gmtui are approved automatically (you are the
human in the loop).

## Live updates

gmtui subscribes to the daemon's event stream: new messages, conversation
updates, approval requests, and connection state changes render as they
happen, no polling.

## License

AGPL-3.0-or-later, same as the rest of the repository.
