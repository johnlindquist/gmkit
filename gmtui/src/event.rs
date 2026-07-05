//! Event multiplexing: terminal input, a render tick, RPC events, and async
//! task results all land on one channel consumed by the main loop.

use std::time::Duration;

use crossterm::event::EventStream;
use futures::StreamExt;
use tokio::sync::mpsc;

use crate::app::AppEvent;
use crate::model::ServerEvent;

#[derive(Debug)]
pub enum Event {
    Tick,
    Term(crossterm::event::Event),
    App(AppEvent),
}

/// Spawn the tasks that feed the main event channel.
pub fn start(
    mut server_events: mpsc::UnboundedReceiver<ServerEvent>,
) -> (
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

    // Daemon-pushed events.
    let server_tx = tx.clone();
    tokio::spawn(async move {
        while let Some(ev) = server_events.recv().await {
            if server_tx.send(Event::App(AppEvent::Server(ev))).is_err() {
                break;
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

    (app_tx, rx)
}
