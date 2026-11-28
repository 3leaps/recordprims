// SPDX-License-Identifier: MIT OR Apache-2.0

//! Third evaluation stage (`shape`) of the durable-record contract: validates a
//! document that passed `wire::ingest` against the pinned Draft 2020-12 JSON
//! Schemas, matching `internal/shape` in the Go implementation.

use std::collections::{BTreeMap, BTreeSet, HashSet};
use std::sync::OnceLock;

use crate::wire::{self, Kind, Member, Node, Reject, REASON_LOCAL_LIMIT};

/// Contract refusal code for a schema-invalid document.
pub(crate) const REASON_SHAPE: &str = "shape";

const DRAFT_2020_12: &str = "https://json-schema.org/draft/2020-12/schema";
const MAX_STEPS: usize = 1 << 20;

const EMBEDDED_SCHEMAS: &[(&str, &str)] = &[
    (
        "v0/journal-entry.schema.json",
        include_str!("../schemas/v0/journal-entry.schema.json"),
    ),
    (
        "v0/record.schema.json",
        include_str!("../schemas/v0/record.schema.json"),
    ),
    (
        "v0/stratum-profile.schema.json",
        include_str!("../schemas/v0/stratum-profile.schema.json"),
    ),
    (
        "v0/write-receipt.schema.json",
        include_str!("../schemas/v0/write-receipt.schema.json"),
    ),
    (
        "v1/body-state-request.schema.json",
        include_str!("../schemas/v1/body-state-request.schema.json"),
    ),
    (
        "v1/export-request.schema.json",
        include_str!("../schemas/v1/export-request.schema.json"),
    ),
    (
        "v1/export-result.schema.json",
        include_str!("../schemas/v1/export-result.schema.json"),
    ),
    (
        "v1/journal-entry.schema.json",
        include_str!("../schemas/v1/journal-entry.schema.json"),
    ),
    (
        "v1/protection-receipt.schema.json",
        include_str!("../schemas/v1/protection-receipt.schema.json"),
    ),
    (
        "v1/protection-request.schema.json",
        include_str!("../schemas/v1/protection-request.schema.json"),
    ),
    (
        "v1/record.schema.json",
        include_str!("../schemas/v1/record.schema.json"),
    ),
    (
        "v1/removal-eligibility-request.schema.json",
        include_str!("../schemas/v1/removal-eligibility-request.schema.json"),
    ),
    (
        "v1/stratum-profile.schema.json",
        include_str!("../schemas/v1/stratum-profile.schema.json"),
    ),
    (
        "v1/stream-seal.schema.json",
        include_str!("../schemas/v1/stream-seal.schema.json"),
    ),
    (
        "v1/write-receipt.schema.json",
        include_str!("../schemas/v1/write-receipt.schema.json"),
    ),
];

const ADMITTED_PATTERNS: &[&str] = &[
    r"^(?:[0-9a-f]{2})*$",
    r"^([^Tt]*|[^Tt]*[Tt][^:]*(:[^:]*)?|[^Tt]*[Tt][^:]*:[^:]*:[0-5][0-9].*)$",
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\.[0-9]{6}Z$",
    r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$",
    r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$",
    r"^[A-Za-z0-9][A-Za-z0-9:._-]{0,255}$",
    r"^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$",
    r"^[a-z0-9][a-z0-9.-]{0,63}$",
    r"^[a-z0-9][a-z0-9._-]{0,127}$",
    r"^[a-z0-9][a-z0-9._-]{0,63}$",
    r"^[a-z0-9][a-z0-9._:-]{0,63}$",
    r"^[a-z0-9][a-z0-9_-]{0,63}$",
    r"^[a-z][a-z0-9_]{0,31}$",
    r"^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$",
    r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$",
    r"^[a-z0-9][a-z0-9._:-]{0,127}$",
    r"^sha256:[0-9a-f]{64}$",
];

fn is_admitted_pattern(p: &str) -> bool {
    ADMITTED_PATTERNS.contains(&p)
}

fn match_admitted_pattern(pat: &str, s: &str) -> bool {
    let b = s.as_bytes();
    match pat {
        r"^(?:[0-9a-f]{2})*$" => {
            b.len() % 2 == 0 && b.iter().all(|&c| matches!(c, b'0'..=b'9' | b'a'..=b'f'))
        }
        r"^([^Tt]*|[^Tt]*[Tt][^:]*(:[^:]*)?|[^Tt]*[Tt][^:]*:[^:]*:[0-5][0-9].*)$" => {
            let Some(t_idx) = b.iter().position(|&c| c == b'T' || c == b't') else {
                return true;
            };
            let after_t = &b[t_idx + 1..];
            let Some(c1) = after_t.iter().position(|&c| c == b':') else {
                return true;
            };
            let after_c1 = &after_t[c1 + 1..];
            let Some(c2) = after_c1.iter().position(|&c| c == b':') else {
                return true;
            };
            let after_c2 = &after_c1[c2 + 1..];
            if after_c2.len() < 2 {
                return false;
            }
            matches!(after_c2[0], b'0'..=b'5')
                && after_c2[1].is_ascii_digit()
                && !after_c2[2..].contains(&b'\n')
        }
        r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\.[0-9]{6}Z$" => {
            b.len() == 27
                && b[0..4].iter().all(u8::is_ascii_digit)
                && b[4] == b'-'
                && b[5..7].iter().all(u8::is_ascii_digit)
                && b[7] == b'-'
                && b[8..10].iter().all(u8::is_ascii_digit)
                && b[10] == b'T'
                && b[11..13].iter().all(u8::is_ascii_digit)
                && b[13] == b':'
                && b[14..16].iter().all(u8::is_ascii_digit)
                && b[16] == b':'
                && matches!(b[17], b'0'..=b'5')
                && b[18].is_ascii_digit()
                && b[19] == b'.'
                && b[20..26].iter().all(u8::is_ascii_digit)
                && b[26] == b'Z'
        }
        r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$" => {
            ident_class(b, 128, u8::is_ascii_alphanumeric, |c| {
                c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b':' | b'-')
            })
        }
        r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$" | r"^[A-Za-z0-9][A-Za-z0-9:._-]{0,255}$" => {
            ident_class(b, 256, u8::is_ascii_alphanumeric, |c| {
                c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b':' | b'-')
            })
        }
        r"^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$" => {
            let Some((left, right)) = s.split_once('/') else {
                return false;
            };
            let mime_part = |part: &[u8]| {
                ident_class(part, 127, is_lower_alnum, |c| {
                    is_lower_alnum(&c)
                        || matches!(
                            c,
                            b'!' | b'#' | b'$' | b'&' | b'^' | b'_' | b'.' | b'+' | b'-'
                        )
                })
            };
            mime_part(left.as_bytes()) && mime_part(right.as_bytes())
        }
        r"^[a-z0-9][a-z0-9.-]{0,63}$" => ident_class(b, 64, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'.' | b'-')
        }),
        r"^[a-z0-9][a-z0-9._-]{0,127}$" => ident_class(b, 128, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'.' | b'_' | b'-')
        }),
        r"^[a-z0-9][a-z0-9._-]{0,63}$" => ident_class(b, 64, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'.' | b'_' | b'-')
        }),
        r"^[a-z0-9][a-z0-9._:-]{0,63}$" => ident_class(b, 64, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'.' | b'_' | b':' | b'-')
        }),
        r"^[a-z0-9][a-z0-9_-]{0,63}$" => ident_class(b, 64, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'_' | b'-')
        }),
        r"^[a-z][a-z0-9_]{0,31}$" => ident_class(b, 32, u8::is_ascii_lowercase, |c| {
            is_lower_alnum(&c) || c == b'_'
        }),
        r"^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$" => {
            let Some(rest) = s.strip_prefix("contract: ") else {
                return false;
            };
            let Some((name, ver)) = rest.split_once("/v") else {
                return false;
            };
            let nb = name.as_bytes();
            let vb = ver.as_bytes();
            !nb.is_empty()
                && is_lower_alnum(&nb[0])
                && nb[1..].iter().all(|&c| is_lower_alnum(&c) || c == b'-')
                && !vb.is_empty()
                && vb.iter().all(u8::is_ascii_digit)
        }
        r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$" => {
            ident_class(b, 64, u8::is_ascii_alphanumeric, |c| {
                c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'+' | b'-')
            })
        }
        r"^[a-z0-9][a-z0-9._:-]{0,127}$" => ident_class(b, 128, is_lower_alnum, |c| {
            is_lower_alnum(&c) || matches!(c, b'.' | b'_' | b':' | b'-')
        }),
        r"^sha256:[0-9a-f]{64}$" => {
            let Some(hex) = s.strip_prefix("sha256:") else {
                return false;
            };
            hex.len() == 64 && hex.bytes().all(|c| matches!(c, b'0'..=b'9' | b'a'..=b'f'))
        }
        _ => false,
    }
}

fn is_lower_alnum(b: &u8) -> bool {
    b.is_ascii_lowercase() || b.is_ascii_digit()
}

fn ident_class(
    b: &[u8],
    max_len: usize,
    first: impl Fn(&u8) -> bool,
    rest: impl Fn(u8) -> bool,
) -> bool {
    !b.is_empty() && b.len() <= max_len && first(&b[0]) && b[1..].iter().copied().all(rest)
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct BigInt {
    neg: bool,
    // Little-endian base-10 digits (0..=9), trimmed of trailing zeros.
    digits: Vec<u8>,
}

impl BigInt {
    fn zero() -> Self {
        Self {
            neg: false,
            digits: Vec::new(),
        }
    }

    fn from_i64(v: i64) -> Self {
        if v == 0 {
            return Self::zero();
        }
        let neg = v < 0;
        let mut mag = v.unsigned_abs();
        let mut digits = Vec::new();
        while mag > 0 {
            digits.push((mag % 10) as u8);
            mag /= 10;
        }
        Self { neg, digits }
    }

    fn parse_str(s: &str) -> Option<Self> {
        if s.is_empty() {
            return None;
        }
        let (neg, rest) = if let Some(r) = s.strip_prefix('-') {
            (true, r)
        } else if let Some(r) = s.strip_prefix('+') {
            (false, r)
        } else {
            (false, s)
        };
        if rest.is_empty() || !rest.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        let trimmed = rest.trim_start_matches('0');
        if trimmed.is_empty() {
            return Some(Self::zero());
        }
        let digits = trimmed.bytes().rev().map(|b| b - b'0').collect();
        Some(Self { neg, digits })
    }

    fn add_i64(&self, delta: i64) -> Self {
        let other = Self::from_i64(delta);
        self.add(&other)
    }

    fn add(&self, other: &Self) -> Self {
        if self.digits.is_empty() {
            return other.clone();
        }
        if other.digits.is_empty() {
            return self.clone();
        }
        if self.neg == other.neg {
            Self {
                neg: self.neg,
                digits: Self::add_mag(&self.digits, &other.digits),
            }
        } else {
            match Self::cmp_mag(&self.digits, &other.digits) {
                std::cmp::Ordering::Equal => Self::zero(),
                std::cmp::Ordering::Greater => Self {
                    neg: self.neg,
                    digits: Self::sub_mag(&self.digits, &other.digits),
                },
                std::cmp::Ordering::Less => Self {
                    neg: other.neg,
                    digits: Self::sub_mag(&other.digits, &self.digits),
                },
            }
        }
    }

    fn add_mag(a: &[u8], b: &[u8]) -> Vec<u8> {
        let n = a.len().max(b.len());
        let mut out = Vec::with_capacity(n + 1);
        let mut carry = 0u8;
        for i in 0..n {
            let da = a.get(i).copied().unwrap_or(0);
            let db = b.get(i).copied().unwrap_or(0);
            let sum = da + db + carry;
            out.push(sum % 10);
            carry = sum / 10;
        }
        if carry > 0 {
            out.push(carry);
        }
        out
    }

    fn sub_mag(a: &[u8], b: &[u8]) -> Vec<u8> {
        let mut out = Vec::with_capacity(a.len());
        let mut borrow = 0i8;
        for (i, &da_u8) in a.iter().enumerate() {
            let da = da_u8 as i8;
            let db = b.get(i).copied().unwrap_or(0) as i8;
            let mut diff = da - db - borrow;
            if diff < 0 {
                diff += 10;
                borrow = 1;
            } else {
                borrow = 0;
            }
            out.push(diff as u8);
        }
        while out.last() == Some(&0) {
            out.pop();
        }
        out
    }

    fn cmp_mag(a: &[u8], b: &[u8]) -> std::cmp::Ordering {
        match a.len().cmp(&b.len()) {
            std::cmp::Ordering::Equal => {
                for i in (0..a.len()).rev() {
                    match a[i].cmp(&b[i]) {
                        std::cmp::Ordering::Equal => continue,
                        ord => return ord,
                    }
                }
                std::cmp::Ordering::Equal
            }
            ord => ord,
        }
    }

    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        if self.digits.is_empty() && other.digits.is_empty() {
            return std::cmp::Ordering::Equal;
        }
        if self.digits.is_empty() {
            return if other.neg {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Less
            };
        }
        if other.digits.is_empty() {
            return if self.neg {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Greater
            };
        }
        if self.neg != other.neg {
            return if self.neg {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Greater
            };
        }
        let mag = Self::cmp_mag(&self.digits, &other.digits);
        if self.neg {
            mag.reverse()
        } else {
            mag
        }
    }

    fn to_dec_string(&self) -> String {
        if self.digits.is_empty() {
            return "0".to_owned();
        }
        let mut out = String::with_capacity(self.digits.len() + usize::from(self.neg));
        if self.neg {
            out.push('-');
        }
        for &d in self.digits.iter().rev() {
            out.push((b'0' + d) as char);
        }
        out
    }
}

#[derive(Debug, Clone)]
struct Decimal {
    neg: bool,
    digits: String,
    point_exp: BigInt,
}

impl Decimal {
    fn parse(token: &str) -> Option<Self> {
        let (neg, rest) = match token.strip_prefix('-') {
            Some(r) => (true, r),
            None => (false, token),
        };
        if rest.is_empty() {
            return None;
        }
        let (mant, exp) = match rest.find(['e', 'E']) {
            Some(idx) => (&rest[..idx], &rest[idx + 1..]),
            None => (rest, ""),
        };
        let (int_part, frac) = match mant.find('.') {
            Some(idx) => (&mant[..idx], &mant[idx + 1..]),
            None => (mant, ""),
        };
        if int_part.is_empty()
            || (int_part.len() > 1 && int_part.starts_with('0'))
            || !int_part.bytes().all(|b| b.is_ascii_digit())
        {
            return None;
        }
        if mant.contains('.') && (frac.is_empty() || !frac.bytes().all(|b| b.is_ascii_digit())) {
            return None;
        }
        let e_base = if rest.contains(['e', 'E']) {
            BigInt::parse_str(exp)?
        } else {
            BigInt::zero()
        };
        let all = format!("{int_part}{frac}");
        let trimmed_left = all.trim_start_matches('0');
        let lead = all.len() - trimmed_left.len();
        let digits = trimmed_left.trim_end_matches('0').to_owned();
        if digits.is_empty() {
            return Some(Self {
                neg: false,
                digits: String::new(),
                point_exp: BigInt::zero(),
            });
        }
        let delta = (int_part.len() as i64) - (lead as i64);
        let point_exp = e_base.add_i64(delta);
        Some(Self {
            neg,
            digits,
            point_exp,
        })
    }

    fn is_zero(&self) -> bool {
        self.digits.is_empty()
    }

    fn is_int(&self) -> bool {
        if self.is_zero() {
            return true;
        }
        self.point_exp
            .cmp(&BigInt::from_i64(self.digits.len() as i64))
            .is_ge()
    }

    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        if self.is_zero() && other.is_zero() {
            return std::cmp::Ordering::Equal;
        }
        if self.is_zero() {
            return if other.neg {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Less
            };
        }
        if other.is_zero() {
            return if self.neg {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Greater
            };
        }
        if self.neg != other.neg {
            return if self.neg {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Greater
            };
        }
        let mut mag = self.point_exp.cmp(&other.point_exp);
        if mag == std::cmp::Ordering::Equal {
            let max_len = self.digits.len().max(other.digits.len());
            let mut a = self.digits.clone();
            let mut b = other.digits.clone();
            if a.len() < max_len {
                a.push_str(&"0".repeat(max_len - a.len()));
            }
            if b.len() < max_len {
                b.push_str(&"0".repeat(max_len - b.len()));
            }
            mag = a.cmp(&b);
        }
        if self.neg {
            mag.reverse()
        } else {
            mag
        }
    }
}

fn days_in_month(y: i32, m: i32) -> i32 {
    match m {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if y % 4 == 0 && (y % 100 != 0 || y % 400 == 0) {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}

fn is_date_time(s: &str) -> bool {
    let b = s.as_bytes();
    if b.len() < 20 {
        return false;
    }
    if !b[0..4].iter().all(u8::is_ascii_digit)
        || b[4] != b'-'
        || !b[5..7].iter().all(u8::is_ascii_digit)
        || b[7] != b'-'
        || !b[8..10].iter().all(u8::is_ascii_digit)
        || !matches!(b[10], b'T' | b't')
        || !b[11..13].iter().all(u8::is_ascii_digit)
        || b[13] != b':'
        || !b[14..16].iter().all(u8::is_ascii_digit)
        || b[16] != b':'
        || !b[17..19].iter().all(u8::is_ascii_digit)
    {
        return false;
    }
    let year: i32 = s[0..4].parse().unwrap_or(-1);
    let month: i32 = s[5..7].parse().unwrap_or(-1);
    let day: i32 = s[8..10].parse().unwrap_or(-1);
    let hour: i32 = s[11..13].parse().unwrap_or(-1);
    let minute: i32 = s[14..16].parse().unwrap_or(-1);
    let sec: i32 = s[17..19].parse().unwrap_or(-1);

    let mut i = 19;
    if b.get(i) == Some(&b'.') {
        i += 1;
        let start = i;
        while b.get(i).is_some_and(u8::is_ascii_digit) {
            i += 1;
        }
        if i == start {
            return false;
        }
    }
    let rest = &b[i..];
    let offset_min = if matches!(rest, [b'Z' | b'z']) {
        0
    } else if rest.len() == 6
        && matches!(rest[0], b'+' | b'-')
        && rest[1..3].iter().all(u8::is_ascii_digit)
        && rest[3] == b':'
        && rest[4..6].iter().all(u8::is_ascii_digit)
    {
        let off_h: i32 = s[i + 1..i + 3].parse().unwrap_or(99);
        let off_m: i32 = s[i + 4..i + 6].parse().unwrap_or(99);
        if off_h > 23 || off_m > 59 {
            return false;
        }
        let total = off_h * 60 + off_m;
        if rest[0] == b'-' {
            -total
        } else {
            total
        }
    } else {
        return false;
    };

    if !(1..=12).contains(&month) {
        return false;
    }
    if day < 1 || day > days_in_month(year, month) {
        return false;
    }
    if hour > 23 || minute > 59 || sec > 60 {
        return false;
    }
    if sec == 60 {
        let mut utc_y = year;
        let mut utc_m = month;
        let mut utc_d = day;
        let mut utc_min = hour * 60 + minute - offset_min;
        if utc_min < 0 {
            utc_min += 1440;
            if utc_d > 1 {
                utc_d -= 1;
            } else if utc_m > 1 {
                utc_m -= 1;
                utc_d = days_in_month(utc_y, utc_m);
            } else {
                utc_y -= 1;
                utc_m = 12;
                utc_d = 31;
            }
        } else if utc_min >= 1440 {
            utc_min -= 1440;
            if utc_d < days_in_month(utc_y, utc_m) {
                utc_d += 1;
            } else if utc_m < 12 {
                utc_m += 1;
                utc_d = 1;
            } else {
                utc_y += 1;
                utc_m = 1;
                utc_d = 1;
            }
        }
        if utc_min != 23 * 60 + 59 || utc_d != days_in_month(utc_y, utc_m) {
            return false;
        }
    }
    true
}

#[derive(Debug)]
struct SchemaNode {
    path: String,
    ref_target: String,
    typ: String,
    consts: Vec<Node>,
    has_enum: bool,
    is_const: bool,
    required: Vec<String>,
    properties: BTreeMap<String, SchemaNode>,
    additional_false: bool,
    pattern: Option<String>,
    date_time: bool,
    min_length: Option<usize>,
    max_length: Option<usize>,
    minimum: Option<Decimal>,
    maximum: Option<Decimal>,
    items: Option<Box<SchemaNode>>,
    contains: Option<Box<SchemaNode>>,
    min_items: Option<usize>,
    max_items: Option<usize>,
    unique_items: bool,
    all_of: Vec<SchemaNode>,
    any_of: Vec<SchemaNode>,
    one_of: Vec<SchemaNode>,
    not: Option<Box<SchemaNode>>,
    if_n: Option<Box<SchemaNode>>,
    then_n: Option<Box<SchemaNode>>,
    else_n: Option<Box<SchemaNode>>,
}

impl SchemaNode {
    fn new(path: String) -> Self {
        Self {
            path,
            ref_target: String::new(),
            typ: String::new(),
            consts: Vec::new(),
            has_enum: false,
            is_const: false,
            required: Vec::new(),
            properties: BTreeMap::new(),
            additional_false: false,
            pattern: None,
            date_time: false,
            min_length: None,
            max_length: None,
            minimum: None,
            maximum: None,
            items: None,
            contains: None,
            min_items: None,
            max_items: None,
            unique_items: false,
            all_of: Vec::new(),
            any_of: Vec::new(),
            one_of: Vec::new(),
            not: None,
            if_n: None,
            then_n: None,
            else_n: None,
        }
    }
}

#[derive(Debug)]
struct Doc {
    root: SchemaNode,
    defs: BTreeMap<String, SchemaNode>,
}

fn is_assertion_keyword(k: &str) -> bool {
    matches!(
        k,
        "$ref"
            | "type"
            | "const"
            | "enum"
            | "required"
            | "properties"
            | "additionalProperties"
            | "pattern"
            | "minLength"
            | "maxLength"
            | "minimum"
            | "maximum"
            | "items"
            | "contains"
            | "minItems"
            | "maxItems"
            | "uniqueItems"
            | "allOf"
            | "anyOf"
            | "oneOf"
            | "not"
            | "if"
            | "then"
            | "else"
    )
}

fn is_metadata_keyword(k: &str) -> bool {
    matches!(
        k,
        "$schema" | "$id" | "$defs" | "title" | "description" | "format"
    )
}

fn is_admitted_type(t: &str) -> bool {
    matches!(
        t,
        "object" | "array" | "string" | "integer" | "boolean" | "null"
    )
}

struct Compiler<'a> {
    name: &'a str,
    defs: BTreeMap<String, &'a Node>,
    err: Option<String>,
}

impl<'a> Compiler<'a> {
    fn fail(&mut self, path: &str, msg: impl Into<String>) {
        if self.err.is_none() {
            self.err = Some(format!(
                "shape: schema admission: {} {}: {}",
                self.name,
                path,
                msg.into()
            ));
        }
    }

    fn sub(&mut self, n: &Node, path: String) -> Option<SchemaNode> {
        if n.kind != Kind::Object {
            self.fail(
                &path,
                "schema position is not an object (boolean schemas are outside the subset)",
            );
            return None;
        }
        Some(self.compile(n, path, false))
    }

    fn list(&mut self, n: &Node, path: &str) -> Vec<SchemaNode> {
        if n.kind != Kind::Array || n.elems.is_empty() {
            self.fail(path, "must be a non-empty array of schemas");
            return Vec::new();
        }
        let mut out = Vec::with_capacity(n.elems.len());
        for (i, e) in n.elems.iter().enumerate() {
            if let Some(sub) = self.sub(e, format!("{path}/{i}")) {
                out.push(sub);
            }
        }
        out
    }

    fn compile(&mut self, s: &Node, path: String, is_root: bool) -> SchemaNode {
        let mut nd = SchemaNode::new(path.clone());
        for m in &s.fields {
            let k = m.name.as_str();
            let v = &m.value;
            if !is_assertion_keyword(k) {
                if is_metadata_keyword(k) {
                    match k {
                        "$schema" | "$id" | "$defs" => {
                            if !is_root {
                                self.fail(
                                    &path,
                                    format!("{k} is admitted only at the document root"),
                                );
                            }
                        }
                        "format" => {
                            if v.kind != Kind::String || v.str_val != "date-time" {
                                self.fail(
                                    &path,
                                    "format other than date-time is outside the subset",
                                );
                            }
                            nd.date_time = true;
                        }
                        _ => {
                            if v.kind != Kind::String {
                                self.fail(&path, format!("{k} must be a string"));
                            }
                        }
                    }
                } else {
                    self.fail(
                        &path,
                        format!("keyword {k:?} is outside the admitted subset"),
                    );
                }
                continue;
            }
            let kp = format!("{path}/{k}");
            match k {
                "$ref" => {
                    let Some(def_name) = (v.kind == Kind::String)
                        .then(|| v.str_val.strip_prefix("#/$defs/"))
                        .flatten()
                    else {
                        self.fail(&kp, "only local #/$defs references are admitted");
                        continue;
                    };
                    if !self.defs.contains_key(def_name) {
                        self.fail(&kp, format!("unresolved reference {:?}", v.str_val));
                    }
                    nd.ref_target = v.str_val.clone();
                }
                "type" => {
                    if v.kind != Kind::String || !is_admitted_type(&v.str_val) {
                        self.fail(
                            &kp,
                            "type must be one of object, array, string, integer, boolean, null",
                        );
                        continue;
                    }
                    nd.typ = v.str_val.clone();
                }
                "const" => {
                    nd.consts = vec![v.clone()];
                    nd.is_const = true;
                }
                "enum" => {
                    if v.kind != Kind::Array || v.elems.is_empty() {
                        self.fail(&kp, "enum must be a non-empty array");
                        continue;
                    }
                    nd.consts = v.elems.clone();
                    nd.has_enum = true;
                }
                "required" => {
                    if v.kind != Kind::Array {
                        self.fail(&kp, "required must be an array of strings");
                        continue;
                    }
                    for e in &v.elems {
                        if e.kind != Kind::String {
                            self.fail(&kp, "required must be an array of strings");
                            continue;
                        }
                        nd.required.push(e.str_val.clone());
                    }
                }
                "properties" => {
                    if v.kind != Kind::Object {
                        self.fail(&kp, "properties must be an object");
                        continue;
                    }
                    for pm in &v.fields {
                        if let Some(sub) = self.sub(&pm.value, format!("{kp}/{}", pm.name)) {
                            nd.properties.insert(pm.name.clone(), sub);
                        }
                    }
                }
                "additionalProperties" => {
                    if v.kind != Kind::Bool {
                        self.fail(&kp, "only boolean additionalProperties is admitted");
                        continue;
                    }
                    nd.additional_false = !v.bool_val;
                }
                "pattern" => {
                    if v.kind != Kind::String {
                        self.fail(&kp, "pattern must be a string");
                        continue;
                    }
                    if !is_admitted_pattern(&v.str_val) {
                        self.fail(
                            &kp,
                            format!(
                                "pattern {:?} is not in the evaluated set (see patterns.go)",
                                v.str_val
                            ),
                        );
                        continue;
                    }
                    nd.pattern = Some(v.str_val.clone());
                }
                "minLength" | "maxLength" | "minItems" | "maxItems" => {
                    let Some(x) = non_neg_int(v) else {
                        self.fail(&kp, format!("{k} must be a non-negative integer"));
                        continue;
                    };
                    match k {
                        "minLength" => nd.min_length = Some(x),
                        "maxLength" => nd.max_length = Some(x),
                        "minItems" => nd.min_items = Some(x),
                        "maxItems" => nd.max_items = Some(x),
                        _ => {}
                    }
                }
                "minimum" | "maximum" => {
                    if v.kind != Kind::Number {
                        self.fail(&kp, format!("{k} must be a number"));
                        continue;
                    }
                    let Some(dec) = Decimal::parse(&v.token) else {
                        self.fail(&kp, format!("{k} is not an exact number"));
                        continue;
                    };
                    if k == "minimum" {
                        nd.minimum = Some(dec);
                    } else {
                        nd.maximum = Some(dec);
                    }
                }
                "uniqueItems" => {
                    if v.kind != Kind::Bool {
                        self.fail(&kp, "uniqueItems must be a boolean");
                        continue;
                    }
                    nd.unique_items = v.bool_val;
                }
                "items" => {
                    nd.items = self.sub(v, kp).map(Box::new);
                }
                "contains" => {
                    nd.contains = self.sub(v, kp).map(Box::new);
                }
                "allOf" => {
                    nd.all_of = self.list(v, &kp);
                }
                "anyOf" => {
                    nd.any_of = self.list(v, &kp);
                }
                "oneOf" => {
                    nd.one_of = self.list(v, &kp);
                }
                "not" => {
                    nd.not = self.sub(v, kp).map(Box::new);
                }
                "if" => {
                    nd.if_n = self.sub(v, kp).map(Box::new);
                }
                "then" => {
                    nd.then_n = self.sub(v, kp).map(Box::new);
                }
                "else" => {
                    nd.else_n = self.sub(v, kp).map(Box::new);
                }
                _ => {}
            }
        }
        if !nd.ref_target.is_empty() {
            for m in &s.fields {
                if m.name == "$ref" || m.name == "title" || m.name == "description" {
                    continue;
                }
                if is_root && matches!(m.name.as_str(), "$schema" | "$id" | "$defs") {
                    continue;
                }
                self.fail(
                    &path,
                    format!(
                        "$ref with sibling {:?}: only title and description may accompany a $ref",
                        m.name
                    ),
                );
                break;
            }
        }
        nd
    }
}

fn non_neg_int(n: &Node) -> Option<usize> {
    if n.kind != Kind::Number {
        return None;
    }
    let v = wire::safe_int(&n.token).ok()?;
    usize::try_from(v).ok()
}

fn compile_doc(name: &str, raw: &[u8]) -> Result<Doc, String> {
    let root =
        wire::parse_json(raw).map_err(|e| format!("shape: schema admission: {name}: {e}"))?;
    if root.kind != Kind::Object {
        return Err(format!(
            "shape: schema admission: {name}: schema document is not an object"
        ));
    }
    let Some(s) = root.get("$schema") else {
        return Err(format!(
            "shape: schema admission: {name}: $schema must be {DRAFT_2020_12}"
        ));
    };
    if s.kind != Kind::String || s.str_val != DRAFT_2020_12 {
        return Err(format!(
            "shape: schema admission: {name}: $schema must be {DRAFT_2020_12}"
        ));
    }
    let mut c = Compiler {
        name,
        defs: BTreeMap::new(),
        err: None,
    };
    if let Some(d) = root.get("$defs") {
        if d.kind != Kind::Object {
            return Err(format!(
                "shape: schema admission: {name}: $defs is not an object"
            ));
        }
        for m in &d.fields {
            c.defs.insert(m.name.clone(), &m.value);
        }
    }
    let def_names: Vec<String> = c.defs.keys().cloned().collect();
    let mut compiled_defs = BTreeMap::new();
    for def_name in def_names {
        let def_node = c.defs[&def_name];
        let compiled = c.compile(def_node, format!("#/$defs/{def_name}"), false);
        compiled_defs.insert(def_name, compiled);
    }
    let compiled_root = c.compile(&root, "#".to_owned(), true);
    if let Some(err) = c.err {
        return Err(err);
    }
    let doc = Doc {
        root: compiled_root,
        defs: compiled_defs,
    };
    check_ref_cycles(name, &doc)?;
    Ok(doc)
}

fn check_ref_cycles(doc_name: &str, d: &Doc) -> Result<(), String> {
    fn visit(
        doc_name: &str,
        d: &Doc,
        n: &SchemaNode,
        stack: &mut BTreeSet<String>,
    ) -> Result<(), String> {
        if !n.ref_target.is_empty() {
            let target = n
                .ref_target
                .strip_prefix("#/$defs/")
                .unwrap_or(&n.ref_target);
            if !stack.insert(target.to_owned()) {
                return Err(format!(
                    "shape: schema admission: {doc_name} {}: reference cycle through {}",
                    n.path, n.ref_target
                ));
            }
            if let Some(def) = d.defs.get(target) {
                visit(doc_name, d, def, stack)?;
            }
            stack.remove(target);
        }
        for group in [&n.all_of, &n.any_of, &n.one_of] {
            for s in group {
                visit(doc_name, d, s, stack)?;
            }
        }
        for s in [&n.not, &n.if_n, &n.then_n, &n.else_n]
            .into_iter()
            .flatten()
        {
            visit(doc_name, d, s, stack)?;
        }
        Ok(())
    }

    let mut stack = BTreeSet::new();
    visit(doc_name, d, &d.root, &mut stack)?;
    for def in d.defs.values() {
        let mut def_stack = BTreeSet::new();
        visit(doc_name, d, def, &mut def_stack)?;
    }
    Ok(())
}

fn docs() -> &'static BTreeMap<&'static str, Doc> {
    static DOCS: OnceLock<BTreeMap<&'static str, Doc>> = OnceLock::new();
    DOCS.get_or_init(|| {
        let mut map = BTreeMap::new();
        for &(name, raw) in EMBEDDED_SCHEMAS {
            let d = compile_doc(name, raw.as_bytes()).unwrap_or_else(|e| panic!("{e}"));
            map.insert(name, d);
        }
        map
    })
}

struct Failure {
    path: String,
    keyword: &'static str,
}

struct EvalState<'a> {
    d: &'a Doc,
    steps: usize,
    limit: bool,
}

/// Validate `n` (which passed `wire::ingest`) against the named schema
/// (e.g. `"v1/journal-entry.schema.json"`).
pub(crate) fn validate(schema: &str, n: &Node) -> Result<(), Reject> {
    let Some(d) = docs().get(schema) else {
        return Err(Reject {
            reason: REASON_SHAPE,
            detail: format!("shape: unknown schema {schema:?}"),
        });
    };
    let mut st = EvalState {
        d,
        steps: 0,
        limit: false,
    };
    let f = st.check(&d.root, n);
    if st.limit {
        return Err(Reject {
            reason: REASON_LOCAL_LIMIT,
            detail: "shape: validation work exceeds the local bound".to_owned(),
        });
    }
    if let Some(fail) = f {
        return Err(Reject {
            reason: REASON_SHAPE,
            detail: format!("{schema} {} {}", fail.path, fail.keyword),
        });
    }
    Ok(())
}

fn fail(s: &SchemaNode, keyword: &'static str) -> Failure {
    Failure {
        path: s.path.clone(),
        keyword,
    }
}

impl EvalState<'_> {
    fn check(&mut self, s: &SchemaNode, x: &Node) -> Option<Failure> {
        if self.limit {
            return None;
        }
        self.steps += 1;
        if self.steps > MAX_STEPS {
            self.limit = true;
            return None;
        }
        if !s.ref_target.is_empty() {
            let def_name = s
                .ref_target
                .strip_prefix("#/$defs/")
                .unwrap_or(&s.ref_target);
            if let Some(def) = self.d.defs.get(def_name) {
                if let Some(f) = self.check(def, x) {
                    return Some(f);
                }
            }
        }
        if !s.typ.is_empty() && !has_type(x, &s.typ) {
            return Some(fail(s, "type"));
        }
        if s.is_const && !equal(x, &s.consts[0]) {
            return Some(fail(s, "const"));
        }
        if s.has_enum && !s.consts.iter().any(|c| equal(x, c)) {
            return Some(fail(s, "enum"));
        }
        match x.kind {
            Kind::String => {
                if s.min_length.is_some() || s.max_length.is_some() {
                    let n = x.str_val.chars().count();
                    if let Some(min_l) = s.min_length {
                        if n < min_l {
                            return Some(fail(s, "minLength"));
                        }
                    }
                    if let Some(max_l) = s.max_length {
                        if n > max_l {
                            return Some(fail(s, "maxLength"));
                        }
                    }
                }
                if let Some(pat) = &s.pattern {
                    if !match_admitted_pattern(pat, &x.str_val) {
                        return Some(fail(s, "pattern"));
                    }
                }
                if s.date_time && !is_date_time(&x.str_val) {
                    return Some(fail(s, "format"));
                }
            }
            Kind::Number => {
                if s.minimum.is_some() || s.maximum.is_some() {
                    let Some(r) = Decimal::parse(&x.token) else {
                        return Some(fail(s, "type"));
                    };
                    if let Some(min_v) = &s.minimum {
                        if r.cmp(min_v).is_lt() {
                            return Some(fail(s, "minimum"));
                        }
                    }
                    if let Some(max_v) = &s.maximum {
                        if r.cmp(max_v).is_gt() {
                            return Some(fail(s, "maximum"));
                        }
                    }
                }
            }
            Kind::Object => {
                for name in &s.required {
                    if x.get(name).is_none() {
                        return Some(fail(s, "required"));
                    }
                }
                for m in &x.fields {
                    if let Some(p) = s.properties.get(&m.name) {
                        if let Some(f) = self.check(p, &m.value) {
                            return Some(f);
                        }
                    } else if s.additional_false {
                        return Some(fail(s, "additionalProperties"));
                    }
                }
            }
            Kind::Array => {
                if let Some(min_i) = s.min_items {
                    if x.elems.len() < min_i {
                        return Some(fail(s, "minItems"));
                    }
                }
                if let Some(max_i) = s.max_items {
                    if x.elems.len() > max_i {
                        return Some(fail(s, "maxItems"));
                    }
                }
                if let Some(items_schema) = &s.items {
                    for e in &x.elems {
                        if let Some(f) = self.check(items_schema, e) {
                            return Some(f);
                        }
                    }
                }
                if let Some(contains_schema) = &s.contains {
                    let mut found = false;
                    for e in &x.elems {
                        if self.check(contains_schema, e).is_none() {
                            found = true;
                            break;
                        }
                    }
                    if self.limit {
                        return None;
                    }
                    if !found {
                        return Some(fail(s, "contains"));
                    }
                }
                if s.unique_items {
                    self.steps += x.elems.len();
                    if self.steps > MAX_STEPS {
                        self.limit = true;
                        return None;
                    }
                    let mut seen = HashSet::with_capacity(x.elems.len());
                    let mut key_buf = String::new();
                    for e in &x.elems {
                        key_buf.clear();
                        canonical_key(&mut key_buf, e);
                        if !seen.insert(key_buf.clone()) {
                            return Some(fail(s, "uniqueItems"));
                        }
                    }
                }
            }
            Kind::Null | Kind::Bool => {}
        }
        for sub in &s.all_of {
            if let Some(f) = self.check(sub, x) {
                return Some(f);
            }
        }
        if !s.any_of.is_empty() {
            let mut ok = false;
            for sub in &s.any_of {
                if self.check(sub, x).is_none() {
                    ok = true;
                    break;
                }
            }
            if self.limit {
                return None;
            }
            if !ok {
                return Some(fail(s, "anyOf"));
            }
        }
        if !s.one_of.is_empty() {
            let mut matches = 0usize;
            for sub in &s.one_of {
                if self.check(sub, x).is_none() {
                    matches += 1;
                }
            }
            if self.limit {
                return None;
            }
            if matches != 1 {
                return Some(fail(s, "oneOf"));
            }
        }
        if let Some(not_schema) = &s.not {
            if self.check(not_schema, x).is_none() {
                if self.limit {
                    return None;
                }
                return Some(fail(s, "not"));
            }
        }
        if let Some(if_schema) = &s.if_n {
            if self.check(if_schema, x).is_none() {
                if let Some(then_schema) = &s.then_n {
                    if let Some(f) = self.check(then_schema, x) {
                        return Some(f);
                    }
                }
            } else if let Some(else_schema) = &s.else_n {
                if let Some(f) = self.check(else_schema, x) {
                    return Some(f);
                }
            }
        }
        None
    }
}

fn has_type(x: &Node, typ: &str) -> bool {
    match typ {
        "object" => x.kind == Kind::Object,
        "array" => x.kind == Kind::Array,
        "string" => x.kind == Kind::String,
        "boolean" => x.kind == Kind::Bool,
        "null" => x.kind == Kind::Null,
        "integer" => {
            if x.kind != Kind::Number {
                return false;
            }
            Decimal::parse(&x.token).is_some_and(|d| d.is_int())
        }
        _ => false,
    }
}

fn canonical_key(b: &mut String, x: &Node) {
    use std::fmt::Write as _;
    match x.kind {
        Kind::Null => b.push('n'),
        Kind::Bool => b.push(if x.bool_val { 't' } else { 'f' }),
        Kind::String => {
            let _ = write!(b, "s{}:{}", x.str_val.len(), x.str_val);
        }
        Kind::Number => match Decimal::parse(&x.token) {
            None => {
                let _ = write!(b, "x{}:{}", x.token.len(), x.token);
            }
            Some(d) if d.is_zero() => b.push_str("d0"),
            Some(d) => {
                let sign = if d.neg { '-' } else { '+' };
                let _ = write!(b, "d{sign}{}:{}", d.digits, d.point_exp.to_dec_string());
            }
        },
        Kind::Array => {
            let _ = write!(b, "a{}[", x.elems.len());
            for e in &x.elems {
                canonical_key(b, e);
                b.push(',');
            }
            b.push(']');
        }
        Kind::Object => {
            let mut ms: Vec<&Member> = x.fields.iter().collect();
            ms.sort_by(|a, b| a.name.cmp(&b.name));
            let _ = write!(b, "o{}{{", ms.len());
            for m in ms {
                let _ = write!(b, "{}:{}=", m.name.len(), m.name);
                canonical_key(b, &m.value);
                b.push(',');
            }
            b.push('}');
        }
    }
}

fn equal(a: &Node, b: &Node) -> bool {
    if a.kind != b.kind {
        return false;
    }
    match a.kind {
        Kind::Null => true,
        Kind::Bool => a.bool_val == b.bool_val,
        Kind::String => a.str_val == b.str_val,
        Kind::Number => {
            let Some(ra) = Decimal::parse(&a.token) else {
                return false;
            };
            let Some(rb) = Decimal::parse(&b.token) else {
                return false;
            };
            ra.cmp(&rb).is_eq()
        }
        Kind::Array => {
            a.elems.len() == b.elems.len() && a.elems.iter().zip(&b.elems).all(|(x, y)| equal(x, y))
        }
        Kind::Object => {
            if a.fields.len() != b.fields.len() {
                return false;
            }
            for m in &a.fields {
                let Some(o) = b.get(&m.name) else {
                    return false;
                };
                if !equal(&m.value, o) {
                    return false;
                }
            }
            true
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_embedded_schemas_admitted() {
        assert_eq!(docs().len(), 15);
    }

    #[test]
    fn pattern_semantics_parity() {
        let v0_sec = r"^([^Tt]*|[^Tt]*[Tt][^:]*(:[^:]*)?|[^Tt]*[Tt][^:]*:[^:]*:[0-5][0-9].*)$";
        let sha_sample = format!("sha256:{}", "ab".repeat(32));
        let samples: &[(&str, &str)] = &[
            (r"^(?:[0-9a-f]{2})*$", "00ff"),
            (v0_sec, "2026-10-20T00:00:01Z"),
            (
                r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\.[0-9]{6}Z$",
                "2026-10-20T00:00:01.000000Z",
            ),
            (r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$", "Rec:1.a-b_c"),
            (r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$", "Key:1.a-b_c"),
            (r"^[A-Za-z0-9][A-Za-z0-9:._-]{0,255}$", "Key:1.a-b_c"),
            (
                r"^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$",
                "text/plain",
            ),
            (r"^[a-z0-9][a-z0-9.-]{0,63}$", "records.example"),
            (r"^[a-z0-9][a-z0-9._-]{0,127}$", "records.example_1"),
            (r"^[a-z0-9][a-z0-9._-]{0,63}$", "records.example_1"),
            (r"^[a-z0-9][a-z0-9._:-]{0,63}$", "store:bodies.1"),
            (r"^[a-z0-9][a-z0-9_-]{0,63}$", "standard_1"),
            (r"^[a-z][a-z0-9_]{0,31}$", "verified"),
            (
                r"^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$",
                "contract: example/v0",
            ),
            (r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$", "Ab+1.c-d_e"),
            (r"^[a-z0-9][a-z0-9._:-]{0,127}$", "trust:profile.1-a_b"),
            (r"^sha256:[0-9a-f]{64}$", &sha_sample),
        ];
        assert_eq!(samples.len(), ADMITTED_PATTERNS.len());
        for &(pat, sample) in samples {
            assert!(
                match_admitted_pattern(pat, sample),
                "pattern {pat} refused sample {sample}"
            );
            let bad: Vec<String> = if pat == v0_sec {
                vec![
                    "2026-10-20T23:59:60Z".to_owned(),
                    "2026-10-20t23:59:60.000000Z".to_owned(),
                    "2026-10-20T23:59:60Z\n".to_owned(),
                    "2026-10-20T23:59:99Z".to_owned(),
                    "2026-10-20T23:59:abZ".to_owned(),
                ]
            } else {
                vec![
                    format!("{sample}\n"),
                    format!("\n{sample}"),
                    format!(" {sample}"),
                    format!("{sample} "),
                    format!("{sample}\u{00a0}"),
                    format!("\u{ff10}{}", &sample[1..]),
                ]
            };
            for b in bad {
                assert!(
                    !match_admitted_pattern(pat, &b),
                    "pattern {pat} accepted bad {b:?}"
                );
            }
        }
    }
}
