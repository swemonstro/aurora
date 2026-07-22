package linuxprocess

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type procStat struct {
	PID        uint64
	Comm       string
	State      byte
	ParentPID  uint64
	GroupID    uint64
	SessionID  uint64
	TTY        int64
	StartTicks uint64
}

// parseProcStat parses /proc/[pid]/stat without splitting the parenthesized
// comm field. The closing delimiter is located from the right so spaces and
// parentheses inside comm remain valid.
func parseProcStat(data []byte) (procStat, error) {
	line := strings.TrimSpace(string(data))
	open := strings.IndexByte(line, '(')
	close := strings.LastIndex(line, ")")
	if open <= 0 || close <= open || close+2 >= len(line) {
		return procStat{}, fmt.Errorf("%w: missing pid or comm delimiters", ErrMalformedStat)
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(line[:open]), 10, 64)
	if err != nil || pid == 0 {
		return procStat{}, fmt.Errorf("%w: invalid pid", ErrMalformedStat)
	}
	comm := line[open+1 : close]
	if comm == "" {
		return procStat{}, fmt.Errorf("%w: empty comm", ErrMalformedStat)
	}
	fields := strings.Fields(line[close+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("%w: insufficient fields", ErrMalformedStat)
	}
	parent, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: invalid parent pid", ErrMalformedStat)
	}
	group, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: invalid process group", ErrMalformedStat)
	}
	session, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: invalid session", ErrMalformedStat)
	}
	tty, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: invalid tty", ErrMalformedStat)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: invalid start ticks", ErrMalformedStat)
	}
	return procStat{
		PID: pid, Comm: comm, State: fields[0][0], ParentPID: parent,
		GroupID: group, SessionID: session, TTY: tty, StartTicks: start,
	}, nil
}

func parseBootTime(data []byte) (time.Time, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, fmt.Errorf("%w: malformed btime", ErrInvalidBootTime)
		}
		return time.Unix(seconds, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: btime missing", ErrInvalidBootTime)
}

func startedAt(bootTime time.Time, startTicks, clockTicks uint64) time.Time {
	seconds := startTicks / clockTicks
	remainder := startTicks % clockTicks
	nanoseconds := remainder * uint64(time.Second) / clockTicks
	return bootTime.Add(time.Duration(seconds)*time.Second + time.Duration(nanoseconds)).UTC()
}
