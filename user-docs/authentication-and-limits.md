# Authentication, request limits and CORS

## Signing in

`GET /v1/sign-in-providers` is public and lists every enabled identity provider with the client ids a
console or CLI needs (`consoleClientId`, `cliClientId`, `isDefault`). It never returns a client secret.

Send a bearer token on every other call:

- **Identity provider token (JWT).** Signed with an asymmetric algorithm (RS, PS, ES or EdDSA). The
  issuer and audience must match an enabled provider. Keys come from the provider's JWKS, cached for
  10 minutes and refetched once when an unknown `kid` appears. If the provider is unreachable, cached
  keys keep working; with none cached the API answers `503 Unavailable`. Clock skew up to 30 seconds is tolerated.
- **API key.** A value starting with `afsk_`. Expired or deleted keys answer `401`.

A missing, expired or unknown credential answers `401 Unauthenticated`. `GET /v1/me` returns the caller,
the bound teams, the role in each and the permissions, for display only; the API makes the real decision on each call.

## Configuration

| Setting | Default | Meaning |
|---|---|---|
| `AFS_API_REPLICAS` | `1` | Number of `afs-api` replicas. The per-principal rate limit is divided by this. |
| `AFS_TRUSTED_PROXY_HOPS` | `0` | Number of trusted proxies in front of the API, used to find the client address in `X-Forwarded-For` for per-address limits. |
| `api.cors_origins` (service setting) | empty | Browser origins allowed to call the API. Cached for 30 seconds. |

## Limits

Each principal may make 20 requests per second with a burst of 40, divided across replicas. Requests
without a valid credential, and public routes, are limited per source address at the same rate. Over
the limit the API answers `429 Throttled` with `Retry-After`.

## Retries and concurrency

- Creating `POST`s and `:verb` actions accept `Idempotency-Key` (1 to 255 characters). A repeat with the same
  body returns the first response with `Idempotent-Replayed: true`; a different body answers `409`.
- Updates need `If-Match` with the `ETag` from the last read: missing answers `428`, stale answers `412`.
