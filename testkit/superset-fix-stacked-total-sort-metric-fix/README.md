# Test kit - superset-fix-stacked-total-sort-metric-fix

Checks the revised jest verifier against the reference patch, an empty patch, the three trajectory patches and two shortcut patches.

## Run

1. Start Docker Desktop.
2. Open Git Bash in this folder and run `./run_tests.sh`.
3. The first build clones superset and runs a full frontend `npm install` (several GB, 20+ min). Later runs reuse the image.
4. Send back `results.txt` (or paste it).

## Expected results

`orig` is the verifier from the submission, `new` is the revised one in the fixed zip.

| Patch | orig (4 tests) | new (5 tests) |
|---|---|---|
| nop (empty) | 2 pass, 2 fail (both sort-only tests) | 3 pass, 2 fail (both sort-only tests) |
| oracle | 4 pass | 5 pass |
| opus | 3 pass, verbose test fails | 4 pass, verbose test fails |
| gpt | 3 pass, verbose test fails | 4 pass, verbose test fails |
| grok | 4 pass | 5 pass |
| shortcut_totals_only | 4 pass (old hole) | 4 pass, verbose test fails |
| shortcut_sortcol | 4 pass (old hole) | 4 pass, "sorting by a displayed metric" fails |

Anything different, especially oracle not passing all 5 or a TypeScript / jest error, means the revised test needs fixing before submit.

## Clean up afterwards

```bash
docker rmi superset-kit:latest superset-fix-stacked-total-sort-metric:latest
docker builder prune -f
```
