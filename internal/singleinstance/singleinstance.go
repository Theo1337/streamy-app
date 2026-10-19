// Package singleinstance makes sure only one tray process runs at a time.
//
// Two copies of Streamy would fight over the same global hotkey (the second
// registration fails) and would put two identical icons in the notification
// area, which is confusing. So the process takes a machine wide lock at
// startup and exits quietly if it cannot get it.
package singleinstance

// Acquire takes the single instance lock. It returns a release function (never
// nil, safe to call twice) and whether this process won the lock.
func Acquire() (release func(), ok bool) {
	return acquire()
}
