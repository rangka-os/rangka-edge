# rangka-edge: notes for AI assistants

- **Owner:** Dede (same owner as rangka-os and rangka-cloud).
- **Scope:** only edit files in this repo. Never edit sibling repos (`../rangka-os`, `../rangka-apps`, `../rangka-connectors`, `../rangka-ai`), even if they are cloned next to this one.
- **Contracts:** if a change needs the kernel, the sync protocol, the connector schema or a shared package to change, write a proposal in `rangka-os/docs/contracts/NNNN-title.md` (in a PR to rangka-os). Don't work around it here. See `rangka-os/docs/contracts/0000-how-contracts-change.md`.
- **Pinning:** build against a released `rangka-os` tag, never its `main`.
- **Rules of the product:**
  - Deterministic only: no LLM in the device data path.
  - Every queued item has an idempotency key.
  - Offline first: the outlet must keep selling and printing without internet.
- **Language:** Go 1.24. Code, comments and docs in English. UI text shown to cashiers in Bahasa Indonesia.
- **Workflow:** one task = one branch = one git worktree. Keep PRs small. Run `go vet ./... && go test ./...` before opening one.
