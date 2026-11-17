// SPDX-License-Identifier: MIT OR Apache-2.0

//! Windows implementation of confined root operations, owner/DACL verification,
//! filesystem allowlist checks, and `LockFileEx` advisory locking via `windows-sys`.
//!
//! This module is the ONLY module in the workspace permitted to use
//! `#![allow(unsafe_code)]` (enforced by `scripts/unsafe-check.sh` per ADR-0001).
//! Every `unsafe` block is confined to an internal helper with stated handle
//! and buffer ownership invariants.

#![allow(unsafe_code)]

use std::fs::{self, File, OpenOptions};
use std::io;
use std::mem::{size_of, zeroed};
use std::os::windows::fs::OpenOptionsExt;
use std::os::windows::io::AsRawHandle;
use std::path::{Path, PathBuf};
use std::ptr::{null, null_mut};

use windows_sys::Win32::Foundation::{
    CloseHandle, LocalFree, ERROR_IO_PENDING, ERROR_LOCK_VIOLATION, ERROR_SUCCESS, GENERIC_ALL,
    GENERIC_READ, GENERIC_WRITE, HANDLE, HLOCAL, MAX_PATH,
};
use windows_sys::Win32::Security::Authorization::{
    GetSecurityInfo, SetEntriesInAclW, SetSecurityInfo, EXPLICIT_ACCESS_W, SET_ACCESS,
    SE_FILE_OBJECT, TRUSTEE_IS_SID, TRUSTEE_IS_UNKNOWN, TRUSTEE_W,
};
use windows_sys::Win32::Security::{
    AclSizeInformation, EqualSid, GetAce, GetAclInformation, GetTokenInformation, IsWellKnownSid,
    TokenOwner, TokenUser, WinBuiltinAdministratorsSid, WinCreatorOwnerRightsSid,
    WinCreatorOwnerSid, WinLocalSystemSid, ACCESS_ALLOWED_ACE, ACE_HEADER, ACL,
    ACL_SIZE_INFORMATION, DACL_SECURITY_INFORMATION, NO_INHERITANCE, OWNER_SECURITY_INFORMATION,
    PROTECTED_DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR, PSID, TOKEN_OWNER, TOKEN_QUERY,
    TOKEN_USER,
};
use windows_sys::Win32::Storage::FileSystem::{
    GetFileInformationByHandle, GetFileType, GetFinalPathNameByHandleW,
    GetVolumeInformationByHandleW, LockFileEx, UnlockFileEx, BY_HANDLE_FILE_INFORMATION,
    FILE_ATTRIBUTE_DIRECTORY, FILE_ATTRIBUTE_REPARSE_POINT, FILE_FLAG_BACKUP_SEMANTICS,
    FILE_FLAG_OPEN_REPARSE_POINT, FILE_SHARE_READ, FILE_SHARE_WRITE, FILE_TYPE_DISK,
    LOCKFILE_EXCLUSIVE_LOCK, LOCKFILE_FAIL_IMMEDIATELY, READ_CONTROL, WRITE_DAC,
};
use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};
use windows_sys::Win32::System::IO::OVERLAPPED;

use crate::fsroot::{FsRootError, Options};

const ACCESS_ALLOWED_ACE_TYPE: u8 = 0x0;
const ACCESS_DENIED_ACE_TYPE: u8 = 0x1;
const ACCESS_DENIED_OBJECT_ACE_TYPE: u8 = 0x6;
const ACCESS_DENIED_CALLBACK_ACE_TYPE: u8 = 0xA;
const ACCESS_DENIED_CALLBACK_OBJECT_ACE_TYPE: u8 = 0xC;

/// Retained handle and file-index identity for the verified store root directory.
/// Holding `handle` open without `FILE_SHARE_DELETE` prevents the root directory
/// from being deleted or renamed while the store is open, and resolving paths from
/// `GetFinalPathNameByHandleW(&self.handle)` makes operations immune to `chdir`.
pub(crate) struct WinRoot {
    handle: File,
    volume_serial: u32,
    file_index_high: u32,
    file_index_low: u32,
}

/// Guard that calls `CloseHandle` on drop for an owned Win32 `HANDLE`.
struct OwnedWinHandle(HANDLE);

impl Drop for OwnedWinHandle {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: `self.0` was returned by `OpenProcessToken` and is owned exclusively by this guard.
            unsafe {
                CloseHandle(self.0);
            }
        }
    }
}

/// Guard that calls `LocalFree` on drop for an owned `PSECURITY_DESCRIPTOR` allocated by `GetSecurityInfo`.
struct SecurityDescriptorGuard(PSECURITY_DESCRIPTOR);

impl Drop for SecurityDescriptorGuard {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: `self.0` was allocated by `GetSecurityInfo` with `LocalAlloc` and is freed once here.
            unsafe {
                LocalFree(self.0 as HLOCAL);
            }
        }
    }
}

/// Guard that calls `LocalFree` on drop for an owned `*mut ACL` allocated by `SetEntriesInAclW`.
struct AclGuard(*mut ACL);

impl Drop for AclGuard {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: `self.0` was allocated by `SetEntriesInAclW` with `LocalAlloc` and is freed once here.
            unsafe {
                LocalFree(self.0 as HLOCAL);
            }
        }
    }
}

/// Open a file or directory without following reparse points and verify its
/// attributes, owner SID, and DACL. Directories omit `FILE_SHARE_DELETE` so
/// they cannot be renamed or deleted while held open.
fn open_handle_no_reparse(path: &Path, want_dir: bool) -> Result<File, FsRootError> {
    let mut flags = FILE_FLAG_OPEN_REPARSE_POINT;
    let mut opts = OpenOptions::new();
    opts.read(true);
    if want_dir {
        flags |= FILE_FLAG_BACKUP_SEMANTICS;
        opts.share_mode(FILE_SHARE_READ | FILE_SHARE_WRITE);
    }
    let f = opts
        .custom_flags(flags)
        .open(path)
        .map_err(|e| FsRootError::Io("open", e))?;
    verify_file_handle(&f, want_dir)?;
    Ok(f)
}

fn file_info_by_handle(file: &File) -> Result<BY_HANDLE_FILE_INFORMATION, FsRootError> {
    let raw = file.as_raw_handle() as HANDLE;
    // SAFETY: `raw` is a live file handle owned by `file` for the duration of this call;
    // `GetFileType` and `GetFileInformationByHandle` read the kernel handle state.
    unsafe {
        if GetFileType(raw) != FILE_TYPE_DISK {
            return Err(FsRootError::NotRegular);
        }
        let mut info: BY_HANDLE_FILE_INFORMATION = zeroed();
        if GetFileInformationByHandle(raw, &mut info) == 0 {
            return Err(FsRootError::Io("stat", io::Error::last_os_error()));
        }
        Ok(info)
    }
}

fn verify_file_handle(file: &File, want_dir: bool) -> Result<u64, FsRootError> {
    let info = file_info_by_handle(file)?;
    if (info.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
        return Err(FsRootError::Symlink);
    }
    let is_dir = (info.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
    if want_dir && !is_dir {
        return Err(FsRootError::NotDir);
    }
    if !want_dir && is_dir {
        return Err(FsRootError::NotRegular);
    }
    if !want_dir && info.nNumberOfLinks > 1 {
        return Err(FsRootError::MultipleLink);
    }
    let raw = file.as_raw_handle() as HANDLE;
    verify_owner_and_dacl(raw)?;
    let size = (u64::from(info.nFileSizeHigh) << 32) | u64::from(info.nFileSizeLow);
    Ok(size)
}

/// Apply an explicit protected owner-only DACL (`NO_INHERITANCE`) to a newly created
/// file or directory using `SetEntriesInAclW` and `SetSecurityInfo` so it never inherits
/// permissive ACEs from an ancestor.
fn apply_owner_only_dacl(file: &File) -> Result<(), FsRootError> {
    let (user_buf, owner_buf) = current_token_sids()?;
    // SAFETY: `user_buf` was populated by `GetTokenInformation(..., TokenUser, ...)` and is 8-byte aligned.
    let user_sid = unsafe { (&*(user_buf.as_ptr() as *const TOKEN_USER)).User.Sid };
    // SAFETY: `owner_buf` was populated by `GetTokenInformation(..., TokenOwner, ...)` and is 8-byte aligned.
    let default_owner_sid = unsafe { (&*(owner_buf.as_ptr() as *const TOKEN_OWNER)).Owner };

    // SAFETY: `user_sid` and `default_owner_sid` are valid SIDs in `user_buf` and `owner_buf` for
    // the duration of this block. `SetEntriesInAclW` allocates `dacl` via `LocalAlloc`, which is
    // immediately wrapped in `AclGuard` so it is freed via `LocalFree` on every return path.
    unsafe {
        let same_sid = EqualSid(user_sid, default_owner_sid) != 0;
        let entries = [
            EXPLICIT_ACCESS_W {
                grfAccessPermissions: GENERIC_ALL,
                grfAccessMode: SET_ACCESS,
                grfInheritance: NO_INHERITANCE,
                Trustee: TRUSTEE_W {
                    pMultipleTrustee: null_mut(),
                    MultipleTrusteeOperation: 0,
                    TrusteeForm: TRUSTEE_IS_SID,
                    TrusteeType: TRUSTEE_IS_UNKNOWN,
                    ptstrName: default_owner_sid as *mut u16,
                },
            },
            EXPLICIT_ACCESS_W {
                grfAccessPermissions: GENERIC_ALL,
                grfAccessMode: SET_ACCESS,
                grfInheritance: NO_INHERITANCE,
                Trustee: TRUSTEE_W {
                    pMultipleTrustee: null_mut(),
                    MultipleTrusteeOperation: 0,
                    TrusteeForm: TRUSTEE_IS_SID,
                    TrusteeType: TRUSTEE_IS_UNKNOWN,
                    ptstrName: user_sid as *mut u16,
                },
            },
        ];
        let count: u32 = if same_sid { 1 } else { 2 };
        let mut dacl: *mut ACL = null_mut();
        let rc_acl = SetEntriesInAclW(count, entries.as_ptr(), null(), &mut dacl);
        if rc_acl != ERROR_SUCCESS || dacl.is_null() {
            return Err(FsRootError::Io(
                "chmod",
                io::Error::from_raw_os_error(rc_acl as i32),
            ));
        }
        let _acl_guard = AclGuard(dacl);

        let raw = file.as_raw_handle() as HANDLE;
        let rc = SetSecurityInfo(
            raw,
            SE_FILE_OBJECT,
            DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION,
            null_mut(),
            null_mut(),
            dacl,
            null(),
        );
        if rc != ERROR_SUCCESS {
            return Err(FsRootError::Io(
                "chmod",
                io::Error::from_raw_os_error(rc as i32),
            ));
        }
    }
    Ok(())
}

fn verify_owner_and_dacl(handle: HANDLE) -> Result<(), FsRootError> {
    let mut owner: PSID = null_mut();
    let mut dacl: *mut ACL = null_mut();
    let mut sd: PSECURITY_DESCRIPTOR = null_mut();

    // SAFETY: `handle` is a valid open file handle; `owner`, `dacl`, and `sd` are valid out-pointers.
    // On success, `sd` owns the allocated security descriptor buffer and is immediately wrapped in `SecurityDescriptorGuard`.
    let rc = unsafe {
        GetSecurityInfo(
            handle,
            SE_FILE_OBJECT,
            OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION,
            &mut owner,
            null_mut(),
            &mut dacl,
            null_mut(),
            &mut sd,
        )
    };
    if rc != ERROR_SUCCESS {
        return Err(FsRootError::Io(
            "stat",
            io::Error::from_raw_os_error(rc as i32),
        ));
    }
    let _sd_guard = SecurityDescriptorGuard(sd);
    if owner.is_null() {
        return Err(FsRootError::Owner);
    }

    let (user_buf, owner_buf) = current_token_sids()?;
    // SAFETY: `user_buf` was populated by `GetTokenInformation(..., TokenUser, ...)` and is 8-byte aligned.
    let user_sid = unsafe { (&*(user_buf.as_ptr() as *const TOKEN_USER)).User.Sid };
    // SAFETY: `owner_buf` was populated by `GetTokenInformation(..., TokenOwner, ...)` and is 8-byte aligned.
    let default_owner_sid = unsafe { (&*(owner_buf.as_ptr() as *const TOKEN_OWNER)).Owner };

    // SAFETY: `owner`, `user_sid`, and `default_owner_sid` are valid SIDs for the duration of this call.
    let is_owner =
        unsafe { EqualSid(owner, user_sid) != 0 || EqualSid(owner, default_owner_sid) != 0 };
    if !is_owner {
        return Err(FsRootError::Owner);
    }
    if dacl.is_null() {
        return Err(FsRootError::Perm);
    }

    // SAFETY: `dacl` points into `_sd_guard` and is non-null; `acl_info` is a stack struct.
    let acl_info = unsafe {
        let mut acl_info: ACL_SIZE_INFORMATION = zeroed();
        if GetAclInformation(
            dacl,
            &mut acl_info as *mut _ as *mut _,
            size_of::<ACL_SIZE_INFORMATION>() as u32,
            AclSizeInformation,
        ) == 0
        {
            return Err(FsRootError::Io("stat", io::Error::last_os_error()));
        }
        acl_info
    };

    for i in 0..acl_info.AceCount {
        let mut ace_ptr: *mut core::ffi::c_void = null_mut();
        // SAFETY: `dacl` is valid and `i < acl_info.AceCount`.
        let ok = unsafe { GetAce(dacl, i, &mut ace_ptr) != 0 };
        if !ok || ace_ptr.is_null() {
            return Err(FsRootError::Perm);
        }
        // SAFETY: Every ACE begins with `ACE_HEADER`. When `AceType == ACCESS_ALLOWED_ACE_TYPE`,
        // the ACE layout is `ACCESS_ALLOWED_ACE`, whose `SidStart` field is the inline first DWORD of the SID.
        unsafe {
            let hdr = &*(ace_ptr as *const ACE_HEADER);
            match hdr.AceType {
                ACCESS_ALLOWED_ACE_TYPE => {
                    let allow = &*(ace_ptr as *const ACCESS_ALLOWED_ACE);
                    let sid = &allow.SidStart as *const u32 as PSID;
                    if !allowed_trustee(sid, owner, user_sid, default_owner_sid) {
                        return Err(FsRootError::Perm);
                    }
                }
                ACCESS_DENIED_ACE_TYPE
                | ACCESS_DENIED_OBJECT_ACE_TYPE
                | ACCESS_DENIED_CALLBACK_ACE_TYPE
                | ACCESS_DENIED_CALLBACK_OBJECT_ACE_TYPE => {}
                _ => return Err(FsRootError::Perm),
            }
        }
    }
    Ok(())
}

/// SAFETY: All `PSID` arguments must be valid SID pointers for the duration of the call.
unsafe fn allowed_trustee(
    sid: PSID,
    owner_sid: PSID,
    user_sid: PSID,
    default_owner_sid: PSID,
) -> bool {
    if EqualSid(sid, owner_sid) != 0
        || EqualSid(sid, user_sid) != 0
        || EqualSid(sid, default_owner_sid) != 0
    {
        return true;
    }
    IsWellKnownSid(sid, WinLocalSystemSid) != 0
        || IsWellKnownSid(sid, WinBuiltinAdministratorsSid) != 0
        || IsWellKnownSid(sid, WinCreatorOwnerSid) != 0
        || IsWellKnownSid(sid, WinCreatorOwnerRightsSid) != 0
}

fn query_token_info(tok: HANDLE, class: i32) -> Result<Vec<u64>, FsRootError> {
    let mut needed: u32 = 0;
    // SAFETY: First call with null buffer queries the required byte length in `needed`.
    unsafe {
        GetTokenInformation(tok, class, null_mut(), 0, &mut needed);
    }
    if needed == 0 {
        return Err(FsRootError::Io("stat", io::Error::last_os_error()));
    }
    let words = (needed as usize).div_ceil(size_of::<u64>());
    let mut buf = vec![0u64; words];
    // SAFETY: `buf` is 8-byte aligned and has at least `needed` bytes of capacity.
    let ok = unsafe {
        GetTokenInformation(
            tok,
            class,
            buf.as_mut_ptr() as *mut _,
            (words * size_of::<u64>()) as u32,
            &mut needed,
        ) != 0
    };
    if !ok {
        return Err(FsRootError::Io("stat", io::Error::last_os_error()));
    }
    Ok(buf)
}

fn current_token_sids() -> Result<(Vec<u64>, Vec<u64>), FsRootError> {
    let mut tok: HANDLE = null_mut();
    // SAFETY: `GetCurrentProcess()` returns a pseudo-handle; `tok` receives a new token handle closed by `OwnedWinHandle`.
    let opened = unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut tok) != 0 };
    if !opened {
        return Err(FsRootError::Io("stat", io::Error::last_os_error()));
    }
    let _tok_guard = OwnedWinHandle(tok);
    let user_buf = query_token_info(tok, TokenUser)?;
    let owner_buf = query_token_info(tok, TokenOwner)?;
    Ok((user_buf, owner_buf))
}

fn final_path_by_handle(file: &File) -> Result<String, FsRootError> {
    let raw = file.as_raw_handle() as HANDLE;
    let mut path_buf = vec![0u16; 512];
    loop {
        // SAFETY: `raw` is a live handle and `path_buf` has `path_buf.len()` `u16` elements.
        let n = unsafe {
            GetFinalPathNameByHandleW(raw, path_buf.as_mut_ptr(), path_buf.len() as u32, 0)
        };
        if n == 0 {
            return Err(FsRootError::Io("stat", io::Error::last_os_error()));
        }
        if (n as usize) < path_buf.len() {
            return Ok(String::from_utf16_lossy(&path_buf[..(n as usize)]));
        }
        path_buf.resize((n as usize) + 1, 0);
    }
}

fn verify_volume_fs_type(file: &File) -> Result<(), FsRootError> {
    let raw = file.as_raw_handle() as HANDLE;
    let mut fs_name = [0u16; (MAX_PATH as usize) + 1];
    // SAFETY: `raw` is a live directory handle and `fs_name` is a stack UTF-16 buffer of `MAX_PATH + 1` elements.
    let ok = unsafe {
        GetVolumeInformationByHandleW(
            raw,
            null_mut(),
            0,
            null_mut(),
            null_mut(),
            null_mut(),
            fs_name.as_mut_ptr(),
            fs_name.len() as u32,
        ) != 0
    };
    if !ok {
        return Err(FsRootError::Io("stat", io::Error::last_os_error()));
    }
    let nul_pos = fs_name
        .iter()
        .position(|&c| c == 0)
        .unwrap_or(fs_name.len());
    let fs_str = String::from_utf16_lossy(&fs_name[..nul_pos]);
    if fs_str != "NTFS" && fs_str != "ReFS" {
        return Err(FsRootError::FsType);
    }

    let final_path = final_path_by_handle(file)?;
    if final_path.starts_with(r"\\?\UNC\") {
        return Err(FsRootError::FsType);
    }
    Ok(())
}

pub(crate) fn open_root_dir(path: &Path, _opts: Options) -> Result<WinRoot, FsRootError> {
    let f = open_handle_no_reparse(path, true)?;
    verify_volume_fs_type(&f)?;
    let info = file_info_by_handle(&f)?;
    Ok(WinRoot {
        handle: f,
        volume_serial: info.dwVolumeSerialNumber,
        file_index_high: info.nFileIndexHigh,
        file_index_low: info.nFileIndexLow,
    })
}

/// Resolve `parts` relative to the open `root` handle, keeping every traversed
/// intermediate directory handle open (without `FILE_SHARE_DELETE`) in the
/// returned `Vec<File>` guard so no ancestor can be replaced or renamed during
/// the leaf operation.
fn resolve_verified_path(
    root: &WinRoot,
    parts: &[&str],
    _opts: Options,
) -> Result<(PathBuf, Vec<File>), FsRootError> {
    verify_file_handle(&root.handle, true)?;
    let info = file_info_by_handle(&root.handle)?;
    if info.dwVolumeSerialNumber != root.volume_serial
        || info.nFileIndexHigh != root.file_index_high
        || info.nFileIndexLow != root.file_index_low
    {
        return Err(FsRootError::Path);
    }
    let root_final = final_path_by_handle(&root.handle)?;
    if root_final.starts_with(r"\\?\UNC\") {
        return Err(FsRootError::FsType);
    }
    let mut cur = PathBuf::from(root_final);
    let mut held_dirs = Vec::new();
    if let Some((&leaf, parents)) = parts.split_last() {
        for &comp in parents {
            cur.push(comp);
            let dir = open_handle_no_reparse(&cur, true)?;
            held_dirs.push(dir);
        }
        cur.push(leaf);
    }
    Ok((cur, held_dirs))
}

pub(crate) fn stat_verified(
    root: &WinRoot,
    parts: &[&str],
    want_dir: bool,
    opts: Options,
) -> Result<u64, FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = open_handle_no_reparse(&p, want_dir)?;
    verify_file_handle(&f, want_dir)
}

pub(crate) fn open_verified_read(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<(File, u64), FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = open_handle_no_reparse(&p, false)?;
    let size = verify_file_handle(&f, false)?;
    Ok((f, size))
}

pub(crate) fn create_exclusive(
    root: &WinRoot,
    parts: &[&str],
    mode: u32,
    opts: Options,
) -> Result<File, FsRootError> {
    if (mode & 0o077) != 0 {
        return Err(FsRootError::Perm);
    }
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = OpenOptions::new()
        .access_mode(GENERIC_READ | GENERIC_WRITE | WRITE_DAC | READ_CONTROL)
        .write(true)
        .create_new(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(&p)
        .map_err(|e| FsRootError::Io("open", e))?;
    if let Err(e) = apply_owner_only_dacl(&f).and_then(|()| verify_file_handle(&f, false)) {
        drop(f);
        let _ = fs::remove_file(&p);
        return Err(e);
    }
    Ok(f)
}

pub(crate) fn open_append(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = OpenOptions::new()
        .append(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(&p)
        .map_err(|e| FsRootError::Io("open", e))?;
    verify_file_handle(&f, false)?;
    Ok(f)
}

pub(crate) fn open_write(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = OpenOptions::new()
        .write(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(&p)
        .map_err(|e| FsRootError::Io("open", e))?;
    verify_file_handle(&f, false)?;
    Ok(f)
}

pub(crate) fn open_lock_file(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = OpenOptions::new()
        .access_mode(GENERIC_READ | GENERIC_WRITE | WRITE_DAC | READ_CONTROL)
        .write(true)
        .create(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(&p)
        .map_err(|e| FsRootError::Io("open", e))?;
    apply_owner_only_dacl(&f)?;
    verify_file_handle(&f, false)?;
    Ok(f)
}

pub(crate) fn mkdir_verified(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let mut created = false;
    match fs::create_dir(&p) {
        Ok(()) => created = true,
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
        Err(e) => return Err(FsRootError::Io("mkdir", e)),
    }
    let dir = OpenOptions::new()
        .access_mode(GENERIC_READ | GENERIC_WRITE | WRITE_DAC | READ_CONTROL)
        .share_mode(FILE_SHARE_READ | FILE_SHARE_WRITE)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS)
        .open(&p)
        .map_err(|e| FsRootError::Io("open", e))?;
    if created {
        apply_owner_only_dacl(&dir)?;
    }
    verify_file_handle(&dir, true)?;
    Ok(())
}

pub(crate) fn rename_in_root(
    root: &WinRoot,
    old_parts: &[&str],
    new_parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (old_p, _old_held) = resolve_verified_path(root, old_parts, opts)?;
    let (new_p, _new_held) = resolve_verified_path(root, new_parts, opts)?;
    fs::rename(old_p, new_p).map_err(|e| FsRootError::Io("rename", e))
}

pub(crate) fn remove_in_root(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let f = open_handle_no_reparse(&p, false)?;
    drop(f);
    fs::remove_file(p).map_err(|e| FsRootError::Io("unlink", e))
}

pub(crate) fn sync_dir_in_root(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let _dir = open_handle_no_reparse(&p, true)?;
    // As in `fsroot_windows.go`, Windows metadata updates on directory entries
    // are committed by NTFS transaction logs and directory `FlushFileBuffers`
    // can return `ERROR_ACCESS_DENIED` on read-only directory handles.
    Ok(())
}

pub(crate) fn read_dir_names_in_root(
    root: &WinRoot,
    parts: &[&str],
    opts: Options,
) -> Result<Vec<String>, FsRootError> {
    let (p, _held) = resolve_verified_path(root, parts, opts)?;
    let _dir = open_handle_no_reparse(&p, true)?;
    let rd = fs::read_dir(&p).map_err(|e| FsRootError::Io("read", e))?;
    let mut names = Vec::new();
    for entry in rd {
        let entry = entry.map_err(|e| FsRootError::Io("read", e))?;
        let name = entry
            .file_name()
            .into_string()
            .map_err(|_| FsRootError::Io("read", io::Error::from(io::ErrorKind::InvalidData)))?;
        names.push(name);
    }
    names.sort();
    Ok(names)
}

pub(crate) fn same_file(a: &File, b: &File) -> bool {
    // SAFETY: Both `a` and `b` are live `File` handles; `GetFileInformationByHandle`
    // populates stack-allocated `BY_HANDLE_FILE_INFORMATION` structs.
    unsafe {
        let mut ia: BY_HANDLE_FILE_INFORMATION = zeroed();
        let mut ib: BY_HANDLE_FILE_INFORMATION = zeroed();
        if GetFileInformationByHandle(a.as_raw_handle() as HANDLE, &mut ia) == 0 {
            return false;
        }
        if GetFileInformationByHandle(b.as_raw_handle() as HANDLE, &mut ib) == 0 {
            return false;
        }
        ia.dwVolumeSerialNumber == ib.dwVolumeSerialNumber
            && ia.nFileIndexHigh == ib.nFileIndexHigh
            && ia.nFileIndexLow == ib.nFileIndexLow
    }
}

pub(crate) fn try_lock_file(file: &File) -> Result<(), Option<io::Error>> {
    let raw = file.as_raw_handle() as HANDLE;
    // SAFETY: `raw` is a live file handle and `ol` is a zero-initialized stack `OVERLAPPED`
    // that is only read synchronously because `LOCKFILE_FAIL_IMMEDIATELY` is set.
    let ok = unsafe {
        let mut ol: OVERLAPPED = zeroed();
        LockFileEx(
            raw,
            LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY,
            0,
            1,
            0,
            &mut ol,
        ) != 0
    };
    if ok {
        return Ok(());
    }
    let err = io::Error::last_os_error();
    if let Some(code) = err.raw_os_error() {
        if code as u32 == ERROR_LOCK_VIOLATION || code as u32 == ERROR_IO_PENDING {
            return Err(None);
        }
    }
    let _ = null::<u8>();
    Err(Some(err))
}

pub(crate) fn unlock_file(file: &File) -> io::Result<()> {
    let raw = file.as_raw_handle() as HANDLE;
    // SAFETY: `raw` is a live file handle and `ol` is a zero-initialized stack `OVERLAPPED`.
    let ok = unsafe {
        let mut ol: OVERLAPPED = zeroed();
        UnlockFileEx(raw, 0, 1, 0, &mut ol) != 0
    };
    if ok {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}
