package engine

import (
	"errors"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/xtls/libxray/memory"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/stats"
	_ "github.com/xtls/xray-core/main/distro/all"
)

var (
	// ErrAlreadyRunning is libXray's own message, so callers see the same refusal as before.
	ErrAlreadyRunning = errors.New("xray is already running")
	ErrNotRunning     = errors.New("xray is not running")
	ErrNoCounters     = errors.New(
		"the running core keeps no traffic counters: its config needs stats and the policy's stats flags")
)

// Counters are a core's traffic byte counters by kind (inbound, outbound, user), then tag or
// user, then direction (uplink, downlink): the shape of the stats in Xray's /debug/vars.
type Counters map[string]map[string]map[string]int64

// tunnelCore is the one core a process runs for its tunnel, started as libXray's runXray starts it.
type tunnelCore struct {
	mu     sync.Mutex
	server *core.Instance
}

var tunnel = &tunnelCore{}

func (s *tunnelCore) run(xrayJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return ErrAlreadyRunning
	}
	memory.InitForceFree()
	config, err := core.LoadConfig("json", strings.NewReader(xrayJSON))
	if err != nil {
		return err
	}
	server, err := startCore(config, nil)
	if err != nil {
		return err
	}
	s.server = server
	debug.FreeOSMemory()
	return nil
}

func (s *tunnelCore) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	err := closeCore(s.server)
	s.server = nil
	return err
}

func (s *tunnelCore) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server != nil && s.server.IsRunning()
}

// counters reads every traffic counter of the running core, in process: no listener needed.
func (s *tunnelCore) counters() (Counters, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil, ErrNotRunning
	}
	manager, ok := s.server.GetFeature(stats.ManagerType()).(stats.Manager)
	if !ok {
		return nil, ErrNoCounters
	}
	counters := Counters{"inbound": {}, "outbound": {}, "user": {}}
	found := false
	manager.VisitCounters(func(name string, counter stats.Counter) bool {
		// kind>>>tag>>>traffic>>>direction, as app/proxyman and app/stats name them.
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[2] != "traffic" {
			return true
		}
		kind, tag, direction := parts[0], parts[1], parts[3]
		if counters[kind] == nil {
			counters[kind] = map[string]map[string]int64{}
		}
		if counters[kind][tag] == nil {
			counters[kind][tag] = map[string]int64{}
		}
		counters[kind][tag][direction] = counter.Value()
		found = true
		return true
	})
	if !found {
		return nil, ErrNoCounters
	}
	return counters, nil
}
