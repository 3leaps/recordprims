// SPDX-License-Identifier: MIT OR Apache-2.0

//! Canonical durable-record/v1 UTC instants (`YYYY-MM-DDTHH:MM:SS.ffffffZ`).
//!
//! Only the exact 27-byte microsecond UTC spelling with proleptic Gregorian
//! year `0001..=9999` is accepted by [`parse_instant`]; any other spelling is
//! refused with [`Error::Schema`], never normalized.

use std::fmt;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::error::Error;

/// Microseconds from `0001-01-01T00:00:00.000000Z` to `1970-01-01T00:00:00.000000Z`
/// (719,162 days * 86,400,000,000 us/day).
const UNIX_EPOCH_MICROS_FROM_YEAR_1: i64 = 719_162 * 86_400_000_000;

/// Minimum valid v1 instant (`0001-01-01T00:00:00.000000Z`) in Unix microseconds.
const MIN_INSTANT_UNIX_MICROS: i64 = -UNIX_EPOCH_MICROS_FROM_YEAR_1;

/// Maximum valid v1 instant (`9999-12-31T23:59:59.999999Z`) in Unix microseconds
/// (3,652,058 days from 0001-01-01 * 86,400s + 86,399.999999s).
const MAX_INSTANT_UNIX_MICROS: i64 = (3_652_058 - 719_162) * 86_400_000_000 + 86_399_999_999;

/// A UTC timestamp at microsecond resolution in the proleptic Gregorian range
/// `0001-01-01T00:00:00.000000Z` through `9999-12-31T23:59:59.999999Z`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Instant {
    unix_micros: i64,
}

impl Instant {
    /// Current wall-clock instant, truncated toward zero/floor to whole microseconds.
    pub fn now() -> Self {
        Self::from_system_time(SystemTime::now())
    }

    /// Convert a `SystemTime` to an `Instant`, truncating sub-microsecond nanoseconds.
    pub fn from_system_time(t: SystemTime) -> Self {
        match t.duration_since(UNIX_EPOCH) {
            Ok(d) => {
                let micros = i64::try_from(d.as_micros()).unwrap_or(MAX_INSTANT_UNIX_MICROS);
                Self {
                    unix_micros: micros.clamp(MIN_INSTANT_UNIX_MICROS, MAX_INSTANT_UNIX_MICROS),
                }
            }
            Err(e) => {
                let d = e.duration();
                let secs = i64::try_from(d.as_secs()).unwrap_or(i64::MAX);
                let sub_micros = i64::from(d.subsec_micros());
                let sub_nanos = d.subsec_nanos() % 1_000;
                // Go's time.Truncate(Microsecond) truncates toward zero/floor on UTC nanoseconds;
                // for pre-1970 SystemTime, duration before UNIX_EPOCH has subsec_nanos:
                let total_micros = if sub_nanos == 0 {
                    secs.saturating_mul(1_000_000).saturating_add(sub_micros)
                } else {
                    secs.saturating_mul(1_000_000)
                        .saturating_add(sub_micros)
                        .saturating_add(1)
                };
                Self {
                    unix_micros: (-total_micros)
                        .clamp(MIN_INSTANT_UNIX_MICROS, MAX_INSTANT_UNIX_MICROS),
                }
            }
        }
    }

    /// Construct an `Instant` from microseconds since `1970-01-01T00:00:00.000000Z`.
    pub fn from_unix_micros(unix_micros: i64) -> Result<Self, Error> {
        if !(MIN_INSTANT_UNIX_MICROS..=MAX_INSTANT_UNIX_MICROS).contains(&unix_micros) {
            return Err(Error::schema("instant outside 0001..=9999 UTC range"));
        }
        Ok(Self { unix_micros })
    }

    /// Construct an `Instant` from proleptic Gregorian UTC components.
    pub fn from_utc(
        year: i32,
        month: u32,
        day: u32,
        hour: u32,
        minute: u32,
        second: u32,
        microsecond: u32,
    ) -> Result<Self, Error> {
        if !(1..=9999).contains(&year)
            || !(1..=12).contains(&month)
            || day < 1
            || day > days_in_month(year, month)
            || hour > 23
            || minute > 59
            || second > 59
            || microsecond > 999_999
        {
            return Err(Error::schema("invalid UTC calendar components"));
        }
        let days = days_from_civil(year, month, day);
        let day_micros = (i64::from(hour) * 3_600 + i64::from(minute) * 60 + i64::from(second))
            * 1_000_000
            + i64::from(microsecond);
        Ok(Self {
            unix_micros: days * 86_400_000_000 + day_micros,
        })
    }

    /// Microseconds since `1970-01-01T00:00:00.000000Z`.
    pub const fn as_unix_micros(self) -> i64 {
        self.unix_micros
    }

    /// Add a `Duration` (truncated to microseconds), saturating at the v1 ceiling.
    pub fn saturating_add(self, d: Duration) -> Self {
        let delta = i64::try_from(d.as_micros()).unwrap_or(i64::MAX);
        Self {
            unix_micros: self
                .unix_micros
                .saturating_add(delta)
                .clamp(MIN_INSTANT_UNIX_MICROS, MAX_INSTANT_UNIX_MICROS),
        }
    }
}

impl fmt::Display for Instant {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&format_instant(*self))
    }
}

/// Clock source for store and stream operations.
pub trait Clock: Send + Sync {
    /// Returns the current instant.
    fn now(&self) -> Instant;
}

/// Default wall-clock implementation using `SystemTime::now()`.
#[derive(Debug, Default, Clone, Copy)]
pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Instant {
        Instant::now()
    }
}

/// Format `t` as a canonical durable-record/v1 UTC instant (`YYYY-MM-DDTHH:MM:SS.ffffffZ`).
pub fn format_instant(t: Instant) -> String {
    let days = t.unix_micros.div_euclid(86_400_000_000);
    let rem = t.unix_micros.rem_euclid(86_400_000_000);
    let (year, month, day) = civil_from_days(days);
    let total_secs = rem / 1_000_000;
    let micros = rem % 1_000_000;
    let hour = total_secs / 3_600;
    let minute = (total_secs % 3_600) / 60;
    let second = total_secs % 60;
    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}.{micros:06}Z")
}

/// Parse a canonical durable-record/v1 UTC instant (`YYYY-MM-DDTHH:MM:SS.ffffffZ`).
///
/// Any other spelling (missing or extra fractional digits, numeric offset,
/// lowercase `t`/`z`, space separator, leap second `60`, or impossible date)
/// is refused with [`Error::Schema`].
pub fn parse_instant(s: &str) -> Result<Instant, Error> {
    let b = s.as_bytes();
    if b.len() != 27
        || b[4] != b'-'
        || b[7] != b'-'
        || b[10] != b'T'
        || b[13] != b':'
        || b[16] != b':'
        || b[19] != b'.'
        || b[26] != b'Z'
    {
        return Err(Error::schema(format!(
            "invalid instant {s:?} (want YYYY-MM-DDTHH:MM:SS.ffffffZ)"
        )));
    }
    let year = parse_digits(b, 0, 4).ok_or_else(|| Error::schema("invalid instant year"))? as i32;
    let month = parse_digits(b, 5, 2).ok_or_else(|| Error::schema("invalid instant month"))?;
    let day = parse_digits(b, 8, 2).ok_or_else(|| Error::schema("invalid instant day"))?;
    let hour = parse_digits(b, 11, 2).ok_or_else(|| Error::schema("invalid instant hour"))?;
    let minute = parse_digits(b, 14, 2).ok_or_else(|| Error::schema("invalid instant minute"))?;
    let second = parse_digits(b, 17, 2).ok_or_else(|| Error::schema("invalid instant second"))?;
    let micro = parse_digits(b, 20, 6).ok_or_else(|| Error::schema("invalid instant fraction"))?;
    Instant::from_utc(year, month, day, hour, minute, second, micro)
}

fn parse_digits(b: &[u8], start: usize, len: usize) -> Option<u32> {
    let mut val = 0u32;
    for &c in &b[start..start + len] {
        if !c.is_ascii_digit() {
            return None;
        }
        val = val * 10 + u32::from(c - b'0');
    }
    Some(val)
}

const fn is_leap_year(year: i32) -> bool {
    (year % 4 == 0 && year % 100 != 0) || (year % 400 == 0)
}

const fn days_in_month(year: i32, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if is_leap_year(year) {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}

/// Howard Hinnant's `days_from_civil`: days since 1970-01-01.
const fn days_from_civil(year: i32, month: u32, day: u32) -> i64 {
    let y = (year as i64) - if month <= 2 { 1 } else { 0 };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = (y - era * 400) as u64;
    let m = month as u64;
    let d = day as u64;
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + (doe as i64) - 719_468
}

/// Howard Hinnant's `civil_from_days`: `(year, month, day)` from days since 1970-01-01.
const fn civil_from_days(days: i64) -> (i32, u32, u32) {
    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let y = (yoe as i64) + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = y + if m <= 2 { 1 } else { 0 };
    (year as i32, m as u32, d as u32)
}
