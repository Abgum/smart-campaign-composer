# Testing rules

- Start every code change with a failing test. It must fail for the **right reason**: an assertion failure, not a compilation error. Add a minimal compiling stub first if needed.
- Show me the exact command and relevant lines from both the red and green output.
- For a bug fix, the test must fail against the code **before** the fix. A test that also passed before the fix proves nothing; change the test.
- Test behavior, not an intermediate value:
  - Rate-limit tests measure actual `Provider.Send` start times, not grants.
  - Stream tests read the events delivered to the client.
  - Tool-schema tests inspect the raw request sent to Claude.
- Database tests use `testutil.NewDB`. Without `TEST_DATABASE_URL`, they SKIP; **SKIP is not PASS.** Run them with `make test-backend` or `docker-compose.test.yml`, where `REQUIRE_DB=1`.
- Run backend tests with `-race`.
- Run the individual test first, then its whole package, then `make test`.
- Do not fix a flaky test by raising a timeout, sleep, or retry count. Reproduce it first, under CPU load if necessary, find the cause, then fix it.
- Use the fake clock (`internal/clock`) for time-dependent logic. Use a real clock only in tests that measure real timing.
