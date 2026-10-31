# Test kit - bytebase-fix-pagination-total-order-fix

Runs the full tests/test.sh in the task image against the downloaded tests (`tests_orig`, 14 results) and this round's tests (`tests_new`, 32 results: 8 test functions plus 24 subtests). Cases are the empty patch, the reference patch, the three trajectory patches, and fourteen probe copies of the reference. No API keys are set, so the rubric and taste judges record skipped:no_api_key, which never affects the reward. This is a fix task with no validation_spec.toml, so the reward printed here is the same correctness Harbor reports.

## Before you start

- The task image clones bytebase, downloads every Go module of the repo and compiles all test binaries twice (once at the authoring commit, once at the pinned commit). Expect roughly 15-25 GB of image and 40-90 min for the first build.
- Check free space first: `df -h /System/Volumes/Data`. If it is under about 40 GB, clean up older task images before building.
- Give colima at least 4 CPUs and 8 GB of memory (`colima start --cpu 4 --memory 8` if it is not already set that way).

## Run

1. Start colima / Docker.
2. Open a terminal in this folder and run `./run_tests.sh`.
3. If `bytebase-fix-pagination-total-order:latest` is already there it is reused. The kit image compiles the store tests once, so each case then takes a few minutes.
4. Send back `results.txt` (or paste it).

## Cases

On `tests_orig` (downloaded tests): `nop`, `oracle`.

On `tests_new` (this round's tests):

- `nop`: no patch.
- `oracle`: solution/oracle.patch.
- `gpt`, `grok`, `opus`: that trajectory's agent.patch.
- `probe_ag_default_only`: access grants get the id tiebreak only on the default order, not after a caller's sort keys.
- `probe_rel_gpt`: releases end on created_at, project, release_id (gpt's tiebreak).
- `probe_rel_iteration`: releases end on created_at, iteration.
- `probe_rel_train`: releases end on created_at, train.
- `probe_grp_name`: groups end on email, name.
- `probe_db_name`: databases end on name without the instance.
- `probe_name_tiebreak`: workload identities, service accounts and users end on name instead of their unique key.
- `probe_no_issue_project`: issues end on id alone.
- `probe_drop_rank`: issue search drops the relevance rank.
- `probe_drop_caller_issue`: issues ignore a caller's order_by keys.
- `probe_lead_pk`: workload identities lead on email instead of creation time.
- `probe_comment_read_only`: comment read gets the resource_id tiebreak, but the batch insert is unchanged.
- `probe_comment_ctid`: comments order by physical row position, batch insert unchanged.
- `probe_fair_asc`: ascending tiebreaks on access grants, issues (project) and releases, a fair alternative.

## Expected results (worked out from the checks, not run yet)

| Set | Case | Expected |
|---|---|---|
| tests_orig | nop | `RESULT 1 of 14` |
| tests_orig | oracle | `RESULT 14 of 14` |
| tests_new | nop | `RESULT 12 of 32` (passes TestListOrderKept and its 10 subtests, and TestIssueOrderByPreserved) |
| tests_new | oracle | `RESULT 32 of 32` |
| tests_new | gpt | `RESULT 20 of 32` |
| tests_new | grok | `RESULT 31 of 32` (comment case) |
| tests_new | opus | `RESULT 24 of 32` |
| tests_new | probe_ag_default_only | `RESULT 30 of 32`: TestCallerOrderByStability/access_grant_by_creator and its parent |
| tests_new | probe_rel_gpt | `RESULT 30 of 32`: TestPaginationStability/release and its parent |
| tests_new | probe_rel_iteration | `RESULT 30 of 32`: same |
| tests_new | probe_rel_train | `RESULT 30 of 32`: same |
| tests_new | probe_grp_name | `RESULT 30 of 32`: TestPaginationStability/group and its parent |
| tests_new | probe_db_name | `RESULT 30 of 32`: TestCallerOrderByStability/database_by_project and its parent |
| tests_new | probe_name_tiebreak | `RESULT 28 of 32`: workload_identity, service_account, principal and their parent |
| tests_new | probe_no_issue_project | `RESULT 25 of 32`: cross-project, 4 caller-order subtests and parent, search |
| tests_new | probe_drop_rank | `RESULT 31 of 32`: search |
| tests_new | probe_drop_caller_issue | `RESULT 26 of 32`: 4 caller-order subtests and parent, TestIssueOrderByPreserved |
| tests_new | probe_lead_pk | `RESULT 30 of 32`: TestListOrderKept/workload_identity_oldest_first and its parent |
| tests_new | probe_comment_read_only | `RESULT 31 of 32`: comment case |
| tests_new | probe_comment_ctid | `RESULT 31 of 32`: comment case |
| tests_new | probe_fair_asc | `RESULT 32 of 32` |

Every case must print the `postgres-ready` line and no `APPLY_FAIL`. Each `WALL` line should stay well under 900 s.

## Clean up afterwards

```bash
docker rmi bytebase-kit:latest bytebase-fix-pagination-total-order:latest
docker image prune -f
docker builder prune -f
colima ssh -- sudo fstrim /var/lib/docker
```
