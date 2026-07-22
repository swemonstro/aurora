package linuxprocess

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseProcStatHandlesSpacesAndParenthesesInComm(t *testing.T) {
	line := statLine(321, "odd (worker) name !", 12, 20, 30, 40, 12345)
	got, err := parseProcStat([]byte(line))
	if err != nil {
		t.Fatalf("parseProcStat() error = %v", err)
	}
	if got.PID != 321 || got.Comm != "odd (worker) name !" || got.ParentPID != 12 ||
		got.GroupID != 20 || got.SessionID != 30 || got.TTY != 40 || got.StartTicks != 12345 {
		t.Fatalf("parseProcStat() = %#v", got)
	}
}

func TestParseProcStatRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		"", "123 no-parentheses R 1 2 3", "123 (name R 1 2 3",
		"not-a-pid (name) R 1 2 3", "123 (name) R too-few",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseProcStat([]byte(input)); !errors.Is(err, ErrMalformedStat) {
				t.Fatalf("parseProcStat(%q) error = %v, want %v", input, err, ErrMalformedStat)
			}
		})
	}
}

func TestStartedAtUsesBootBoundTicks(t *testing.T) {
	boot := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	if got, want := startedAt(boot, 250, 100), boot.Add(2500*time.Millisecond); !got.Equal(want) {
		t.Fatalf("startedAt() = %s, want %s", got, want)
	}
}

func statLine(pid uint64, comm string, ppid, group, session uint64, tty int64, start uint64) string {
	fields := make([]string, 23)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0] = "R"
	fields[1] = strconv.FormatUint(ppid, 10)
	fields[2] = strconv.FormatUint(group, 10)
	fields[3] = strconv.FormatUint(session, 10)
	fields[4] = strconv.FormatInt(tty, 10)
	fields[19] = strconv.FormatUint(start, 10)
	return fmt.Sprintf("%d (%s) %s", pid, comm, strings.Join(fields, " "))
}
