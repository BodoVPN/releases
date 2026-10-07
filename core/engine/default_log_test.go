package engine

import (
	"testing"

	"github.com/xtls/xray-core/common/log"
)

type collected struct{ messages []log.Message }

func (c *collected) Handle(message log.Message) { c.messages = append(c.messages, message) }

func TestTheDefaultLogKeepsWarningsAndErrorsOnly(t *testing.T) {
	sink := &collected{}
	handler := warningsOnly{sink}
	for _, severity := range []log.Severity{log.Severity_Debug, log.Severity_Info, log.Severity_Warning, log.Severity_Error} {
		handler.Handle(&log.GeneralMessage{Severity: severity, Content: "x"})
	}
	handler.Handle(&log.AccessMessage{})
	if len(sink.messages) != 3 {
		t.Fatalf("kept %d messages, want the warning, the error and the access line", len(sink.messages))
	}
	if handler.Severity() != log.Severity_Warning {
		t.Fatalf("severity = %v, want warning, so Xray skips formatting the rest", handler.Severity())
	}
}
