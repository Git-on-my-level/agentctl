//go:build !linux

package main

import (
	"errors"
	"time"
)

func admissionProcessStart(pid int) (string, error) {
	obs := processStart(pid)
	if obs.err != nil {
		return "", obs.err
	}
	if !obs.present {
		return "", nil
	}
	if !obs.startKnown {
		return "", errors.New("process start unavailable")
	}
	return obs.started.UTC().Format(time.RFC3339Nano), nil
}

func admissionLiveChildStart(pid int) (string, error) {
	return admissionProcessStart(pid)
}
