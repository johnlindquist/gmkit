# gmcli

A standalone Go CLI that connects to Google Messages, archives conversations
into a local SQLite + FTS5 database, and exposes a query surface suitable for
shell use and LLM tool integrations.

> **Status:** Feature-complete (Phases 1–4). Pairing, session persistence,
> sync loop, query CLI (`messages`, `contacts`, `chats`), and an LLM skill
> (`skills/google-messages`) are all wired up. See
> [`docs/research/phase-1-libgmessages.md`](docs/research/phase-1-libgmessages.md)
> for the design notes that motivated this layout, and
> [`skills/README.md`](skills/README.md) for the skill installation guide.

## What it is

- **Standalone.** No Matrix server, no Docker, no bridge daemon. Just a Go
  binary, a SQLite file, and a phone running Google Messages.
- **Read-first.** Mutating operations (sending, deleting, reacting) are gated
  behind explicit flags. The default is to observe, not to act.
- **Local.** Messages live in a single SQLite database under your data
  directory (XDG-compliant). Nothing is uploaded anywhere.
- **AGPL-3.0.** gmcli imports `pkg/libgm` from
  [mautrix/gmessages](https://github.com/mautrix/gmessages), which is licensed
  AGPL-3.0. That makes gmcli a derivative work and obligates the same license
  for the whole program. See `LICENSE` and `NOTICE`.

## How it works

`pkg/libgm` reverse-engineers the Google Messages web client protocol. After a
one-time QR pairing handshake, it maintains an authenticated session with
your paired phone — all messages flow through the phone, which proxies them
to Google's relay infrastructure. gmcli wraps that session with an event loop
that writes incoming messages, conversation updates, and contact data to a
local SQLite database, and exposes the database through a CLI.

The phone must be online and have Google Messages installed for the relay to
work. Pairing tokens are refreshed automatically; full re-pairing is required
roughly every 14 days of inactivity (Google's policy, not ours).

## Install

Requires Go 1.24 or newer.

```sh
git clone https://github.com/fdsouvenir/gmcli
cd gmcli
go build -o gmcli ./...
```

A pre-built binary distribution and Homebrew formula will land alongside the
v0.1 release.

## Quick start

```sh
# 1. One-time pairing (renders a QR code in the terminal — scan with the
#    Google Messages app on your phone, Settings → Device pairing → QR code).
gmcli auth

# 2. Sync messages from the phone into the local database. --follow keeps
#    the connection open and writes new messages as they arrive.
gmcli sync --follow

# 3. Query the local archive (read-only).
gmcli chats list                              # most-recent conversations
gmcli chats show <conversation-id>            # header + recent messages
gmcli messages search "dinner"                # FTS5 across all conversations
gmcli messages list --conv <conv-id>          # message list with filters
gmcli messages show <message-id>              # single message detail
gmcli messages context <message-id>           # surrounding messages
gmcli contacts search alice                   # name/number/alias substring match
gmcli contacts show <participant-id-or-num>   # contact detail

# 4. Local-only labels (also requires --read-only=false).
gmcli --read-only=false contacts alias set --id <pid> --alias "Mom"
gmcli contacts alias list                     # list all set aliases
gmcli --read-only=false contacts alias rm --id <pid>

# 5. Write to the phone (always requires --read-only=false).
gmcli --read-only=false send text --to <conv-id> --message "on my way"
gmcli --read-only=false send react --message <msg-id> --emoji "👍"
gmcli --read-only=false media download --message <msg-id>

# Every command supports --json for machine-readable output and --full to
# disable truncation in tables.
gmcli --json chats list | jq '.[0].name'
```

## Global flags

| Flag             | Default                            | Purpose                                                  |
| ---------------- | ---------------------------------- | -------------------------------------------------------- |
| `--store DIR`    | `$XDG_STATE_HOME/gmcli`            | Where session, SQLite, and downloaded media live.        |
| `--read-only`    | `true`                             | Block any command that would mutate the phone or store. |
| `--json`         | `false`                            | Emit machine-readable output.                            |
| `--full`         | `false`                            | Disable truncation in tabular output.                    |
| `--log-level`    | `info`                             | Verbosity (`trace`/`debug`/`info`/`warn`).               |

## Layout

```
cmd/                  Cobra command tree (auth, sync, version, doctor,
                      messages, contacts, chats, send, media)
internal/
  gm/                 libgm wrapper — pairing, session, events, send/react,
                      WaitForReady, DownloadMedia
  store/              SQLite + FTS5 store (schema v2: + aliases table)
  sync/               Event-to-store pump
  output/             Shared JSON / tab-aligned table renderers
  paths/              XDG path resolution (XDG_STATE_HOME)
  logging/            zerolog setup
skills/
  google-messages/    LLM skill bundle — read-only playbook for assistants
docs/research/        Phase 1 research notes
```

## LLM integration

`skills/google-messages/SKILL.md` is a Claude Code / OpenClaw–compatible
skill that wraps `gmcli` so an assistant can answer questions like "what
did Alice text me about dinner?" or "search my messages for flight
confirmation". The skill always invokes `gmcli --read-only --json`,
includes a verb decision tree, and carries a strong prompt-injection
preamble: untrusted message content is treated as data, never as
instructions. See [`skills/README.md`](skills/README.md) for install
instructions.

## Privacy

- All data is local. gmcli does not phone home.
- Session tokens are stored in `$XDG_DATA_HOME/gmcli/session.json` with mode
  0600.
- Media attachments are referenced by ID in the database; bytes are not
  downloaded by default. Phase 3 will add `gmcli media download <message-id>`
  for explicit downloads.
- The SQLite file is unencrypted. If you need at-rest encryption, layer your
  own filesystem encryption (FileVault, LUKS, etc.).

## Attribution

- **libgm** — the Google Messages protocol library this CLI depends on — was
  written by Tulir Asokan and the
  [mautrix](https://github.com/mautrix/gmessages) contributors. License:
  AGPL-3.0. gmcli would not be possible without their reverse-engineering
  work.
- The CLI verb structure is inspired by Peter Steinberger's
  [wacli](https://github.com/steipete/wacli) for WhatsApp.
- Storage and MCP-tool patterns draw from
  [openmessage](https://github.com/MaxGhenis/openmessage) by Max Ghenis,
  released under the Unlicense.

## License

GNU Affero General Public License, version 3 or later. See `LICENSE` for the
full text and `NOTICE` for the third-party notices required by upstream
licenses.
