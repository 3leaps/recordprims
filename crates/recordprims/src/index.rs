// SPDX-License-Identifier: MIT OR Apache-2.0

//! Per-stream derived indexes (`heads.idx`, `keys.idx`, `records.idx`).
//!
//! The indexes are a cache over a stream's journal, never a source of truth.
//! Each is RFC 8785 JCS JSON carrying a watermark (the cut it was computed
//! at) and a content digest over its entries, byte-identical to the Go
//! implementation's output. Idempotency keys appear only as a keyed MAC
//! under the store's diagnostic key, never in the clear.

use std::collections::HashMap;

use sha2::{Digest as _, Sha256};

use crate::canon;
use crate::error::Error;
use crate::journal::State;
use crate::store::{
    hex_encode, read_file_verified, write_file_durable, Store, FILE_DIAG_KEY, KEY_LEN,
};
use crate::wire::{self, Member, Node};

pub(crate) const FILE_HEADS_IDX: &str = "heads.idx";
pub(crate) const FILE_KEYS_IDX: &str = "keys.idx";
pub(crate) const FILE_RECORDS_IDX: &str = "records.idx";

const MAX_INDEX_BYTES: i64 = 64 << 20;

pub(crate) fn sha256_hex_prefixed(b: &[u8]) -> String {
    format!("sha256:{}", hex_encode(&Sha256::digest(b)))
}

fn hmac_sha256(key: &[u8], parts: &[&[u8]]) -> [u8; 32] {
    const BLOCK: usize = 64;
    let mut k = [0u8; BLOCK];
    if key.len() > BLOCK {
        k[..32].copy_from_slice(&Sha256::digest(key));
    } else {
        k[..key.len()].copy_from_slice(key);
    }
    let mut inner = Sha256::new();
    inner.update(k.map(|b| b ^ 0x36));
    for p in parts {
        inner.update(p);
    }
    let inner = inner.finalize();
    let mut outer = Sha256::new();
    outer.update(k.map(|b| b ^ 0x5c));
    outer.update(inner);
    outer.finalize().into()
}

/// The keyed MAC id of an idempotency key on a stream (first 8 bytes, hex).
pub(crate) fn diag_key_id(diag_key: &[u8], stream: &str, raw_key: &str) -> String {
    let sl = u32::try_from(stream.len())
        .unwrap_or(u32::MAX)
        .to_be_bytes();
    let kl = u32::try_from(raw_key.len())
        .unwrap_or(u32::MAX)
        .to_be_bytes();
    let sum = hmac_sha256(
        diag_key,
        &[
            b"recordprims/key_id/v1",
            &sl,
            stream.as_bytes(),
            &kl,
            raw_key.as_bytes(),
        ],
    );
    hex_encode(&sum[..8])
}

fn line_offsets(raw: &[u8]) -> Vec<u64> {
    let mut out = Vec::new();
    let mut off = 0u64;
    for (i, b) in raw.iter().enumerate() {
        if *b == b'\n' {
            out.push(off);
            off = i as u64 + 1;
        }
    }
    out
}

fn m(name: &str, value: Node) -> Member {
    Member {
        name: name.to_owned(),
        value,
    }
}

fn s(v: &str) -> Node {
    Node::string(v)
}

fn str_at<'a>(n: &'a Node, name: &str) -> &'a str {
    n.get(name).map_or("", |v| v.str_val.as_str())
}

fn int_at(n: &Node, name: &str) -> i64 {
    n.get(name).map_or(0, |v| v.int_val)
}

/// Canonical JCS bytes of `n` plus a trailing LF.
fn canonical(n: &Node) -> Result<Vec<u8>, Error> {
    let mut out = canon::canonicalize(&wire::encode(n))?;
    out.push(b'\n');
    Ok(out)
}

fn imported_from_node(e: &Node) -> Option<Node> {
    let imp = e.get("imported_from")?;
    let p = imp.get("pin")?;
    Some(Node::object(vec![
        m("capability", s(str_at(imp, "capability"))),
        m(
            "pin",
            Node::object(vec![
                m("stream", s(str_at(p, "stream"))),
                m("record_id", s(str_at(p, "record_id"))),
                m("revision", Node::int(int_at(p, "revision"))),
                m("digest", s(str_at(p, "digest"))),
            ]),
        ),
    ]))
}

fn doc(watermark: &Node, entries: Vec<Node>) -> Result<Vec<u8>, Error> {
    let list = Node::array(entries);
    let ent = canonical(&list)?;
    let digest = sha256_hex_prefixed(&ent[..ent.len() - 1]);
    canonical(&Node::object(vec![
        m("watermark", watermark.clone()),
        m("content_digest", s(&digest)),
        m("entries", list),
    ]))
}

/// The expected bytes of `heads.idx`, `keys.idx` and `records.idx` for a
/// stream whose journal bytes are `raw` and whose replayed state is `st`.
pub(crate) fn expected_indexes(
    diag_key: &[u8],
    st: &State,
    raw: &[u8],
) -> Result<[Vec<u8>; 3], Error> {
    let watermark = Node::object(vec![
        m(
            "cut_seq",
            Node::int(i64::try_from(st.len()).unwrap_or(i64::MAX)),
        ),
        m(
            "cut_offset",
            Node::int(i64::try_from(raw.len()).unwrap_or(i64::MAX)),
        ),
        m("journal_sha256_through_cut", s(&sha256_hex_prefixed(raw))),
        m("seal_generation", Node::int(0)),
    ]);
    let offsets = line_offsets(raw);
    let stream = st.stream();

    struct Head {
        rev: i64,
        digest: String,
        seq: i64,
        offset: i64,
        imported: Option<Node>,
    }
    let mut latest: HashMap<String, Head> = HashMap::new();
    let mut recs: Vec<(String, i64, Node)> = Vec::new();
    let mut keys: Vec<Node> = Vec::new();
    let key_entry = |raw_key: &str, kind: &str, seq: i64, off: i64, at: &str| {
        Node::object(vec![
            m("key_id", s(&diag_key_id(diag_key, stream, raw_key))),
            m("kind", s(kind)),
            m("seq", Node::int(seq)),
            m("offset", Node::int(off)),
            m("at", s(at)),
        ])
    };

    for e in st.journal() {
        let seq = int_at(e, "seq");
        let off = usize::try_from(seq - 1)
            .ok()
            .and_then(|i| offsets.get(i))
            .map_or(0, |o| i64::try_from(*o).unwrap_or(i64::MAX));
        let at = str_at(e, "at");
        match str_at(e, "type") {
            "appended" | "revised" => {
                let Some(r) = e.get("record") else { continue };
                let rid = str_at(r, "record_id").to_owned();
                let rev = int_at(r, "revision");
                let dig = str_at(e, "digest").to_owned();
                let imported = imported_from_node(e);
                let mut fields = vec![
                    m("record_id", s(&rid)),
                    m("revision", Node::int(rev)),
                    m("digest", s(&dig)),
                    m("seq", Node::int(seq)),
                    m("offset", Node::int(off)),
                ];
                if let Some(imp) = &imported {
                    fields.push(m("imported_from", imp.clone()));
                }
                recs.push((rid.clone(), rev, Node::object(fields)));
                if latest.get(&rid).is_none_or(|cur| rev >= cur.rev) {
                    latest.insert(
                        rid,
                        Head {
                            rev,
                            digest: dig,
                            seq,
                            offset: off,
                            imported,
                        },
                    );
                }
                keys.push(key_entry(
                    str_at(e, "idempotency_key"),
                    "content",
                    seq,
                    off,
                    at,
                ));
            }
            "tombstoned" | "untombstoned" => {
                let k = e
                    .get("request")
                    .map_or("", |r| str_at(r, "idempotency_key"));
                keys.push(key_entry(k, "protection", seq, off, at));
            }
            "retired" => {
                if let Some(k) = e.get("idempotency_key") {
                    keys.push(key_entry(&k.str_val, "retirement", seq, off, at));
                }
            }
            _ => {}
        }
    }

    let mut heads: Vec<(String, Head)> = latest.into_iter().collect();
    heads.sort_by(|a, b| a.0.cmp(&b.0));
    let heads: Vec<Node> = heads
        .into_iter()
        .map(|(rid, h)| {
            let mut fields = vec![
                m("record_id", s(&rid)),
                m("revision", Node::int(h.rev)),
                m("digest", s(&h.digest)),
                m("retired", Node::boolean(st.is_retired(&rid))),
                m("seq", Node::int(h.seq)),
                m("offset", Node::int(h.offset)),
            ];
            if let Some(imp) = h.imported {
                fields.push(m("imported_from", imp));
            }
            Node::object(fields)
        })
        .collect();
    recs.sort_by(|a, b| a.0.cmp(&b.0).then(a.1.cmp(&b.1)));
    let recs: Vec<Node> = recs.into_iter().map(|(_, _, n)| n).collect();

    Ok([
        doc(&watermark, heads)?,
        doc(&watermark, keys)?,
        doc(&watermark, recs)?,
    ])
}

impl Store {
    fn clean_stale_index_temps(&self, sdir: &str) {
        let Ok(names) = self.root.read_dir_names(sdir, self.fso) else {
            return;
        };
        for n in names {
            if n.starts_with(".tmp-") {
                let _ = self.root.remove(&format!("{sdir}/{n}"), self.fso);
            }
        }
    }

    fn write_index_file_atomic(&self, sdir: &str, name: &str, data: &[u8]) -> Result<(), Error> {
        self.clean_stale_index_temps(sdir);
        let mut id = [0u8; 12];
        crate::store::fill_random(&mut id)?;
        let tmp = format!("{sdir}/.tmp-{name}-{}", hex_encode(&id));
        if let Err(e) = write_file_durable(&self.root, &tmp, data, self.fso) {
            let _ = self.root.remove(&tmp, self.fso);
            return Err(e);
        }
        let dst = format!("{sdir}/{name}");
        if let Err(e) = self.root.rename(&tmp, &dst, self.fso) {
            let _ = self.root.remove(&tmp, self.fso);
            return Err(e.into_store_error("rename", false));
        }
        self.root
            .sync_dir(sdir, self.fso)
            .map_err(|e| e.into_store_error("fsync_dir", false))
    }

    /// Verifies a stream's indexes against its journal and rewrites all
    /// three when any differs from the expected bytes.
    pub(crate) fn sync_stream_indexes(
        &self,
        sdir: &str,
        st: &State,
        raw: &[u8],
    ) -> Result<(), Error> {
        let diag_key = read_file_verified(&self.root, FILE_DIAG_KEY, self.fso, KEY_LEN as i64)?;
        let want = expected_indexes(&diag_key, st, raw)?;
        let names = [FILE_HEADS_IDX, FILE_KEYS_IDX, FILE_RECORDS_IDX];
        let all_ok = names.iter().zip(&want).all(|(n, w)| {
            read_file_verified(
                &self.root,
                &format!("{sdir}/{n}"),
                self.fso,
                MAX_INDEX_BYTES,
            )
            .is_ok_and(|got| got == *w)
        });
        if all_ok {
            return Ok(());
        }
        for (n, w) in names.iter().zip(&want) {
            self.write_index_file_atomic(sdir, n, w)?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hmac_sha256_matches_rfc4231() {
        // RFC 4231 test case 2.
        let mac = hmac_sha256(b"Jefe", &[b"what do ya want ", b"for nothing?"]);
        assert_eq!(
            hex_encode(&mac),
            "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
        );
        // RFC 4231 test case 6: a key longer than the block is hashed first.
        let key = [0xaau8; 131];
        let mac = hmac_sha256(
            &key,
            &[b"Test Using Larger Than Block-Size Key - Hash Key First"],
        );
        assert_eq!(
            hex_encode(&mac),
            "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"
        );
    }
}
