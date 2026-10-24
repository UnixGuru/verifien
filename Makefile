FILES = actions.go funcs.go include.go main.go action_email/email.go action_logfile/logfile.go action_opentel/opentel.go action_script/proc.go action_script/script.go action_stdout/stdout.go action_syslog/local.go action_syslog/syslog.go action_webhook/webhook.go disk/disk.go env/environ.go inode/stat.go metrics/prometheus.go shell/cmd.go spec/action.go spec/attempt.go spec/check.go spec/owner.go spec/perm.go spec/spec.go text/sed.go
# .DELETE_ON_ERROR: verifien
verifien : go.mod go.sum ${FILES} vendor/modules.txt
	go build -o verifien -mod vendor
	chmod 755 verifien

go.mod :
	rm -fr go.mod go.sum vendor
	go mod init verifien

go.sum : go.mod
	rm -fr go.sum vendor
	go mod tidy

vendor vendor/modules.txt : go.sum go.mod
	rm -fr vendor
	go mod vendor

.PHONY: clean
clean:
	rm -fr cover.out verifien vendor go.sum go.mod

