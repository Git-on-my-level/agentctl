//go:build linux

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func processStart(pid int) processObservation {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return processObservation{}
		}
		return processObservation{err: err}
	}
	text := string(data)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return processObservation{present: true, err: errors.New("unreadable process stat")}
	}
	fields := strings.Fields(text[end+2:])
	// Field 22 is starttime in clock ticks. Field 3 is the first token after comm.
	if len(fields) < 20 {
		return processObservation{present: true, err: errors.New("short process stat")}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return processObservation{present: true, err: err}
	}
	boot, err := linuxBootTime()
	if err != nil {
		return processObservation{present: true, err: err}
	}
	clk, err := unix.Sysconf(unix.SC_CLK_TCK)
	if err != nil || clk <= 0 {
		return processObservation{present: true, err: errors.New("clock ticks unavailable")}
	}
	seconds := ticks / clk
	nanos := (ticks % clk) * int64(time.Second) / clk
	return processObservation{present: true, startKnown: true, started: time.Unix(boot+seconds, nanos).UTC()}
}

func linuxBootTime() (int64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, errors.New("btime missing")
}
