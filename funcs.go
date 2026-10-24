package main

// Each folder registers itself from init() (see package spec), so importing the
// folder is all that is needed. Add a line here when you create a new folder.
// Adding a function or action to an existing folder needs no change here.
import (
	// functions: what a section checks
	_ "verifien/disk"
	_ "verifien/env"
	_ "verifien/inode"
	_ "verifien/metrics"
	_ "verifien/shell"
	_ "verifien/text"

	// actions: what a section does when a ck fails
	_ "verifien/action_email"
	_ "verifien/action_logfile"
	_ "verifien/action_opentel"
	_ "verifien/action_script"
	_ "verifien/action_stdout"
	_ "verifien/action_syslog"
	_ "verifien/action_webhook"
)
