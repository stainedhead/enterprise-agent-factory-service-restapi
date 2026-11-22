# Contributing to enterprise-agent-factory-service-restapi

Start with the product-wide
[contributing guide](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/CONTRIBUTING.md);
this file adds what is specific to the control plane. The full rules are in [AGENTS.md](AGENTS.md).

## Before you start

- Read this repository's [PRD](enterprise-agent-factory-service-restapi-PRD.md), the
  [API contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/api-contract.md)
  and the root [solution design](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/solution-design.md).
- Name the layers you will touch: `domain`, `usecase`, `adapter/*`, `cmd/*`. Dependencies point inward;
  `domain` and `usecase` never import HTTP, SQL, pgx, client-go or the AWS SDK.
- Create or update a spec in `specs/YYMMDD-<feature>/` and keep its `status.md` current.

## Make the change

1. **Red, Green, Refactor**, table-driven. Every authorization rule gets a denied-case test.
2. **API changes** land in [`api/openapi.yaml`](api/openapi.yaml) in the same PR as the handler. Then tell
   `-website` (SDK) and `-cli` to re-pin the contract.
3. **Schema changes** never happen here. Ask `-db` for a migration and adopt its tag in `go.mod`.
4. Update `user-docs/` for any behavior a caller or operator sees, and `docs/` for architectural changes.

Design review before every commit:

- [ ] No adapter, SQL, HTTP or AWS import in `domain` or `usecase`
- [ ] Every new route declares a permission and has allowed and denied tests per role
- [ ] No secret value stored, returned, logged or put in an error message
- [ ] Mutations write audit (and, where the contract has an event, outbox) in the same transaction
- [ ] Context passed through; no `context.Background()` below `cmd/`
- [ ] No hardcoded account ids, ARNs, regions, endpoints or credentials

## Run the gate

```bash
make gate                                     # tidy, fmt, vet, contract checks, lint, race tests, build
AFS_TEST_DATABASE_URL=postgres://... make test-db
make cover                                    # domain, usecase, authz >= 90 %
```

## Open a pull request

Branch from `main`, reference the spec directory in your commits, and say which permissions, events and
contract operations changed. CI must be green.
