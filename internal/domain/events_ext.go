package domain

// ExtensionEventEmitters lists event types the service emits that the API contract does not list yet.
// Each is an additive contract change recorded in implementation-notes.md; once the root contract lists
// the type, contractgen emits it and its entry (and constant) here is removed. Known accepts them.
// Empty since contract 1.2.0 listed notification.test_requested (CD-3).
var ExtensionEventEmitters = map[EventType][]string{}
