package actionsyslog

import "log/syslog"

// sendLocal writes one err-level message per line to the local syslog daemon.
func sendLocal(facility int, tag string, lines []string) error {
	w, err := syslog.New(syslog.Priority(facility*8)|syslog.LOG_ERR, tag)
	if err != nil {
		return err
	}
	defer w.Close()
	for _, l := range lines {
		if err := w.Err(l); err != nil {
			return err
		}
	}
	return nil
}
