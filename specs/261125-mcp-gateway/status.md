# Status: governed MCP servers (1.3.0)

| Phase | State |
|---|---|
| 0 Research | Complete (root PRD, verified against MCP 2026-07-28) |
| 1 Specification | Complete (spec.md; root contracts merged in root PR #29) |
| 2 Research and data modeling | Complete (database contract §7.5) |
| 3 Architecture and planning | Complete (plan.md, architecture.md) |
| 4 Task breakdown | Complete (tasks.md) |
| 5 Implementation | In progress: T1, T2, T3, T4, T7a, T8a, T8c done |
| 6 Completion | Not started |

## Deferred (no AWS environment yet, J 2026-11-25)

Real EKS rollout of hosted servers, IdP federation against Entra or Okta, image signature admission policy, and load
tests at PRD Appendix F scale. Fakes and local tests stand in until then.
