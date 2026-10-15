# Task 04: scale the shop for the launch

**Expected route:** `infra-two-people` → `multi_approve` with 2 approvers
(exit 4, `pending_human`). Two people approve; then
`wayglass-land-when-approved` lands it.

Marketing expects five times the usual traffic next week. In
`infra/wrangler.jsonc`:

- raise the container's `max_instances` from 2 to 10 and move it from
  `basic` to the `standard` instance type;
- add `"observability": { "enabled": true, "head_sampling_rate": 0.25 }`
  in place of the current observability block, so logs stay affordable at
  the higher volume.

In `infra/Dockerfile`, nothing changes. In `docs/architecture.md`, update
the last paragraph to say the shop scales to ten container instances.

```bash
wayglass intent declare --json \
  --goal "chore(infra): scale to 10 standard instances for the launch" \
  --reason "Launch week traffic is expected at five times normal; 2 basic instances will not hold" \
  --path "infra/**" --path "docs/architecture.md"
```

No Go tests are affected; do not run `wrangler`.
