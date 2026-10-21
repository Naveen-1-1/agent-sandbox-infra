package agent

import (
	"testing"
	"time"
)

func TestTerminalCommandsExpireWithoutReports(t *testing.T) {
	a := NewActivity(nil)
	now := time.Now()
	tm := newTerminals(a)
	tm.now = func() time.Time { return now }
	tm.start("t1", "npm run watch")
	tm.start("t2", "make")
	now = now.Add(time.Minute)
	tm.alive([]string{"t1"})
	now = now.Add(90 * time.Second)
	tm.prune()
	cmds := a.Signals().GetRunningCommands()
	if len(cmds) != 1 || cmds[0].GetCommand() != "npm run watch" {
		t.Fatalf("running = %v, want only the one still reported", cmds)
	}
}
