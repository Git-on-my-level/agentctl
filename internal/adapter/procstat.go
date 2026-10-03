package adapter

import (
	"errors"
	"strconv"
	"strings"
)

// ProcStartTicks reads field 22 of /proc/<pid>/stat (starttime) in clock ticks.
// The comm field may contain spaces and parentheses, so parsing starts after
// the last ')'.
func ProcStartTicks(stat []byte) (int64, error) {
	fields, err := procStatFields(stat)
	if err != nil {
		return 0, err
	}
	if len(fields) < 20 {
		return 0, errors.New("short process stat")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || ticks < 0 {
		return 0, errors.New("invalid starttime")
	}
	return ticks, nil
}

// ProcParentPID reads the ppid field of /proc/<pid>/stat.
func ProcParentPID(stat []byte) (int, error) {
	fields, err := procStatFields(stat)
	if err != nil {
		return 0, err
	}
	if len(fields) < 2 {
		return 0, errors.New("short process stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid <= 0 {
		return 0, errors.New("invalid ppid")
	}
	return ppid, nil
}

func procStatFields(stat []byte) ([]string, error) {
	text := string(stat)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return nil, errors.New("unreadable process stat")
	}
	return strings.Fields(text[end+2:]), nil
}
