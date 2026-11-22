# Data dictionary

Tables and the ten `afs_edge` SECURITY DEFINER functions are owned by `-db` (migration 000020, see its `docs/data-model.md`).
Edge-side values: session id (256-bit random, only SHA-256 stored), sealed sign-in state (AES-256-GCM, two keys), one-time handoff code, cookie `__Host-afs-collab`, access counts (5-minute buckets, flush 30 s, <=1000 rows per call, buffer cap 100,000 keys).
Metrics: afs_edge_requests_total, afs_edge_request_duration_seconds, afs_edge_route_refresh_failures_total, afs_edge_access_counts_dropped_total.
