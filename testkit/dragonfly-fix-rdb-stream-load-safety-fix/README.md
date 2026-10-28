# Test kit - dragonfly-fix-rdb-stream-load-safety-fix

Runs the eleven-test verifier in the task image against an empty patch and the reference patch, and checks the stale-binary case the reviewer reported.

## Run

1. Start Docker Desktop.
2. Open Git Bash in this folder and run `./run_tests.sh`.
3. The first build clones dragonfly and does a full debug build (a few GB, 30+ min). Later runs reuse the image.
4. Send back `results.txt` (or paste it).

## Cases

- `nop`: no patch, revised test-setup.sh, verifier.
- `oracle`: reference patch, revised test-setup.sh, verifier.
- `stale_old`: reference patch built, then a compile error added to rdb_load.cc, then the previous test-setup.sh runs again. This is the reviewer's case.
- `stale_new`: same as `stale_old` but with the revised test-setup.sh, which deletes the binary when the build fails.

## Expected results

| Case | Expected |
|---|---|
| nop | 4 passed, 7 failed. Passing: the four well-formed tests (`test_wellformed_stream_roundtrips`, `test_wellformed_consumer_group_roundtrips`, `test_wellformed_group_with_deleted_entry_roundtrips`, `test_replication_full_sync_wellformed_loads`) |
| oracle | 11 passed |
| stale_old | 11 passed (the old hole: the reference binary is still there) |
| stale_new | 0 passed, all 11 failed or errored, the setup log shows `build failed, removing the stale binary` and `ls` shows no binary |

In every case the setup lines must end with `SETUP_RETURNED`. If it is missing, the setup script stopped test.sh, which is a bug.

Anything else, especially oracle not passing all 11 or nop not showing 4 of 11, means the verifier or setup needs another look before submit.

## Clean up afterwards

```bash
docker rmi dragonfly-kit:latest dragonfly-fix-rdb-stream-load-safety:latest
docker builder prune -f
```
