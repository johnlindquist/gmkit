//! Rendering. Pure function of App state — no I/O here.

use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span, Text};
use ratatui::widgets::{Block, Clear, List, ListItem, Paragraph, Wrap};
use ratatui::Frame;

use crate::app::{App, Focus, InputMode, View};
use crate::model::{format_ts, now_ms, Conversation, Message};

const ACCENT: Color = Color::Cyan;
const DIM: Color = Color::DarkGray;
const ME: Color = Color::Green;
const OTHER: Color = Color::White;
const WARN: Color = Color::Yellow;

pub fn render(frame: &mut Frame, app: &mut App) {
    let [main, status] =
        Layout::vertical([Constraint::Min(1), Constraint::Length(1)]).areas(frame.area());
    let [sidebar, content] =
        Layout::horizontal([Constraint::Length(34), Constraint::Min(1)]).areas(main);

    render_chats(frame, app, sidebar);

    let show_input = app.focus == Focus::Input;
    if show_input {
        let [pane, input] =
            Layout::vertical([Constraint::Min(1), Constraint::Length(3)]).areas(content);
        render_content(frame, app, pane);
        render_input(frame, app, input);
    } else {
        render_content(frame, app, content);
    }

    render_status(frame, app, status);

    if app.show_approvals {
        render_approvals(frame, app);
    }
}

fn sender_name(conv: Option<&Conversation>, msg: &Message) -> String {
    if msg.is_from_me {
        return "me".to_string();
    }
    if let Some(conv) = conv {
        for p in conv.participants() {
            if p.id == msg.sender_id {
                if !p.name.is_empty() {
                    return p.name;
                }
                if !p.e164.is_empty() {
                    return p.e164;
                }
            }
        }
        if !conv.is_group {
            let name = conv.display_name();
            if !name.is_empty() {
                return name;
            }
        }
    }
    if msg.sender_id.is_empty() {
        "?".to_string()
    } else {
        msg.sender_id.clone()
    }
}

fn render_chats(frame: &mut Frame, app: &mut App, area: Rect) {
    let focused = app.focus == Focus::Chats && !app.show_approvals;
    let now = now_ms();
    let items: Vec<ListItem> = app
        .chats
        .iter()
        .map(|c| {
            let name = c.display_name();
            let mut badges = String::new();
            if c.pinned {
                badges.push('📌');
            }
            let marker = if c.unread { "●" } else { " " };
            let ts = format_ts(c.last_message_time_ms, now);
            let width = area.width.saturating_sub(4) as usize;
            let name_width = width.saturating_sub(ts.len() + badges.len() + 3);
            let line = Line::from(vec![
                Span::styled(
                    marker.to_string(),
                    Style::default().fg(if c.unread { ACCENT } else { DIM }),
                ),
                Span::raw(" "),
                Span::styled(
                    clip(&name, name_width),
                    if c.unread {
                        Style::default().add_modifier(Modifier::BOLD)
                    } else {
                        Style::default()
                    },
                ),
                Span::raw(badges),
                Span::styled(format!(" {ts}"), Style::default().fg(DIM)),
            ]);
            ListItem::new(line)
        })
        .collect();

    let block = pane_block("chats", focused);
    let list = List::new(items)
        .block(block)
        .highlight_style(Style::default().bg(Color::Rgb(40, 44, 52)).fg(ACCENT))
        .highlight_symbol("▍");
    frame.render_stateful_widget(list, area, &mut app.chat_state);
}

fn render_content(frame: &mut Frame, app: &mut App, area: Rect) {
    match app.view {
        View::SearchResults => render_search_results(frame, app, area),
        View::Messages => render_messages(frame, app, area),
    }
}

fn render_messages(frame: &mut Frame, app: &mut App, area: Rect) {
    let focused = app.focus == Focus::Messages && !app.show_approvals;
    let conv = app
        .current_chat
        .as_ref()
        .and_then(|id| app.chats.iter().find(|c| &c.conversation_id == id))
        .cloned();
    let title = conv
        .as_ref()
        .map(|c| c.display_name())
        .unwrap_or_else(|| "messages".to_string());

    if app.current_chat.is_none() {
        let help = Paragraph::new(Text::from(vec![
            Line::raw(""),
            Line::styled(
                "  gmtui — Google Messages in your terminal",
                Style::default().bold(),
            ),
            Line::raw(""),
            Line::raw("  enter  open conversation      /  search all messages"),
            Line::raw("  i      compose a message      a  review agent send requests"),
            Line::raw("  j/k    move                   r  refresh"),
            Line::raw("  tab    switch pane            q  quit"),
        ]))
        .block(pane_block(&title, focused));
        frame.render_widget(help, area);
        return;
    }

    let now = now_ms();
    let wrap_width = area.width.saturating_sub(6).max(20) as usize;
    let items: Vec<ListItem> = app
        .messages
        .iter()
        .map(|m| {
            let who = sender_name(conv.as_ref(), m);
            let color = if m.is_from_me { ME } else { OTHER };
            let anchor = app.anchor_id.as_deref() == Some(m.message_id.as_str());
            let mut header = vec![
                Span::styled(who, Style::default().fg(color).bold()),
                Span::styled(
                    format!("  {}", format_ts(m.timestamp_ms, now)),
                    Style::default().fg(DIM),
                ),
            ];
            if m.media_id.is_some() {
                let kind = m.mime_type.as_deref().unwrap_or("attachment");
                header.push(Span::styled(
                    format!("  [{kind}]"),
                    Style::default().fg(WARN),
                ));
            }
            if anchor {
                header.push(Span::styled("  ◀ match", Style::default().fg(WARN).bold()));
            }
            let mut lines = vec![Line::from(header)];
            let body = m.body.clone().unwrap_or_default();
            for raw_line in body.lines() {
                for chunk in wrap_text(raw_line, wrap_width) {
                    lines.push(Line::from(Span::raw(format!("  {chunk}"))));
                }
            }
            if body.is_empty() && m.media_id.is_some() {
                lines.push(Line::from(Span::styled(
                    "  (media message — `gmcli media download`)",
                    Style::default().fg(DIM),
                )));
            }
            lines.push(Line::raw(""));
            ListItem::new(Text::from(lines))
        })
        .collect();

    let list = List::new(items)
        .block(pane_block(&title, focused))
        .highlight_style(Style::default().bg(Color::Rgb(35, 38, 46)));
    frame.render_stateful_widget(list, area, &mut app.msg_state);
}

fn render_search_results(frame: &mut Frame, app: &mut App, area: Rect) {
    let focused = app.focus == Focus::Messages && !app.show_approvals;
    let now = now_ms();
    let items: Vec<ListItem> = app
        .search_hits
        .iter()
        .map(|h| {
            // The daemon enriches hits with names; fall back to the loaded
            // chat list for older daemons.
            let conv_name = if !h.conversation_name.is_empty() {
                h.conversation_name.clone()
            } else {
                app.chats
                    .iter()
                    .find(|c| c.conversation_id == h.conversation_id)
                    .map(|c| c.display_name())
                    .unwrap_or_else(|| h.conversation_id.clone())
            };
            let sender = if h.sender_name.is_empty() {
                String::new()
            } else {
                format!(" · {}", h.sender_name)
            };
            let snippet = if h.snippet.is_empty() {
                h.body.clone()
            } else {
                h.snippet.clone()
            };
            ListItem::new(Text::from(vec![
                Line::from(vec![
                    Span::styled(conv_name, Style::default().fg(ACCENT).bold()),
                    Span::styled(sender, Style::default().fg(DIM)),
                    Span::styled(
                        format!("  {}", format_ts(h.timestamp_ms, now)),
                        Style::default().fg(DIM),
                    ),
                ]),
                Line::from(Span::raw(format!("  {}", snippet.replace('\n', " ")))),
                Line::raw(""),
            ]))
        })
        .collect();
    let title = format!(
        "search results ({}) — enter opens context, esc back",
        app.search_hits.len()
    );
    let list = List::new(items)
        .block(pane_block(&title, focused))
        .highlight_style(Style::default().bg(Color::Rgb(35, 38, 46)));
    frame.render_stateful_widget(list, area, &mut app.hit_state);
}

fn render_input(frame: &mut Frame, app: &App, area: Rect) {
    let label = match app.input_mode {
        InputMode::Search => "search (FTS5) — enter runs, esc cancels",
        InputMode::Compose => "compose — enter sends, esc cancels",
    };
    let width = area.width.saturating_sub(3) as usize;
    let scroll = app.input.visual_scroll(width);
    let text = Paragraph::new(app.input.value())
        .scroll((0, scroll as u16))
        .block(pane_block(label, true));
    frame.render_widget(text, area);
    let x = (app.input.visual_cursor().saturating_sub(scroll)) as u16;
    frame.set_cursor_position((area.x + 1 + x, area.y + 1));
}

fn render_status(frame: &mut Frame, app: &App, area: Rect) {
    let conn = if app.status.connected {
        Span::styled("● connected", Style::default().fg(ME))
    } else {
        Span::styled("○ disconnected", Style::default().fg(WARN))
    };
    let mode = match app.status.send_mode.as_str() {
        "approve" => Span::styled(" · sends: approve", Style::default().fg(ACCENT)),
        "direct" => Span::styled(" · sends: direct", Style::default().fg(WARN)),
        _ => Span::styled(" · read-only", Style::default().fg(DIM)),
    };
    let pending = if app.status.pending_approvals > 0 {
        Span::styled(
            format!(
                " · {} pending approval(s) — press a",
                app.status.pending_approvals
            ),
            Style::default().fg(WARN).bold(),
        )
    } else {
        Span::raw("")
    };
    let flash = match &app.flash {
        Some((msg, _)) => Span::styled(format!("  {msg}"), Style::default().fg(WARN)),
        None => Span::raw(""),
    };
    let keys = Span::styled(
        "   q quit · / search · i compose · a approvals",
        Style::default().fg(DIM),
    );
    frame.render_widget(
        Paragraph::new(Line::from(vec![conn, mode, pending, flash, keys])),
        area,
    );
}

fn render_approvals(frame: &mut Frame, app: &mut App) {
    let area = centered(frame.area(), 80, 60);
    frame.render_widget(Clear, area);

    if app.approvals.is_empty() {
        let p = Paragraph::new("\n  No pending approvals.\n\n  esc close · r refresh")
            .block(pane_block("approvals", true))
            .wrap(Wrap { trim: false });
        frame.render_widget(p, area);
        return;
    }

    let now = now_ms();
    let items: Vec<ListItem> = app
        .approvals
        .iter()
        .map(|a| {
            let conv_name = app
                .chats
                .iter()
                .find(|c| c.conversation_id == a.conversation_id)
                .map(|c| c.display_name())
                .unwrap_or_else(|| a.conversation_id.clone());
            ListItem::new(Text::from(vec![
                Line::from(vec![
                    Span::styled(format!("→ {conv_name}"), Style::default().fg(ACCENT).bold()),
                    Span::styled(
                        format!(
                            "  from {}  {}",
                            a.requested_by,
                            format_ts(a.created_at_ms, now)
                        ),
                        Style::default().fg(DIM),
                    ),
                ]),
                Line::from(Span::raw(format!("  “{}”", a.body))),
                Line::raw(""),
            ]))
        })
        .collect();
    let title = format!(
        "approvals ({}) — y approve+send · n deny · esc close",
        app.approvals.len()
    );
    let list = List::new(items)
        .block(pane_block(&title, true))
        .highlight_style(Style::default().bg(Color::Rgb(60, 50, 20)));
    frame.render_stateful_widget(list, area, &mut app.approval_state);
}

fn pane_block(title: &str, focused: bool) -> Block<'static> {
    let style = if focused {
        Style::default().fg(ACCENT)
    } else {
        Style::default().fg(DIM)
    };
    Block::bordered()
        .border_style(style)
        .title(Line::styled(format!(" {title} "), style.bold()))
}

fn centered(area: Rect, pct_x: u16, pct_y: u16) -> Rect {
    let [_, mid_v, _] = Layout::vertical([
        Constraint::Percentage((100 - pct_y) / 2),
        Constraint::Percentage(pct_y),
        Constraint::Percentage((100 - pct_y) / 2),
    ])
    .areas(area);
    let [_, mid, _] = Layout::horizontal([
        Constraint::Percentage((100 - pct_x) / 2),
        Constraint::Percentage(pct_x),
        Constraint::Percentage((100 - pct_x) / 2),
    ])
    .areas(mid_v);
    mid
}

fn clip(s: &str, max: usize) -> String {
    if max == 0 {
        return String::new();
    }
    let mut out = String::new();
    for (i, ch) in s.chars().enumerate() {
        if i + 1 >= max {
            out.push('…');
            return out;
        }
        out.push(ch);
    }
    out
}

/// Simple character wrap; good enough for chat bodies.
fn wrap_text(s: &str, width: usize) -> Vec<String> {
    if width == 0 {
        return vec![s.to_string()];
    }
    let mut lines = Vec::new();
    let mut current = String::new();
    for word in s.split(' ') {
        let add = if current.is_empty() {
            word.chars().count()
        } else {
            word.chars().count() + 1
        };
        if current.chars().count() + add > width && !current.is_empty() {
            lines.push(std::mem::take(&mut current));
        }
        // Hard-break words longer than the width.
        if word.chars().count() > width {
            let mut chunk = String::new();
            for ch in word.chars() {
                if chunk.chars().count() == width {
                    lines.push(std::mem::take(&mut chunk));
                }
                chunk.push(ch);
            }
            current = chunk;
            continue;
        }
        if !current.is_empty() {
            current.push(' ');
        }
        current.push_str(word);
    }
    if !current.is_empty() || lines.is_empty() {
        lines.push(current);
    }
    lines
}
