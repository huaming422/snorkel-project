# Test kit - dragonfly-fix-rdb-stream-load-safety-fix

Runs the twelve-test verifier in the task image against an empty patch, the reference patch, and the reference patch with its end-of-listpack check turned off.

## Run

1. Start Docker Desktop.
2. Open Git Bash in this folder and run `./run_tests.sh`.
3. If the `dragonfly-fix-rdb-stream-load-safety:latest` image from the last run is still there it is reused. Otherwise the first build clones dragonfly and does a full debug build (a few GB, 30+ min).
4. Send back `results.txt` (or paste it).

## Cases

- `nop`: no patch, test-setup.sh, verifier.
- `oracle`: reference patch, test-setup.sh, verifier.
- `oracle_no_eof`: reference patch, then its `if (next || p != lp + size - 1)` check is replaced with `if (false)`, then setup and verifier. This is the partial-fix probe for the new trailing-bytes test.

## Expected results

| Case | Expected |
|---|---|
| nop | 4 passed, 8 failed. Passing: `test_wellformed_stream_roundtrips`, `test_wellformed_consumer_group_roundtrips`, `test_wellformed_group_with_deleted_entry_roundtrips`, `test_replication_full_sync_wellformed_loads` |
| oracle | 12 passed |
| oracle_no_eof | `EOF_CHECK_DISABLED=1`, then 11 passed and 1 failed, the failing one is `test_snapshot_trailing_bytes_rejected` |

In every case the setup lines must end with `SETUP_RETURNED`.

Anything else means the verifier needs another look before submit. The most likely surprise is `nop` passing `test_snapshot_trailing_bytes_rejected` or one of the new RESTORE variants, or `oracle` failing one of them.

## Clean up afterwards

```bash
docker rmi dragonfly-kit:latest dragonfly-fix-rdb-stream-load-safety:latest
docker builder prune -f
```
