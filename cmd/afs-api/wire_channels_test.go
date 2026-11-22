package main

import "time"

// The authorization matrix has no reconciler: TestNotificationChannel must not wait the full 10 s.
func init() { channelTestWait = 20 * time.Millisecond }
