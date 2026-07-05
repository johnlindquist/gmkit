mod app;
mod event;
mod model;
mod rpc;
mod ui;

use std::path::PathBuf;

use clap::Parser;
use color_eyre::eyre::{eyre, Result};

use crate::app::App;
use crate::event::Event;
use crate::rpc::RpcClient;

/// Terminal UI for gmcli — browse, search, and approve Google Messages.
///
/// If no daemon is running, one is started automatically (`gmcli serve
/// --auto`: approval-gated sends, exits when idle). Run `gmcli serve`
/// yourself for an always-on daemon.
#[derive(Parser, Debug)]
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

/// Spawn `gmcli serve --auto` detached and wait for the socket to come up.
async fn autostart_daemon(args: &Args, socket: &std::path::Path) -> Result<()> {
    use std::os::unix::process::CommandExt;

    eprintln!(
        "gmtui: no daemon on {}; starting `{} serve --auto`...",
        socket.display(),
        args.gmcli
    );
    let mut cmd = std::process::Command::new(&args.gmcli);
    if let Some(store) = &args.store {
        cmd.arg("--store").arg(store);
    }
    cmd.args(["--log-level", "warn", "serve", "--auto"]);
    if args.offline {
        cmd.arg("--offline");
    }
    // Log where the Go-side autostart logs too: <store dir>/daemon.log
    // (the daemon's store dir is the socket's parent for default layouts).
    let log = socket
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
            args.gmcli
        )
    })?;

    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while std::time::Instant::now() < deadline {
        if tokio::net::UnixStream::connect(socket).await.is_ok() {
            return Ok(());
        }
        tokio::time::sleep(std::time::Duration::from_millis(150)).await;
    }
    Err(eyre!(
        "daemon did not come up within 30s; check {}/daemon.log (unpaired? run `gmcli auth`)",
        socket
            .parent()
            .map(|p| p.display().to_string())
            .unwrap_or_default()
    ))
}

#[tokio::main]
async fn main() -> Result<()> {
    color_eyre::install()?;
    let args = Args::parse();
    let socket = resolve_socket(&args)?;

    // Connect before touching the terminal so connection errors print
    // normally instead of corrupting the screen. Auto-start the daemon if
    // nothing is listening.
    if tokio::net::UnixStream::connect(&socket).await.is_err() && !args.no_autostart {
        autostart_daemon(&args, &socket).await?;
    }
    let (client, server_events) = RpcClient::connect(&socket)
        .await
        .map_err(|e| eyre!("{e}"))?;
    client.subscribe().await.map_err(|e| eyre!("{e}"))?;

    let (app_tx, mut events) = event::start(server_events);
    let mut app = App::new(client, app_tx);
    app.fetch_chats();
    app.fetch_status();
    app.fetch_approvals();

    let mut terminal = ratatui::init();
    let result = run(&mut terminal, &mut app, &mut events).await;
    ratatui::restore();
    result
}

async fn run(
    terminal: &mut ratatui::DefaultTerminal,
    app: &mut App,
    events: &mut tokio::sync::mpsc::UnboundedReceiver<Event>,
) -> Result<()> {
    while !app.should_quit {
        terminal.draw(|frame| ui::render(frame, app))?;
        match events.recv().await {
            Some(Event::Tick) => app.on_tick(),
            Some(Event::Term(ev)) => app.handle_term_event(ev),
            Some(Event::App(ev)) => app.handle_app_event(ev),
            None => break,
        }
    }
    Ok(())
}
