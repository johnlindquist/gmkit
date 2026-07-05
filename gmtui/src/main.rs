mod app;
mod event;
mod model;
mod rpc;
mod ui;

use std::path::{Path, PathBuf};
use std::time::Duration;

use clap::Parser;
use color_eyre::eyre::{eyre, Result};
use tokio::sync::mpsc;

use crate::app::App;
use crate::event::Event;
use crate::model::ServerEvent;
use crate::rpc::RpcClient;

/// Terminal UI for gmcli — browse, search, and approve Google Messages.
///
/// If no daemon is running, one is started automatically (`gmcli serve
/// --auto --notify`: approval-gated sends, desktop notifications, exits
/// when idle). Run `gmcli serve` yourself for an always-on daemon.
#[derive(Parser, Debug, Clone)]
#[command(version, about)]
struct Args {
    /// gmcli store directory (default: $XDG_STATE_HOME/gmcli or
    /// ~/.local/state/gmcli). The daemon socket lives at <store>/gmcli.sock.
    #[arg(long)]
    store: Option<PathBuf>,

    /// Explicit socket path; overrides --store.
    #[arg(long)]
    socket: Option<PathBuf>,

    /// Path to the gmcli binary used for daemon auto-start.
    #[arg(long, default_value = "gmcli")]
    gmcli: String,

    /// Auto-start the daemon in offline mode (browse the archive without a
    /// phone connection).
    #[arg(long)]
    offline: bool,

    /// Fail instead of auto-starting a daemon when none is running.
    #[arg(long)]
    no_autostart: bool,
}

/// Everything a (re)connect attempt needs; cheap to clone into tasks.
#[derive(Clone)]
struct ConnectCfg {
    socket: PathBuf,
    store: Option<PathBuf>,
    gmcli: String,
    offline: bool,
    no_autostart: bool,
}

fn resolve_socket(args: &Args) -> Result<PathBuf> {
    if let Some(sock) = &args.socket {
        return Ok(sock.clone());
    }
    if let Some(store) = &args.store {
        return Ok(store.join("gmcli.sock"));
    }
    // Mirror gmcli's paths.Resolve: XDG_STATE_HOME, else ~/.local/state.
    if let Ok(xdg) = std::env::var("XDG_STATE_HOME") {
        if !xdg.is_empty() {
            return Ok(PathBuf::from(xdg).join("gmcli").join("gmcli.sock"));
        }
    }
    let home = std::env::var("HOME").map_err(|_| eyre!("cannot determine home directory"))?;
    Ok(PathBuf::from(home)
        .join(".local")
        .join("state")
        .join("gmcli")
        .join("gmcli.sock"))
}

/// Spawn `gmcli serve --auto --notify` detached and wait for the socket to
/// come up. `quiet` suppresses stderr chatter (reconnects happen while the
/// alternate screen is active).
async fn autostart_daemon(cfg: &ConnectCfg, quiet: bool) -> Result<()> {
    use std::os::unix::process::CommandExt;

    if !quiet {
        eprintln!(
            "gmtui: no daemon on {}; starting `{} serve --auto`...",
            cfg.socket.display(),
            cfg.gmcli
        );
    }
    let mut cmd = std::process::Command::new(&cfg.gmcli);
    if let Some(store) = &cfg.store {
        cmd.arg("--store").arg(store);
    }
    cmd.args(["--log-level", "warn", "serve", "--auto", "--notify"]);
    if cfg.offline {
        cmd.arg("--offline");
    }
    // Log where the Go-side autostart logs too: <store dir>/daemon.log
    // (the daemon's store dir is the socket's parent for default layouts).
    let log = cfg
        .socket
        .parent()
        .map(|dir| dir.join("daemon.log"))
        .and_then(|p| {
            std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(p)
                .ok()
        });
    match log {
        Some(f) => {
            cmd.stdout(f.try_clone().map_err(|e| eyre!("daemon log: {e}"))?);
            cmd.stderr(f);
        }
        None => {
            cmd.stdout(std::process::Stdio::null());
            cmd.stderr(std::process::Stdio::null());
        }
    }
    cmd.stdin(std::process::Stdio::null());
    // Own process group: Ctrl-C in gmtui must not kill the daemon, and it
    // must outlive us (it retires itself when idle).
    cmd.process_group(0);
    cmd.spawn().map_err(|e| {
        eyre!(
            "could not run `{}` (is gmcli installed and on PATH?): {e}",
            cfg.gmcli
        )
    })?;

    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    while std::time::Instant::now() < deadline {
        if tokio::net::UnixStream::connect(&cfg.socket).await.is_ok() {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(150)).await;
    }
    Err(eyre!(
        "daemon did not come up within 30s; check {}/daemon.log (unpaired? run `gmcli auth`)",
        cfg.socket
            .parent()
            .map(|p| Path::display(p).to_string())
            .unwrap_or_default()
    ))
}

/// Dial the daemon (starting one if allowed and needed) and subscribe.
async fn connect_daemon(
    cfg: &ConnectCfg,
    quiet: bool,
) -> Result<(RpcClient, mpsc::UnboundedReceiver<ServerEvent>)> {
    if tokio::net::UnixStream::connect(&cfg.socket).await.is_err() && !cfg.no_autostart {
        autostart_daemon(cfg, quiet).await?;
    }
    let (client, server_events) = RpcClient::connect(&cfg.socket)
        .await
        .map_err(|e| eyre!("{e}"))?;
    client.subscribe().await.map_err(|e| eyre!("{e}"))?;
    Ok((client, server_events))
}

/// Background loop that keeps trying to restore the daemon connection.
/// Attaches the new event stream and hands the client to the main loop.
fn spawn_reconnect(cfg: ConnectCfg, tx: mpsc::UnboundedSender<Event>) {
    tokio::spawn(async move {
        let mut delay = Duration::from_secs(1);
        loop {
            match connect_daemon(&cfg, true).await {
                Ok((client, server_events)) => {
                    event::attach_server_events(tx.clone(), server_events);
                    let _ = tx.send(Event::Reconnected(client));
                    return;
                }
                Err(_) => {
                    tokio::time::sleep(delay).await;
                    delay = (delay * 2).min(Duration::from_secs(15));
                }
            }
        }
    });
}

#[tokio::main]
async fn main() -> Result<()> {
    color_eyre::install()?;
    let args = Args::parse();
    let cfg = ConnectCfg {
        socket: resolve_socket(&args)?,
        store: args.store.clone(),
        gmcli: args.gmcli.clone(),
        offline: args.offline,
        no_autostart: args.no_autostart,
    };

    // Connect before touching the terminal so first-run connection errors
    // print normally instead of corrupting the screen.
    let (client, server_events) = connect_daemon(&cfg, false).await?;

    let (event_tx, app_tx, mut events) = event::start();
    event::attach_server_events(event_tx.clone(), server_events);
    let mut app = App::new(client, app_tx);
    app.fetch_chats();
    app.fetch_status();
    app.fetch_approvals();
    // Catch up on anything the archive missed while no daemon was running;
    // the daemon broadcasts "refreshed" and we refetch.
    app.request_refresh();

    let mut terminal = ratatui::init();
    let result = run(&mut terminal, &mut app, &mut events, &cfg, &event_tx).await;
    ratatui::restore();
    result
}

async fn run(
    terminal: &mut ratatui::DefaultTerminal,
    app: &mut App,
    events: &mut mpsc::UnboundedReceiver<Event>,
    cfg: &ConnectCfg,
    event_tx: &mpsc::UnboundedSender<Event>,
) -> Result<()> {
    while !app.should_quit {
        if app.take_bell() {
            // Terminal bell for incoming messages; terminals surface this
            // as a sound and/or a dock/tab attention marker.
            use std::io::Write;
            let mut out = std::io::stdout();
            let _ = out.write_all(b"\x07");
            let _ = out.flush();
        }
        terminal.draw(|frame| ui::render(frame, app))?;
        match events.recv().await {
            Some(Event::Tick) => app.on_tick(),
            Some(Event::Term(ev)) => app.handle_term_event(ev),
            Some(Event::App(ev)) => {
                app.handle_app_event(ev);
                // The daemon vanished (idle-exit, crash, upgrade): bring it
                // back without the user doing anything.
                if app.daemon_lost && !app.reconnecting {
                    app.reconnecting = true;
                    spawn_reconnect(cfg.clone(), event_tx.clone());
                }
            }
            Some(Event::Reconnected(client)) => app.adopt_rpc(client),
            None => break,
        }
    }
    Ok(())
}
