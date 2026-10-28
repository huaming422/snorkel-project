# Test kit - beads-fix-migration-survives-drift-fix

Runs the full tests/test.sh in the task image against an empty patch, the reference patch, and the three trajectory patches, with no API keys. The rubric and taste judges then record skipped:no_api_key, which never affects the reward. This task has no validation_spec.toml, so the reward printed here is the same correctness Harbor reports.

## Run

1. Start Docker Desktop.
2. Open Git Bash in this folder and run `./run_tests.sh`.
3. If the `beads-fix-migration-survives-drift:latest` image is already there it is reused. Otherwise the first build clones beads twice and downloads the Go module closure (a few GB, 20+ min).
4. Send back `results.txt` (or paste it).

## Cases

- `nop`: no patch, full test.sh.
- `oracle`: solution/oracle.patch, full test.sh.
- `grok`, `gpt`, `opus`: that trajectory's verifier/agent.patch, full test.sh.

## Expected results

| Case | Expected |
|---|---|
| nop | `"reward": 0.0`, `RESULT 5 of 16`. Passing: TestVerifyNonDriftAborts, the three non-drift TestVerifyDriftToleranceBreadth subtests (unrelated_recovered_panic, ordinary_sql_error, lock_wait_timeout), TestVerifyPackageSuiteGreen |
| oracle | `"reward": 1.0`, `RESULT 16 of 16` |
| grok | `"reward": 1.0`, `RESULT 16 of 16` |
| gpt | `"reward": 0.0`, `RESULT 10 of 16`. Failing: TestVerifyDriftSkipResumes and TestVerifyDriftResumeOnlySkippedTables, each parent plus both subtests |
| opus | `"reward": 0.0`, `RESULT 5 of 16`. Same passing set as nop |

Every case must print the `[test-setup] no-op` line and no `APPLY_FAIL`.

Anything else means the verifier needs another look before submit.

## Clean up afterwards

```bash
docker rmi beads-kit:latest beads-fix-migration-survives-drift:latest
docker builder prune -f
```
