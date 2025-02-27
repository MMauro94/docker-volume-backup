// Copyright 2022 - Offen Authors <hioffen@posteo.de>
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"sync"
	"time"
)

// ContainersStats stats about the docker containers
type ContainersStats struct {
	All        uint
	ToStop     uint
	Stopped    uint
	StopErrors uint
}

// BackupFileStats stats about the created backup file
type BackupFileStats struct {
	Name     string
	FullPath string
	Size     uint64
}

// StorageStats stats about the status of an archival directory
type StorageStats struct {
	Total       uint
	Pruned      uint
	PruneErrors uint
}

// Stats global stats regarding script execution
type Stats struct {
	sync.Mutex
	StartTime  time.Time
	EndTime    time.Time
	TookTime   time.Duration
	LockedTime time.Duration
	LogOutput  *bytes.Buffer
	Containers ContainersStats
	BackupFile BackupFileStats
	Storages   map[string]StorageStats
}
