//! Serde mirrors of the JSON shapes the gmcli daemon emits. Field names
//! match the Go store structs' json tags; everything optional is Option or
//! defaulted so protocol additions never break deserialization.
//!
//! Structs mirror the wire protocol completely, so some fields are not (yet)
//! read by the UI.
#![allow(dead_code)]

use serde::Deserialize;

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Conversation {
    pub conversation_id: String,
    #[serde(default)]
    pub name: String,
    /// Local user label (set with `n` / alias.set); overrides name.
    #[serde(default)]
    pub alias: String,
    #[serde(default)]
    pub is_group: bool,
    /// JSON-encoded array of participants, verbatim from the store.
    #[serde(default)]
    pub participants_json: String,
    #[serde(default)]
    pub last_message_time_ms: i64,
    #[serde(default)]
    pub unread: bool,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub archived: bool,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Participant {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub e164: String,
    #[serde(default)]
    pub is_me: bool,
}

impl Conversation {
    pub fn participants(&self) -> Vec<Participant> {
        serde_json::from_str(&self.participants_json).unwrap_or_default()
    }

    /// Best display name: local alias, explicit name, else the other
    /// participants' names, else the conversation ID.
    pub fn display_name(&self) -> String {
        if !self.alias.trim().is_empty() {
            return self.alias.clone();
        }
        if !self.name.trim().is_empty() {
            return self.name.clone();
        }
        let others: Vec<String> = self
            .participants()
            .into_iter()
            .filter(|p| !p.is_me)
            .map(|p| if !p.name.is_empty() { p.name } else { p.e164 })
            .filter(|s| !s.is_empty())
            .collect();
        if others.is_empty() {
            self.conversation_id.clone()
        } else {
            others.join(", ")
        }
    }
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Message {
    pub message_id: String,
    pub conversation_id: String,
    #[serde(default)]
    pub sender_id: String,
    /// Resolved by the daemon (alias > contact name > number); may be empty
    /// when talking to an older daemon.
    #[serde(default)]
    pub sender_name: String,
    #[serde(default)]
    pub body: Option<String>,
    #[serde(default)]
    pub timestamp_ms: i64,
    #[serde(default)]
    pub timestamp_iso: String,
    #[serde(default)]
    pub is_from_me: bool,
    #[serde(default)]
    pub media_id: Option<String>,
    #[serde(default)]
    pub mime_type: Option<String>,
    #[serde(default)]
    pub reply_to_id: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct SearchHit {
    pub message_id: String,
    pub conversation_id: String,
    #[serde(default)]
    pub conversation_name: String,
    #[serde(default)]
    pub sender_name: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub snippet: String,
    #[serde(default)]
    pub timestamp_ms: i64,
    #[serde(default)]
    pub timestamp_iso: String,
    #[serde(default)]
    pub is_from_me: bool,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Approval {
    pub approval_id: String,
    pub conversation_id: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub reply_to_id: Option<String>,
    #[serde(default)]
    pub requested_by: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub message_id: Option<String>,
    #[serde(default)]
    pub created_at_ms: i64,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct DaemonStatus {
    #[serde(default)]
    pub connected: bool,
    /// Daemon deliberately running without a phone connection (--offline).
    #[serde(default)]
    pub offline: bool,
    /// The phone reported this pairing logged out; `gmcli auth` required.
    #[serde(default)]
    pub auth_expired: bool,
    #[serde(default)]
    pub send_mode: String,
    #[serde(default)]
    pub pending_approvals: i64,
    #[serde(default)]
    pub conversations: i64,
    #[serde(default)]
    pub messages: i64,
    #[serde(default)]
    pub last_event_ms: i64,
}

/// One event notification from the daemon ("event" params).
#[derive(Debug, Clone, Deserialize)]
pub struct ServerEvent {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default)]
    pub data: serde_json::Value,
}

/// Render a millisecond timestamp as local wall-clock-ish text without
/// pulling in chrono: relative for recent, date for older.
pub fn format_ts(ts_ms: i64, now_ms: i64) -> String {
    if ts_ms <= 0 {
        return String::new();
    }
    let delta_s = (now_ms - ts_ms).max(0) / 1000;
    match delta_s {
        0..=59 => format!("{delta_s}s"),
        60..=3599 => format!("{}m", delta_s / 60),
        3600..=86_399 => format!("{}h", delta_s / 3600),
        _ => {
            let days = delta_s / 86_400;
            if days <= 30 {
                format!("{days}d")
            } else {
                // Rough civil date from the unix epoch; fine for a list badge.
                let days_since_epoch = ts_ms / 86_400_000;
                let (y, m, d) = civil_from_days(days_since_epoch);
                format!("{y:04}-{m:02}-{d:02}")
            }
        }
    }
}

/// Howard Hinnant's days-to-civil algorithm.
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// Shorten an RFC3339 timestamp to "YYYY-MM-DD HH:MM" for display.
pub fn iso_short(iso: &str) -> String {
    if iso.len() < 16 {
        return iso.to_string();
    }
    iso[..16].replace('T', " ")
}

pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}
