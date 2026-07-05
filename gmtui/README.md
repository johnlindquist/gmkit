# gmtui

A terminal UI for [gmcli](https://github.com/johnlindquist/gmkit): browse,
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
gmtui          # that's it — pairing happens in the TUI too (press p)
```

On first run (or when Google expires the pairing), press `p`: a QR code
renders right in the terminal — scan it with Google Messages → Settings →
Device pairing. The daemon restarts itself with the new session and
everything syncs. (`gmcli auth` in a terminal still works if you prefer.)

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

## Search-first

gmtui launches straight into search: just start typing. People and chats
filter instantly (names, group titles, phone numbers — including contacts
resolved server-side); full-text message hits stream in as you type,
debounced against the daemon's FTS index. Each hit shows who said it,
where, when (relative + absolute), and a readable excerpt with the match
highlighted — and on wide terminals a live **thread preview pane** shows
the selected hit in its surrounding conversation, anchor marked. `Enter`
opens a chat, or opens a message hit in its full context. `Esc` drops into
the classic two-pane browse layout; `/` gets you back to search from
anywhere.

## Connection self-healing

The status bar always tells you the truth and gmtui fixes problems itself:
daemon status is re-polled every ~10s, and if the daemon exits or crashes,
gmtui restarts it and reconnects automatically (with backoff), refetching
everything once it's back. `● connected` / `◌ offline archive` /
`○ phone relay down — auto-retrying` (with what to check) /
`○ daemon lost — restarting it now…` are the four states; none of them
need manual intervention. Press `r` in browse mode to force a refresh.

## Fresh data & notifications

On connect, gmtui asks the daemon to pull the latest messages from your
phone (`sync.refresh`) and refetches when the import lands — so the launch
screen reflects texts from a minute ago, not from the last time a daemon
ran. Incoming messages ring the terminal bell and flash in the status bar;
daemons auto-started by gmtui also run with `--notify`, which posts desktop
notifications (macOS/Linux) for incoming texts while the daemon is alive.
For always-on notifications, run `gmcli --read-only=false serve --notify`
under a supervisor.

## Keys

| Key            | Action                                                  |
| -------------- | ------------------------------------------------------- |
| type           | search people, chats, and messages (launch screen)      |
| `↑`/`↓`        | move through results while typing (`ctrl-j/k/n/p` too)  |
| `enter`        | open conversation / open message hit in context         |
| `ctrl-u`       | clear the search query                                  |
| `esc`          | search → browse; messages → back where you came from    |
| `/` or `s`     | back to search from browse                              |
| `j`/`k`, `tab` | move / switch pane in browse mode                       |
| `i`            | compose to the selected conversation                    |
| `p`            | pair / re-pair with your phone (QR in the terminal)     |
| `b`            | backfill older messages for the open conversation      |
| `a`            | review pending agent send requests                      |
| `y` / `n`      | approve (sends!) / deny the selected request            |
| `r`            | refresh · `g`/`G` top/bottom · `q` quit (`ctrl-c` anywhere) |

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
