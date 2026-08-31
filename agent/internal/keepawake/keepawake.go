// Package keepawake stops the machine from idle-sleeping while a backup
// is running.
//
// Without this, a laptop that goes to sleep mid-upload doesn't pause the
// backup and resume it later - the network connection drops outright, and
// because a connection drop isn't treated as a permission failure (see
// backupjob.isDiskAccessDenied, deliberately scoped to real access
// denials, not transient disconnects), the run can end up logged as
// finished with a pile of skipped files rather than as the interrupted
// backup it actually was. Preventing the sleep in the first place is a
// more honest fix than trying to guess, after the fact, whether a given
// batch of connection errors means "the machine slept" or "the server
// restarted".
//
// Start returns a stop function; call it (once, via defer) when the
// backup finishes so the machine can sleep normally again the rest of the
// time. This only prevents *idle* sleep - the system deciding on its own
// that nothing is happening. It does not override a user explicitly
// closing a laptop lid or choosing "mettre en veille" by hand, which both
// platforms treat as an unconditional instruction to sleep regardless of
// what's running.
package keepawake
