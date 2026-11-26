---
name: fix-with-red-test
description: Use when fixing a bug, implementing a plan slice, or changing code behavior in smart-campaign-composer.
---

# Fix with a failing test

Use this workflow for every code change. Documentation-only and research tasks do not require it.

## Steps

1. **Define the scope.**
   - Read the relevant plan section and decision in `DECISIONS.md`.
   - Define completion in one sentence. Ask if the requirement is unclear.
2. **Reread the files.** Someone else may be working in the same tree; do not revert their changes.
3. **Write the failing test.** Test behavior, not an intermediate value (see `.claude/rules/testing.md`).
4. **Run the test against the current code and interpret the result.**
   - Assertion fails for the intended reason → continue.
   - Compilation fails → add the smallest compiling stub and rerun.
   - **Test passes → it does not expose the bug.** Change the test before fixing the code.
   - Test SKIPs → run it in the Docker database-test environment.
5. **Show the red output.** Include the exact command and relevant output lines.
6. **Make the smallest fix.** Stay within scope. Note other issues you encounter without fixing them here.
7. **Show the green output.** Run, in order:
   1. the individual test,
   2. the whole package with `-race`,
   3. `make test`.
8. **Additional checks.**
   - Run `make lint`.
   - Answer the questions in the applicable `.claude/rules/` file (for example, `delivery.md` for delivery changes).
   - If README or DECISIONS describes the behavior, update both documents.
   - Remove code that is no longer used.
9. **Report.**
   - List changed files and summarize the diff.
   - Include the red and green test output.
   - Note issues you noticed but left untouched, and any open questions.
   - **Do not commit.**

## Commands

| Task | Command |
| --- | --- |
| One database test | `make volumes` → `docker compose -f docker-compose.test.yml run --rm backend-test go test -race -count=1 -v -run '^TestName$' ./internal/package/` → `docker compose -f docker-compose.test.yml down` |
| One test without a database | `cd backend && go test -count=1 -v -run '^TestName$' ./internal/package/` |
| All backend tests | `make test-backend` |
| All tests | `make test` |
| Formatting and static checks | `make lint` |

## Do not

- Do not raise a timeout, sleep, or retry count merely to make a test pass.
- Do not report a test that passed as having failed; show the output.
- Do not proceed to the next slice without being asked.
