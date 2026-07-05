//! Event multiplexing: terminal input, a render tick, RPC events, and async
//! task results all land on one channel consumed by the main loop.

use std::time::Duration;

use crossterm::event::EventStream;
use futures::StreamExt;
use tokio::sync::mpsc;

use crate::app::AppEvent;
use crate::model::ServerEvent;
use crate::rpc::RpcClient;

pub enum Event {
    Tick,
    Term(crossterm::event::Event),
    App(AppEvent),
    /// The background reconnect loop established a fresh daemon connection.
    Reconnected(RpcClient),
}

/// Spawn the terminal-input/tick task and the app-event adapter. Daemon
/// event streams are attached separately (and re-attached on reconnect)
/// with [attach_server_events].
pub fn start() -> (
    mpsc::UnboundedSender<Event>,
    mpsc::UnboundedSender<AppEvent>,
    mpsc::UnboundedReceiver<Event>,
) {
    let (tx, rx) = mpsc::unbounded_channel::<Event>();

    // Terminal input + tick.
    let term_tx = tx.clone();
    tokio::spawn(async move {
        let mut stream = EventStream::new();
        let mut tick = tokio::time::interval(Duration::from_millis(250));
        loop {
            tokio::select! {
                _ = tick.tick() => {
                    if term_tx.send(Event::Tick).is_err() {
                        break;
                    }
                }
                maybe = stream.next() => {
                    match maybe {
                        Some(Ok(ev)) => {
                            if term_tx.send(Event::Term(ev)).is_err() {
                                break;
                            }
                        }
                        Some(Err(_)) => continue,
                        None => break,
                    }
                }
            }
        }
    });

    // App-task results ride the same channel through this adapter.
    let (app_tx, mut app_rx) = mpsc::unbounded_channel::<AppEvent>();
    let adapter_tx = tx.clone();
    tokio::spawn(async move {
        while let Some(ev) = app_rx.recv().await {
            if adapter_tx.send(Event::App(ev)).is_err() {
                break;
            }
        }
    });

    (tx, app_tx, rx)
}

/// Forward daemon-pushed events onto the main channel. When the stream ends
/// — the socket died or the daemon exited — emit DaemonLost so the app can
/// start reconnecting.
pub fn attach_server_events(
    tx: mpsc::UnboundedSender<Event>,
    mut server_events: mpsc::UnboundedReceiver<ServerEvent>,
) {
    tokio::spawn(async move {
        while let Some(ev) = server_events.recv().await {
            if tx.send(Event::App(AppEvent::Server(ev))).is_err() {
                return;
            }
        }
        let _ = tx.send(Event::App(AppEvent::DaemonLost));
    });
}
