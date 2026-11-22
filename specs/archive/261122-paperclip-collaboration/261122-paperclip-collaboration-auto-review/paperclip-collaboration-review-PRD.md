# Review PRD: Paperclip collaboration

Findings from the code and design review (checklist in AGENTS.md).

| # | Sev | Finding | Disposition |
|---|---|---|---|
| 1 | P0 | CI envtest refused selector-less Services | Fixed (8b06a35) |
| 2 | P2 | Edge server had no MaxHeaderBytes (default 1 MiB) | Fixed (64 KiB) |
| 3 | P2 | Paperclip client, RC-25/26, PaperclipReady are ports only | Parked |
| 4 | P2 | Sealing key rotation is manual | Parked, documented |
| 5 | P2 | RA-21 report kind, member sync, export/import | Parked |

Checked clean: no context.Background below cmd (edge), no cookie/token logging in edge code, cookies `__Host-`, Secure, HttpOnly, no secret values stored.
