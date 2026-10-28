# Test kit - beads-fix-migration-survives-drift-fix

Runs the sixteen-entry verifier (tests/verify/verify_drift_test.go through run_verify.py) in the task image against an empty patch, the reference patch, and the three trajectory patches.

## Run

1. Start Docker Desktop.
2. Open Git Bash in this folder and run `./run_tests.sh`.
3. If the `beads-fix-migration-survives-drift:latest` image is already there it is reused. Otherwise the first build clones beads twice and downloads the Go module closure (a few GB, 20+ min).
4. Send back `results.txt` (or paste it).

## Cases

- `nop`: no patch, test-setup.sh, verifier.
- `oracle`: solution/oracle.patch, test-setup.sh, verifier.
- `grok`, `gpt`, `opus`: that trajectory's verifier/agent.patch, test-setup.sh, verifier.

## Expected results

| Case | Expected |
|---|---|
| nop | `RESULT 5 of 16`. Passing: TestVerifyNonDriftAborts, the three non-drift TestVerifyDriftToleranceBreadth subtests (unrelated_recovered_panic, ordinary_sql_error, lock_wait_timeout), TestVerifyPackageSuiteGreen |
| oracle | `RESULT 16 of 16` |
| grok | `RESULT 16 of 16` |
| gpt | `RESULT 10 of 16`. Failing: TestVerifyDriftSkipResumes and TestVerifyDriftResumeOnlySkippedTables, each parent plus both subtests |
| opus | `RESULT 5 of 16`. Same passing set as nop |

Every case must print `SETUP_RETURNED` and no `APPLY_FAIL`.

Anything else means the verifier needs another look before submit.

## Clean up afterwards

```bash
docker rmi beads-kit:latest beads-fix-migration-survives-drift:latest
docker builder prune -f
```
