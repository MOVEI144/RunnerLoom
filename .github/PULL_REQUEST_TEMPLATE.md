## What and why

## Verification
- [ ] Unit/race tests and vet
- [ ] Regression test fails before the fix
- [ ] Actual VM qualification when host/image/network behavior changes
- [ ] Package/schema/docs match CLI behavior

## Safety
- [ ] No credentials, JIT configuration, private state or machine-specific keys
- [ ] No allocation released merely because a node is unreachable
- [ ] No unowned host resources changed or deleted
- [ ] Compatibility and migration impact described

Implemented, tested with doubles, and verified against real systems must be reported separately.
