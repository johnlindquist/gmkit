# gmcli daemon RPC protocol

`gmcli serve` exposes a local control surface over a unix domain socket
(default `<store>/gmcli.sock`, mode 0600). The protocol is newline-delimited
JSON-RPC 2.0 — one JSON document per line — chosen so any language (and any
agent runtime) can speak it without codegen. gmtui and `gmcli mcp` are the
first-party clients; this document is the reference for writing your own.

## Framing

```
-> {"jsonrpc":"2.0","id":1,"method":"chats.list","params":{"limit":20}}
<- {"jsonrpc":"2.0","id":1,"result":[ ... ]}
<- {"jsonrpc":"2.0","id":2,"error":{"code":1002,"message":"conversation not found: x"}}
```

Requests may be pipelined; responses can arrive out of order (match on `id`).
A request without an `id` is treated as a notification and gets no reply.
Lines are capped at 4 MiB.

After a client calls `subscribe`, the server pushes events as JSON-RPC
notifications on the same connection:

```
<- {"jsonrpc":"2.0","method":"event","params":{"type":"message.new","data":{...}}}
```

## Error codes

| Code   | Meaning                                                        |
| ------ | -------------------------------------------------------------- |
| -32700 | parse error                                                    |
| -32600 | invalid request                                                |
| -32601 | method not found                                               |
| -32602 | invalid params                                                 |
| -32603 | internal error                                                 |
| 1001   | sends disabled (daemon started read-only)                      |
| 1002   | entity not found                                               |
| 1003   | approval already resolved                                      |
| 1004   | phone/relay unavailable (offline daemon, disconnected, or send failure) |

## Methods

Read methods (always available):

| Method             | Params                                                              | Result |
| ------------------ | ------------------------------------------------------------------- | ------ |
| `ping`             | —                                                                   | `{pong, version, schema_version}` |
| `status`           | —                                                                   | `{connected, send_mode, pending_approvals, conversations, messages, last_event_ms, last_connect_ms, updated_at_ms}` |
| `subscribe`        | —                                                                   | `{subscribed: true}`; events start flowing on this connection |
| `chats.list`       | `{limit?, unread_only?, pinned?}`                                   | array of conversations |
| `chats.find`       | `{query, limit?}` — person/group/number fragment                    | array of conversations, matched on name, alias, participants, and contacts |
| `chats.show`       | `{conversation_id, limit?}`                                         | `{conversation, messages}` (messages newest-first) |
| `messages.list`    | `{conversation_id?, sender_id?, since_ms?, until_ms?, limit?, order?}` | array of messages |
| `sync.refresh`     | —                                                                   | `{started}`; pulls the latest inbox conversations/messages from the phone in the background (rate-limited, 30s) and then broadcasts `sync.status {state: "refreshed"}`. Clients call it on connect. |
| `messages.search`  | `{query, conversation_id?, since_ms?, until_ms?, limit?}`           | array of `{message_id, conversation_id, conversation_name, sender_name, body, snippet, timestamp_ms, timestamp_iso, is_from_me}` |
| `messages.show`    | `{message_id}`                                                      | message |
| `messages.context` | `{message_id, before?, after?}` (default 5/5)                       | array of messages, oldest-first, anchor included |
| `contacts.search`  | `{query?, limit?}`                                                  | array of contacts |
| `contacts.show`    | `{id}` — participant_id or phone number                             | contact |

Phone-touching methods (need a non-`--offline` daemon; sends additionally
need `--read-only=false`):

| Method              | Params                                    | Result |
| ------------------- | ----------------------------------------- | ------ |
| `history.backfill`  | `{conversation_id, requests?, count?}`    | backfill report |
| `history.lookup`    | `{phone, requests?, count?}` (E.164)      | `{conversation_id, name, backfill}` |
| `send.text`         | `{conversation_id, body, reply_to_id?, requested_by?}` | approval row (see below) |
| `approvals.list`    | `{status?, limit?}`                       | array of approvals |
| `approvals.approve` | `{approval_id}` — **performs the send**   | resolved approval |
| `approvals.deny`    | `{approval_id, reason?}`                  | resolved approval |

### Search semantics

`messages.search` degrades gracefully so natural-language queries always
work: the query runs verbatim first (full FTS5 syntax — quoted phrases,
AND/OR/NOT), and if FTS5 rejects it, terms are cleaned (edge punctuation
trimmed, sub-3-character terms and stopwords dropped) and retried quoted
with AND semantics. When requiring every term finds nothing — and the query
contains no explicit FTS5 syntax — a final OR pass surfaces per-term
matches. Terms need 3+ characters to match (trigram index). Results are
newest-first.

Messages returned by `chats.show`, `messages.list`, and `messages.context`
are enriched with `sender_name` (alias > contact name > number) and
`timestamp_iso`; search hits additionally carry `conversation_name`.

## Send policy and the approval queue

The daemon's send mode is fixed at startup:

- **off** (default; daemon started without `--read-only=false`): `send.text`
  and `approvals.approve` return code 1001.
- **approve** (`--read-only=false serve`): `send.text` inserts a `pending`
  approval row and returns it — *nothing is sent*. A human resolves it via
  `approvals.approve` (gmtui `y`, or `gmcli approvals approve`). Approving
  performs the send and resolves the row to `sent` or `failed`; a failed send
  leaves useful detail in `error`.
- **direct** (`--read-only=false serve --sends direct`): `send.text` sends
  immediately; an audit row is still written to the approvals table.

Approval rows: `{approval_id, conversation_id, body, reply_to_id?,
requested_by, status: pending|sent|failed|denied|canceled, error?,
message_id?, created_at_ms, updated_at_ms}`. Double-approve races lose
cleanly with code 1003.

## Events

| Type                   | Data                                             |
| ---------------------- | ------------------------------------------------ |
| `message.new`          | `{message, is_old}` — after the row is persisted |
| `conversation.updated` | `{conversation}`                                 |
| `approval.requested`   | approval row                                     |
| `approval.resolved`    | approval row                                     |
| `sync.status`          | `{state}`: `ready`, `refreshed` (bulk import finished — refetch), `refresh_failed`, `phone_not_responding`, `phone_responding`, `listen_temporary_error`, `listen_recovered`, `logged_out` |

Slow consumers may have events dropped rather than stall the daemon; treat
the stream as advisory and re-query when reconnecting.

## Daemon lifecycle

- The socket accepts connections immediately on startup; the phone
  connection and initial import proceed in the background. Query the archive
  right away and watch `sync.status` / `status` for connection state.
- `gmcli serve --auto` is the on-demand mode used by gmtui, `gmcli mcp`, and
  `gmcli approvals`: approval-gated sends (unless `--read-only` is passed
  explicitly) and `--idle-exit 10m` by default — the daemon exits after ten
  minutes with no connected clients. Clients spawn it automatically when the
  socket is dead, so users normally never run `serve` by hand.
- `--idle-exit <duration>` works on any serve invocation; `0` (the default
  outside `--auto`) means run forever.
- Auto-started daemons log to `<store>/daemon.log`.

## Concurrency notes

- Only run **one** connected session per store: the daemon owns the libgm
  long-poll. Don't run `gmcli sync`/`send`/`history` CLI commands against the
  same store while `serve` is up — use the socket instead.
- `gmcli serve --offline` serves queries and the approval queue without a
  phone connection (useful for browsing an archive or developing clients).
  Auto-start falls back to offline mode automatically when the store has no
  paired session.
- Two clients racing to auto-start is safe: the second daemon finds the
  socket live and exits; the winner serves both.
