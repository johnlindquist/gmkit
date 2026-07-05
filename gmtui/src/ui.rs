//! Rendering. Pure function of App state — no I/O here.

use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span, Text};
use ratatui::widgets::{Block, Clear, List, ListItem, Paragraph, Wrap};
use ratatui::Frame;

use crate::app::{App, Focus};
use crate::model::{format_ts, now_ms, Conversation, Message};

const ACCENT: Color = Color::Cyan;
const DIM: Color = Color::DarkGray;
const ME: Color = Color::Green;
const OTHER: Color = Color::White;
const WARN: Color = Color::Yellow;
const SELECT_BG: Color = Color::Rgb(40, 44, 52);

pub fn render(frame: &mut Frame, app: &mut App) {
    let [main, status] =
        Layout::vertical([Constraint::Min(1), Constraint::Length(1)]).areas(frame.area());

    if app.focus == Focus::Omni {
        // The launch screen: full-width search over people, chats, messages.
        render_omni(frame, app, main);
    } else {
        let [sidebar, content] =
            Layout::horizontal([Constraint::Length(34), Constraint::Min(1)]).areas(main);
        render_chats(frame, app, sidebar);
        if app.focus == Focus::Compose {
            let [pane, input] =
                Layout::vertical([Constraint::Min(1), Constraint::Length(3)]).areas(content);
            render_messages(frame, app, pane);
            render_compose(frame, app, input);
        } else {
            render_messages(frame, app, content);
        }
    }

    render_status(frame, app, status);

    if app.show_approvals {
        render_approvals(frame, app);
    }
}

// ---------------------------------------------------------------- omni

fn render_omni(frame: &mut Frame, app: &mut App, area: Rect) {
    let [input_area, results_area] =
        Layout::vertical([Constraint::Length(3), Constraint::Min(1)]).areas(area);

    // Query box.
    let title = if app.omni.searching {
        "search people & messages ⋯"
    } else {
        "search people & messages"
    };
    let width = input_area.width.saturating_sub(3) as usize;
    let scroll = app.omni.input.visual_scroll(width);
    let query = Paragraph::new(app.omni.input.value())
        .scroll((0, scroll as u16))
        .block(pane_block(title, true));
    frame.render_widget(query, input_area);
    let x = (app.omni.input.visual_cursor().saturating_sub(scroll)) as u16;
    frame.set_cursor_position((input_area.x + 1 + x, input_area.y + 1));

    // Unified results: chats first, then message hits, with section headers
    // interleaved as unselectable rows.
    let now = now_ms();
    let nchats = app.omni.chat_results.len();
    let query_empty = app.omni.input.value().trim().is_empty();
    let mut items: Vec<ListItem> = Vec::new();
    let mut selected_item_idx: Option<usize> = None;

    let header = |label: String| {
        ListItem::new(Line::styled(
            label,
            Style::default().fg(DIM).add_modifier(Modifier::BOLD),
        ))
    };

    if nchats > 0 {
        items.push(header(if query_empty {
            String::from(" recent chats — type to search")
        } else {
            format!(" people & chats ({nchats})")
        }));
        for (i, c) in app.omni.chat_results.iter().enumerate() {
            let selected = app.omni.selected == i;
            if selected {
                selected_item_idx = Some(items.len());
            }
            items.push(omni_chat_row(c, now, selected));
        }
    }
    if !app.omni.msg_results.is_empty() {
        items.push(header(format!(
            " messages ({})",
            app.omni.msg_results.len()
        )));
        for (i, h) in app.omni.msg_results.iter().enumerate() {
            let selected = app.omni.selected == nchats + i;
            if selected {
                selected_item_idx = Some(items.len());
            }
            let conv_name = if h.conversation_name.is_empty() {
                h.conversation_id.clone()
            } else {
                h.conversation_name.clone()
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
            let base = if selected {
                Style::default().bg(SELECT_BG)
            } else {
                Style::default()
            };
            items.push(ListItem::new(Text::from(vec![
                Line::from(vec![
                    Span::styled(
                        format!(" {conv_name}"),
                        base.fg(ACCENT).add_modifier(Modifier::BOLD),
                    ),
                    Span::styled(sender, base.fg(DIM)),
                    Span::styled(
                        format!("  {}", format_ts(h.timestamp_ms, now)),
                        base.fg(DIM),
                    ),
                ]),
                Line::styled(format!("   {}", snippet.replace('\n', " ")), base),
            ])));
        }
    }
    if items.is_empty() {
        let hint = if query_empty {
            "\n  Loading chats…\n\n  Type to search people and messages."
        } else {
            "\n  No matches.\n\n  Tip: message search needs terms of 3+ characters."
        };
        let p = Paragraph::new(hint).block(pane_block("results", false));
        frame.render_widget(p, results_area);
        return;
    }

    let footer = format!(
        "results ({}) — ↑↓ move · enter open · esc browse",
        app.omni.total()
    );
    // Drive scrolling through ListState so the selection stays visible.
    app.omni.list_state.select(selected_item_idx);
    let list = List::new(items).block(pane_block(&footer, false));
    frame.render_stateful_widget(list, results_area, &mut app.omni.list_state);
}

fn omni_chat_row(c: &Conversation, now: i64, selected: bool) -> ListItem<'static> {
    let base = if selected {
        Style::default().bg(SELECT_BG)
    } else {
        Style::default()
    };
    let marker = if selected { "▍" } else { " " };
    let kind = if c.is_group { "👥" } else { "  " };
    let unread = if c.unread { " ●" } else { "" };
    ListItem::new(Line::from(vec![
        Span::styled(marker.to_string(), base.fg(ACCENT)),
        Span::styled(format!("{kind} "), base),
        Span::styled(
            c.display_name(),
            if selected {
                base.fg(ACCENT).add_modifier(Modifier::BOLD)
            } else {
                base
            },
        ),
        Span::styled(unread.to_string(), base.fg(ACCENT)),
        Span::styled(
            format!("  {}", format_ts(c.last_message_time_ms, now)),
            base.fg(DIM),
        ),
    ]))
}

// ------------------------------------------------------------- browse UI

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
        .highlight_style(Style::default().bg(SELECT_BG).fg(ACCENT))
        .highlight_symbol("▍");
    frame.render_stateful_widget(list, area, &mut app.chat_state);
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
            Line::raw("  /      search people & messages (the launch screen)"),
            Line::raw("  enter  open conversation      i  compose a message"),
            Line::raw("  j/k    move                   a  review agent send requests"),
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

fn render_compose(frame: &mut Frame, app: &App, area: Rect) {
    let width = area.width.saturating_sub(3) as usize;
    let scroll = app.compose_input.visual_scroll(width);
    let text = Paragraph::new(app.compose_input.value())
        .scroll((0, scroll as u16))
        .block(pane_block("compose — enter sends, esc cancels", true));
    frame.render_widget(text, area);
    let x = (app.compose_input.visual_cursor().saturating_sub(scroll)) as u16;
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
    let keys = if app.focus == Focus::Omni {
        Span::styled(
            "   type to search · ↑↓ move · enter open · esc browse · ctrl-c quit",
            Style::default().fg(DIM),
        )
    } else {
        Span::styled(
            "   q quit · / search · i compose · a approvals",
            Style::default().fg(DIM),
        )
    };
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
