// Package engine is the reconciler's control loop (RC-4, RC-5): it turns change notifications, a
// 30 s poll of stale generations and a full resync into a work queue keyed by (kind, id), runs the
// controller registered for each kind, writes the outcome to resource_status through a port and
// retries failures with capped exponential backoff. It knows neither Kubernetes nor AWS: controllers
// and adapters are plugged in through the interfaces in this package.
package engine
