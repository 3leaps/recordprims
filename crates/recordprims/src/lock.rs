// SPDX-License-Identifier: MIT OR Apache-2.0

//! Store-wide cross-process and cross-handle advisory lock (`internal/lock` parity).

use std::fs::File;
use std::io;
use std::thread;
use std::time::{Duration, Instant};

use crate::sys::imp;

/// Poll interval when waiting for a lock with a positive timeout.
const POLL_INTERVAL: Duration = Duration::from_millis(20);

/// Guard that releases the advisory lock on an open `File` when dropped or explicitly released.
pub struct FileLock<'a> {
    file: &'a File,
    released: bool,
}

impl<'a> FileLock<'a> {
    /// Acquire the advisory lock on `file`, retrying up to `timeout` when held by another handle/process.
    pub fn acquire(file: &'a File, timeout: Duration) -> Result<Self, Option<io::Error>> {
        let deadline = Instant::now() + timeout;
        loop {
            match imp::try_lock_file(file) {
                Ok(()) => {
                    return Ok(Self {
                        file,
                        released: false,
                    })
                }
                Err(None) => {
                    if timeout.is_zero() || Instant::now() >= deadline {
                        return Err(None);
                    }
                    thread::sleep(POLL_INTERVAL);
                }
                Err(Some(e)) => return Err(Some(e)),
            }
        }
    }

    /// Release the lock explicitly.
    pub fn release(mut self) -> io::Result<()> {
        self.released = true;
        imp::unlock_file(self.file)
    }
}

impl Drop for FileLock<'_> {
    fn drop(&mut self) {
        if !self.released {
            let _ = imp::unlock_file(self.file);
        }
    }
}
