"""Run development database contracts without silently skipping a database."""

import json
import os
from pathlib import Path
import subprocess
import sys


def validate_environment(environment):
    required = [
        "TEST_MYSQL_DSN", "TEST_MYSQL_LOG_DSN",
        "TEST_POSTGRES_DSN", "TEST_POSTGRES_LOG_DSN",
        "TEST_WS_MANAGER_REDIS_ADDR",
    ]
    missing = [name for name in required if not environment.get(name, "").strip()]
    if missing:
        raise ValueError("missing development test configuration: " + ", ".join(missing))


def validate_results(events):
    completed = set()
    for event in events:
        identity = (event.get("Package", ""), event.get("Test", ""))
        if event.get("Action") == "skip":
            raise ValueError("database matrix skipped: " + "/".join(identity))
        if event.get("Action") == "fail":
            raise ValueError("database matrix failed: " + "/".join(identity))
        if event.get("Action") == "pass":
            completed.add(identity)
    required = set()
    for package, test in [
        ("model", "TestMigrationSchemaStability"),
        ("model", "TestRequestPolicyDatabaseMatrix"),
        ("model", "TestCreditPackDatabaseMatrix"),
        ("model", "TestSubscriptionVersionDatabaseMatrix"),
        ("model", "TestSubscriptionVersionDatabaseMatrix/{dialect}/window_consumption"),
        ("service", "TestFixedPriceBillingDatabaseMatrix"),
        ("controller", "TestPreConsumePolicyDatabaseMatrix"),
        ("controller", "TestRequestPolicyRoutingDatabaseMatrix"),
        ("controller", "TestDeleteRedemptionBatch"),
        ("controller", "TestCreditBillingDatabaseMatrix"),
    ]:
        for dialect in ["sqlite", "mysql", "postgres"]:
            required.add((f"github.com/QuantumNous/new-api/{package}", test.format(dialect=dialect) if "{dialect}" in test else f"{test}/{dialect}"))
    required.add(("github.com/QuantumNous/new-api/pkg/wsmanager", "TestRedisChannelCloseEventsStayWithinDatabase"))
    missing = required - completed
    if missing:
        raise ValueError("database matrix missing completed contracts: " + ", ".join("/".join(item) for item in sorted(missing)))


def main():
    environment = dict(os.environ)
    try:
        validate_environment(environment)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 1
    environment["GOWORK"] = "off"
    for dialect in ["MYSQL", "POSTGRES"]:
        environment.setdefault(f"AUDIT_{dialect}_DSN", environment[f"TEST_{dialect}_DSN"])
        for suffix in ["DSN", "LOG_DSN"]:
            environment.setdefault(f"TEST_FIXED_{dialect}_{suffix}", environment[f"TEST_{dialect}_{suffix}"])
    command = [
        "go", "test", "-json", "-count=1", "-p", "1", "-timeout", "10m",
        "./model", "./service", "./controller", "./pkg/wsmanager",
        "-run", "^Test(.*DatabaseMatrix|MigrationSchemaStability|DeleteRedemptionBatch|RedisChannelCloseEventsStayWithinDatabase)$",
    ]
    events = []
    with subprocess.Popen(command, cwd=Path(__file__).resolve().parent.parent,
                          env=environment, stdout=subprocess.PIPE, text=True) as process:
        for line in process.stdout:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                print(line, end="")
                continue
            events.append(event)
            if "Output" in event:
                print(event["Output"], end="", flush=True)
        exit_code = process.wait()
    if exit_code:
        return exit_code
    try:
        validate_results(events)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 1
    print("Development database matrix passed: SQLite, MySQL, PostgreSQL and real Redis; no skipped contracts.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
