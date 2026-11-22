package domain

// ExtensionEventEmitters lists event types the service emits that API contract 1.1.0 does not list yet.
// Each is an additive contract change recorded in implementation-notes.md (CD-3); once the root
// contract lists the type, contractgen emits it and its entry here is removed. Known accepts them.
var ExtensionEventEmitters = map[EventType][]string{
	// EventNotificationTestRequested is written by afs-api when a person runs TestNotificationChannel
	// (OQ-S1); the reconciler delivers one test message and records the result on the channel.
	EventNotificationTestRequested: {"api"},
}

// EventNotificationTestRequested is "notification.test_requested" (data: channelId, testId).
const EventNotificationTestRequested EventType = "notification.test_requested"
