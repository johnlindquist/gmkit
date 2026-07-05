//! Application state and update logic. All RPC work happens in spawned
//! tasks that report back through the AppEvent channel — the update/draw
//! loop never blocks on the network.

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
    SearchHits(Vec<SearchHit>),
    Context {
        anchor: String,
        conversation_id: String,
        messages: Vec<Message>,
    },
    Approvals(Vec<Approval>),
    Status(DaemonStatus),
    Flash(String),
    Server(ServerEvent),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Focus {
    Chats,
    Messages,
    Input,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InputMode {
    Search,
    Compose,
}

/// What the right-hand pane is showing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum View {
    Messages,
    SearchResults,
}

pub struct App {
    rpc: RpcClient,
    tx: UnboundedSender<AppEvent>,
    pub should_quit: bool,

    pub focus: Focus,
    pub prev_focus: Focus,
    pub input_mode: InputMode,
    pub input: Input,

    pub chats: Vec<Conversation>,
    pub chat_state: ListState,

    pub view: View,
    pub current_chat: Option<String>,
    pub messages: Vec<Message>,
    pub msg_state: ListState,
    pub anchor_id: Option<String>,

    pub search_hits: Vec<SearchHit>,
    pub hit_state: ListState,

    pub show_approvals: bool,
    pub approvals: Vec<Approval>,
    pub approval_state: ListState,

    pub status: DaemonStatus,
    pub flash: Option<(String, i64)>,
}

impl App {
    pub fn new(rpc: RpcClient, tx: UnboundedSender<AppEvent>) -> Self {
        Self {
            rpc,
            tx,
            should_quit: false,
            focus: Focus::Chats,
            prev_focus: Focus::Chats,
            input_mode: InputMode::Search,
            input: Input::default(),
            chats: Vec::new(),
            chat_state: ListState::default(),
            view: View::Messages,
            current_chat: None,
            messages: Vec::new(),
            msg_state: ListState::default(),
            anchor_id: None,
            search_hits: Vec::new(),
            hit_state: ListState::default(),
            show_approvals: false,
            approvals: Vec::new(),
            approval_state: ListState::default(),
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
            match rpc.call_as("chats.list", json!({"limit": 200})).await {
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
        self.view = View::Messages;
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

    fn run_search(&mut self, query: String) {
        let rpc = self.rpc.clone();
        self.spawn(async move {
            match rpc
                .call_as("messages.search", json!({"query": query, "limit": 100}))
                .await
            {
                Ok(hits) => Some(AppEvent::SearchHits(hits)),
                Err(e) => Some(AppEvent::Flash(format!("search: {e}"))),
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

    // ---- event handling -------------------------------------------------

    pub fn on_tick(&mut self) {
        if let Some((_, at)) = self.flash {
            if now_ms() - at > 5000 {
                self.flash = None;
            }
        }
    }

    pub fn handle_app_event(&mut self, ev: AppEvent) {
        match ev {
            AppEvent::Chats(chats) => {
                self.chats = chats;
                self.sort_chats();
                if self.chat_state.selected().is_none() && !self.chats.is_empty() {
                    self.chat_state.select(Some(0));
                }
            }
            AppEvent::Messages {
                conversation_id,
                messages,
            } => {
                if self.current_chat.as_deref() == Some(conversation_id.as_str()) {
                    self.messages = messages;
                    self.view = View::Messages;
                    self.select_message_anchor();
                }
            }
            AppEvent::SearchHits(hits) => {
                self.search_hits = hits;
                self.view = View::SearchResults;
                self.focus = Focus::Messages;
                self.hit_state.select(if self.search_hits.is_empty() {
                    None
                } else {
                    Some(0)
                });
                if self.search_hits.is_empty() {
                    self.flash("no matches");
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
                self.view = View::Messages;
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
            "sync.status" => {
                let state = ev
                    .data
                    .get("state")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or("");
                match state {
                    "ready" | "listen_recovered" | "phone_responding" => {
                        self.status.connected = true;
                    }
                    "phone_not_responding" => {
                        self.status.connected = false;
                        self.flash("phone not responding");
                    }
                    "logged_out" => {
                        self.status.connected = false;
                        self.flash("logged out — re-run `gmcli auth`");
                    }
                    _ => {}
                }
            }
            _ => {}
        }
    }

    fn apply_incoming_message(&mut self, msg: Message, is_old: bool) {
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
        if let TermEvent::Key(key) = ev {
            if key.kind != KeyEventKind::Press {
                return;
            }
            // Ctrl-C always quits.
            if key.modifiers.contains(KeyModifiers::CONTROL) && key.code == KeyCode::Char('c') {
                self.should_quit = true;
                return;
            }
            if self.show_approvals {
                self.handle_approvals_key(key);
                return;
            }
            match self.focus {
                Focus::Input => self.handle_input_key(key, ev),
                Focus::Chats => self.handle_chats_key(key),
                Focus::Messages => self.handle_messages_key(key),
            }
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

    fn handle_input_key(&mut self, key: KeyEvent, raw: TermEvent) {
        match key.code {
            KeyCode::Esc => {
                self.input.reset();
                self.focus = self.prev_focus;
            }
            KeyCode::Enter => {
                let value = self.input.value().trim().to_string();
                self.input.reset();
                self.focus = self.prev_focus;
                if value.is_empty() {
                    return;
                }
                match self.input_mode {
                    InputMode::Search => self.run_search(value),
                    InputMode::Compose => {
                        if let Some(chat) = self.current_chat.clone() {
                            self.send_message(chat, value);
                        } else {
                            self.flash("no conversation selected");
                        }
                    }
                }
            }
            _ => {
                self.input.handle_event(&raw);
            }
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
                    self.focus = Focus::Messages;
                }
            }
            KeyCode::Tab => self.focus = Focus::Messages,
            KeyCode::Char('/') => self.enter_input(InputMode::Search),
            KeyCode::Char('i') => {
                // Compose to the selected chat, opening it first if needed.
                if let Some(c) = self.chat_state.selected().and_then(|i| self.chats.get(i)) {
                    let id = c.conversation_id.clone();
                    if self.current_chat.as_deref() != Some(id.as_str()) {
                        self.open_chat(id);
                    }
                    self.enter_input(InputMode::Compose);
                }
            }
            KeyCode::Char('a') => {
                self.show_approvals = true;
                self.fetch_approvals();
            }
            KeyCode::Char('r') => {
                self.fetch_chats();
                self.fetch_status();
                self.flash("refreshed");
            }
            _ => {}
        }
    }

    fn handle_messages_key(&mut self, key: KeyEvent) {
        if self.view == View::SearchResults {
            match key.code {
                KeyCode::Char('q') => self.should_quit = true,
                KeyCode::Esc | KeyCode::Char('h') => {
                    self.view = View::Messages;
                    self.focus = Focus::Chats;
                }
                KeyCode::Char('j') | KeyCode::Down => {
                    list_next(&mut self.hit_state, self.search_hits.len())
                }
                KeyCode::Char('k') | KeyCode::Up => {
                    list_prev(&mut self.hit_state, self.search_hits.len())
                }
                KeyCode::Enter => {
                    if let Some(hit) = self
                        .hit_state
                        .selected()
                        .and_then(|i| self.search_hits.get(i))
                    {
                        self.open_context(hit.clone());
                    }
                }
                KeyCode::Char('/') => self.enter_input(InputMode::Search),
                KeyCode::Tab => self.focus = Focus::Chats,
                _ => {}
            }
            return;
        }
        match key.code {
            KeyCode::Char('q') => self.should_quit = true,
            KeyCode::Esc | KeyCode::Char('h') | KeyCode::Tab => self.focus = Focus::Chats,
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
                    self.enter_input(InputMode::Compose);
                }
            }
            KeyCode::Char('/') => self.enter_input(InputMode::Search),
            KeyCode::Char('a') => {
                self.show_approvals = true;
                self.fetch_approvals();
            }
            KeyCode::Char('r') => {
                if let Some(chat) = self.current_chat.clone() {
                    self.open_chat(chat);
                }
            }
            _ => {}
        }
    }

    fn enter_input(&mut self, mode: InputMode) {
        self.prev_focus = self.focus;
        self.input_mode = mode;
        self.input.reset();
        self.focus = Focus::Input;
    }
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
