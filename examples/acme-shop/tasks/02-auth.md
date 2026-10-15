# Task 02: lock accounts after repeated failed sign-ins

**Expected route:** `auth-boundary` → `human_approve` (exit 4, `pending_human`).
A person approves in `wayglass review` or the desktop app; then
`wayglass-land-when-approved` lands it.

The sign-in endpoint has no limit on password guesses. In
`internal/auth/auth.go`, lock an account for 15 minutes after 5 consecutive
failed sign-ins:

- While locked, `Login` returns a new `ErrLocked` error, even with the
  right password. A successful sign-in resets the count.
- Unknown emails must behave exactly like before (`ErrInvalidCredentials`,
  no lock state kept for them), so the lockout cannot be used to discover
  which emails have accounts.
- Use the store's `now` function for time so tests can move the clock.
- In `internal/server/server.go`, answer `ErrLocked` with
  `429 Too Many Requests` and `{"error":"account locked, try again later"}`.

Add table-driven tests in `internal/auth/auth_test.go`: lock after five
failures, still locked with the right password, unlocked after 15 minutes,
reset on success, unknown email never locks. Add the 429 case to
`internal/server/server_test.go`.

```bash
wayglass intent declare --json \
  --goal "feat(auth): lock an account for 15 minutes after 5 failed sign-ins" \
  --reason "Sign-in has no guess limit, so a password can be brute-forced online" \
  --path "internal/auth/**" --path "internal/server/**"
```
