package engine

import "github.com/xtls/xray-core/common/log"

// Until a core's log app takes over, Xray logs every severity, debug included, to stdout
// (logcat on Android). A ping core has no log app, so a probe batch in a process without a
// tunnel core would log every dial; the process's default logs warnings and errors instead.
func init() {
	log.RegisterHandler(warningsOnly{log.NewLogger(log.CreateStdoutLogWriter())})
}

// warningsOnly passes general messages of warning severity or worse, and every other kind.
type warningsOnly struct{ log.Handler }

func (h warningsOnly) Handle(message log.Message) {
	if general, ok := message.(*log.GeneralMessage); ok && general.Severity > log.Severity_Warning {
		return
	}
	h.Handler.Handle(message)
}

// Severity lets Xray skip formatting what Handle would drop.
func (warningsOnly) Severity() log.Severity { return log.Severity_Warning }
