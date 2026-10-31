# Test kit - superset-fix-jinja-dataset-refresh-fix

Runs the full tests/test.sh in the task image (built from the upload's environment/Dockerfile) with no API keys. The rubric and taste judges record skipped:no_api_key, which never affects the reward. This task has no validation_spec.toml, so the reward printed here is the verifier result.

`tests_new` is the upload's tests folder, `tests_orig` is the downloaded task's tests folder.

## Run

1. Start Docker Desktop.
2. Open a terminal in this folder and run `./run_tests.sh`.
3. If `superset-fix-jinja-dataset-refresh:latest` already exists it is reused. Otherwise the first build clones superset and installs requirements/development.txt (a few GB, 15-30 min).
4. Each case takes about a minute (the verifier migrates a SQLite metadata DB once per run).
5. Send back `results.txt` (or paste it).

## Cases

| Case | What it is |
|---|---|
| nop | no patch |
| oracle | the upload's solution/oracle.patch |
| oracle_old | the downloaded task's oracle.patch (handles only the wrapped undefined-variable error) |
| gpt, grok, opus | that trajectory's verifier/agent.patch |
| empty_columns | the reference, but the softened cases return an empty column list instead of raising |
| no_marker_check | the reference, but every parse failure is softened, Jinja markers or not |
| soften_any_template_error | the reference, but any jinja2 TemplateError (sandbox errors too) is softened |
| catch_all | no reference; the refresh command swallows every exception from fetch_metadata |
| alt_cause_check | a valid alternative: no new exception class, the refresh command inspects the error's cause |

## Expected results

| Tests | Case | Expected |
|---|---|---|
| tests_new | nop | reward 0.0, 5 of 7, fails both test_save_is_best_effort_* tests |
| tests_new | oracle | reward 1.0, 7 of 7 |
| tests_new | oracle_old | reward 0.0, 6 of 7, fails test_save_is_best_effort_on_undefined_template_variable |
| tests_new | gpt | reward 0.0, 4 of 7, fails the static SQL, security violation and genuine DB error tests |
| tests_new | grok | reward 0.0, 4 of 7, same three as gpt |
| tests_new | opus | reward 0.0, 5 of 7, fails both test_save_is_best_effort_* tests |
| tests_new | empty_columns | reward 0.0, 5 of 7, both test_save_is_best_effort_* tests fail on the column check |
| tests_new | no_marker_check | reward 0.0, 6 of 7, fails test_save_still_hard_errors_on_static_sql_parse_failure |
| tests_new | soften_any_template_error | reward 0.0, 6 of 7, fails test_save_still_hard_errors_on_template_security_violation |
| tests_new | catch_all | reward 0.0, 4 of 7, same three as gpt |
| tests_new | alt_cause_check | reward 1.0, 7 of 7 |
| tests_orig | nop | reward 0.0, 5 of 7, fails both test_save_is_best_effort_* tests |
| tests_orig | oracle_old | reward 1.0, 7 of 7 |
| tests_orig | oracle | reward 1.0, 7 of 7 |

Every case must print the `[offline-first]` line and no `APPLY_FAIL`.

Anything else means the verifier or a probe needs another look before submit.

## Clean up afterwards

```bash
docker rmi superset-jinja-kit:latest superset-fix-jinja-dataset-refresh:latest
docker builder prune -f
```
