//! Application state and update logic. All RPC work happens in spawned
//! tasks that report back through the AppEvent channel — the update/draw
//! loop never blocks on the network.
//!
//! The default experience is the omni search: gmtui launches into a query
//! box over a unified people/chats/messages result list. Chats filter
//! instantly from memory; message hits stream in from the daemon's FTS
//! index, debounced by the tick loop and guarded by a generation counter so
//! stale responses never clobber fresh ones.

use crossterm::event::{Event as TermEvent, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use ratatui::widgets::ListState;
use serde_json::json;
use tokio::sync::mpsc::UnboundedSender;
use tui_input::backend::crossterm::EventHandler;
use tui_input::Input;

use crate::model::{now_ms, Approval, Conversation, DaemonStatus, Message, SearchHit, ServerEvent};
use crate::rpc::{RpcClient, CODE_ALREADY_RESOLVED, CODE_SENDS_DISABLED};

/// Results of async work, delivered to the main loop.
#[derive(Debug)]
pub enum AppEvent {
    Chats(Vec<Conversation>),
    Messages {
        conversation_id: String,
        messages: Vec<Message>,
    },
    OmniResults {
        generation: u64,
        chats: Vec<Conversation>,
        msgs: Vec<SearchHit>,
    },
    OmniPreview(Preview),
    Context {
        anchor: String,
        conversation_id: String,
        messages: Vec<Message>,
    },
    Approvals(Vec<Approval>),
    Status(DaemonStatus),
    Flash(String),
    Server(ServerEvent),
    /// The daemon socket closed (daemon exited or crashed).
    DaemonLost,
    /// The auth.pair RPC itself failed (old daemon, offline, ...).
    PairingFailed(String),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Focus {
    /// The launch screen: live search over people, chats, and messages.
    Omni,
    Chats,
    Messages,
    Compose,
}

/// Where the in-TUI pairing flow currently is. Google killed QR pairing on
/// current Messages builds, so the Google-account (cookie + emoji) flow is
/// the primary path; QR remains reachable as a legacy fallback.
#[derive(PartialEq)]
pub enum PairStage {
    /// Collecting pasted browser cookies for the Google-account flow.
    EnterCookies,
    /// RPC accepted; waiting for the daemon to talk to Google.
    Waiting,
    /// Legacy QR flow: render this code.
    ShowQr,
    /// Google flow: tap this emoji on the phone.
    Emoji(String),
    Failed,
    Succeeded,
}

/// State of the in-TUI pairing flow.
pub struct Pairing {
    pub stage: PairStage,
    pub input: Input,
    /// Rendered unicode QR (multi-line), legacy flow only.
    pub qr: Option<String>,
    pub url: Option<String>,
    pub message: String,
}

/// A one-line text prompt overlay (alias editing, reactions).
pub enum PromptKind {
    Alias { conversation_id: String },
    React { message_id: String },
}

pub struct Prompt {
    pub kind: PromptKind,
    pub input: Input,
    pub title: String,
}

/// Identifies what the omni preview pane should show for a selection.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum PreviewKey {
    /// A message hit: preview its surrounding thread context.
    Msg(String),
    /// A chat hit: preview its most recent messages.
    Chat(String),
}

/// A loaded thread preview for the selected omni result.
#[derive(Debug, Clone)]
pub struct Preview {
    pub key: PreviewKey,
    pub title: String,
    /// Highlighted message within the preview (the search hit).
    pub anchor: Option<String>,
    pub messages: Vec<Message>,
}

/// State for the omni search screen.
#[derive(Default)]
pub struct OmniState {
    pub input: Input,
    /// Query changed since the last dispatch; the tick loop debounces.
    pub dirty: bool,
    /// Monotonic id matching in-flight requests to the query that sent them.
    pub generation: u64,
    pub searching: bool,
    pub chat_results: Vec<Conversation>,
    pub msg_results: Vec<SearchHit>,
    /// Index into the unified selectable list: chats first, then messages.
    pub selected: usize,
    pub list_state: ListState,
    /// What the preview pane currently targets (may still be loading).
    pub preview_key: Option<PreviewKey>,
    pub preview: Option<Preview>,
}

impl OmniState {
    pub fn total(&self) -> usize {
        self.chat_results.len() + self.msg_results.len()
    }
}

/// How many chat rows the omni list shows at most.
const OMNI_CHAT_LIMIT: usize = 50;

pub struct App {
    rpc: RpcClient,
    tx: UnboundedSender<AppEvent>,
    pub should_quit: bool,
    /// Ring the terminal bell on the next draw (incoming message).
    bell: bool,
    /// The daemon socket is gone; a background loop is bringing it back.
    pub daemon_lost: bool,
    /// A reconnect task is already running (main loop's guard).
    pub reconnecting: bool,
    ticks: u64,

    pub focus: Focus,
    pub omni: OmniState,
    preview_cache: std::collections::HashMap<PreviewKey, Preview>,
    pub compose_input: Input,
    compose_return: Focus,
    /// Where Esc from the messages pane goes back to.
    messages_back: Focus,

    pub chats: Vec<Conversation>,
    pub chat_state: ListState,

    pub current_chat: Option<String>,
    pub messages: Vec<Message>,
    pub msg_state: ListState,
    pub anchor_id: Option<String>,

    pub show_approvals: bool,
    pub approvals: Vec<Approval>,
    pub approval_state: ListState,

    /// Some(_) while the pairing overlay is open.
    pub pairing: Option<Pairing>,
    /// Some(_) while a text prompt (alias, reaction) is open.
    pub prompt: Option<Prompt>,
    /// Diagnostics / key-reference overlay.
    pub show_doctor: bool,

    pub status: DaemonStatus,
    pub flash: Option<(String, i64)>,
}

impl App {
    pub fn new(rpc: RpcClient, tx: UnboundedSender<AppEvent>) -> Self {
        Self {
            rpc,
            tx,
            should_quit: false,
            bell: false,
            daemon_lost: false,
            reconnecting: false,
            ticks: 0,
            focus: Focus::Omni,
            omni: OmniState::default(),
            preview_cache: std::collections::HashMap::new(),
            compose_input: Input::default(),
            compose_return: Focus::Omni,
            messages_back: Focus::Omni,
            chats: Vec::new(),
            chat_state: ListState::default(),
            current_chat: None,
            messages: Vec::new(),
            msg_state: ListState::default(),
            anchor_id: None,
            show_approvals: false,
            approvals: Vec::new(),
            approval_state: ListState::default(),
            pairing: None,
            prompt: None,
            show_doctor: false,
            status: DaemonStatus::default(),
            flash: None,
        }
    }

    // ---- async fetch helpers -------------------------------------------

    fn spawn<F>(&self, fut: F)
    where
        F: std::future::Future<Output = Option<AppEvent>> + Send + 'static,
    {
        let tx = self.tx.clone();
        tokio::spawn(async move {
            if let Some(ev) = fut.await {
                let _ = tx.send(ev);
            }
        });
    }

    pub fn fetch_chats(&self) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc.call_as("chats.list", json!({"limit": 500})).await {
                Ok(chats) => Some(AppEvent::Chats(chats)),
                Err(e) => Some(AppEvent::Flash(format!("chats: {e}"))),
            }
        });
    }

    pub fn fetch_status(&self) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc.call_as("status", json!({})).await {
                Ok(status) => Some(AppEvent::Status(status)),
                Err(e) => Some(AppEvent::Flash(format!("status: {e}"))),
            }
        });
    }

    pub fn fetch_approvals(&self) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc
                .call_as("approvals.list", json!({"status": "pending", "limit": 100}))
                .await
            {
                Ok(approvals) => Some(AppEvent::Approvals(approvals)),
                Err(e) => Some(AppEvent::Flash(format!("approvals: {e}"))),
            }
        });
    }

    fn open_chat(&mut self, conversation_id: String) {
        self.current_chat = Some(conversation_id.clone());
        self.anchor_id = None;
        let rpc = self.rpc.clone();
        self.spawn(async move {
            #[derive(serde::Deserialize)]
            struct Show {
                messages: Vec<Message>,
            }
            match rpc
                .call_as::<Show>(
                    "chats.show",
                    json!({"conversation_id": conversation_id, "limit": 200}),
                )
                .await
            {
                Ok(mut show) => {
                    // Server returns newest-first; the pane renders oldest-first.
                    show.messages.sort_by_key(|m| m.timestamp_ms);
                    Some(AppEvent::Messages {
                        conversation_id,
                        messages: show.messages,
                    })
                }
                Err(e) => Some(AppEvent::Flash(format!("open chat: {e}"))),
            }
        });
    }

    fn open_context(&mut self, hit: SearchHit) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc
                .call_as::<Vec<Message>>(
                    "messages.context",
                    json!({"message_id": hit.message_id, "before": 25, "after": 25}),
                )
                .await
            {
                Ok(messages) => Some(AppEvent::Context {
                    anchor: hit.message_id,
                    conversation_id: hit.conversation_id,
                    messages,
                }),
                Err(e) => Some(AppEvent::Flash(format!("context: {e}"))),
            }
        });
    }

    /// Compose flow. The human typed this in the TUI, so in approve mode we
    /// queue and immediately approve our own proposal — the review queue is
    /// for messages *other* processes (agents) want to send.
    fn send_message(&mut self, conversation_id: String, body: String) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            let approval: Approval = match rpc
                .call_as(
                    "send.text",
                    json!({
                        "conversation_id": conversation_id,
                        "body": body,
                        "requested_by": "gmtui",
                    }),
                )
                .await
            {
                Ok(a) => a,
                Err(e) => {
                    if e.code() == Some(CODE_SENDS_DISABLED) {
                        return Some(AppEvent::Flash(
                            "sends disabled — restart daemon with `gmcli --read-only=false serve`"
                                .to_string(),
                        ));
                    }
                    return Some(AppEvent::Flash(format!("send: {e}")));
                }
            };
            if approval.status == "sent" {
                return Some(AppEvent::Flash("sent ✓".to_string()));
            }
            match rpc
                .call(
                    "approvals.approve",
                    json!({"approval_id": approval.approval_id}),
                )
                .await
            {
                Ok(_) => Some(AppEvent::Flash("sent ✓".to_string())),
                Err(e) => Some(AppEvent::Flash(format!("send failed: {e}"))),
            }
        });
    }

    fn resolve_approval(&mut self, approval_id: String, approve: bool) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            let method = if approve {
                "approvals.approve"
            } else {
                "approvals.deny"
            };
            match rpc.call(method, json!({"approval_id": approval_id})).await {
                Ok(_) => Some(AppEvent::Flash(if approve {
                    "approved and sent ✓".to_string()
                } else {
                    "denied".to_string()
                })),
                Err(e) if e.code() == Some(CODE_ALREADY_RESOLVED) => {
                    Some(AppEvent::Flash("already resolved".to_string()))
                }
                Err(e) if e.code() == Some(CODE_SENDS_DISABLED) => Some(AppEvent::Flash(
                    "sends disabled — restart daemon with `gmcli --read-only=false serve`"
                        .to_string(),
                )),
                Err(e) => Some(AppEvent::Flash(format!("approval: {e}"))),
            }
        });
    }

    pub fn flash(&mut self, msg: impl Into<String>) {
        self.flash = Some((msg.into(), now_ms()));
    }

    // ---- omni search ----------------------------------------------------

    /// Instant, in-memory filter over the loaded chat list. Matches display
    /// names, participant names, and phone numbers. Empty query = recent
    /// chats, so the launch screen doubles as a recents picker.
    fn local_chat_matches(&self, query: &str) -> Vec<Conversation> {
        if query.is_empty() {
            return self.chats.iter().take(OMNI_CHAT_LIMIT).cloned().collect();
        }
        let q = query.to_lowercase();
        self.chats
            .iter()
            .filter(|c| {
                c.display_name().to_lowercase().contains(&q)
                    || c.participants().into_iter().any(|p| {
                        p.name.to_lowercase().contains(&q) || (!q.is_empty() && p.e164.contains(&q))
                    })
            })
            .take(OMNI_CHAT_LIMIT)
            .cloned()
            .collect()
    }

    /// Re-filter chats immediately after a keystroke and mark the query
    /// dirty so the tick loop dispatches the remote search.
    fn refresh_omni_local(&mut self) {
        let query = self.omni.input.value().trim().to_string();
        self.omni.chat_results = self.local_chat_matches(&query);
        if query.chars().count() < 3 {
            // Below the trigram minimum nothing can match server-side.
            self.omni.msg_results.clear();
            self.omni.searching = false;
        }
        self.omni.selected = 0;
        self.omni.dirty = true;
    }

    /// Fire the remote search for the current query. Called from the tick
    /// loop (~250ms cadence), which is the debounce.
    fn dispatch_omni_search(&mut self) {
        let query = self.omni.input.value().trim().to_string();
        self.omni.generation += 1;
        let generation = self.omni.generation;
        if query.chars().count() < 2 {
            return; // local filter already covers this
        }
        self.omni.searching = true;
        let rpc = self.rpc.clone();
        let want_messages = query.chars().count() >= 3;
        self.spawn(async move {
            // Errors here are non-fatal (mid-typing): keep prior results.
            let chats: Vec<Conversation> = rpc
                .call_as("chats.find", json!({"query": query, "limit": 20}))
                .await
                .unwrap_or_default();
            let msgs: Vec<SearchHit> = if want_messages {
                rpc.call_as("messages.search", json!({"query": query, "limit": 50}))
                    .await
                    .unwrap_or_default()
            } else {
                Vec::new()
            };
            Some(AppEvent::OmniResults {
                generation,
                chats,
                msgs,
            })
        });
    }

    fn omni_activate(&mut self) {
        let nchats = self.omni.chat_results.len();
        if self.omni.selected < nchats {
            if let Some(c) = self.omni.chat_results.get(self.omni.selected) {
                let id = c.conversation_id.clone();
                self.open_chat(id);
                self.messages_back = Focus::Omni;
                self.focus = Focus::Messages;
            }
        } else if let Some(hit) = self.omni.msg_results.get(self.omni.selected - nchats) {
            self.open_context(hit.clone());
            self.messages_back = Focus::Omni;
            self.focus = Focus::Messages;
        }
    }

    pub fn enter_omni(&mut self) {
        self.focus = Focus::Omni;
        self.refresh_omni_local();
    }

    /// True once per requested bell; the main loop rings it.
    pub fn take_bell(&mut self) -> bool {
        std::mem::take(&mut self.bell)
    }

    /// Ask the daemon to pull the latest messages from the phone. Cheap and
    /// rate-limited server-side; errors (offline daemon, older daemon
    /// without the method) are ignored — reads still work from the archive.
    pub fn request_refresh(&self) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            let _ = rpc.call("sync.refresh", json!({})).await;
            None
        });
    }

    /// Open the pairing overlay in the Google-account flow: paste browser
    /// cookies, then tap the emoji Google shows on the phone. The daemon
    /// broadcasts pair.emoji / pair.success / pair.error; on success it
    /// restarts itself and our reconnect loop finishes the job.
    fn start_pairing(&mut self) {
        self.pairing = Some(Pairing {
            stage: PairStage::EnterCookies,
            input: Input::default(),
            qr: None,
            url: None,
            message: String::new(),
        });
    }

    /// Submit the pasted cookies to the daemon's Google pairing flow.
    fn submit_pairing_cookies(&mut self) {
        let Some(p) = self.pairing.as_mut() else {
            return;
        };
        let cookies = p.input.value().trim().to_string();
        if cookies.is_empty() {
            p.message = "paste your messages.google.com cookies first".to_string();
            return;
        }
        p.stage = PairStage::Waiting;
        p.message = "checking cookies with Google…".to_string();
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc
                .call("auth.pair.google", json!({"cookies_input": cookies}))
                .await
            {
                Ok(_) => None,
                Err(e) => Some(AppEvent::PairingFailed(format!("{e}"))),
            }
        });
    }

    /// Legacy QR flow (Google no longer offers the scanner on current
    /// Messages builds, but older builds still have it).
    fn start_qr_pairing(&mut self) {
        if let Some(p) = self.pairing.as_mut() {
            p.stage = PairStage::Waiting;
            p.message = "requesting a pairing QR from Google…".to_string();
        }
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc.call("auth.pair", json!({})).await {
                Ok(_) => None,
                Err(e) => Some(AppEvent::PairingFailed(format!(
                    "could not start QR pairing: {e}"
                ))),
            }
        });
    }

    /// Download the selected message's attachment and open it with the
    /// system opener.
    fn open_media(&mut self) {
        let Some(m) = self.msg_state.selected().and_then(|i| self.messages.get(i)) else {
            self.flash("no message selected");
            return;
        };
        if m.media_id.is_none() {
            self.flash("selected message has no attachment");
            return;
        }
        let message_id = m.message_id.clone();
        self.flash("downloading attachment…");
        let rpc = self.rpc.clone();
        self.spawn(async move {
            #[derive(serde::Deserialize, Default)]
            struct Dl {
                #[serde(default)]
                path: String,
            }
            match rpc
                .call_as::<Dl>("media.download", json!({"message_id": message_id}))
                .await
            {
                Ok(dl) if !dl.path.is_empty() => {
                    let opener = if cfg!(target_os = "macos") {
                        "open"
                    } else {
                        "xdg-open"
                    };
                    let _ = std::process::Command::new(opener).arg(&dl.path).spawn();
                    Some(AppEvent::Flash(format!("opened {}", dl.path)))
                }
                Ok(_) => Some(AppEvent::Flash("download returned no path".to_string())),
                Err(e) => Some(AppEvent::Flash(format!("media: {e}"))),
            }
        });
    }

    fn submit_prompt(&mut self) {
        let Some(prompt) = self.prompt.take() else {
            return;
        };
        let value = prompt.input.value().trim().to_string();
        let rpc = self.rpc.clone();
        match prompt.kind {
            PromptKind::Alias { conversation_id } => {
                // Empty input clears the alias.
                let clearing = value.is_empty();
                self.spawn(async move {
                    let (method, params) = if clearing {
                        ("alias.rm", json!({"target_type": "conversation", "target_id": conversation_id}))
                    } else {
                        ("alias.set", json!({"target_type": "conversation", "target_id": conversation_id, "alias": value}))
                    };
                    match rpc.call(method, params).await {
                        Ok(_) => Some(AppEvent::Flash(if clearing {
                            "alias cleared".to_string()
                        } else {
                            "renamed ✓".to_string()
                        })),
                        Err(e) => Some(AppEvent::Flash(format!("alias: {e}"))),
                    }
                });
                self.fetch_chats();
            }
            PromptKind::React { message_id } => {
                if value.is_empty() {
                    return;
                }
                self.spawn(async move {
                    match rpc
                        .call(
                            "send.react",
                            json!({"message_id": message_id, "emoji": value}),
                        )
                        .await
                    {
                        Ok(_) => Some(AppEvent::Flash(format!("reacted {value}"))),
                        Err(e) => Some(AppEvent::Flash(format!("react: {e}"))),
                    }
                });
            }
        }
    }

    /// Backfill older history for the currently open conversation.
    fn backfill_current_chat(&mut self) {
        let Some(chat) = self.current_chat.clone() else {
            self.flash("open a conversation first");
            return;
        };
        self.flash("backfilling older messages…");
        let rpc = self.rpc.clone();
        self.spawn(async move {
            #[derive(serde::Deserialize, Default)]
            struct Backfill {
                #[serde(default)]
                messages_added_for_chat: i64,
            }
            match rpc
                .call_as::<Backfill>(
                    "history.backfill",
                    json!({"conversation_id": chat, "requests": 10, "count": 50}),
                )
                .await
            {
                Ok(res) => Some(AppEvent::Flash(format!(
                    "backfill: +{} older message(s)",
                    res.messages_added_for_chat
                ))),
                Err(e) => Some(AppEvent::Flash(format!("backfill: {e}"))),
            }
        });
    }

    // ---- omni preview ----------------------------------------------------

    fn selected_preview_key(&self) -> Option<PreviewKey> {
        let nchats = self.omni.chat_results.len();
        if self.omni.total() == 0 {
            return None;
        }
        if self.omni.selected < nchats {
            self.omni
                .chat_results
                .get(self.omni.selected)
                .map(|c| PreviewKey::Chat(c.conversation_id.clone()))
        } else {
            self.omni
                .msg_results
                .get(self.omni.selected - nchats)
                .map(|h| PreviewKey::Msg(h.message_id.clone()))
        }
    }

    /// Keep the preview pane in sync with the selection. Runs on the tick
    /// (~250ms), so holding j/k scrolls freely without a fetch per row.
    fn sync_preview(&mut self) {
        let key = self.selected_preview_key();
        if key == self.omni.preview_key {
            return;
        }
        self.omni.preview_key = key.clone();
        self.omni.preview = None;
        let Some(key) = key else { return };
        if let Some(cached) = self.preview_cache.get(&key) {
            self.omni.preview = Some(cached.clone());
            return;
        }
        let rpc = self.rpc.clone();
        match key {
            PreviewKey::Msg(message_id) => {
                let nchats = self.omni.chat_results.len();
                let title = self
                    .omni
                    .msg_results
                    .get(self.omni.selected.saturating_sub(nchats))
                    .map(|h| {
                        if h.conversation_name.is_empty() {
                            h.conversation_id.clone()
                        } else {
                            h.conversation_name.clone()
                        }
                    })
                    .unwrap_or_default();
                self.spawn(async move {
                    let messages: Vec<Message> = rpc
                        .call_as(
                            "messages.context",
                            json!({"message_id": message_id, "before": 6, "after": 6}),
                        )
                        .await
                        .unwrap_or_default();
                    Some(AppEvent::OmniPreview(Preview {
                        key: PreviewKey::Msg(message_id.clone()),
                        title,
                        anchor: Some(message_id),
                        messages,
                    }))
                });
            }
            PreviewKey::Chat(conversation_id) => {
                let title = self
                    .omni
                    .chat_results
                    .iter()
                    .find(|c| c.conversation_id == conversation_id)
                    .map(|c| c.display_name())
                    .unwrap_or_default();
                self.spawn(async move {
                    #[derive(serde::Deserialize, Default)]
                    struct Show {
                        #[serde(default)]
                        messages: Vec<Message>,
                    }
                    let mut show: Show = rpc
                        .call_as(
                            "chats.show",
                            json!({"conversation_id": conversation_id, "limit": 12}),
                        )
                        .await
                        .unwrap_or_default();
                    show.messages.sort_by_key(|m| m.timestamp_ms);
                    Some(AppEvent::OmniPreview(Preview {
                        key: PreviewKey::Chat(conversation_id),
                        title,
                        anchor: None,
                        messages: show.messages,
                    }))
                });
            }
        }
    }

    // ---- event handling -------------------------------------------------

    pub fn on_tick(&mut self) {
        self.ticks += 1;
        if let Some((_, at)) = self.flash {
            if now_ms() - at > 5000 {
                self.flash = None;
            }
        }
        if self.omni.dirty {
            self.omni.dirty = false;
            self.dispatch_omni_search();
        }
        if self.focus == Focus::Omni {
            self.sync_preview();
        }
        // Poll daemon status every ~10s so the connection indicator heals
        // itself (the launch-time fetch can race the daemon's own phone
        // connect) and pending-approval counts stay honest.
        if !self.daemon_lost && self.ticks % 40 == 0 {
            self.fetch_status();
        }
    }

    /// Swap in a fresh daemon connection after a reconnect and re-sync
    /// everything visible.
    pub fn adopt_rpc(&mut self, rpc: RpcClient) {
        self.rpc = rpc;
        self.daemon_lost = false;
        self.reconnecting = false;
        self.preview_cache.clear();
        self.fetch_chats();
        self.fetch_status();
        self.fetch_approvals();
        self.request_refresh();
        self.flash("reconnected ✓");
    }

    pub fn handle_app_event(&mut self, ev: AppEvent) {
        match ev {
            AppEvent::Chats(chats) => {
                self.chats = chats;
                self.sort_chats();
                if self.chat_state.selected().is_none() && !self.chats.is_empty() {
                    self.chat_state.select(Some(0));
                }
                // Keep the launch screen's recents fresh.
                if self.focus == Focus::Omni && self.omni.input.value().trim().is_empty() {
                    self.omni.chat_results = self.local_chat_matches("");
                }
            }
            AppEvent::Messages {
                conversation_id,
                messages,
            } => {
                if self.current_chat.as_deref() == Some(conversation_id.as_str()) {
                    self.messages = messages;
                    self.select_message_anchor();
                }
            }
            AppEvent::OmniResults {
                generation,
                chats,
                msgs,
            } => {
                if generation != self.omni.generation {
                    return; // stale response from an older keystroke
                }
                self.omni.searching = false;
                // Server-side matches (contacts, aliases, numbers) that the
                // in-memory filter missed go after the local hits.
                for conv in chats {
                    if self.omni.chat_results.len() >= OMNI_CHAT_LIMIT {
                        break;
                    }
                    if !self
                        .omni
                        .chat_results
                        .iter()
                        .any(|c| c.conversation_id == conv.conversation_id)
                    {
                        self.omni.chat_results.push(conv);
                    }
                }
                self.omni.msg_results = msgs;
                let total = self.omni.total();
                if total == 0 {
                    self.omni.selected = 0;
                } else if self.omni.selected >= total {
                    self.omni.selected = total - 1;
                }
            }
            AppEvent::OmniPreview(preview) => {
                if self.preview_cache.len() > 200 {
                    self.preview_cache.clear();
                }
                self.preview_cache
                    .insert(preview.key.clone(), preview.clone());
                if self.omni.preview_key.as_ref() == Some(&preview.key) {
                    self.omni.preview = Some(preview);
                }
            }
            AppEvent::Context {
                anchor,
                conversation_id,
                messages,
            } => {
                self.current_chat = Some(conversation_id);
                self.messages = messages;
                self.anchor_id = Some(anchor);
                self.focus = Focus::Messages;
                self.select_message_anchor();
            }
            AppEvent::Approvals(approvals) => {
                self.approvals = approvals;
                let len = self.approvals.len();
                match self.approval_state.selected() {
                    Some(i) if i >= len && len > 0 => self.approval_state.select(Some(len - 1)),
                    None if len > 0 => self.approval_state.select(Some(0)),
                    _ if len == 0 => self.approval_state.select(None),
                    _ => {}
                }
            }
            AppEvent::Status(status) => {
                self.status = status;
            }
            AppEvent::Flash(msg) => self.flash(msg),
            AppEvent::Server(ev) => self.handle_server_event(ev),
            AppEvent::DaemonLost => {
                self.daemon_lost = true;
                self.status.connected = false;
            }
            AppEvent::PairingFailed(msg) => {
                if let Some(p) = self.pairing.as_mut() {
                    p.stage = PairStage::Failed;
                    p.message = format!("{msg} — press p to retry, esc to close");
                }
            }
        }
    }

    fn handle_server_event(&mut self, ev: ServerEvent) {
        match ev.kind.as_str() {
            "message.new" => {
                let msg: Option<Message> = ev
                    .data
                    .get("message")
                    .and_then(|m| serde_json::from_value(m.clone()).ok());
                let is_old = ev
                    .data
                    .get("is_old")
                    .and_then(serde_json::Value::as_bool)
                    .unwrap_or(false);
                if let Some(msg) = msg {
                    self.apply_incoming_message(msg, is_old);
                }
            }
            "conversation.updated" => {
                if let Some(conv) = ev
                    .data
                    .get("conversation")
                    .and_then(|c| serde_json::from_value::<Conversation>(c.clone()).ok())
                {
                    match self
                        .chats
                        .iter_mut()
                        .find(|c| c.conversation_id == conv.conversation_id)
                    {
                        Some(existing) => *existing = conv,
                        None => self.chats.push(conv),
                    }
                    self.sort_chats();
                    self.refresh_omni_recents();
                }
            }
            "approval.requested" => {
                if let Ok(a) = serde_json::from_value::<Approval>(ev.data.clone()) {
                    self.flash(format!(
                        "⚠ send approval requested by {} — press a to review",
                        a.requested_by
                    ));
                    self.approvals.retain(|x| x.approval_id != a.approval_id);
                    self.approvals.insert(0, a);
                    if self.approval_state.selected().is_none() {
                        self.approval_state.select(Some(0));
                    }
                    self.status.pending_approvals = self.approvals.len() as i64;
                }
            }
            "approval.resolved" => {
                if let Ok(a) = serde_json::from_value::<Approval>(ev.data.clone()) {
                    self.approvals.retain(|x| x.approval_id != a.approval_id);
                    self.status.pending_approvals = self.approvals.len() as i64;
                    if self.approvals.is_empty() {
                        self.approval_state.select(None);
                    } else if let Some(i) = self.approval_state.selected() {
                        if i >= self.approvals.len() {
                            self.approval_state.select(Some(self.approvals.len() - 1));
                        }
                    }
                }
            }
            "pair.qr" => {
                if let Some(p) = self.pairing.as_mut() {
                    let url = ev
                        .data
                        .get("url")
                        .and_then(serde_json::Value::as_str)
                        .unwrap_or_default()
                        .to_string();
                    p.stage = PairStage::ShowQr;
                    p.qr = render_qr(&url);
                    p.url = Some(url);
                    p.message =
                        "scan with Google Messages → Settings → Device pairing → QR scanner"
                            .to_string();
                }
            }
            "pair.emoji" => {
                if let Some(p) = self.pairing.as_mut() {
                    let emoji = ev
                        .data
                        .get("emoji")
                        .and_then(serde_json::Value::as_str)
                        .unwrap_or("?")
                        .to_string();
                    p.stage = PairStage::Emoji(emoji);
                    p.message = "on your phone: Google Messages shows several emoji — tap this one"
                        .to_string();
                }
            }
            "pair.success" => {
                if let Some(p) = self.pairing.as_mut() {
                    p.stage = PairStage::Succeeded;
                    p.qr = None;
                    p.message =
                        "paired ✓ — restarting the daemon with the new session…".to_string();
                }
                self.status.auth_expired = false;
            }
            "pair.error" => {
                if let Some(p) = self.pairing.as_mut() {
                    p.stage = PairStage::Failed;
                    p.qr = None;
                    p.message = format!(
                        "pairing failed: {} — press p to retry, esc to close",
                        ev.data
                            .get("error")
                            .and_then(serde_json::Value::as_str)
                            .unwrap_or("unknown error")
                    );
                }
            }
            "sync.status" => {
                let state = ev
                    .data
                    .get("state")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or("");
                match state {
                    "ready" | "listen_recovered" | "phone_responding" => {
                        self.status.connected = true;
                        self.status.auth_expired = false;
                    }
                    "refreshed" => {
                        // The daemon just imported fresh data outside the
                        // event stream: refetch everything visible.
                        self.status.connected = true;
                        self.status.auth_expired = false;
                        self.fetch_chats();
                        self.fetch_status();
                        if let Some(chat) = self.current_chat.clone() {
                            self.open_chat(chat);
                        }
                        self.preview_cache.clear();
                        // Pairing epilogue: the post-restart daemon has
                        // synced — we're fully back.
                        if self
                            .pairing
                            .as_ref()
                            .is_some_and(|p| p.stage == PairStage::Succeeded)
                        {
                            self.pairing = None;
                            self.flash("paired ✓ — messages syncing");
                        }
                    }
                    "phone_not_responding" => {
                        self.status.connected = false;
                        self.flash("phone not responding");
                    }
                    "listen_temporary_error" => {
                        self.status.connected = false;
                    }
                    "logged_out" => {
                        self.status.connected = false;
                        self.status.auth_expired = true;
                    }
                    _ => {}
                }
            }
            _ => {}
        }
    }

    fn apply_incoming_message(&mut self, msg: Message, is_old: bool) {
        // Announce live incoming messages: terminal bell + flash, unless
        // the user is already looking at that conversation.
        if !is_old && !msg.is_from_me {
            let viewing = self.focus == Focus::Messages
                && self.current_chat.as_deref() == Some(msg.conversation_id.as_str());
            if !viewing {
                let who = if msg.sender_name.is_empty() {
                    self.chats
                        .iter()
                        .find(|c| c.conversation_id == msg.conversation_id)
                        .map(|c| c.display_name())
                        .unwrap_or_else(|| "new message".to_string())
                } else {
                    msg.sender_name.clone()
                };
                self.flash(format!("✉ {who}"));
                self.bell = true;
            }
        }

        // Keep the chat list ordering fresh.
        if let Some(chat) = self
            .chats
            .iter_mut()
            .find(|c| c.conversation_id == msg.conversation_id)
        {
            if msg.timestamp_ms > chat.last_message_time_ms {
                chat.last_message_time_ms = msg.timestamp_ms;
            }
            self.sort_chats();
            self.refresh_omni_recents();
        } else if !is_old {
            // Unknown conversation: refresh the list.
            self.fetch_chats();
        }

        if self.current_chat.as_deref() != Some(msg.conversation_id.as_str()) {
            return;
        }
        let at_bottom = self
            .msg_state
            .selected()
            .map(|i| self.messages.len().saturating_sub(1) == i)
            .unwrap_or(true);
        match self
            .messages
            .iter_mut()
            .find(|m| m.message_id == msg.message_id)
        {
            Some(existing) => *existing = msg,
            None => {
                self.messages.push(msg);
                self.messages.sort_by_key(|m| m.timestamp_ms);
            }
        }
        if at_bottom && !self.messages.is_empty() {
            self.msg_state.select(Some(self.messages.len() - 1));
        }
    }

    /// Update the launch screen's recents when chats change under it.
    fn refresh_omni_recents(&mut self) {
        if self.focus == Focus::Omni && self.omni.input.value().trim().is_empty() {
            self.omni.chat_results = self.local_chat_matches("");
        }
    }

    fn sort_chats(&mut self) {
        let selected_id = self
            .chat_state
            .selected()
            .and_then(|i| self.chats.get(i))
            .map(|c| c.conversation_id.clone());
        self.chats.sort_by(|a, b| {
            b.pinned
                .cmp(&a.pinned)
                .then(b.last_message_time_ms.cmp(&a.last_message_time_ms))
        });
        if let Some(id) = selected_id {
            if let Some(pos) = self.chats.iter().position(|c| c.conversation_id == id) {
                self.chat_state.select(Some(pos));
            }
        }
    }

    fn select_message_anchor(&mut self) {
        if self.messages.is_empty() {
            self.msg_state.select(None);
            return;
        }
        let idx = self
            .anchor_id
            .as_ref()
            .and_then(|anchor| self.messages.iter().position(|m| &m.message_id == anchor))
            .unwrap_or(self.messages.len() - 1);
        self.msg_state.select(Some(idx));
    }

    pub fn handle_term_event(&mut self, ev: TermEvent) {
        if let TermEvent::Paste(text) = &ev {
            let text = text.clone();
            self.handle_paste(&text);
            return;
        }
        if let TermEvent::Key(key) = ev {
            if key.kind != KeyEventKind::Press {
                return;
            }
            // Ctrl-C always quits.
            if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
                self.should_quit = true;
                return;
            }
            if self.pairing.is_some() {
                self.handle_pairing_key(key, ev);
                return;
            }
            if self.prompt.is_some() {
                self.handle_prompt_key(key, ev);
                return;
            }
            if self.show_doctor {
                self.show_doctor = false;
                return;
            }
            if self.show_approvals {
                self.handle_approvals_key(key);
                return;
            }
            match self.focus {
                Focus::Omni => self.handle_omni_key(key, ev),
                Focus::Compose => self.handle_compose_key(key, ev),
                Focus::Chats => self.handle_chats_key(key),
                Focus::Messages => self.handle_messages_key(key),
            }
        }
    }

    fn handle_omni_key(&mut self, key: KeyEvent, raw: TermEvent) {
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        match key.code {
            KeyCode::Esc | KeyCode::Tab => self.focus = Focus::Chats,
            KeyCode::Enter => self.omni_activate(),
            KeyCode::Down => self.omni_move(1),
            KeyCode::Up => self.omni_move(-1),
            KeyCode::Char('n') if ctrl => self.omni_move(1),
            KeyCode::Char('p') if ctrl => self.omni_move(-1),
            KeyCode::Char('j') if ctrl => self.omni_move(1),
            KeyCode::Char('k') if ctrl => self.omni_move(-1),
            KeyCode::PageDown => self.omni_move(10),
            KeyCode::PageUp => self.omni_move(-10),
            _ => {
                let before = self.omni.input.value().to_string();
                self.omni.input.handle_event(&raw);
                if self.omni.input.value() != before {
                    self.refresh_omni_local();
                }
            }
        }
    }

    fn omni_move(&mut self, delta: i64) {
        let total = self.omni.total();
        if total == 0 {
            self.omni.selected = 0;
            return;
        }
        let cur = self.omni.selected as i64;
        self.omni.selected = (cur + delta).clamp(0, total as i64 - 1) as usize;
    }

    fn handle_compose_key(&mut self, key: KeyEvent, raw: TermEvent) {
        match key.code {
            KeyCode::Esc => {
                self.compose_input.reset();
                self.focus = self.compose_return;
            }
            KeyCode::Enter => {
                let value = self.compose_input.value().trim().to_string();
                self.compose_input.reset();
                self.focus = self.compose_return;
                if value.is_empty() {
                    return;
                }
                if let Some(chat) = self.current_chat.clone() {
                    self.send_message(chat, value);
                } else {
                    self.flash("no conversation selected");
                }
            }
            _ => {
                self.compose_input.handle_event(&raw);
            }
        }
    }

    fn handle_pairing_key(&mut self, key: KeyEvent, raw: TermEvent) {
        let stage_entering = self
            .pairing
            .as_ref()
            .is_some_and(|p| p.stage == PairStage::EnterCookies);
        let failed = self
            .pairing
            .as_ref()
            .is_some_and(|p| p.stage == PairStage::Failed);
        match key.code {
            KeyCode::Esc => {
                // Closing early is harmless: post-success the reconnect
                // continues regardless, and in-flight flows just expire.
                self.pairing = None;
            }
            KeyCode::Enter if stage_entering => self.submit_pairing_cookies(),
            // Legacy QR flow escape hatch (older Messages builds only).
            KeyCode::Char('r')
                if key.modifiers.contains(KeyModifiers::CONTROL) && stage_entering =>
            {
                self.start_qr_pairing()
            }
            KeyCode::Char('p') if failed => self.start_pairing(),
            KeyCode::Char('q') if !stage_entering => self.pairing = None,
            _ if stage_entering => {
                if let Some(p) = self.pairing.as_mut() {
                    p.input.handle_event(&raw);
                }
            }
            _ => {}
        }
    }

    fn handle_prompt_key(&mut self, key: KeyEvent, raw: TermEvent) {
        match key.code {
            KeyCode::Esc => self.prompt = None,
            KeyCode::Enter => self.submit_prompt(),
            _ => {
                if let Some(p) = self.prompt.as_mut() {
                    p.input.handle_event(&raw);
                }
            }
        }
    }

    /// Route bracketed-paste text into whichever input is active.
    fn handle_paste(&mut self, text: &str) {
        let target = if let Some(p) = self
            .pairing
            .as_mut()
            .filter(|p| p.stage == PairStage::EnterCookies)
        {
            Some(&mut p.input)
        } else if let Some(p) = self.prompt.as_mut() {
            Some(&mut p.input)
        } else if self.focus == Focus::Compose {
            Some(&mut self.compose_input)
        } else if self.focus == Focus::Omni {
            Some(&mut self.omni.input)
        } else {
            None
        };
        let Some(input) = target else { return };
        for c in text.chars() {
            if c != '\n' && c != '\r' {
                let _ = input.handle(tui_input::InputRequest::InsertChar(c));
            }
        }
        if self.focus == Focus::Omni && self.pairing.is_none() && self.prompt.is_none() {
            self.refresh_omni_local();
        }
    }

    fn handle_approvals_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Esc | KeyCode::Char('a') | KeyCode::Char('q') => {
                self.show_approvals = false;
            }
            KeyCode::Char('j') | KeyCode::Down => {
                list_next(&mut self.approval_state, self.approvals.len())
            }
            KeyCode::Char('k') | KeyCode::Up => {
                list_prev(&mut self.approval_state, self.approvals.len())
            }
            KeyCode::Char('r') => self.fetch_approvals(),
            KeyCode::Char('y') | KeyCode::Enter => {
                if let Some(a) = self
                    .approval_state
                    .selected()
                    .and_then(|i| self.approvals.get(i))
                {
                    self.resolve_approval(a.approval_id.clone(), true);
                }
            }
            KeyCode::Char('n') | KeyCode::Char('d') => {
                if let Some(a) = self
                    .approval_state
                    .selected()
                    .and_then(|i| self.approvals.get(i))
                {
                    self.resolve_approval(a.approval_id.clone(), false);
                }
            }
            _ => {}
        }
    }

    fn handle_chats_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Char('q') => self.should_quit = true,
            KeyCode::Char('j') | KeyCode::Down => list_next(&mut self.chat_state, self.chats.len()),
            KeyCode::Char('k') | KeyCode::Up => list_prev(&mut self.chat_state, self.chats.len()),
            KeyCode::Char('g') => {
                self.chat_state
                    .select(if self.chats.is_empty() { None } else { Some(0) })
            }
            KeyCode::Char('G') => self.chat_state.select(self.chats.len().checked_sub(1)),
            KeyCode::Enter | KeyCode::Char('l') => {
                if let Some(c) = self.chat_state.selected().and_then(|i| self.chats.get(i)) {
                    let id = c.conversation_id.clone();
                    self.open_chat(id);
                    self.messages_back = Focus::Chats;
                    self.focus = Focus::Messages;
                }
            }
            KeyCode::Tab => self.focus = Focus::Messages,
            KeyCode::Char('/') | KeyCode::Char('s') => self.enter_omni(),
            KeyCode::Char('i') => {
                // Compose to the selected chat, opening it first if needed.
                if let Some(c) = self.chat_state.selected().and_then(|i| self.chats.get(i)) {
                    let id = c.conversation_id.clone();
                    if self.current_chat.as_deref() != Some(id.as_str()) {
                        self.open_chat(id);
                    }
                    self.enter_compose(Focus::Chats);
                }
            }
            KeyCode::Char('a') => {
                self.show_approvals = true;
                self.fetch_approvals();
            }
            KeyCode::Char('p') => self.start_pairing(),
            KeyCode::Char('d') => self.show_doctor = true,
            KeyCode::Char('n') => {
                if let Some(c) = self.chat_state.selected().and_then(|i| self.chats.get(i)) {
                    let id = c.conversation_id.clone();
                    let name = c.display_name();
                    self.prompt = Some(Prompt {
                        kind: PromptKind::Alias {
                            conversation_id: id,
                        },
                        input: Input::default(),
                        title: format!("rename “{name}” (empty clears) — enter saves, esc cancels"),
                    });
                }
            }
            KeyCode::Char('r') => {
                self.fetch_chats();
                self.fetch_status();
                self.request_refresh();
                self.flash("refreshing…");
            }
            _ => {}
        }
    }

    fn handle_messages_key(&mut self, key: KeyEvent) {
        match key.code {
            KeyCode::Char('q') => self.should_quit = true,
            KeyCode::Esc | KeyCode::Char('h') => {
                self.focus = self.messages_back;
                if self.focus == Focus::Omni {
                    self.enter_omni();
                }
            }
            KeyCode::Tab => self.focus = Focus::Chats,
            KeyCode::Char('j') | KeyCode::Down => {
                list_next(&mut self.msg_state, self.messages.len())
            }
            KeyCode::Char('k') | KeyCode::Up => list_prev(&mut self.msg_state, self.messages.len()),
            KeyCode::Char('g') => self.msg_state.select(if self.messages.is_empty() {
                None
            } else {
                Some(0)
            }),
            KeyCode::Char('G') => self.msg_state.select(self.messages.len().checked_sub(1)),
            KeyCode::Char('i') => {
                if self.current_chat.is_some() {
                    self.enter_compose(Focus::Messages);
                }
            }
            KeyCode::Char('/') | KeyCode::Char('s') => self.enter_omni(),
            KeyCode::Char('a') => {
                self.show_approvals = true;
                self.fetch_approvals();
            }
            KeyCode::Char('p') => self.start_pairing(),
            KeyCode::Char('b') => self.backfill_current_chat(),
            KeyCode::Char('d') => self.show_doctor = true,
            KeyCode::Char('o') => self.open_media(),
            KeyCode::Char('e') => {
                if let Some(m) = self.msg_state.selected().and_then(|i| self.messages.get(i)) {
                    let id = m.message_id.clone();
                    self.prompt = Some(Prompt {
                        kind: PromptKind::React { message_id: id },
                        input: Input::default(),
                        title: "react — type/paste an emoji (👍 ❤️ 😂 😮 😢 👎), enter sends"
                            .to_string(),
                    });
                }
            }
            KeyCode::Char('r') => {
                if let Some(chat) = self.current_chat.clone() {
                    self.open_chat(chat);
                }
            }
            _ => {}
        }
    }

    fn enter_compose(&mut self, back: Focus) {
        self.compose_return = back;
        self.compose_input.reset();
        self.focus = Focus::Compose;
    }
}

/// Render a pairing URL as a unicode half-block QR, or None if encoding
/// fails (the raw URL is shown as fallback either way).
fn render_qr(url: &str) -> Option<String> {
    if url.is_empty() {
        return None;
    }
    let code = qrcode::QrCode::new(url.as_bytes()).ok()?;
    Some(
        code.render::<qrcode::render::unicode::Dense1x2>()
            .quiet_zone(true)
            .build(),
    )
}

fn list_next(state: &mut ListState, len: usize) {
    if len == 0 {
        state.select(None);
        return;
    }
    let next = match state.selected() {
        Some(i) if i + 1 < len => i + 1,
        Some(i) => i,
        None => 0,
    };
    state.select(Some(next));
}

fn list_prev(state: &mut ListState, len: usize) {
    if len == 0 {
        state.select(None);
        return;
    }
    let prev = match state.selected() {
        Some(i) if i > 0 => i - 1,
        Some(_) => 0,
        None => 0,
    };
    state.select(Some(prev));
}
