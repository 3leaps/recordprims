// SPDX-License-Identifier: MIT OR Apache-2.0

//! Safe Unix implementation of confined root operations, owner/mode and
//! filesystem-type verification, and `flock` advisory locking via `rustix`.

use std::fs::File;
use std::io;
use std::os::fd::{AsFd, OwnedFd};
use std::path::Path;

use rustix::fs::{
    fcntl_getfl, fcntl_setfl, flock, fstat, fstatfs, fsync, mkdirat, openat, renameat, statat,
    unlinkat, AtFlags, Dir, FileType, FlockOperation, Mode, OFlags, Stat, CWD,
};
use rustix::io::Errno;
use rustix::process::geteuid;

use crate::fsroot::{FsRootError, Options};

/// Open and verify the store root directory at `path`.
pub(crate) fn open_root_dir(path: &Path, opts: Options) -> Result<OwnedFd, FsRootError> {
    let st = statat(CWD, path, AtFlags::SYMLINK_NOFOLLOW)
        .map_err(|e| FsRootError::Io("open", e.into()))?;
    let ft = FileType::from_raw_mode(st.st_mode);
    if ft == FileType::Symlink {
        return Err(FsRootError::Symlink);
    }
    if ft != FileType::Directory {
        return Err(FsRootError::NotDir);
    }
    let flags = OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC;
    let fd = openat(CWD, path, flags, Mode::empty()).map_err(|e| {
        if e == Errno::LOOP {
            FsRootError::Symlink
        } else if e == Errno::NOTDIR {
            FsRootError::NotDir
        } else {
            FsRootError::Io("open", e.into())
        }
    })?;
    verify_fd(&fd, true, opts)?;
    check_fs_type(&fd, opts)?;
    Ok(fd)
}

fn verify_stat(st: &Stat, want_dir: bool, _opts: Options) -> Result<u64, FsRootError> {
    let ft = FileType::from_raw_mode(st.st_mode);
    if ft == FileType::Symlink {
        return Err(FsRootError::Symlink);
    }
    if want_dir {
        if ft != FileType::Directory {
            return Err(FsRootError::NotDir);
        }
    } else {
        if ft != FileType::RegularFile {
            return Err(FsRootError::NotRegular);
        }
        if st.st_nlink > 1 {
            return Err(FsRootError::MultipleLink);
        }
    }
    if st.st_uid != geteuid().as_raw() {
        return Err(FsRootError::Owner);
    }
    if (u32::from(st.st_mode) & 0o077) != 0 {
        return Err(FsRootError::Perm);
    }
    Ok(st.st_size as u64)
}

/// Verify that `fd` is owned by the effective UID, has no group/other
/// permissions (`mode & 0o077 == 0`), and is a directory (`want_dir == true`)
/// or single-link regular file (`want_dir == false`).
pub(crate) fn verify_fd<Fd: AsFd>(
    fd: Fd,
    want_dir: bool,
    opts: Options,
) -> Result<u64, FsRootError> {
    let st = fstat(fd).map_err(|e| FsRootError::Io("stat", e.into()))?;
    verify_stat(&st, want_dir, opts)
}

/// Clear `O_NONBLOCK` on an opened regular-file descriptor after `verify_fd`
/// has confirmed it is a regular file (never a FIFO).
fn clear_nonblock<Fd: AsFd>(fd: Fd, op: &'static str) -> Result<(), FsRootError> {
    let flags = fcntl_getfl(&fd).map_err(|e| FsRootError::Io(op, e.into()))?;
    if flags.contains(OFlags::NONBLOCK) {
        fcntl_setfl(&fd, flags & !OFlags::NONBLOCK).map_err(|e| FsRootError::Io(op, e.into()))?;
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn check_fs_type<Fd: AsFd>(fd: Fd, _opts: Options) -> Result<(), FsRootError> {
    let st = fstatfs(fd).map_err(|e| FsRootError::Io("stat", e.into()))?;
    let mut name = Vec::with_capacity(st.f_fstypename.len());
    for &c in &st.f_fstypename {
        if c == 0 {
            break;
        }
        name.push(c as u8);
    }
    if name == b"apfs" || name == b"hfs" {
        Ok(())
    } else {
        Err(FsRootError::FsType)
    }
}

#[cfg(target_os = "linux")]
fn check_fs_type<Fd: AsFd>(fd: Fd, opts: Options) -> Result<(), FsRootError> {
    const MAGIC_EXT4: u64 = 0xEF53;
    const MAGIC_XFS: u64 = 0x5846_5342;
    const MAGIC_BTRFS: u64 = 0x9123_683E;
    const MAGIC_TMPFS: u64 = 0x0102_1994;
    const MAGIC_OVERLAY: u64 = 0x794C_7630;

    let st = fstatfs(fd).map_err(|e| FsRootError::Io("stat", e.into()))?;
    let ftype = st.f_type as u64;
    match ftype {
        MAGIC_EXT4 | MAGIC_XFS | MAGIC_BTRFS => Ok(()),
        MAGIC_TMPFS if opts.allow_tmpfs => Ok(()),
        MAGIC_OVERLAY if opts.allow_overlay => Ok(()),
        _ => Err(FsRootError::FsType),
    }
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn check_fs_type<Fd: AsFd>(_fd: Fd, _opts: Options) -> Result<(), FsRootError> {
    // Other Unix systems are outside the estate platform matrix; refuse rather than guess.
    Err(FsRootError::FsType)
}

/// Walk intermediate directory components under `root_fd` using `O_NOFOLLOW | O_DIRECTORY`,
/// verifying each traversed directory, and return the parent directory `OwnedFd`
/// (or `None` when `parts.len() == 1`, meaning `root_fd` is the parent) and the leaf name.
fn resolve_parent<'a>(
    root_fd: &OwnedFd,
    parts: &'a [&'a str],
    opts: Options,
) -> Result<(Option<OwnedFd>, &'a str), FsRootError> {
    let (&leaf, parents) = parts.split_last().ok_or(FsRootError::Path)?;
    let mut cur: Option<OwnedFd> = None;
    for &comp in parents {
        let base = cur.as_ref().unwrap_or(root_fd);
        let st = statat(base, comp, AtFlags::SYMLINK_NOFOLLOW)
            .map_err(|e| FsRootError::Io("open", e.into()))?;
        let ft = FileType::from_raw_mode(st.st_mode);
        if ft == FileType::Symlink {
            return Err(FsRootError::Symlink);
        }
        if ft != FileType::Directory {
            return Err(FsRootError::NotDir);
        }
        let next = openat(
            base,
            comp,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .map_err(|e| map_traverse_errno("open", e))?;
        verify_fd(&next, true, opts)?;
        cur = Some(next);
    }
    Ok((cur, leaf))
}

/// Walk all directory components in `parts` (which may be empty for the root itself)
/// and return an `OwnedFd` to the target directory.
fn resolve_dir(root_fd: &OwnedFd, parts: &[&str], opts: Options) -> Result<OwnedFd, FsRootError> {
    let mut cur = openat(
        root_fd,
        ".",
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| FsRootError::Io("open", e.into()))?;
    verify_fd(&cur, true, opts)?;
    for &comp in parts {
        let st = statat(&cur, comp, AtFlags::SYMLINK_NOFOLLOW)
            .map_err(|e| FsRootError::Io("open", e.into()))?;
        let ft = FileType::from_raw_mode(st.st_mode);
        if ft == FileType::Symlink {
            return Err(FsRootError::Symlink);
        }
        if ft != FileType::Directory {
            return Err(FsRootError::NotDir);
        }
        let next = openat(
            &cur,
            comp,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .map_err(|e| map_traverse_errno("open", e))?;
        verify_fd(&next, true, opts)?;
        cur = next;
    }
    Ok(cur)
}

fn map_traverse_errno(op: &'static str, e: Errno) -> FsRootError {
    if e == Errno::LOOP {
        FsRootError::Symlink
    } else {
        FsRootError::Io(op, e.into())
    }
}

/// Check the leaf component with `statat(..., AT_SYMLINK_NOFOLLOW)` before
/// opening it so symlinks, FIFOs, sockets, and device nodes are refused
/// without ever blocking in `openat`.
fn check_final_leaf<Fd: AsFd>(
    base: Fd,
    leaf: &str,
    want_dir: bool,
    op: &'static str,
) -> Result<Stat, FsRootError> {
    let st =
        statat(base, leaf, AtFlags::SYMLINK_NOFOLLOW).map_err(|e| FsRootError::Io(op, e.into()))?;
    let ft = FileType::from_raw_mode(st.st_mode);
    if ft == FileType::Symlink {
        return Err(FsRootError::Symlink);
    }
    if want_dir {
        if ft != FileType::Directory {
            return Err(FsRootError::NotDir);
        }
    } else if ft != FileType::RegularFile {
        return Err(FsRootError::NotRegular);
    }
    Ok(st)
}

pub(crate) fn stat_verified(
    root_fd: &OwnedFd,
    parts: &[&str],
    want_dir: bool,
    opts: Options,
) -> Result<u64, FsRootError> {
    if parts.is_empty() {
        return verify_fd(root_fd, want_dir, opts);
    }
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    let st = check_final_leaf(base, leaf, want_dir, "stat")?;
    verify_stat(&st, want_dir, opts)
}

pub(crate) fn open_verified_read(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<(File, u64), FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    check_final_leaf(base, leaf, false, "open")?;
    let fd = openat(
        base,
        leaf,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| map_traverse_errno("open", e))?;
    let size = verify_fd(&fd, false, opts)?;
    clear_nonblock(&fd, "open")?;
    Ok((File::from(fd), size))
}

pub(crate) fn create_exclusive(
    root_fd: &OwnedFd,
    parts: &[&str],
    mode: u32,
    opts: Options,
) -> Result<File, FsRootError> {
    if (mode & 0o077) != 0 {
        return Err(FsRootError::Perm);
    }
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    let fd = openat(
        base,
        leaf,
        OFlags::RDWR | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::from_raw_mode(mode as _),
    )
    .map_err(|e| map_traverse_errno("open", e))?;
    verify_fd(&fd, false, opts)?;
    Ok(File::from(fd))
}

pub(crate) fn open_append(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    check_final_leaf(base, leaf, false, "open")?;
    let fd = openat(
        base,
        leaf,
        OFlags::WRONLY | OFlags::APPEND | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| map_traverse_errno("open", e))?;
    verify_fd(&fd, false, opts)?;
    clear_nonblock(&fd, "open")?;
    Ok(File::from(fd))
}

pub(crate) fn open_write(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    check_final_leaf(base, leaf, false, "open")?;
    let fd = openat(
        base,
        leaf,
        OFlags::WRONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| map_traverse_errno("open", e))?;
    verify_fd(&fd, false, opts)?;
    clear_nonblock(&fd, "open")?;
    Ok(File::from(fd))
}

pub(crate) fn open_lock_file(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<File, FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    match statat(base, leaf, AtFlags::SYMLINK_NOFOLLOW) {
        Ok(st) => {
            let ft = FileType::from_raw_mode(st.st_mode);
            if ft == FileType::Symlink {
                return Err(FsRootError::Symlink);
            }
            if ft != FileType::RegularFile {
                return Err(FsRootError::NotRegular);
            }
        }
        Err(Errno::NOENT) => {}
        Err(e) => return Err(FsRootError::Io("open", e.into())),
    }
    let fd = openat(
        base,
        leaf,
        OFlags::RDWR | OFlags::CREATE | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::from_raw_mode(0o600),
    )
    .map_err(|e| map_traverse_errno("open", e))?;
    verify_fd(&fd, false, opts)?;
    clear_nonblock(&fd, "open")?;
    Ok(File::from(fd))
}

pub(crate) fn mkdir_verified(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    match mkdirat(base, leaf, Mode::from_raw_mode(0o700)) {
        Ok(()) => {}
        Err(Errno::EXIST) => {}
        Err(e) => return Err(FsRootError::Io("mkdir", e.into())),
    }
    check_final_leaf(base, leaf, true, "open")?;
    let dir_fd = openat(
        base,
        leaf,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| {
        if e == Errno::LOOP {
            FsRootError::Symlink
        } else if e == Errno::NOTDIR {
            FsRootError::NotDir
        } else {
            FsRootError::Io("open", e.into())
        }
    })?;
    verify_fd(&dir_fd, true, opts)?;
    Ok(())
}

pub(crate) fn rename_in_root(
    root_fd: &OwnedFd,
    old_parts: &[&str],
    new_parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (old_parent, old_leaf) = resolve_parent(root_fd, old_parts, opts)?;
    let (new_parent, new_leaf) = resolve_parent(root_fd, new_parts, opts)?;
    let old_base = old_parent.as_ref().unwrap_or(root_fd);
    let new_base = new_parent.as_ref().unwrap_or(root_fd);
    renameat(old_base, old_leaf, new_base, new_leaf)
        .map_err(|e| FsRootError::Io("rename", e.into()))
}

pub(crate) fn remove_in_root(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let (parent, leaf) = resolve_parent(root_fd, parts, opts)?;
    let base = parent.as_ref().unwrap_or(root_fd);
    let st = statat(base, leaf, AtFlags::SYMLINK_NOFOLLOW)
        .map_err(|e| FsRootError::Io("unlink", e.into()))?;
    let ft = FileType::from_raw_mode(st.st_mode);
    if ft == FileType::Symlink {
        return Err(FsRootError::Symlink);
    }
    if ft != FileType::RegularFile && ft != FileType::Directory {
        return Err(FsRootError::NotRegular);
    }
    unlinkat(base, leaf, AtFlags::empty()).map_err(|e| FsRootError::Io("unlink", e.into()))
}

pub(crate) fn sync_dir_in_root(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<(), FsRootError> {
    let dir_fd = resolve_dir(root_fd, parts, opts)?;
    fsync(&dir_fd).map_err(|e| FsRootError::Io("fsync_dir", e.into()))
}

pub(crate) fn read_dir_names_in_root(
    root_fd: &OwnedFd,
    parts: &[&str],
    opts: Options,
) -> Result<Vec<String>, FsRootError> {
    let dir_fd = resolve_dir(root_fd, parts, opts)?;
    let dir = Dir::read_from(&dir_fd).map_err(|e| FsRootError::Io("read", e.into()))?;
    let mut names = Vec::new();
    for entry in dir {
        let entry = entry.map_err(|e| FsRootError::Io("read", e.into()))?;
        let name_bytes = entry.file_name().to_bytes();
        if name_bytes == b"." || name_bytes == b".." {
            continue;
        }
        let name = std::str::from_utf8(name_bytes)
            .map_err(|_| FsRootError::Io("read", io::Error::from(io::ErrorKind::InvalidData)))?;
        names.push(name.to_owned());
    }
    names.sort();
    Ok(names)
}

pub(crate) fn same_file(a: &File, b: &File) -> bool {
    let Ok(sa) = fstat(a) else {
        return false;
    };
    let Ok(sb) = fstat(b) else {
        return false;
    };
    sa.st_dev == sb.st_dev && sa.st_ino == sb.st_ino
}

/// Attempt a non-blocking exclusive `flock` on `file`.
pub(crate) fn try_lock_file(file: &File) -> Result<(), Option<io::Error>> {
    match flock(file, FlockOperation::NonBlockingLockExclusive) {
        Ok(()) => Ok(()),
        #[allow(unreachable_patterns)]
        Err(Errno::WOULDBLOCK | Errno::AGAIN) => Err(None),
        Err(e) => Err(Some(e.into())),
    }
}

/// Release an `flock` on `file`.
pub(crate) fn unlock_file(file: &File) -> io::Result<()> {
    flock(file, FlockOperation::Unlock).map_err(Into::into)
}
