// SPDX-License-Identifier: MIT OR Apache-2.0

//! Strict reader and encoder for durable-record/v1 wire text (`ingest` and
//! `numeric_domain` evaluation stages), matching `internal/wire` in the Go tree.

use std::collections::HashSet;
use std::fmt;

/// Refusal reason for stage 1 syntax / opaque-number errors.
pub(crate) const REASON_INGEST: &str = "ingest";
/// Refusal reason for stage 2 protocol-integer errors.
pub(crate) const REASON_NUMERIC_DOMAIN: &str = "numeric_domain";
/// Refusal reason for implementation nesting/line-length limits.
pub(crate) const REASON_LOCAL_LIMIT: &str = "local_limit";

/// Largest protocol integer (`2^53 - 1`).
pub(crate) const MAX_SAFE_INTEGER: i64 = (1i64 << 53) - 1;

/// Maximum nesting depth before refusing with `REASON_LOCAL_LIMIT`.
const MAX_DEPTH: usize = 512;

/// Maximum single NDJSON line length (16 MiB).
pub(crate) const MAX_LINE_BYTES: usize = 16 << 20;

/// Document kinds accepted by [`ingest`].
pub(crate) const KIND_JOURNAL_ENTRY: &str = "journal_entry";
pub(crate) const KIND_STRATUM_PROFILE: &str = "stratum_profile";

/// Wire-stage refusal with a contract reason code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reject {
    pub reason: &'static str,
    pub detail: String,
}

impl Reject {
    pub(crate) fn ingest(detail: impl Into<String>) -> Self {
        Self {
            reason: REASON_INGEST,
            detail: detail.into(),
        }
    }

    pub(crate) fn numeric(detail: impl Into<String>) -> Self {
        Self {
            reason: REASON_NUMERIC_DOMAIN,
            detail: detail.into(),
        }
    }

    pub(crate) fn local_limit(detail: impl Into<String>) -> Self {
        Self {
            reason: REASON_LOCAL_LIMIT,
            detail: detail.into(),
        }
    }
}

impl fmt::Display for Reject {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.reason, self.detail)
    }
}

/// JSON value kind.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Null,
    Bool,
    Number,
    String,
    Array,
    Object,
}

/// One object member in document order.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Member {
    pub name: String,
    pub value: Node,
}

/// Parsed JSON value preserving raw number tokens and member order.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Node {
    pub kind: Kind,
    pub bool_val: bool,
    pub str_val: String,
    pub token: String,
    pub int_val: i64,
    pub is_int: bool,
    pub elems: Vec<Node>,
    pub fields: Vec<Member>,
}

impl Node {
    pub fn null() -> Self {
        Self {
            kind: Kind::Null,
            bool_val: false,
            str_val: String::new(),
            token: String::new(),
            int_val: 0,
            is_int: false,
            elems: Vec::new(),
            fields: Vec::new(),
        }
    }

    pub fn boolean(v: bool) -> Self {
        Self {
            kind: Kind::Bool,
            bool_val: v,
            ..Self::null()
        }
    }

    pub fn string(s: impl Into<String>) -> Self {
        Self {
            kind: Kind::String,
            str_val: s.into(),
            ..Self::null()
        }
    }

    pub fn int(v: i64) -> Self {
        Self {
            kind: Kind::Number,
            token: v.to_string(),
            int_val: v,
            is_int: true,
            ..Self::null()
        }
    }

    pub fn uint(v: u64) -> Self {
        let iv = i64::try_from(v).unwrap_or(i64::MAX);
        Self {
            kind: Kind::Number,
            token: v.to_string(),
            int_val: iv,
            is_int: v <= (MAX_SAFE_INTEGER as u64),
            ..Self::null()
        }
    }

    pub fn array(elems: Vec<Node>) -> Self {
        Self {
            kind: Kind::Array,
            elems,
            ..Self::null()
        }
    }

    pub fn object(fields: Vec<Member>) -> Self {
        Self {
            kind: Kind::Object,
            fields,
            ..Self::null()
        }
    }

    pub fn get(&self, name: &str) -> Option<&Node> {
        if self.kind != Kind::Object {
            return None;
        }
        self.fields
            .iter()
            .find(|m| m.name == name)
            .map(|m| &m.value)
    }
}

/// Strict RFC 8259 syntax pass over UTF-8 bytes: unique object member names at
/// every depth, no lone surrogates, no BOM, and no trailing bytes.
pub fn parse_json(raw: &[u8]) -> Result<Node, Reject> {
    if std::str::from_utf8(raw).is_err() {
        return Err(Reject::ingest("invalid UTF-8"));
    }
    let mut p = Parser { s: raw, i: 0 };
    p.ws();
    let n = p.value(0)?;
    p.ws();
    if p.i != p.s.len() {
        return Err(Reject::ingest(format!("trailing data at offset {}", p.i)));
    }
    Ok(n)
}

struct Parser<'a> {
    s: &'a [u8],
    i: usize,
}

impl Parser<'_> {
    fn ws(&mut self) {
        while self.i < self.s.len() {
            match self.s[self.i] {
                b' ' | b'\t' | b'\n' | b'\r' => self.i += 1,
                _ => break,
            }
        }
    }

    fn value(&mut self, depth: usize) -> Result<Node, Reject> {
        if depth > MAX_DEPTH {
            return Err(Reject::local_limit(format!(
                "nesting deeper than {MAX_DEPTH}"
            )));
        }
        if self.i >= self.s.len() {
            return Err(Reject::ingest("unexpected end of input"));
        }
        match self.s[self.i] {
            b'{' => self.object(depth),
            b'[' => self.array(depth),
            b'"' => Ok(Node::string(self.str_val()?)),
            b't' => self.literal(b"true", Node::boolean(true)),
            b'f' => self.literal(b"false", Node::boolean(false)),
            b'n' => self.literal(b"null", Node::null()),
            b'-' | b'0'..=b'9' => self.number(),
            _ => Err(Reject::ingest(format!(
                "unexpected character at offset {}",
                self.i
            ))),
        }
    }

    fn literal(&mut self, word: &[u8], node: Node) -> Result<Node, Reject> {
        if self.s.get(self.i..self.i + word.len()) != Some(word) {
            return Err(Reject::ingest(format!(
                "invalid literal at offset {}",
                self.i
            )));
        }
        self.i += word.len();
        Ok(node)
    }

    fn object(&mut self, depth: usize) -> Result<Node, Reject> {
        self.i += 1; // {
        let mut fields = Vec::new();
        let mut seen = HashSet::new();
        self.ws();
        if self.s.get(self.i) == Some(&b'}') {
            self.i += 1;
            return Ok(Node::object(fields));
        }
        loop {
            self.ws();
            if self.s.get(self.i) != Some(&b'"') {
                return Err(Reject::ingest(format!(
                    "expected member name at offset {}",
                    self.i
                )));
            }
            let name = self.str_val()?;
            if !seen.insert(name.clone()) {
                return Err(Reject::ingest("duplicate object member name"));
            }
            self.ws();
            if self.s.get(self.i) != Some(&b':') {
                return Err(Reject::ingest(format!("expected ':' at offset {}", self.i)));
            }
            self.i += 1;
            self.ws();
            let value = self.value(depth + 1)?;
            fields.push(Member { name, value });
            self.ws();
            match self.s.get(self.i) {
                Some(b',') => self.i += 1,
                Some(b'}') => {
                    self.i += 1;
                    return Ok(Node::object(fields));
                }
                Some(_) => {
                    return Err(Reject::ingest(format!(
                        "expected ',' or '}}' at offset {}",
                        self.i
                    )))
                }
                None => return Err(Reject::ingest("unexpected end of input in object")),
            }
        }
    }

    fn array(&mut self, depth: usize) -> Result<Node, Reject> {
        self.i += 1; // [
        let mut elems = Vec::new();
        self.ws();
        if self.s.get(self.i) == Some(&b']') {
            self.i += 1;
            return Ok(Node {
                kind: Kind::Array,
                elems,
                ..Node::null()
            });
        }
        loop {
            self.ws();
            let v = self.value(depth + 1)?;
            elems.push(v);
            self.ws();
            match self.s.get(self.i) {
                Some(b',') => self.i += 1,
                Some(b']') => {
                    self.i += 1;
                    return Ok(Node {
                        kind: Kind::Array,
                        elems,
                        ..Node::null()
                    });
                }
                Some(_) => {
                    return Err(Reject::ingest(format!(
                        "expected ',' or ']' at offset {}",
                        self.i
                    )))
                }
                None => return Err(Reject::ingest("unexpected end of input in array")),
            }
        }
    }

    fn str_val(&mut self) -> Result<String, Reject> {
        self.i += 1; // "
        let mut out = Vec::new();
        loop {
            let Some(&c) = self.s.get(self.i) else {
                return Err(Reject::ingest("unterminated string"));
            };
            if c == b'"' {
                self.i += 1;
                return String::from_utf8(out).map_err(|_| Reject::ingest("invalid UTF-8"));
            }
            if c < 0x20 {
                return Err(Reject::ingest(format!(
                    "control character in string at offset {}",
                    self.i
                )));
            }
            if c != b'\\' {
                out.push(c);
                self.i += 1;
                continue;
            }
            self.i += 1;
            let Some(&e) = self.s.get(self.i) else {
                return Err(Reject::ingest("unterminated escape"));
            };
            self.i += 1;
            match e {
                b'"' | b'\\' | b'/' => out.push(e),
                b'b' => out.push(0x08),
                b'f' => out.push(0x0c),
                b'n' => out.push(b'\n'),
                b'r' => out.push(b'\r'),
                b't' => out.push(b'\t'),
                b'u' => {
                    let u1 = self.hex4()?;
                    let ch = if (0xd800..=0xdfff).contains(&u1) {
                        if u1 >= 0xdc00 || self.s.get(self.i..self.i + 2) != Some(b"\\u") {
                            return Err(Reject::ingest("lone Unicode surrogate"));
                        }
                        self.i += 2;
                        let u2 = self.hex4()?;
                        if !(0xdc00..=0xdfff).contains(&u2) {
                            return Err(Reject::ingest("lone Unicode surrogate"));
                        }
                        let cp =
                            0x10000 + (((u32::from(u1) - 0xd800) << 10) | (u32::from(u2) - 0xdc00));
                        char::from_u32(cp)
                            .ok_or_else(|| Reject::ingest("lone Unicode surrogate"))?
                    } else {
                        char::from_u32(u32::from(u1))
                            .ok_or_else(|| Reject::ingest("invalid \\u escape"))?
                    };
                    let mut buf = [0u8; 4];
                    out.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                }
                _ => {
                    return Err(Reject::ingest(format!(
                        "invalid escape at offset {}",
                        self.i - 1
                    )))
                }
            }
        }
    }

    fn hex4(&mut self) -> Result<u16, Reject> {
        let Some(slice) = self.s.get(self.i..self.i + 4) else {
            return Err(Reject::ingest("truncated \\u escape"));
        };
        let mut val = 0u16;
        for &c in slice {
            let d = match c {
                b'0'..=b'9' => c - b'0',
                b'a'..=b'f' => c - b'a' + 10,
                b'A'..=b'F' => c - b'A' + 10,
                _ => return Err(Reject::ingest("invalid \\u escape")),
            };
            val = (val << 4) | u16::from(d);
        }
        self.i += 4;
        Ok(val)
    }

    fn number(&mut self) -> Result<Node, Reject> {
        let start = self.i;
        if self.s.get(self.i) == Some(&b'-') {
            self.i += 1;
        }
        match self.s.get(self.i) {
            Some(b'0') => self.i += 1,
            Some(b'1'..=b'9') => {
                while matches!(self.s.get(self.i), Some(b'0'..=b'9')) {
                    self.i += 1;
                }
            }
            _ => return Err(Reject::ingest(format!("invalid number at offset {start}"))),
        }
        if self.s.get(self.i) == Some(&b'.') {
            self.i += 1;
            let d = self.i;
            while matches!(self.s.get(self.i), Some(b'0'..=b'9')) {
                self.i += 1;
            }
            if self.i == d {
                return Err(Reject::ingest(format!("invalid number at offset {start}")));
            }
        }
        if matches!(self.s.get(self.i), Some(b'e' | b'E')) {
            self.i += 1;
            if matches!(self.s.get(self.i), Some(b'+' | b'-')) {
                self.i += 1;
            }
            let d = self.i;
            while matches!(self.s.get(self.i), Some(b'0'..=b'9')) {
                self.i += 1;
            }
            if self.i == d {
                return Err(Reject::ingest(format!("invalid number at offset {start}")));
            }
        }
        let token = std::str::from_utf8(&self.s[start..self.i])
            .map_err(|_| Reject::ingest("invalid UTF-8"))?
            .to_owned();
        Ok(Node {
            kind: Kind::Number,
            token,
            ..Node::null()
        })
    }
}

/// Encode `n` as compact JSON text with members in document order.
pub fn encode(n: &Node) -> Vec<u8> {
    let mut b = Vec::new();
    append_node(&mut b, n);
    b
}

fn append_node(b: &mut Vec<u8>, n: &Node) {
    match n.kind {
        Kind::Null => b.extend_from_slice(b"null"),
        Kind::Bool => b.extend_from_slice(if n.bool_val { b"true" } else { b"false" }),
        Kind::Number => b.extend_from_slice(n.token.as_bytes()),
        Kind::String => append_string(b, &n.str_val),
        Kind::Array => {
            b.push(b'[');
            for (i, e) in n.elems.iter().enumerate() {
                if i > 0 {
                    b.push(b',');
                }
                append_node(b, e);
            }
            b.push(b']');
        }
        Kind::Object => {
            b.push(b'{');
            for (i, m) in n.fields.iter().enumerate() {
                if i > 0 {
                    b.push(b',');
                }
                append_string(b, &m.name);
                b.push(b':');
                append_node(b, &m.value);
            }
            b.push(b'}');
        }
    }
}

const HEX: &[u8; 16] = b"0123456789abcdef";

fn append_string(b: &mut Vec<u8>, s: &str) {
    b.push(b'"');
    for &c in s.as_bytes() {
        match c {
            b'"' | b'\\' => {
                b.push(b'\\');
                b.push(c);
            }
            0x00..=0x1f => {
                b.extend_from_slice(b"\\u00");
                b.push(HEX[usize::from(c >> 4)]);
                b.push(HEX[usize::from(c & 0x0f)]);
            }
            _ => b.push(c),
        }
    }
    b.push(b'"');
}

/// Stage 2 protocol-integer check: exact integer value with magnitude `<= MAX_SAFE_INTEGER`.
pub(crate) fn safe_int(token: &str) -> Result<i64, Reject> {
    let (neg, int_part, frac, exp) = split_number(token)
        .ok_or_else(|| Reject::ingest(format!("invalid JSON numeric token {token:?}")))?;
    let combined = format!("{int_part}{frac}");
    let digits = combined.trim_start_matches('0');
    if digits.is_empty() {
        return Ok(0);
    }
    let trimmed = digits.trim_end_matches('0');
    let base_scale = (digits.len() as isize - trimmed.len() as isize) - (frac.len() as isize);
    let exp_val = if !exp.is_empty() {
        let exp_neg = exp.starts_with('-');
        let exp_digits = exp.trim_start_matches(['+', '-']).trim_start_matches('0');
        let exp_digits = if exp_digits.is_empty() {
            "0"
        } else {
            exp_digits
        };
        let bound = base_scale.unsigned_abs() + 16;
        let bound_str = bound.to_string();
        if exp_digits.len() > bound_str.len() {
            return Err(Reject::numeric(format!(
                "exponent outside safe integer domain ({token:?})"
            )));
        }
        let Ok(e) = exp_digits.parse::<usize>() else {
            return Err(Reject::numeric(format!(
                "exponent outside safe integer domain ({token:?})"
            )));
        };
        if e > bound {
            return Err(Reject::numeric(format!(
                "exponent outside safe integer domain ({token:?})"
            )));
        }
        if exp_neg {
            -(e as isize)
        } else {
            e as isize
        }
    } else {
        0
    };
    let scale = base_scale + exp_val;
    if scale < 0 {
        return Err(Reject::numeric(format!(
            "non-integral numeric token {token:?}"
        )));
    }
    let scale_usize = scale as usize;
    if trimmed.len() > 16 || trimmed.len() + scale_usize > 16 {
        return Err(Reject::numeric(format!(
            "{token:?} exceeds safe integer ceiling {MAX_SAFE_INTEGER}"
        )));
    }
    let expanded = format!("{trimmed}{}", "0".repeat(scale_usize));
    let Ok(mag) = expanded.parse::<i64>() else {
        return Err(Reject::numeric(format!(
            "{token:?} exceeds safe integer ceiling {MAX_SAFE_INTEGER}"
        )));
    };
    if mag > MAX_SAFE_INTEGER {
        return Err(Reject::numeric(format!(
            "{token:?} exceeds safe integer ceiling {MAX_SAFE_INTEGER}"
        )));
    }
    Ok(if neg { -mag } else { mag })
}

fn split_number(token: &str) -> Option<(bool, &str, &str, &str)> {
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
    if rest.contains(['e', 'E']) {
        let exp_digits = exp.strip_prefix(['+', '-']).unwrap_or(exp);
        if exp_digits.is_empty() || !exp_digits.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
    }
    Some((neg, int_part, frac, exp))
}

fn check_opaque(token: &str) -> Result<(), Reject> {
    let Some((_, int_part, frac, exp)) = split_number(token) else {
        return Err(Reject::ingest(format!(
            "invalid JSON numeric token {token:?}"
        )));
    };
    let is_float = !frac.is_empty() || !exp.is_empty();
    if int_part.trim_start_matches('0').is_empty() && frac.trim_start_matches('0').is_empty() {
        return Ok(());
    }
    if !is_float {
        if int_part.len() > 309 {
            return Err(Reject::ingest("integer overflows IEEE 754 binary64"));
        }
        let Ok(f) = token.parse::<f64>() else {
            return Err(Reject::ingest("invalid JSON numeric token"));
        };
        if !f.is_finite() || int_part != exact_integer_decimal(f.abs()) {
            return Err(Reject::ingest(
                "integer is outside the I-JSON IEEE 754 binary64 domain",
            ));
        }
        return Ok(());
    }
    if !exp.is_empty() {
        let neg = exp.starts_with('-');
        let exp_digits = exp.trim_start_matches(['+', '-']).trim_start_matches('0');
        let exp_digits = if exp_digits.is_empty() {
            "0"
        } else {
            exp_digits
        };
        let limit = token.len() + 400;
        let limit_str = limit.to_string();
        let over = exp_digits.len() > limit_str.len()
            || exp_digits
                .parse::<usize>()
                .map(|e| e > limit)
                .unwrap_or(true);
        if over {
            if neg {
                return Ok(());
            }
            return Err(Reject::ingest(format!(
                "non-finite IEEE 754 number {token:?}"
            )));
        }
    }
    let f = token.parse::<f64>().unwrap_or(f64::NAN);
    if !f.is_finite() {
        return Err(Reject::ingest(format!(
            "non-finite IEEE 754 number {token:?}"
        )));
    }
    Ok(())
}

fn exact_integer_decimal(value: f64) -> String {
    const BASE: u64 = 1_000_000_000;
    let bits = value.to_bits();
    let exponent = ((bits >> 52) & 0x7ff) as i32;
    let fraction = bits & ((1 << 52) - 1);
    let (mantissa, shift) = if exponent == 0 {
        (fraction, -1074)
    } else {
        (fraction | (1 << 52), exponent - 1075)
    };
    if shift < 0 {
        return (mantissa >> shift.unsigned_abs().min(63)).to_string();
    }
    let mut limbs = vec![
        mantissa % BASE,
        mantissa / BASE % BASE,
        mantissa / BASE / BASE,
    ];
    for _ in 0..shift {
        let mut carry = 0;
        for limb in &mut limbs {
            let v = *limb * 2 + carry;
            *limb = v % BASE;
            carry = v / BASE;
        }
        if carry > 0 {
            limbs.push(carry);
        }
    }
    while limbs.len() > 1 && limbs.last() == Some(&0) {
        limbs.pop();
    }
    let mut out = limbs.last().copied().unwrap_or(0).to_string();
    for limb in limbs.iter().rev().skip(1) {
        out.push_str(&format!("{limb:09}"));
    }
    out
}

fn member_spec(kind: &str, name: &str) -> &'static str {
    match (kind, name) {
        ("pin" | "revision_pin" | "link", "revision") => "int",
        ("body_ref" | "lifecycle_body", "length") => "int",
        ("source_cut" | "stream_recovery" | "ancestor_seal", "cut_seq") => "int",
        ("source_pin", "pin") => "pin",
        ("reconciled_body", "body") => "lifecycle_body",
        ("reconciled_body", "floor") => "int",
        ("stream_recovery", "reconciled_bodies") => "list:reconciled_body",
        ("stream_seal", "v" | "cut_seq") => "int",
        ("stream_seal", "ancestor_seals") => "list:ancestor_seal",
        ("seal_context", "seal") => "stream_seal",
        ("seal_context", "ancestor_seal_contexts") => "list:seal_context",
        ("record", "v" | "revision") => "int",
        ("record", "supersedes") => "revision_pin",
        ("record", "body") => "body_ref",
        ("record", "links") => "list:link",
        ("record", "payload") => "opaque",
        ("protection_request", "v" | "restores") => "int",
        ("protection_request", "target") => "pin",
        ("protection_request", "body") => "lifecycle_body",
        (
            "journal_entry",
            "v"
            | "seq"
            | "group"
            | "group_size"
            | "revision"
            | "destination_cut_seq"
            | "incarnation",
        ) => "int",
        ("journal_entry", "record") => "record",
        ("journal_entry", "presented" | "current") => "revision_pin",
        ("journal_entry", "request") => "protection_request",
        ("journal_entry", "continues" | "source") => "source_cut",
        ("journal_entry", "imported_from") => "source_pin",
        ("journal_entry", "body") => "lifecycle_body",
        ("journal_entry", "recovery") => "stream_recovery",
        ("profile_runtime", "clock_skew_bound_ms" | "idempotency_window_ms") => "int",
        ("stratum_profile", "runtime") => "profile_runtime",
        _ => "opaque",
    }
}

/// Run stage 1 (`ingest`) and stage 2 (`numeric_domain`) on `raw` as document `kind`.
pub(crate) fn ingest(raw: &[u8], kind: &str) -> Result<Node, Reject> {
    let mut n = parse_json(raw)?;
    opaque_pass(&n, kind)?;
    int_pass(&mut n, kind)?;
    Ok(n)
}

/// Run NDJSON framing + `ingest` on every line in `data`.
pub(crate) fn ingest_lines(data: &[u8], kind: &str) -> Result<Vec<Node>, Reject> {
    if data.contains(&b'\r') {
        return Err(Reject::ingest("CR byte in NDJSON framing"));
    }
    if !data.is_empty() && data.last() != Some(&b'\n') {
        return Err(Reject::ingest(
            "final NDJSON line has no terminating LF (incomplete append)",
        ));
    }
    let mut out = Vec::new();
    let mut rest = data;
    let mut line_no = 1usize;
    while !rest.is_empty() {
        let end = rest
            .iter()
            .position(|&b| b == b'\n')
            .expect("verified trailing LF");
        let line = &rest[..end];
        rest = &rest[end + 1..];
        if line.len() > MAX_LINE_BYTES {
            return Err(Reject::local_limit(format!(
                "NDJSON line {line_no} is longer than {MAX_LINE_BYTES} bytes"
            )));
        }
        out.push(ingest(line, kind)?);
        line_no += 1;
    }
    Ok(out)
}

fn opaque_pass(n: &Node, kind: &str) -> Result<(), Reject> {
    if kind == "int" {
        if n.kind == Kind::Number {
            return Ok(());
        }
        return opaque_pass(n, "opaque");
    }
    if kind == "opaque" {
        match n.kind {
            Kind::Number => return check_opaque(&n.token),
            Kind::Array => {
                for e in &n.elems {
                    opaque_pass(e, "opaque")?;
                }
            }
            Kind::Object => {
                for m in &n.fields {
                    opaque_pass(&m.value, "opaque")?;
                }
            }
            _ => {}
        }
        return Ok(());
    }
    if let Some(elem_kind) = kind.strip_prefix("list:") {
        if n.kind != Kind::Array {
            return opaque_pass(n, "opaque");
        }
        for e in &n.elems {
            opaque_pass(e, elem_kind)?;
        }
        return Ok(());
    }
    if n.kind != Kind::Object {
        return opaque_pass(n, "opaque");
    }
    for m in &n.fields {
        opaque_pass(&m.value, member_spec(kind, &m.name))?;
    }
    Ok(())
}

fn int_pass(n: &mut Node, kind: &str) -> Result<(), Reject> {
    if kind == "int" {
        if n.kind != Kind::Number {
            return Ok(());
        }
        let v = safe_int(&n.token)?;
        n.int_val = v;
        n.is_int = true;
        return Ok(());
    }
    if kind == "opaque" {
        return Ok(());
    }
    if let Some(elem_kind) = kind.strip_prefix("list:") {
        if n.kind != Kind::Array {
            return Ok(());
        }
        for e in &mut n.elems {
            int_pass(e, elem_kind)?;
        }
        return Ok(());
    }
    if n.kind != Kind::Object {
        return Ok(());
    }
    for m in &mut n.fields {
        let sub = member_spec(kind, &m.name);
        if sub != "opaque" {
            int_pass(&mut m.value, sub)?;
        }
    }
    Ok(())
}
