//! Minimal JSON-RPC 2.0 client over the gmcli daemon's unix socket.
//! Newline-delimited JSON both ways; requests are correlated by id, event
//! notifications (`method: "event"`) are forwarded on a channel.

use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use futures::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::net::UnixStream;
use tokio::sync::{mpsc, oneshot};
use tokio_util::codec::{FramedRead, FramedWrite, LinesCodec};

use crate::model::ServerEvent;

/// Bound a single inbound line; the daemon enforces 4 MiB on its side.
const MAX_LINE: usize = 8 << 20;

#[derive(Debug)]
pub enum RpcError {
    /// The daemon answered with a JSON-RPC error object.
    Server { code: i64, message: String },
    /// Transport-level failure (socket closed, serialization, ...).
    Transport(String),
}

impl std::fmt::Display for RpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RpcError::Server { code, message } => write!(f, "{message} (code {code})"),
            RpcError::Transport(msg) => write!(f, "{msg}"),
        }
    }
}

impl std::error::Error for RpcError {}

impl RpcError {
    /// gmcli application error codes worth branching on in the UI.
    pub fn code(&self) -> Option<i64> {
        match self {
            RpcError::Server { code, .. } => Some(*code),
            RpcError::Transport(_) => None,
        }
    }
}

pub const CODE_SENDS_DISABLED: i64 = 1001;
pub const CODE_ALREADY_RESOLVED: i64 = 1003;

type Pending = Arc<Mutex<HashMap<u64, oneshot::Sender<Result<Value, RpcError>>>>>;

/// Cloneable handle for issuing calls from spawned tasks.
#[derive(Clone)]
pub struct RpcClient {
    out_tx: mpsc::UnboundedSender<String>,
    pending: Pending,
    next_id: Arc<AtomicU64>,
}

impl RpcClient {
    /// Connect to the daemon socket. Returns the client plus the stream of
    /// server-pushed events (after `subscribe` is called).
    pub async fn connect(
        socket: &Path,
    ) -> Result<(RpcClient, mpsc::UnboundedReceiver<ServerEvent>), RpcError> {
        let stream = UnixStream::connect(socket).await.map_err(|e| {
            RpcError::Transport(format!(
                "connect to gmcli daemon at {} (is `gmcli serve` running?): {e}",
                socket.display()
            ))
        })?;
        let (read_half, write_half) = stream.into_split();

        let (out_tx, mut out_rx) = mpsc::unbounded_channel::<String>();
        let (event_tx, event_rx) = mpsc::unbounded_channel::<ServerEvent>();
        let pending: Pending = Arc::new(Mutex::new(HashMap::new()));

        // Writer task: serialize access to the socket's write half.
        let mut sink = FramedWrite::new(write_half, LinesCodec::new());
        tokio::spawn(async move {
            while let Some(line) = out_rx.recv().await {
                if sink.send(line).await.is_err() {
                    break;
                }
            }
        });

        // Reader task: route responses to pending calls, events to the app.
        let pending_reader = pending.clone();
        let mut lines = FramedRead::new(read_half, LinesCodec::new_with_max_length(MAX_LINE));
        tokio::spawn(async move {
            while let Some(item) = lines.next().await {
                let line = match item {
                    Ok(l) => l,
                    Err(_) => break,
                };
                let msg: Value = match serde_json::from_str(&line) {
                    Ok(v) => v,
                    Err(_) => continue,
                };
                if msg.get("method").and_then(Value::as_str) == Some("event") {
                    if let Some(params) = msg.get("params") {
                        if let Ok(ev) = serde_json::from_value::<ServerEvent>(params.clone()) {
                            let _ = event_tx.send(ev);
                        }
                    }
                    continue;
                }
                let Some(id) = msg.get("id").and_then(Value::as_u64) else {
                    continue;
                };
                let sender = pending_reader.lock().unwrap().remove(&id);
                if let Some(sender) = sender {
                    let result = if let Some(err) = msg.get("error") {
                        Err(RpcError::Server {
                            code: err.get("code").and_then(Value::as_i64).unwrap_or(0),
                            message: err
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("unknown server error")
                                .to_string(),
                        })
                    } else {
                        Ok(msg.get("result").cloned().unwrap_or(Value::Null))
                    };
                    let _ = sender.send(result);
                }
            }
            // Socket gone: fail everything still in flight.
            let mut map = pending_reader.lock().unwrap();
            for (_, sender) in map.drain() {
                let _ = sender.send(Err(RpcError::Transport(
                    "daemon connection closed".to_string(),
                )));
            }
        });

        Ok((
            RpcClient {
                out_tx,
                pending,
                next_id: Arc::new(AtomicU64::new(1)),
            },
            event_rx,
        ))
    }

    /// One request/response round trip.
    pub async fn call(&self, method: &str, params: Value) -> Result<Value, RpcError> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let req = json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params});
        let (tx, rx) = oneshot::channel();
        self.pending.lock().unwrap().insert(id, tx);
        let line = serde_json::to_string(&req)
            .map_err(|e| RpcError::Transport(format!("encode request: {e}")))?;
        if self.out_tx.send(line).is_err() {
            self.pending.lock().unwrap().remove(&id);
            return Err(RpcError::Transport("daemon connection closed".to_string()));
        }
        rx.await
            .map_err(|_| RpcError::Transport("daemon connection closed".to_string()))?
    }

    /// Typed convenience wrapper around call().
    pub async fn call_as<T: serde::de::DeserializeOwned>(
        &self,
        method: &str,
        params: Value,
    ) -> Result<T, RpcError> {
        let value = self.call(method, params).await?;
        serde_json::from_value(value)
            .map_err(|e| RpcError::Transport(format!("decode {method} result: {e}")))
    }

    pub async fn subscribe(&self) -> Result<(), RpcError> {
        self.call("subscribe", json!({})).await.map(|_| ())
    }
}
