//go:build !darwin && !linux

package store

import "go.etcd.io/bbolt"

func configureOpenLock(*bbolt.Options) {}
