//! WASI shim exposing ast-grep-core pattern matching to Codebeam.
//!
//! The Go side (internal/structural) instantiates this module on wazero and
//! drives it through a bytes-in/JSON-out ABI:
//!
//!   sg_alloc(len) -> ptr            allocate a buffer the host writes into
//!   sg_dealloc(ptr, len)            release a host-written buffer
//!   sg_compile(pat, lang) -> id     parse+validate a pattern, 0 on error
//!   sg_match(id, src, max) -> u64   match one file, packed ptr<<32|len of JSON
//!   sg_free_pattern(id)             drop a compiled pattern
//!   sg_last_error() -> u64          packed ptr/len of the last error message
//!   sg_meta() -> u64                packed ptr/len of build metadata JSON
//!
//! All offsets in the output JSON are zero-based byte offsets into the exact
//! source buffer the host passed to sg_match; line numbers are zero-based.
//! Returned buffers are owned by the module and valid until the next call.

use ast_grep_core::matcher::Pattern;
use ast_grep_core::meta_var::MetaVariable;
use ast_grep_core::tree_sitter::{LanguageExt, StrDoc};
use ast_grep_core::NodeMatch;
use ast_grep_language::SupportLang;
use serde::Serialize;
use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, HashMap};

/// Languages whose tree-sitter grammars are compiled into this module.
/// Must stay in sync with the feature list in Cargo.toml: calling a
/// SupportLang whose grammar feature is off panics in ast-grep-language.
const ENABLED_LANGS: &[SupportLang] = &[
    SupportLang::Bash,
    SupportLang::C,
    SupportLang::Go,
    SupportLang::Java,
    SupportLang::JavaScript,
    SupportLang::Json,
    SupportLang::Python,
    SupportLang::Rust,
    SupportLang::Tsx,
    SupportLang::TypeScript,
    SupportLang::Yaml,
];

thread_local! {
    static PATTERNS: RefCell<HashMap<u32, (Pattern, SupportLang)>> = RefCell::new(HashMap::new());
    static NEXT_ID: Cell<u32> = const { Cell::new(1) };
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
    static ERROR: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

#[derive(Serialize)]
struct Range {
    start: usize,
    end: usize,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct MatchOut {
    byte_start: usize,
    byte_end: usize,
    start_line: usize,
    end_line: usize,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    vars: BTreeMap<String, Range>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    multi_vars: BTreeMap<String, Range>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct MatchesOut {
    matches: Vec<MatchOut>,
    truncated: bool,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct MetaOut {
    ast_grep: &'static str,
    languages: Vec<String>,
}

fn pack(bytes: &[u8]) -> u64 {
    ((bytes.as_ptr() as u64) << 32) | (bytes.len() as u64)
}

fn set_result(v: Vec<u8>) -> u64 {
    RESULT.with(|r| {
        *r.borrow_mut() = v;
        pack(&r.borrow())
    })
}

fn set_error(msg: impl Into<String>) {
    ERROR.with(|e| *e.borrow_mut() = msg.into().into_bytes());
}

/// # Safety
/// ptr/len must describe a live buffer previously written by the host.
unsafe fn read_bytes<'a>(ptr: u32, len: u32) -> &'a [u8] {
    std::slice::from_raw_parts(ptr as *const u8, len as usize)
}

#[no_mangle]
pub extern "C" fn sg_alloc(len: u32) -> u32 {
    let mut buf = vec![0u8; len as usize];
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr as u32
}

/// # Safety
/// ptr/len must come from a matching sg_alloc call.
#[no_mangle]
pub unsafe extern "C" fn sg_dealloc(ptr: u32, len: u32) {
    drop(Vec::from_raw_parts(
        ptr as *mut u8,
        len as usize,
        len as usize,
    ));
}

#[no_mangle]
pub extern "C" fn sg_last_error() -> u64 {
    ERROR.with(|e| pack(&e.borrow()))
}

#[no_mangle]
pub extern "C" fn sg_meta() -> u64 {
    let meta = MetaOut {
        ast_grep: "0.44.0",
        languages: ENABLED_LANGS
            .iter()
            .map(|l| l.to_string().to_lowercase())
            .collect(),
    };
    set_result(serde_json::to_vec(&meta).expect("meta serializes"))
}

/// # Safety
/// Pointer arguments must describe live host-written buffers.
#[no_mangle]
pub unsafe extern "C" fn sg_compile(
    pat_ptr: u32,
    pat_len: u32,
    lang_ptr: u32,
    lang_len: u32,
) -> u32 {
    let pat = match std::str::from_utf8(read_bytes(pat_ptr, pat_len)) {
        Ok(s) => s,
        Err(_) => {
            set_error("pattern is not valid UTF-8");
            return 0;
        }
    };
    let lang_str = match std::str::from_utf8(read_bytes(lang_ptr, lang_len)) {
        Ok(s) => s,
        Err(_) => {
            set_error("language is not valid UTF-8");
            return 0;
        }
    };
    let lang: SupportLang = match lang_str.parse() {
        Ok(l) => l,
        Err(e) => {
            set_error(format!("{e}"));
            return 0;
        }
    };
    if !ENABLED_LANGS.contains(&lang) {
        set_error(format!("language {lang} is not bundled in this build"));
        return 0;
    }
    let pattern = match Pattern::try_new(pat, lang) {
        Ok(p) => p,
        Err(e) => {
            set_error(format!("invalid pattern: {e}"));
            return 0;
        }
    };
    if pattern.has_error() {
        set_error(format!(
            "invalid pattern: `{pat}` does not parse as valid {lang} code"
        ));
        return 0;
    }
    PATTERNS.with(|p| {
        let id = NEXT_ID.with(|n| {
            let id = n.get();
            n.set(id.wrapping_add(1).max(1));
            id
        });
        p.borrow_mut().insert(id, (pattern, lang));
        id
    })
}

#[no_mangle]
pub extern "C" fn sg_free_pattern(id: u32) {
    PATTERNS.with(|p| {
        p.borrow_mut().remove(&id);
    });
}

/// # Safety
/// src_ptr/src_len must describe a live host-written buffer.
#[no_mangle]
pub unsafe extern "C" fn sg_match(id: u32, src_ptr: u32, src_len: u32, max_matches: u32) -> u64 {
    let src = match std::str::from_utf8(read_bytes(src_ptr, src_len)) {
        Ok(s) => s,
        Err(_) => {
            set_error("source is not valid UTF-8");
            return 0;
        }
    };
    PATTERNS.with(|p| {
        let map = p.borrow();
        let Some((pattern, lang)) = map.get(&id) else {
            set_error(format!("unknown pattern handle {id}"));
            return 0;
        };
        let root = lang.ast_grep(src);
        let node = root.root();
        let mut matches = Vec::new();
        let mut truncated = false;
        for m in node.find_all(pattern) {
            if max_matches > 0 && matches.len() >= max_matches as usize {
                truncated = true;
                break;
            }
            matches.push(to_match_out(&m));
        }
        let out = MatchesOut { matches, truncated };
        set_result(serde_json::to_vec(&out).expect("matches serialize"))
    })
}

fn to_match_out(m: &NodeMatch<'_, StrDoc<SupportLang>>) -> MatchOut {
    let range = m.range();
    let env = m.get_env();
    let mut vars = BTreeMap::new();
    let mut multi_vars = BTreeMap::new();
    for v in env.get_matched_variables() {
        match v {
            MetaVariable::Capture(name, _) => {
                if let Some(n) = env.get_match(&name) {
                    let r = n.range();
                    vars.insert(
                        name,
                        Range {
                            start: r.start,
                            end: r.end,
                        },
                    );
                }
            }
            MetaVariable::MultiCapture(name) => {
                let ns = env.get_multiple_matches(&name);
                if let (Some(first), Some(last)) = (ns.first(), ns.last()) {
                    multi_vars.insert(
                        name,
                        Range {
                            start: first.range().start,
                            end: last.range().end,
                        },
                    );
                }
            }
            _ => {}
        }
    }
    MatchOut {
        byte_start: range.start,
        byte_end: range.end,
        start_line: m.start_pos().line(),
        end_line: m.end_pos().line(),
        vars,
        multi_vars,
    }
}
